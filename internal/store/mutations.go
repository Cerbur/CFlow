package store

// Applying a Decision: the closed Mutation union is applied to the
// aggregate in memory (for model validation before any write) and then
// persisted row by row inside the same transaction (design 9.2). The
// Effect Intent closed union is codec'd to the effects table.

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"cflow.local/cflow/internal/model"
)

// ---------------------------------------------------------------------------
// in-memory application (validation mirror of the persistence)
// ---------------------------------------------------------------------------

// applyMutation applies one Mutation to the aggregate in memory. It
// mirrors the persistence below exactly; the result is validated with
// model.ValidateState before any row is written.
func applyMutation(st *model.State, m model.Mutation) error {
	switch m := m.(type) {
	case model.WorkflowMutation:
		// Identity is passed through unchanged; Execution Facts are not
		// Kernel-owned and survive every Workflow Mutation.
		st.Workflow = model.Workflow{
			ID: m.ID, Project: m.Project, Stage: m.Stage, Runtime: m.Runtime,
			TargetBranch: m.TargetBranch, BaseCommit: m.BaseCommit,
			IntegrationBranch: m.IntegrationBranch, IntegrationHead: m.IntegrationHead,
			LayoutVersion:             m.LayoutVersion,
			WorkspacePath:             m.WorkspacePath,
			WorkspaceBranch:           m.WorkspaceBranch,
			CandidateWorkspaceHead:    m.CandidateWorkspaceHead,
			VerifiedWorkspaceHead:     m.VerifiedWorkspaceHead,
			WorkspaceDirtyFingerprint: m.WorkspaceDirtyFingerprint,
			CancelIntent:              m.CancelIntent, ExecutionFacts: st.Workflow.ExecutionFacts,
		}
	case model.PlanMutation:
		if st.Plan == nil {
			return fmt.Errorf("plan mutation without a plan")
		}
		st.Plan.Status = m.Status
	case model.ArtifactRefMutation:
		if !m.Type.Valid() {
			return fmt.Errorf("unknown artifact ref type %q", m.Type)
		}
		if m.Type == model.ArtifactPlan {
			status := model.PlanStatus("")
			if st.Plan != nil {
				status = st.Plan.Status
			}
			st.Plan = &model.Plan{
				Revision: m.Revision,
				Status:   status,
				Artifact: model.ArtifactRef{Workflow: st.Workflow.ID, Type: m.Type,
					Revision: m.Revision, Hash: m.Hash},
				Hash: m.Hash,
			}
		}
	case model.PreflightRecordMutation:
		// The aggregate carries preflights only through the hydrated
		// ExecutionFacts; the row itself is append-only SQLite evidence.
	case model.SessionEndMutation:
		se := findSession(st, m.ID)
		if se == nil {
			return fmt.Errorf("session %s does not exist", m.ID)
		}
		se.ProviderSessionID = m.ProviderSessionID
		se.Status = m.Status
	case model.SessionBindMutation:
		se := findSession(st, m.ID)
		if se == nil {
			return fmt.Errorf("session %s does not exist", m.ID)
		}
		se.ProviderSessionID = m.ProviderSessionID
	case model.SessionStatusMutation:
		se := findSession(st, m.ID)
		if se == nil {
			return fmt.Errorf("session %s does not exist", m.ID)
		}
		se.Status = m.Status
	case model.NodeStatusMutation:
		n := st.Nodes[m.Node]
		if n == nil {
			return fmt.Errorf("node %s does not exist", m.Node)
		}
		n.Status = m.Status
		n.RetryCharged = m.RetryCharged
	case model.NodeAppendMutation:
		if _, exists := st.Nodes[m.Node.ID]; exists {
			return fmt.Errorf("node %s is already installed", m.Node.ID)
		}
		n := m.Node
		st.Nodes[n.ID] = &n
	case model.TaskMutation:
		// The Task projection row is Store-owned (branch, base commit,
		// worktree path); the aggregate carries the branch through the
		// nodes/tasks hydration join only.
	case model.AttemptAppendMutation:
		key := m.Attempt.Key
		if _, exists := st.Attempts[key]; exists {
			return fmt.Errorf("attempt identity %s already exists and is never reused", key)
		}
		att := m.Attempt
		st.Attempts[key] = &att
	case model.AttemptEndMutation:
		a := st.Attempts[m.Key]
		if a == nil {
			return fmt.Errorf("attempt %s does not exist", m.Key)
		}
		if a.Status.IsTerminal() {
			return fmt.Errorf("terminal attempt %s reopened", m.Key)
		}
		a.Status = m.Status
		a.EndHead = m.EndHead
		a.EndDirtyFingerprint = m.EndDirtyFingerprint
		a.FailureCode = m.FailureCode
		a.Evidence = m.Evidence
		a.RetryCharged = m.RetryCharged
		a.EndedAt = m.EndedAt
	case model.FindingAppendMutation:
		st.Findings = append(st.Findings, m.Finding)
	case model.ApprovalAppendMutation:
		st.Approvals = append(st.Approvals, m.Approval)
	case model.RunAppendMutation:
		st.Runs = append(st.Runs, m.Run)
	case model.RunMutation:
		r := findRun(st, m.ID)
		if r == nil {
			return fmt.Errorf("run %s does not exist", m.ID)
		}
		r.Status = m.Status
		r.DispatchGate = m.DispatchGate
		r.StopReason = m.StopReason
		r.QuiesceSnapshot = m.QuiesceSnapshot
		if m.Status.IsTerminal() {
			r.EndedAt = st.Now
		}
	case model.SessionAppendMutation:
		st.Sessions = append(st.Sessions, m.Session)
	case model.ProcessAppendMutation:
		// A committed process row must be attributable to the workflow
		// aggregate: hydration resolves processes through their Session's
		// workflow (queryProcesses), so a session-less process would
		// commit and then vanish from every View.
		if m.Process.Session == "" {
			return fmt.Errorf("process %s has no session: a committed process row must be attributable to the workflow aggregate", m.Process.ID)
		}
		st.Processes = append(st.Processes, m.Process)
	case model.ProcessEndMutation:
		p := findProcess(st, m.ID)
		if p == nil {
			return fmt.Errorf("process %s does not exist", m.ID)
		}
		p.Status = m.Status
		p.ExitCode = m.ExitCode
		p.EndedAt = m.EndedAt
	case model.QuarantineAppendMutation:
		st.Quarantines = append(st.Quarantines, m.Quarantine)
	case model.ApplyAppendMutation:
		st.ApplyAttempts = append(st.ApplyAttempts, m.ApplyAttempt)
	case model.ApplyMutation:
		a := findApplyAttempt(st, m.ID)
		if a == nil {
			return fmt.Errorf("apply attempt %s does not exist", m.ID)
		}
		a.Status = m.Status
		a.EndedAt = m.EndedAt
		if m.StagingHead != "" {
			a.StagingHead = m.StagingHead
		}
	case model.ApplyConfirmationMutation:
		a := findApplyAttempt(st, m.ID)
		if a == nil {
			return fmt.Errorf("apply attempt %s does not exist", m.ID)
		}
		a.Preflight = m.Preflight
		a.PreflightHash = m.PreflightHash
		a.Fingerprint = m.Fingerprint
	case model.CleanupAppendMutation:
		st.CleanupAttempts = append(st.CleanupAttempts, m.CleanupAttempt)
	case model.CleanupMutation:
		c := findCleanupAttempt(st, m.ID)
		if c == nil {
			return fmt.Errorf("cleanup attempt %s does not exist", m.ID)
		}
		c.Status = m.Status
		c.EndedAt = m.EndedAt
	case model.CleanupItemMutation:
		c := findCleanupAttempt(st, m.Attempt)
		if c == nil {
			return fmt.Errorf("cleanup attempt %s does not exist", m.Attempt)
		}
		if m.Index < 0 || m.Index >= len(c.Items) {
			return fmt.Errorf("cleanup item index %d out of range", m.Index)
		}
		item := &c.Items[m.Index]
		if item.Status.IsTerminal() {
			return fmt.Errorf("terminal cleanup item %d reopened", m.Index)
		}
		item.Status = m.Status
		item.FailureCode = m.FailureCode
	default:
		return fmt.Errorf("unknown mutation %T", m)
	}
	return nil
}

func findSession(st *model.State, id model.SessionID) *model.Session {
	for i := range st.Sessions {
		if st.Sessions[i].ID == id {
			return &st.Sessions[i]
		}
	}
	return nil
}

func findRun(st *model.State, id model.RunID) *model.Run {
	for i := range st.Runs {
		if st.Runs[i].ID == id {
			return &st.Runs[i]
		}
	}
	return nil
}

func findProcess(st *model.State, id model.ProcessID) *model.ProcessRecord {
	for i := range st.Processes {
		if st.Processes[i].ID == id {
			return &st.Processes[i]
		}
	}
	return nil
}

func findApplyAttempt(st *model.State, id model.ApplyAttemptID) *model.ApplyAttempt {
	for i := range st.ApplyAttempts {
		if st.ApplyAttempts[i].ID == id {
			return &st.ApplyAttempts[i]
		}
	}
	return nil
}

func findCleanupAttempt(st *model.State, id model.CleanupAttemptID) *model.CleanupAttempt {
	for i := range st.CleanupAttempts {
		if st.CleanupAttempts[i].ID == id {
			return &st.CleanupAttempts[i]
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// persistence
// ---------------------------------------------------------------------------

// persistMutation writes one Mutation to the database inside the open
// transaction. st is the aggregate after application (used for
// count-derived identities); existed reports whether the Workflow row
// predates this Decision (the identity-establishing INSERT vs UPDATE);
// now is the decision clock.
func persistMutation(ctx context.Context, q querier, st model.State, existed bool, m model.Mutation, now time.Time) error {
	nowText := now.Format(time.RFC3339Nano)
	switch m := m.(type) {
	case model.WorkflowMutation:
		var cancelAt, cancelReason any // nil -> SQL NULL
		if m.CancelIntent != nil {
			cancelAt = nowText
			cancelReason = m.CancelIntent.Reason
		}
		// The aggregate admits only {1=legacy, 2=aggregated}; a create
		// that predates layout wiring carries 0, which is persisted as
		// the design default 1 (design 7: new Workflows are created with
		// the aggregated layout once the workspace is wired, Task 4).
		layoutVersion := m.LayoutVersion
		if layoutVersion < 1 {
			layoutVersion = 1
		}
		if !existed {
			// The identity-establishing INSERT (creation).
			if _, err := q.ExecContext(ctx, `INSERT INTO workflows
				(id, project_id, stage, runtime_status, target_branch, base_commit,
				 integration_branch, integration_head,
				 layout_version, workspace_path, workspace_branch,
				 candidate_workspace_head, verified_workspace_head, workspace_dirty_fingerprint,
				 cancel_requested_at, cancel_reason, created_at, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				m.ID, m.Project, string(m.Stage), string(m.Runtime),
				m.TargetBranch, m.BaseCommit, m.IntegrationBranch, m.IntegrationHead,
				layoutVersion, m.WorkspacePath, m.WorkspaceBranch,
				m.CandidateWorkspaceHead, m.VerifiedWorkspaceHead, m.WorkspaceDirtyFingerprint,
				cancelAt, cancelReason, nowText, nowText); err != nil {
				return fmt.Errorf("insert workflow: %w", err)
			}
			return nil
		}
		if _, err := q.ExecContext(ctx, `UPDATE workflows
			SET stage = ?, runtime_status = ?, target_branch = ?, base_commit = ?,
			    integration_branch = ?, integration_head = ?,
			    layout_version = ?, workspace_path = ?, workspace_branch = ?,
			    candidate_workspace_head = ?, verified_workspace_head = ?, workspace_dirty_fingerprint = ?,
			    cancel_requested_at = ?, cancel_reason = ?, updated_at = ?
			WHERE id = ?`,
			string(m.Stage), string(m.Runtime), m.TargetBranch, m.BaseCommit,
			m.IntegrationBranch, m.IntegrationHead,
			layoutVersion, m.WorkspacePath, m.WorkspaceBranch,
			m.CandidateWorkspaceHead, m.VerifiedWorkspaceHead, m.WorkspaceDirtyFingerprint,
			cancelAt, cancelReason, nowText, m.ID); err != nil {
			return fmt.Errorf("update workflow: %w", err)
		}
		return nil

	case model.PlanMutation:
		if _, err := q.ExecContext(ctx, `UPDATE workflows SET plan_status = ? WHERE id = ?`,
			string(m.Status), st.Workflow.ID); err != nil {
			return fmt.Errorf("update plan status: %w", err)
		}
		return nil

	case model.PreflightRecordMutation:
		// One immutable Git Commit Preflight row per revision (PRD 已确
		// 认：Git Commit Identity 与 Signing Preflight). The revision is
		// Kernel-assigned; the row is append-only.
		if _, err := q.ExecContext(ctx, `INSERT INTO git_commit_preflights
			(id, workflow_id, revision, repository_context, git_version,
			 commit_policy_fingerprint, identity_json, signing_policy_json,
			 probe_status, artifact_path, artifact_sha256, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			fmt.Sprintf("preflight-%s-%d", st.Workflow.ID, m.Revision), st.Workflow.ID, m.Revision,
			nullIfEmpty(m.RepositoryContext), nullIfEmpty(m.GitVersion), m.Fingerprint,
			nullIfEmpty(m.IdentityJSON), nullIfEmpty(m.SigningPolicyJSON),
			nullIfEmpty(m.ProbeStatus), nullIfEmpty(m.ArtifactPath),
			nullIfEmpty(m.ArtifactHash), nowText); err != nil {
			return fmt.Errorf("insert preflight: %w", err)
		}
		return nil

	case model.ArtifactRefMutation:
		if !m.Type.Valid() {
			return fmt.Errorf("unknown artifact ref type %q", m.Type)
		}
		// The upsert records the active Revision of one Artifact Type; the
		// immutable body itself lives in the Artifact Store.
		if _, err := q.ExecContext(ctx, `INSERT INTO workflow_artifact_refs
			(workflow_id, artifact_type, active_revision, artifact_path, artifact_sha256, updated_at)
			VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT(workflow_id, artifact_type) DO UPDATE SET
				active_revision = excluded.active_revision,
				artifact_path = excluded.artifact_path,
				artifact_sha256 = excluded.artifact_sha256,
				updated_at = excluded.updated_at`,
			st.Workflow.ID, string(m.Type), m.Revision, m.Path, m.Hash, nowText); err != nil {
			return fmt.Errorf("upsert artifact ref: %w", err)
		}
		return nil

	case model.NodeStatusMutation:
		if _, err := q.ExecContext(ctx, `UPDATE nodes
			SET status = ?, retry_budget_consumed = ?, updated_at = ?
			WHERE id = ? AND workflow_id = ?`,
			string(m.Status), m.RetryCharged, nowText, m.Node, st.Workflow.ID); err != nil {
			return fmt.Errorf("update node: %w", err)
		}
		return nil

	case model.NodeAppendMutation:
		n := m.Node
		// The Task projection row precedes the Node row (the nodes.task_id
		// foreign key references it) and carries the deterministic Task
		// Branch. Non-task Nodes (verify, merge, checkpoint, final-verify)
		// have no Task row.
		var taskID any
		if n.Branch != "" {
			// The Task projection row: the spec_id column binds the
			// UNIQUE(workflow_id, spec_id) row identity, so it carries the
			// Node identity (the spec linkage itself lives in the approved
			// Workflow Artifact).
			if _, err := q.ExecContext(ctx, `INSERT INTO tasks
				(id, workflow_id, spec_id, title, branch_name, created_at, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, ?)`,
				n.ID, st.Workflow.ID, n.ID, "task "+string(n.ID), n.Branch, nowText, nowText); err != nil {
				return fmt.Errorf("insert task: %w", err)
			}
			taskID = n.ID
		}
		if _, err := q.ExecContext(ctx, `INSERT INTO nodes
			(id, workflow_id, task_id, node_type, status, max_retry_budget, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			n.ID, st.Workflow.ID, taskID, string(n.Kind), string(n.Status), n.RetryBudget, nowText, nowText); err != nil {
			return fmt.Errorf("insert node: %w", err)
		}
		return nil

	case model.TaskMutation:
		// task_base_commit is recorded once at readiness and never
		// replaced (PRD Worktree 策略: the Task never silently rebases);
		// worktree_path records the created Task Worktree.
		if m.BaseCommit != "" {
			if _, err := q.ExecContext(ctx, `UPDATE tasks
				SET task_base_commit = CASE WHEN task_base_commit IS NULL OR task_base_commit = '' THEN ? ELSE task_base_commit END,
				    updated_at = ?
				WHERE id = ? AND workflow_id = ?`,
				m.BaseCommit, nowText, m.Node, st.Workflow.ID); err != nil {
				return fmt.Errorf("record task base: %w", err)
			}
		}
		if m.WorktreePath != "" {
			if _, err := q.ExecContext(ctx, `UPDATE tasks
				SET worktree_path = ?, updated_at = ?
				WHERE id = ? AND workflow_id = ?`,
				m.WorktreePath, nowText, m.Node, st.Workflow.ID); err != nil {
				return fmt.Errorf("record task worktree: %w", err)
			}
		}
		return nil

	case model.AttemptAppendMutation:
		a := m.Attempt
		var session any
		if a.Session != "" {
			session = a.Session
		}
		if _, err := q.ExecContext(ctx, `INSERT INTO node_attempts
			(id, node_id, attempt_number, status, session_id, start_head_commit,
			 start_dirty_fingerprint, evidence_manifest_json, started_at, retry_budget_charged)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			fmt.Sprintf("%s#%d", a.Key.Node, a.Key.Number), a.Key.Node, int(a.Key.Number),
			string(a.Status), session, nullIfEmpty(a.StartHead), nullIfEmpty(a.StartDirtyFingerprint),
			marshalEvidence(nil), a.StartedAt.UTC().Format(time.RFC3339Nano), boolInt(a.RetryCharged)); err != nil {
			return fmt.Errorf("insert attempt: %w", err)
		}
		return nil

	case model.AttemptEndMutation:
		if _, err := q.ExecContext(ctx, `UPDATE node_attempts
			SET status = ?, end_head_commit = ?, end_dirty_fingerprint = ?,
			    error_code = ?, evidence_manifest_json = ?, retry_budget_charged = ?, ended_at = ?
			WHERE node_id = ? AND attempt_number = ?`,
			string(m.Status), nullIfEmpty(m.EndHead), nullIfEmpty(m.EndDirtyFingerprint),
			nullIfEmpty(string(m.FailureCode)), marshalEvidence(m.Evidence), boolInt(m.RetryCharged),
			m.EndedAt.UTC().Format(time.RFC3339Nano), m.Key.Node, int(m.Key.Number)); err != nil {
			return fmt.Errorf("end attempt: %w", err)
		}
		return nil

	case model.FindingAppendMutation:
		f := m.Finding
		severity := "INFO"
		if f.Blocking {
			severity = "BLOCKING"
		}
		evidence := "{}"
		if f.Evidence != (model.EvidenceRef{}) {
			if body, err := json.Marshal(f.Evidence); err == nil {
				evidence = string(body)
			}
		}
		if _, err := q.ExecContext(ctx, `INSERT INTO findings
			(id, project_id, workflow_id, code, severity, scope, subject, finding_text, seq,
			 evidence_json, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			f.ID, st.Workflow.Project, st.Workflow.ID, string(f.Code), severity,
			string(f.Scope), f.Subject, f.Text, f.Seq, evidence, nowText); err != nil {
			return fmt.Errorf("insert finding: %w", err)
		}
		return nil

	case model.ApprovalAppendMutation:
		a := m.Approval
		var planRev, specsRev, catalogRev, workflowRev any
		var planHash, specsHash, catalogHash, workflowHash any
		for _, r := range a.Refs {
			switch r.Type {
			case model.ArtifactPlan:
				planRev, planHash = r.Revision, r.Hash
			case model.ArtifactSpec:
				specsRev, specsHash = r.Revision, r.Hash
			case model.ArtifactCatalog:
				catalogRev, catalogHash = r.Revision, r.Hash
			case model.ArtifactWorkflow:
				workflowRev, workflowHash = r.Revision, r.Hash
			}
		}
		decisionContext := a.DecisionContext
		if decisionContext == "" {
			decisionContext = "{}"
		}
		preflightRev := any(nil)
		if a.PreflightRevision > 0 {
			preflightRev = a.PreflightRevision
		}
		if _, err := q.ExecContext(ctx, `INSERT INTO approvals
			(id, workflow_id, gate_type, decision, seq, plan_revision, plan_sha256,
			 specs_revision, specs_sha256, verification_catalog_revision, verification_catalog_sha256,
			 dynamic_workflow_revision, dynamic_workflow_sha256, git_commit_preflight_revision,
			 git_commit_policy_fingerprint, decision_context_json, created_at)
			VALUES (?, ?, ?, 'APPROVE', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			a.ID, st.Workflow.ID, approvalGateType(a.Kind), a.Seq, planRev, planHash,
			specsRev, specsHash, catalogRev, catalogHash, workflowRev, workflowHash,
			preflightRev, nullIfEmpty(a.Fingerprint), decisionContext, nowText); err != nil {
			return fmt.Errorf("insert approval: %w", err)
		}
		return nil

	case model.RunAppendMutation:
		r := m.Run
		if _, err := q.ExecContext(ctx, `INSERT INTO runs
			(id, workflow_id, status, dispatch_gate, started_at)
			VALUES (?, ?, ?, ?, ?)`,
			r.ID, st.Workflow.ID, string(r.Status), boolInt(r.DispatchGate),
			r.StartedAt.UTC().Format(time.RFC3339Nano)); err != nil {
			return fmt.Errorf("insert run: %w", err)
		}
		return nil

	case model.RunMutation:
		var endedAt any
		if m.Status.IsTerminal() {
			endedAt = nowText
		}
		if _, err := q.ExecContext(ctx, `UPDATE runs
			SET status = ?, dispatch_gate = ?, stop_reason = ?, quiesce_snapshot_json = ?, ended_at = ?
			WHERE id = ? AND workflow_id = ?`,
			string(m.Status), boolInt(m.DispatchGate), nullIfEmpty(string(m.StopReason)),
			marshalKeys(m.QuiesceSnapshot), endedAt, m.ID, st.Workflow.ID); err != nil {
			return fmt.Errorf("update run: %w", err)
		}
		return nil

	case model.SessionAppendMutation:
		se := m.Session
		var supersedes any
		if se.Supersedes != "" {
			supersedes = se.Supersedes
		}
		if _, err := q.ExecContext(ctx, `INSERT INTO sessions
			(id, workflow_id, supersedes_session_id, purpose, provider, provider_session_id, status,
			 context_bundle_revision, context_bundle_path, context_bundle_sha256, started_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			se.ID, st.Workflow.ID, supersedes, string(se.Purpose),
			nullIfEmpty(m.Provider), nullIfEmpty(se.ProviderSessionID), string(se.Status),
			intOrNil(se.ContextBundleRevision), nullIfEmpty(se.ContextBundlePath), nullIfEmpty(se.ContextBundleSha256),
			nowText); err != nil {
			return fmt.Errorf("insert session: %w", err)
		}
		return nil

	case model.SessionEndMutation:
		if _, err := q.ExecContext(ctx, `UPDATE sessions
			SET provider_session_id = ?, status = ?, ended_at = ?
			WHERE id = ? AND workflow_id = ?`,
			nullIfEmpty(m.ProviderSessionID), string(m.Status),
			m.EndedAt.UTC().Format(time.RFC3339Nano), m.ID, st.Workflow.ID); err != nil {
			return fmt.Errorf("end session: %w", err)
		}
		return nil

	case model.SessionBindMutation:
		if _, err := q.ExecContext(ctx, `UPDATE sessions
			SET provider_session_id = ?
			WHERE id = ? AND workflow_id = ?`,
			nullIfEmpty(m.ProviderSessionID), m.ID, st.Workflow.ID); err != nil {
			return fmt.Errorf("bind session: %w", err)
		}
		return nil

	case model.SessionStatusMutation:
		if _, err := q.ExecContext(ctx, `UPDATE sessions
			SET status = ?
			WHERE id = ? AND workflow_id = ?`,
			string(m.Status), m.ID, st.Workflow.ID); err != nil {
			return fmt.Errorf("set session status: %w", err)
		}
		return nil

	case model.ProcessAppendMutation:
		p := m.Process
		// The Session must belong to this workflow: hydration resolves
		// processes through their Session's workflow, so a process bound
		// to another workflow's session would commit and then never
		// hydrate into this aggregate.
		var n int
		if err := q.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sessions WHERE id = ? AND workflow_id = ?`,
			p.Session, st.Workflow.ID).Scan(&n); err != nil {
			return fmt.Errorf("process session check: %w", err)
		}
		if n == 0 {
			return fmt.Errorf("process %s session %s does not belong to workflow %s",
				p.ID, p.Session, st.Workflow.ID)
		}
		if _, err := q.ExecContext(ctx, `INSERT INTO managed_processes
			(id, session_id, process_type, status, started_at)
			VALUES (?, ?, ?, ?, ?)`,
			p.ID, p.Session, string(p.Purpose), string(p.Status),
			p.StartedAt.UTC().Format(time.RFC3339Nano)); err != nil {
			return fmt.Errorf("insert process: %w", err)
		}
		return nil

	case model.ProcessEndMutation:
		if _, err := q.ExecContext(ctx, `UPDATE managed_processes
			SET status = ?, exit_code = ?, ended_at = ? WHERE id = ?`,
			string(m.Status), m.ExitCode, m.EndedAt.UTC().Format(time.RFC3339Nano), m.ID); err != nil {
			return fmt.Errorf("end process: %w", err)
		}
		return nil

	case model.QuarantineAppendMutation:
		qm := m.Quarantine
		if qm.ID == "" || qm.AuditRef == "" {
			return fmt.Errorf("insert quarantine: the unique id and audit ref are required")
		}
		if _, err := q.ExecContext(ctx, `INSERT INTO branch_quarantines
			(id, workflow_id, branch_name, head_commit, audit_ref, reason_code, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			qm.ID, st.Workflow.ID, qm.Branch, nullIfEmpty(qm.ToHead), qm.AuditRef,
			string(qm.Code), nowText); err != nil {
			return fmt.Errorf("insert quarantine: %w", err)
		}
		return nil

	case model.ApplyAppendMutation:
		a := m.ApplyAttempt
		preflightType := a.Preflight.Type
		if preflightType == "" {
			preflightType = "commit-preflight"
		}
		// attempt_number follows the same count-derived identity the
		// Kernel uses for Apply Attempt IDs.
		if _, err := q.ExecContext(ctx, `INSERT INTO apply_attempts
			(id, workflow_id, attempt_number, status, target_head_at_start, integration_head,
			 git_commit_preflight_type, git_commit_preflight_revision, git_commit_preflight_sha256,
			 git_commit_policy_fingerprint, started_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			a.ID, st.Workflow.ID, len(st.ApplyAttempts), string(a.Status),
			a.TargetHead, a.IntegrationHead, preflightType,
			intOrNil(a.Preflight.Revision), nullIfEmpty(a.Preflight.Hash),
			nullIfEmpty(a.Fingerprint), a.StartedAt.UTC().Format(time.RFC3339Nano)); err != nil {
			return fmt.Errorf("insert apply attempt: %w", err)
		}
		return nil

	case model.ApplyMutation:
		// A facts-only mutation (the staging head) carries no status: the
		// transition columns stay unchanged then. The staging head is
		// persisted (004): the explicit delivery re-asserts the live Apply
		// Branch ref against the recorded reviewed head.
		status := string(m.Status)
		ended := m.EndedAt.UTC().Format(time.RFC3339Nano)
		if status == "" {
			ended = ""
		}
		if _, err := q.ExecContext(ctx, `UPDATE apply_attempts
			SET status = COALESCE(NULLIF(?, ''), status), ended_at = COALESCE(NULLIF(?, ''), ended_at),
			    staging_head = COALESCE(NULLIF(?, ''), staging_head)
			WHERE id = ? AND workflow_id = ?`,
			status, ended, m.StagingHead, m.ID, st.Workflow.ID); err != nil {
			return fmt.Errorf("update apply attempt: %w", err)
		}
		return nil

	case model.ApplyConfirmationMutation:
		if _, err := q.ExecContext(ctx, `UPDATE apply_attempts
			SET git_commit_preflight_type = ?, git_commit_preflight_revision = ?,
			    git_commit_preflight_sha256 = ?, git_commit_policy_fingerprint = ?
			WHERE id = ? AND workflow_id = ?`,
			m.Preflight.Type, intOrNil(m.Preflight.Revision), nullIfEmpty(m.PreflightHash),
			nullIfEmpty(m.Fingerprint), m.ID, st.Workflow.ID); err != nil {
			return fmt.Errorf("update apply confirmation: %w", err)
		}
		return nil

	case model.CleanupAppendMutation:
		c := m.CleanupAttempt
		planPath := fmt.Sprintf("cleanup/cleanup-plan-%s.json", c.ID)
		if _, err := q.ExecContext(ctx, `INSERT INTO cleanup_attempts
			(id, workflow_id, status, plan_path, plan_sha256, started_at)
			VALUES (?, ?, ?, ?, ?, ?)`,
			c.ID, st.Workflow.ID, string(c.Status), planPath, c.Manifest.Hash,
			c.StartedAt.UTC().Format(time.RFC3339Nano)); err != nil {
			return fmt.Errorf("insert cleanup attempt: %w", err)
		}
		for i, item := range c.Items {
			if _, err := q.ExecContext(ctx, `INSERT INTO cleanup_items
				(id, cleanup_attempt_id, ordinal, target_type, canonical_path,
				 expected_branch, expected_head_commit, expected_fingerprint, status)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				fmt.Sprintf("%s-%d", c.ID, i), c.ID, item.Index, string(item.Kind),
				item.CanonicalPath, nullIfEmpty(item.Branch), nullIfEmpty(item.ExpectedHead),
				item.Fingerprint, string(item.Status)); err != nil {
				return fmt.Errorf("insert cleanup item: %w", err)
			}
		}
		// 002 capability: the immutable Manifest binding row.
		if _, err := q.ExecContext(ctx, `INSERT INTO cleanup_manifest_bindings
			(cleanup_attempt_id, manifest_path, manifest_sha256, binding_sha256, created_at)
			VALUES (?, ?, ?, ?, ?)`,
			c.ID, planPath, c.Manifest.Hash,
			cleanupBindingHash(string(c.ID), planPath, c.Manifest.Hash), nowText); err != nil {
			return fmt.Errorf("insert cleanup manifest binding: %w", err)
		}
		return nil

	case model.CleanupMutation:
		// ended_at is set only when the attempt settles terminal; a RUNNING
		// or AWAITING_CONFIRMATION transition leaves it NULL (the zero-time
		// string is never persisted).
		ended := any(nil)
		switch m.Status {
		case model.CleanupStatusSucceeded, model.CleanupStatusFailed,
			model.CleanupStatusBlocked, model.CleanupStatusCancelled:
			ended = m.EndedAt.UTC().Format(time.RFC3339Nano)
		}
		if _, err := q.ExecContext(ctx, `UPDATE cleanup_attempts
			SET status = ?, ended_at = ? WHERE id = ? AND workflow_id = ?`,
			string(m.Status), ended, m.ID, st.Workflow.ID); err != nil {
			return fmt.Errorf("update cleanup attempt: %w", err)
		}
		return nil

	case model.CleanupItemMutation:
		if _, err := q.ExecContext(ctx, `UPDATE cleanup_items
			SET status = ?, error_code = ?
			WHERE cleanup_attempt_id = ? AND ordinal = ?`,
			string(m.Status), nullIfEmpty(string(m.FailureCode)), m.Attempt, m.Index); err != nil {
			return fmt.Errorf("update cleanup item: %w", err)
		}
		return nil

	default:
		return fmt.Errorf("unknown mutation %T", m)
	}
}
