package store

// Aggregate hydration and persistence statements. The schema itself lives
// in the embedded migrations; this file owns every SQL statement the Store
// executes and the model<->row mapping (design 9, PRD 核心数据库表).

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"cflow.local/cflow/internal/model"
)

// querier is the subset of *sql.DB and *sql.Tx hydration needs.
type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// rowScanner is the rows interface hydration closures need.
type rowScanner interface{ Scan(dest ...any) error }

// forEachRow iterates one hydration query. It is deliberately free of
// generics: hydration rows carry per-entity scan logic.
func forEachRow(ctx context.Context, q querier, query string, args []any, fn func(row rowScanner) error) error {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("query: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		if err := fn(rows); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("rows: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// hydration queries
// ---------------------------------------------------------------------------

const queryWorkflowRow = `
	SELECT id, project_id, stage, runtime_status, plan_status, aggregate_version,
	       COALESCE(target_branch, ''), COALESCE(base_commit, ''),
	       COALESCE(integration_branch, ''), cancel_requested_at, cancel_reason
	FROM workflows WHERE id = ?`

const queryArtifactRefs = `
	SELECT artifact_type, active_revision, artifact_path, artifact_sha256
	FROM workflow_artifact_refs WHERE workflow_id = ? ORDER BY artifact_type`

const queryLatestPreflight = `
	SELECT artifact_sha256, commit_policy_fingerprint
	FROM git_commit_preflights WHERE workflow_id = ? ORDER BY revision DESC LIMIT 1`

const queryNodes = `
	SELECT n.id, COALESCE(t.branch_name, ''), n.node_type, n.status,
	       n.retry_budget_consumed, n.max_retry_budget
	FROM nodes n LEFT JOIN tasks t ON t.id = n.task_id
	WHERE n.workflow_id = ? ORDER BY n.id`

const queryAttempts = `
	SELECT na.node_id, na.attempt_number, na.status, COALESCE(na.session_id, ''),
	       COALESCE(na.start_head_commit, ''), COALESCE(na.start_dirty_fingerprint, ''),
	       COALESCE(na.end_head_commit, ''), COALESCE(na.end_dirty_fingerprint, ''),
	       na.evidence_manifest_json, na.retry_budget_charged, COALESCE(na.error_code, ''),
	       na.started_at, COALESCE(na.ended_at, '')
	FROM node_attempts na JOIN nodes n ON n.id = na.node_id
	WHERE n.workflow_id = ? ORDER BY na.node_id, na.attempt_number`

const queryApprovals = `
	SELECT id, gate_type, seq, plan_revision, plan_sha256, specs_revision, specs_sha256,
	       verification_catalog_revision, verification_catalog_sha256,
	       dynamic_workflow_revision, dynamic_workflow_sha256,
	       COALESCE(git_commit_policy_fingerprint, '')
	FROM approvals WHERE workflow_id = ? ORDER BY created_at, id`

const queryFindings = `
	SELECT id, code, severity, scope, subject, finding_text, seq, evidence_json
	FROM findings WHERE workflow_id = ? ORDER BY id`

const querySessions = `
	SELECT id, COALESCE(supersedes_session_id, ''), purpose,
	       COALESCE(provider, ''), COALESCE(provider_session_id, ''), status
	FROM sessions WHERE workflow_id = ? ORDER BY id`

const queryProcesses = `
	SELECT mp.id, COALESCE(mp.session_id, ''), mp.process_type, mp.status,
	       mp.exit_code, mp.started_at, COALESCE(mp.ended_at, '')
	FROM managed_processes mp
	LEFT JOIN sessions ss ON ss.id = mp.session_id
	LEFT JOIN runs r ON r.id = mp.run_id
	WHERE ss.workflow_id = ? OR r.workflow_id = ? ORDER BY mp.id`

const queryRuns = `
	SELECT id, status, dispatch_gate, COALESCE(stop_reason, ''),
	       COALESCE(quiesce_snapshot_json, '[]'), started_at, COALESCE(ended_at, '')
	FROM runs WHERE workflow_id = ? ORDER BY id`

const queryQuarantines = `
	SELECT branch_name, head_commit, reason_code
	FROM branch_quarantines WHERE workflow_id = ? ORDER BY id`

const queryApplyAttempts = `
	SELECT id, status, COALESCE(target_head_at_start, ''), COALESCE(integration_head, ''),
	       COALESCE(git_commit_preflight_type, 'commit-preflight'),
	       COALESCE(git_commit_preflight_revision, 0),
	       COALESCE(git_commit_preflight_sha256, ''),
	       COALESCE(git_commit_policy_fingerprint, ''),
	       started_at, COALESCE(ended_at, '')
	FROM apply_attempts WHERE workflow_id = ? ORDER BY id`

const queryCleanupAttempts = `
	SELECT id, status, COALESCE(plan_path, ''), COALESCE(plan_sha256, ''),
	       started_at, COALESCE(ended_at, '')
	FROM cleanup_attempts WHERE workflow_id = ? ORDER BY id`

const queryCleanupItems = `
	SELECT cleanup_attempt_id, ordinal, target_type, canonical_path,
	       COALESCE(expected_branch, ''), COALESCE(expected_head_commit, ''),
	       COALESCE(expected_fingerprint, ''), status, COALESCE(error_code, '')
	FROM cleanup_items WHERE cleanup_attempt_id IN (SELECT id FROM cleanup_attempts WHERE workflow_id = ?)
	ORDER BY cleanup_attempt_id, ordinal`

const queryCancelRequestSeq = `
	SELECT sequence FROM events
	WHERE workflow_id = ? AND event_type = 'WORKFLOW_CANCEL_REQUESTED'
	ORDER BY sequence DESC LIMIT 1`

const queryNextEventSeq = `SELECT COALESCE(MAX(sequence), 0) + 1 FROM events`

const queryEvents = `
	SELECT sequence, event_type, workflow_id, payload_json, created_at
	FROM events WHERE workflow_id = ? AND sequence >= ? ORDER BY sequence LIMIT ?`

const queryPendingEffects = `
	SELECT id, kind, payload_json, decision_version
	FROM effects WHERE workflow_id = ? AND status = 'PENDING' ORDER BY id`

// hydrate reconstructs the aggregate for one Workflow from the database
// (design 7.1, 8.1). A Workflow that does not exist returns the zero
// State, which the Kernel treats as "not created yet". Maps are never nil
// so decisions may append into them.
func hydrate(ctx context.Context, q querier, workflow model.WorkflowID) (model.State, error) {
	st := model.State{
		Now:      time.Now().UTC(),
		Nodes:    map[model.NodeID]*model.Node{},
		Attempts: map[model.AttemptKey]*model.Attempt{},
	}
	if workflow == "" {
		return st, nil
	}

	var (
		id, project, stage, runtime string
		planStatus                  sql.NullString
		version                     uint64
		targetBranch, baseCommit    string
		integrationBranch           string
		cancelAt, cancelReason      sql.NullString
	)
	err := q.QueryRowContext(ctx, queryWorkflowRow, workflow).Scan(
		&id, &project, &stage, &runtime, &planStatus, &version,
		&targetBranch, &baseCommit, &integrationBranch, &cancelAt, &cancelReason)
	if errors.Is(err, sql.ErrNoRows) {
		return st, nil
	}
	if err != nil {
		return model.State{}, fmt.Errorf("hydrate workflow: %w", err)
	}
	st.Version = model.AggregateVersion(version)
	st.Workflow = model.Workflow{
		ID: model.WorkflowID(id), Project: model.ProjectID(project),
		Stage: model.WorkflowStage(stage), Runtime: model.RuntimeStatus(runtime),
		TargetBranch: targetBranch, BaseCommit: baseCommit,
		IntegrationBranch: integrationBranch,
	}
	if cancelAt.Valid {
		st.Workflow.CancelIntent = &model.CancelIntent{Reason: cancelReason.String}
		if seq, err := cancelRequestSeq(ctx, q, workflow); err == nil {
			st.Workflow.CancelIntent.RequestedSeq = seq
		}
	}

	// Active Artifact references and the current Commit Preflight compose
	// the Execution Facts an Execution Approval binds by exact hash
	// (design 20.1). No refs at all means the Workflow has no Execution
	// Facts yet.
	refs := map[model.ArtifactType]artifactRefRow{}
	if err := forEachRow(ctx, q, queryArtifactRefs, []any{workflow}, func(row rowScanner) error {
		var r artifactRefRow
		if err := row.Scan(&r.Type, &r.Revision, &r.Path, &r.Hash); err != nil {
			return fmt.Errorf("scan artifact ref: %w", err)
		}
		refs[model.ArtifactType(r.Type)] = r
		return nil
	}); err != nil {
		return model.State{}, err
	}
	if planRef, ok := refs[model.ArtifactPlan]; ok {
		status := model.PlanStatus("")
		if planStatus.Valid {
			status = model.PlanStatus(planStatus.String)
		}
		st.Plan = &model.Plan{
			Revision: planRef.Revision,
			Status:   status,
			Artifact: model.ArtifactRef{Workflow: workflow, Type: model.ArtifactPlan,
				Revision: planRef.Revision, Hash: planRef.Hash},
			Hash: planRef.Hash,
		}
	}
	var facts *model.ExecutionFacts
	if h, ok := refs[model.ArtifactPlan]; ok {
		facts = &model.ExecutionFacts{PlanHash: h.Hash}
	}
	if h, ok := refs[model.ArtifactSpec]; ok {
		if facts == nil {
			facts = &model.ExecutionFacts{}
		}
		facts.SpecHashes = []string{h.Hash}
	}
	if h, ok := refs[model.ArtifactCatalog]; ok {
		if facts == nil {
			facts = &model.ExecutionFacts{}
		}
		facts.CatalogHash = h.Hash
	}
	if h, ok := refs[model.ArtifactWorkflow]; ok {
		if facts == nil {
			facts = &model.ExecutionFacts{}
		}
		facts.WorkflowHash = h.Hash
	}
	if h, ok := refs["routing-policy"]; ok {
		if facts == nil {
			facts = &model.ExecutionFacts{}
		}
		facts.RoutingHash = h.Hash
	}
	if h, ok := refs["budget-policy"]; ok {
		if facts == nil {
			facts = &model.ExecutionFacts{}
		}
		facts.BudgetHash = h.Hash
	}
	if facts != nil {
		var preflightHash, fingerprint sql.NullString
		if err := q.QueryRowContext(ctx, queryLatestPreflight, workflow).Scan(&preflightHash, &fingerprint); err == nil {
			facts.CommitPolicyHash = preflightHash.String
			facts.Fingerprint = fingerprint.String
		}
		st.Workflow.ExecutionFacts = facts
	}

	// Nodes (branch comes from the Task projection row).
	if err := forEachRow(ctx, q, queryNodes, []any{workflow}, func(row rowScanner) error {
		var n model.Node
		if err := row.Scan(&n.ID, &n.Branch, &n.Kind, &n.Status, &n.RetryCharged, &n.RetryBudget); err != nil {
			return fmt.Errorf("scan node: %w", err)
		}
		st.Nodes[n.ID] = &n
		return nil
	}); err != nil {
		return model.State{}, err
	}

	// Attempts.
	if err := forEachRow(ctx, q, queryAttempts, []any{workflow}, func(row rowScanner) error {
		var a model.Attempt
		var node string
		var number int
		var evidenceJSON, startedAt, endedAt string
		var retryCharged int
		if err := row.Scan(&node, &number, &a.Status, &a.Session, &a.StartHead, &a.StartDirtyFingerprint,
			&a.EndHead, &a.EndDirtyFingerprint, &evidenceJSON, &retryCharged, &a.FailureCode,
			&startedAt, &endedAt); err != nil {
			return fmt.Errorf("scan attempt: %w", err)
		}
		a.Key = model.AttemptKey{Node: model.NodeID(node), Number: model.AttemptNumber(number)}
		a.RetryCharged = retryCharged != 0
		a.StartedAt = parseTime(startedAt)
		a.EndedAt = parseTime(endedAt)
		if err := json.Unmarshal([]byte(evidenceJSON), &a.Evidence); err != nil {
			return fmt.Errorf("attempt %s evidence: %w", a.Key, err)
		}
		st.Attempts[a.Key] = &a
		return nil
	}); err != nil {
		return model.State{}, err
	}

	// Approvals (append-only, in commit order).
	if err := forEachRow(ctx, q, queryApprovals, []any{workflow}, func(row rowScanner) error {
		var ap model.Approval
		var kind string
		var planRev, specsRev, catalogRev, workflowRev sql.NullInt64
		var planHash, specsHash, catalogHash, workflowHash sql.NullString
		if err := row.Scan(&ap.ID, &kind, &ap.Seq, &planRev, &planHash, &specsRev, &specsHash,
			&catalogRev, &catalogHash, &workflowRev, &workflowHash, &ap.Fingerprint); err != nil {
			return fmt.Errorf("scan approval: %w", err)
		}
		ap.Kind = gateToApprovalKind(kind)
		ref := func(rev sql.NullInt64, hash sql.NullString, typ model.ArtifactType) {
			if rev.Valid && hash.Valid {
				ap.Refs = append(ap.Refs, model.ArtifactRef{Workflow: workflow, Type: typ,
					Revision: int(rev.Int64), Hash: hash.String})
			}
		}
		ref(planRev, planHash, model.ArtifactPlan)
		ref(specsRev, specsHash, model.ArtifactSpec)
		ref(catalogRev, catalogHash, model.ArtifactCatalog)
		ref(workflowRev, workflowHash, model.ArtifactWorkflow)
		st.Approvals = append(st.Approvals, ap)
		return nil
	}); err != nil {
		return model.State{}, err
	}

	// Findings.
	if err := forEachRow(ctx, q, queryFindings, []any{workflow}, func(row rowScanner) error {
		var f model.Finding
		var severity, scope, subject, text, evidenceJSON string
		if err := row.Scan(&f.ID, &f.Code, &severity, &scope, &subject, &text, &f.Seq, &evidenceJSON); err != nil {
			return fmt.Errorf("scan finding: %w", err)
		}
		f.Scope = model.FaultScope(scope)
		f.Subject = subject
		f.Text = text
		f.Blocking = severity == "BLOCKING"
		if err := json.Unmarshal([]byte(evidenceJSON), &f.Evidence); err != nil {
			return fmt.Errorf("finding %s evidence: %w", f.ID, err)
		}
		st.Findings = append(st.Findings, f)
		return nil
	}); err != nil {
		return model.State{}, err
	}

	// Sessions.
	if err := forEachRow(ctx, q, querySessions, []any{workflow}, func(row rowScanner) error {
		var se model.Session
		if err := row.Scan(&se.ID, &se.Supersedes, &se.Purpose, &se.Provider, &se.ProviderSessionID, &se.Status); err != nil {
			return fmt.Errorf("scan session: %w", err)
		}
		st.Sessions = append(st.Sessions, se)
		return nil
	}); err != nil {
		return model.State{}, err
	}

	// Managed Process records.
	if err := forEachRow(ctx, q, queryProcesses, []any{workflow, workflow}, func(row rowScanner) error {
		var p model.ProcessRecord
		var purpose, startedAt, endedAt string
		if err := row.Scan(&p.ID, &p.Session, &purpose, &p.Status, &p.ExitCode, &startedAt, &endedAt); err != nil {
			return fmt.Errorf("scan process: %w", err)
		}
		p.Purpose = model.AgentPurpose(purpose)
		p.StartedAt = parseTime(startedAt)
		p.EndedAt = parseTime(endedAt)
		st.Processes = append(st.Processes, p)
		return nil
	}); err != nil {
		return model.State{}, err
	}

	// Runs.
	if err := forEachRow(ctx, q, queryRuns, []any{workflow}, func(row rowScanner) error {
		var r model.Run
		var gate int
		var snapshotJSON, startedAt, endedAt string
		if err := row.Scan(&r.ID, &r.Status, &gate, &r.StopReason, &snapshotJSON, &startedAt, &endedAt); err != nil {
			return fmt.Errorf("scan run: %w", err)
		}
		r.DispatchGate = gate != 0
		if err := json.Unmarshal([]byte(snapshotJSON), &r.QuiesceSnapshot); err != nil {
			return fmt.Errorf("run %s quiesce snapshot: %w", r.ID, err)
		}
		r.StartedAt = parseTime(startedAt)
		r.EndedAt = parseTime(endedAt)
		st.Runs = append(st.Runs, r)
		return nil
	}); err != nil {
		return model.State{}, err
	}

	// Branch Quarantines.
	if err := forEachRow(ctx, q, queryQuarantines, []any{workflow}, func(row rowScanner) error {
		var qr model.Quarantine
		var head string
		if err := row.Scan(&qr.Branch, &head, &qr.Code); err != nil {
			return fmt.Errorf("scan quarantine: %w", err)
		}
		qr.FromHead = head
		qr.ToHead = head
		qr.Reason = string(qr.Code)
		st.Quarantines = append(st.Quarantines, qr)
		return nil
	}); err != nil {
		return model.State{}, err
	}

	// Apply Attempts.
	if err := forEachRow(ctx, q, queryApplyAttempts, []any{workflow}, func(row rowScanner) error {
		var a model.ApplyAttempt
		var preflightType string
		var preflightRev sql.NullInt64
		var preflightHash sql.NullString
		var startedAt, endedAt string
		if err := row.Scan(&a.ID, &a.Status, &a.TargetHead, &a.IntegrationHead, &preflightType,
			&preflightRev, &preflightHash, &a.Fingerprint, &startedAt, &endedAt); err != nil {
			return fmt.Errorf("scan apply attempt: %w", err)
		}
		a.Preflight = model.ArtifactRef{Workflow: workflow, Type: model.ArtifactType(preflightType),
			Revision: int(preflightRev.Int64), Hash: preflightHash.String}
		a.PreflightHash = preflightHash.String
		a.StartedAt = parseTime(startedAt)
		a.EndedAt = parseTime(endedAt)
		st.ApplyAttempts = append(st.ApplyAttempts, a)
		return nil
	}); err != nil {
		return model.State{}, err
	}

	// Cleanup Attempts and their immutable manifest Items.
	if err := forEachRow(ctx, q, queryCleanupAttempts, []any{workflow}, func(row rowScanner) error {
		var c model.CleanupAttempt
		var planPath, planHash, startedAt, endedAt string
		if err := row.Scan(&c.ID, &c.Status, &planPath, &planHash, &startedAt, &endedAt); err != nil {
			return fmt.Errorf("scan cleanup attempt: %w", err)
		}
		c.Manifest = model.ArtifactRef{Workflow: workflow, Type: model.ArtifactCleanupManifest,
			Revision: 0, Hash: planHash}
		c.StartedAt = parseTime(startedAt)
		c.EndedAt = parseTime(endedAt)
		st.CleanupAttempts = append(st.CleanupAttempts, c)
		return nil
	}); err != nil {
		return model.State{}, err
	}
	if err := forEachRow(ctx, q, queryCleanupItems, []any{workflow}, func(row rowScanner) error {
		var attempt string
		var item model.CleanupItem
		var failureCode string
		if err := row.Scan(&attempt, &item.Index, &item.Kind, &item.CanonicalPath, &item.Branch,
			&item.ExpectedHead, &item.Fingerprint, &item.Status, &failureCode); err != nil {
			return fmt.Errorf("scan cleanup item: %w", err)
		}
		item.Dirty = item.Fingerprint != ""
		item.FailureCode = model.Code(failureCode)
		for i := range st.CleanupAttempts {
			if st.CleanupAttempts[i].ID == model.CleanupAttemptID(attempt) {
				st.CleanupAttempts[i].Items = append(st.CleanupAttempts[i].Items, item)
				break
			}
		}
		return nil
	}); err != nil {
		return model.State{}, err
	}

	// The authoritative Event sequence: global max + 1 (design 9.2). The
	// Kernel assigns Event sequence numbers from it; the Store owns the
	// authoritative Event log.
	var next uint64
	if err := q.QueryRowContext(ctx, queryNextEventSeq).Scan(&next); err != nil {
		return model.State{}, fmt.Errorf("next event seq: %w", err)
	}
	st.NextEventSeq = next
	return st, nil
}

func cancelRequestSeq(ctx context.Context, q querier, workflow model.WorkflowID) (uint64, error) {
	var seq uint64
	if err := q.QueryRowContext(ctx, queryCancelRequestSeq, workflow).Scan(&seq); err != nil {
		return 0, err
	}
	return seq, nil
}

type artifactRefRow struct {
	Type     string
	Revision int
	Path     string
	Hash     string
}

// parseTime decodes the canonical RFC3339Nano UTC text back into a
// time.Time; an empty string yields the zero time.
func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}
