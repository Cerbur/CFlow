package app

// The Task 17 command and projection implementations (PRD 已确认：Cancel
// 逻辑终止 step 1; 执行期间 Commit Policy 漂移确认 step 3; 漂移窗口 Commit 的
// 隔离与替代执行; Replacement Execution Approval 吸收 Policy 确认): the
// cancel confirmation summary, the pending Commit Policy confirmation
// gate, and the unified Replacement Execution Approval preview — the
// Repair Specs with new Spec IDs and replaces_task_id, the successor
// Dynamic Workflow Revision, the fixed Reconciliation Manifest, and the
// fresh Commit Preflight. Same-package split of the Application seam: no
// public seam added.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	yaml "go.yaml.in/yaml/v3"

	"cflow.local/cflow/internal/artifact"
	"cflow.local/cflow/internal/compile"
	"cflow.local/cflow/internal/decision"
	"cflow.local/cflow/internal/gitflow"
	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/store"
)

// ---------------------------------------------------------------------------
// Cancel confirmation summary (PRD 已确认：Cancel 逻辑终止 step 1)
// ---------------------------------------------------------------------------

// queryCancelSummary projects the cancel confirmation: the Workflow ID,
// Stage, active Sessions and Nodes, every managed Worktree/Branch with
// its dirty state and unmerged Commits, and the preserved paths. The
// summary is display-only; the Kernel persists the cancel intent only
// after the user's default-negative confirmation.
func (a *Application) queryCancelSummary(ctx context.Context, q CancelSummaryQuery) (View, error) {
	wf, err := a.resolveQueryWorkflow(q.Workflow)
	if err != nil {
		return nil, orCtx(ctx, err)
	}
	view, err := a.readAggregate(ctx, wf, store.StoreQuery{})
	if err != nil {
		return nil, orCtx(ctx, err)
	}
	st := view.State
	if st.Workflow.ID == "" {
		return nil, model.InvalidInputFault("no such workflow: " + string(wf))
	}
	out := CancelSummaryView{
		Workflow: wf,
		Stage:    st.Workflow.Stage,
		Runtime:  st.Workflow.Runtime,
		Preserved: []string{
			"artifacts", "sqlite state", "events", "sessions", "logs",
			"verification evidence", "context bundles", "audit refs", "commits",
			"worktrees with all uncommitted content",
		},
	}
	for id, n := range st.Nodes {
		if n.Status == model.NodeRunning || n.Status == model.NodeReady {
			out.ActiveNodes = append(out.ActiveNodes, id)
		}
	}
	sort.Slice(out.ActiveNodes, func(i, j int) bool { return out.ActiveNodes[i] < out.ActiveNodes[j] })
	for _, s := range st.Sessions {
		if !s.Status.IsTerminal() {
			out.ActiveSessions = append(out.ActiveSessions, s.ID)
		}
	}
	// The managed Worktrees: the Planning Snapshot, the Integration
	// Worktree, and every Task Worktree of the persisted Nodes. The dirty
	// state and the unmerged Commit facts are observed per Worktree.
	// Layout Version 2 workflows run their planning sessions inside the
	// single Workspace (design 8.1), so the Workspace is listed there.
	planningPath, err := a.planningCWD(ctx, wf)
	if err != nil {
		return nil, err
	}
	entries := []CancelWorktree{
		{Path: planningPath, Branch: "detached@base"},
		{Path: a.integrationWorktreePath(wf), Branch: st.Workflow.IntegrationBranch},
	}
	for id, n := range st.Nodes {
		if n.Kind != model.NodeAgentTask {
			continue
		}
		path, err := a.taskWorktreePath(ctx, wf, id)
		if err != nil {
			return nil, err
		}
		entries = append(entries, CancelWorktree{Path: path, Branch: n.Branch})
	}
	for i := range entries {
		status, err := a.observeWorktree(ctx, entries[i].Path, "")
		if err != nil {
			continue
		}
		entries[i].Dirty = !status.Clean()
		entries[i].Unmerged = a.branchUnmerged(ctx, entries[i].Branch, st.Workflow.IntegrationHead)
		if entries[i].Unmerged {
			out.UnmergedCommits++
		}
	}
	out.Worktrees = entries
	return out, nil
}

// branchUnmerged reports whether the Branch's HEAD is not contained in
// the verified Integration HEAD (a Commit that never entered the trusted
// chain).
func (a *Application) branchUnmerged(ctx context.Context, branch, integrationHead string) bool {
	if a.git == nil || branch == "" || integrationHead == "" {
		return false
	}
	facts, err := a.git.Observe(ctx, gitflow.RefLookup{Ref: "refs/heads/" + branch})
	if err != nil {
		return false
	}
	rf, ok := facts.(gitflow.RefFacts)
	if !ok || !rf.Exists {
		return false
	}
	if rf.Value == integrationHead {
		return false
	}
	rangeFacts, err := a.git.Observe(ctx, gitflow.HistoryRange{From: integrationHead, To: rf.Value})
	if err != nil {
		return false
	}
	rr, ok := rangeFacts.(gitflow.RangeFacts)
	return ok && len(rr.Commits) > 0
}

// ---------------------------------------------------------------------------
// Commit Policy confirmation gate (PRD 已确认：执行期间 Commit Policy 漂移确认)
// ---------------------------------------------------------------------------

// queryPolicyConfirmation projects the pending confirmation gate: the
// exact new Preflight Revision/Hash/Fingerprint and the old fingerprint
// the drift moved away from (the finding records it).
func (a *Application) queryPolicyConfirmation(ctx context.Context, q PolicyConfirmationQuery) (View, error) {
	wf, err := a.resolveQueryWorkflow(q.Workflow)
	if err != nil {
		return nil, orCtx(ctx, err)
	}
	view, err := a.readAggregate(ctx, wf, store.StoreQuery{})
	if err != nil {
		return nil, orCtx(ctx, err)
	}
	st := view.State
	if st.Workflow.ID == "" {
		return nil, model.InvalidInputFault("no such workflow: " + string(wf))
	}
	out := PolicyConfirmationView{Workflow: wf, Stage: st.Workflow.Stage, Runtime: st.Workflow.Runtime}
	facts := st.Workflow.ExecutionFacts
	if facts != nil && !policyConfirmedOf(st) {
		out.Pending = true
		out.PreflightRevision = facts.PreflightRevision
		out.PreflightHash = facts.CommitPolicyHash
		out.Fingerprint = facts.Fingerprint
		out.OldFingerprint = driftOldFingerprint(st, facts.Fingerprint)
	}
	return out, nil
}

// driftOldFingerprint is the fingerprint the drift moved away from (the
// confirmation finding's subject records the new fingerprint; the old
// fingerprint is the previous preflight's — the finding text carries the
// diff for display).
func driftOldFingerprint(st model.State, newFingerprint string) string {
	for _, f := range st.Findings {
		if f.Code == model.CodeCommitPolicyConfirmationRequired && f.Subject != "" && f.Subject != newFingerprint {
			return f.Subject
		}
	}
	return ""
}

// policyConfirmedOf mirrors the Kernel's confirmation judgment for the
// projection: an Approval binds the latest Preflight Revision.
func policyConfirmedOf(st model.State) bool {
	facts := st.Workflow.ExecutionFacts
	if facts == nil || facts.PreflightRevision == 0 {
		return true
	}
	for _, ap := range st.Approvals {
		if ap.PreflightRevision == facts.PreflightRevision {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Replacement Execution Approval preview (PRD 已确认：漂移窗口 Commit 的隔
// 离与替代执行 / Replacement Execution Approval 吸收 Policy 确认)
// ---------------------------------------------------------------------------

// replacementPreviewInput builds the successor execution of a drift-window
// quarantine (called by prepare, before the mutation locks): the Repair
// Specs (new Spec IDs with replaces_task_id), the successor Dynamic
// Workflow Revision compiled from the approved Catalog and the successor
// Spec set, the fixed Reconciliation Manifest, and the fresh Commit
// Preflight. The Kernel records the successor references and pauses the
// Workflow at the unified Replacement Execution Approval gate.
func (a *Application) replacementPreviewInput(ctx context.Context, wf model.WorkflowID) (model.ReplacementPreviewInput, error) {
	view, err := a.readAggregate(ctx, wf, store.StoreQuery{})
	if err != nil {
		return model.ReplacementPreviewInput{}, err
	}
	st := view.State
	if st.Workflow.ID == "" {
		return model.ReplacementPreviewInput{}, model.InvalidInputFault("no such workflow: " + string(wf))
	}
	if len(st.Quarantines) == 0 || !hasDriftWindowFinding(st) {
		return model.ReplacementPreviewInput{}, model.InvalidInputFault("no drift-window blocker to replace")
	}
	if st.Workflow.Runtime != model.RuntimeBlocked && st.Workflow.Runtime != model.RuntimePaused {
		return model.ReplacementPreviewInput{}, model.InvalidInputFault("a replacement preview requires the blocked workflow at the drift gate")
	}
	facts := st.Workflow.ExecutionFacts
	if facts == nil {
		return model.ReplacementPreviewInput{}, model.InvalidInputFault("no execution facts to replace")
	}
	store, err := a.artifactStore(wf)
	if err != nil {
		return model.ReplacementPreviewInput{}, err
	}
	resolve := func(typ model.ArtifactType) (model.ArtifactRef, []byte, error) {
		ref, err := store.Resolve(ctx, artifact.ResolveRequest{WorkflowID: wf, Type: typ})
		if err != nil {
			return model.ArtifactRef{}, nil, err
		}
		body, err := store.Get(ctx, ref)
		if err != nil {
			return model.ArtifactRef{}, nil, err
		}
		return ref, body, nil
	}
	specRef, specBody, err := resolve(model.ArtifactSpec)
	if err != nil {
		return model.ReplacementPreviewInput{}, err
	}
	workflowRef, workflowBody, err := resolve(model.ArtifactWorkflow)
	if err != nil {
		return model.ReplacementPreviewInput{}, err
	}
	_, catalogBody, err := resolve(model.ArtifactCatalog)
	if err != nil {
		return model.ReplacementPreviewInput{}, err
	}

	// The successor Spec set: every Spec whose Task Node owns a
	// quarantined Branch (or is FAILED inside the drift window) is cloned
	// with a new Spec ID and replaces_task_id; uncontaminated Specs stay
	// byte-identical (their Task Nodes resume on the same Branch/Worktree).
	specBodies, err := splitSpecSetBody(specBody)
	if err != nil {
		return model.ReplacementPreviewInput{}, err
	}
	successorSpecs, _, err := a.replacementSpecs(ctx, wf, st, specBodies)
	if err != nil {
		return model.ReplacementPreviewInput{}, err
	}
	newSpecBody, err := marshalSpecSet(successorSpecs)
	if err != nil {
		return model.ReplacementPreviewInput{}, err
	}
	newSpecRef, err := store.Put(ctx, artifact.PutRequest{
		WorkflowID:    wf,
		Type:          model.ArtifactSpec,
		Revision:      specRef.Revision + 1,
		SchemaVersion: "1.0.0",
		CreatedAt:     a.now().UTC().Format(time.RFC3339),
		Producer:      artifact.ProducerRef{Purpose: "replacement"},
		Body:          newSpecBody,
	})
	if err != nil {
		return model.ReplacementPreviewInput{}, err
	}

	// The successor Dynamic Workflow Revision: the deterministic
	// Compiler over the successor Spec set, the approved Catalog, and no
	// patch (the original patch operations referenced the old Node ids
	// and are inert against the successor skeleton).
	newWorkflowBody, err := a.compileReplacementWorkflow(ctx, wf, newSpecBody, catalogBody, facts)
	if err != nil {
		return model.ReplacementPreviewInput{}, err
	}
	newWorkflowRef, err := store.Put(ctx, artifact.PutRequest{
		WorkflowID:    wf,
		Type:          model.ArtifactWorkflow,
		Revision:      workflowRef.Revision + 1,
		SchemaVersion: "1.0.0",
		CreatedAt:     a.now().UTC().Format(time.RFC3339),
		Producer:      artifact.ProducerRef{Purpose: "replacement"},
		Body:          newWorkflowBody,
	})
	if err != nil {
		return model.ReplacementPreviewInput{}, err
	}

	// The fixed Reconciliation Manifest: the classification is computed
	// from Git, Attempt, Session, and evidence facts — never from an
	// Agent claim (design 15.6). The successor references stay identical
	// for uncontaminated Nodes; the replacement Nodes are new ids.
	oldNodes, err := compile.ParseWorkflow(workflowBody)
	if err != nil {
		return model.ReplacementPreviewInput{}, err
	}
	newNodes, err := compile.ParseWorkflow(newWorkflowBody)
	if err != nil {
		return model.ReplacementPreviewInput{}, err
	}
	unchanged, resumable := a.manifestFacts(ctx, wf, st, oldNodes, newNodes)
	revision, err := a.latestManifestRevision(ctx, wf)
	if err != nil {
		return model.ReplacementPreviewInput{}, err
	}
	manifest := model.ReconciliationManifest{
		Revision: revision + 1,
		Actions:  decision.ClassifyManifest(st, unchanged, resumable),
	}
	// The manifest's self-hash is embedded in the persisted body: the
	// hash covers the canonical body without the hash field (marshal →
	// hash → set → re-marshal), so the displayed and approval-bound hash
	// equals the persisted body's identity and the Recovery re-check can
	// recompute it deterministically.
	body, err := manifestBody(manifest)
	if err != nil {
		return model.ReplacementPreviewInput{}, err
	}
	hash := sha256Hex(body)
	manifest.Hash = hash
	body, err = manifestBody(manifest)
	if err != nil {
		return model.ReplacementPreviewInput{}, err
	}
	if _, err := a.writeReconciliationManifest(ctx, wf, manifest.Revision, body); err != nil {
		return model.ReplacementPreviewInput{}, err
	}

	// The fresh Commit Preflight: the new policy is fully validated only
	// after the Safety Stop settled (PRD step 1); a failure blocks with
	// GIT_IDENTITY_NOT_CONFIGURED or GIT_SIGNING_PREFLIGHT_FAILED.
	preflight, err := a.observePreflight(ctx, wf, facts.PreflightRevision+1)
	if err != nil {
		return model.ReplacementPreviewInput{}, err
	}

	return model.ReplacementPreviewInput{
		PlanHash:             facts.PlanHash,
		SpecHashes:           []string{newSpecRef.Hash},
		SpecRevision:         newSpecRef.Revision,
		CatalogHash:          facts.CatalogHash,
		WorkflowHash:         newWorkflowRef.Hash,
		WorkflowRevision:     newWorkflowRef.Revision,
		RoutingHash:          facts.RoutingHash,
		BudgetHash:           facts.BudgetHash,
		Preflight:            preflight,
		QuarantineIDs:        quarantineIDsOf(st),
		SupersededApprovalID: string(latestExecutionApproval(st)),
		ManifestRevision:     manifest.Revision,
		ManifestHash:         hash,
	}, nil
}

// hasDriftWindowFinding reports whether the blocking drift-window
// Finding is present.
func hasDriftWindowFinding(st model.State) bool {
	for _, f := range st.Findings {
		if f.Blocking && f.Code == model.CodeCommitDuringPolicyDriftWindow {
			return true
		}
	}
	return false
}

// quarantineIDsOf returns the sorted Quarantine IDs of one aggregate.
func quarantineIDsOf(st model.State) []string {
	ids := make([]string, 0, len(st.Quarantines))
	for _, q := range st.Quarantines {
		ids = append(ids, q.ID)
	}
	sort.Strings(ids)
	return ids
}

// latestExecutionApproval is the most recent EXECUTION Approval ("" when
// none).
func latestExecutionApproval(st model.State) model.ApprovalID {
	var latest model.ApprovalID
	for _, ap := range st.Approvals {
		if ap.Kind == model.ApprovalExecution {
			latest = ap.ID
		}
	}
	return latest
}

// replacementSpecs clones every Spec whose Task Node owns a quarantined
// Branch (or is FAILED inside the drift window) into a successor Spec
// with a new Spec ID and replaces_task_id; uncontaminated Specs stay
// byte-identical (their Task Nodes resume on the same Branch/Worktree).
func (a *Application) replacementSpecs(ctx context.Context, wf model.WorkflowID, st model.State, specBodies [][]byte) ([]map[string]any, []string, error) {
	contaminated := map[string]bool{}
	for id, n := range st.Nodes {
		if n.Kind != model.NodeAgentTask {
			continue
		}
		for _, q := range st.Quarantines {
			if q.Branch == n.Branch {
				contaminated[string(id)] = true
			}
		}
		if n.Status == model.NodeFailed {
			contaminated[string(id)] = true
		}
	}
	var out []map[string]any
	var replaced []string
	for _, body := range specBodies {
		var m map[string]any
		if err := yaml.Unmarshal(body, &m); err != nil {
			return nil, nil, model.InvalidInputFault("spec body cannot be parsed")
		}
		id, _ := m["id"].(string)
		if !contaminated["task-"+id] {
			out = append(out, m)
			continue
		}
		replaced = append(replaced, id)
		successor := map[string]any{}
		for k, v := range m {
			successor[k] = v
		}
		successor["id"] = id + "-replacement-" + string(rune('1'+len(replaced)-1))
		successor["replaces_task_id"] = id
		out = append(out, successor)
	}
	sort.Strings(replaced)
	return out, replaced, nil
}

// compileReplacementWorkflow runs the deterministic Compiler over the
// successor Spec set and the approved Catalog (no patch: the original
// patch operations reference the old Node ids and are inert).
func (a *Application) compileReplacementWorkflow(ctx context.Context, wf model.WorkflowID, newSpecBody, catalogBody []byte, facts *model.ExecutionFacts) ([]byte, error) {
	planRef := model.ArtifactRef{Workflow: wf, Type: model.ArtifactPlan, Revision: 1, Hash: facts.PlanHash}
	catalogRef := model.ArtifactRef{Workflow: wf, Type: model.ArtifactCatalog, Revision: facts.CatalogRevision, Hash: facts.CatalogHash}
	specBodies, err := splitSpecSetBody(newSpecBody)
	if err != nil {
		return nil, err
	}
	out, err := (&compile.Compiler{}).Compile(ctx, compile.CompileRequest{
		PlanRef:        planRef,
		WorkflowID:     string(wf),
		Revision:       facts.WorkflowRevision + 1,
		SpecBodies:     specBodies,
		CatalogBody:    catalogBody,
		CatalogRef:     catalogRef,
		MaxConcurrency: a.concurrencyCap(),
	})
	if err != nil {
		return nil, err
	}
	return out.Body, nil
}

// manifestFacts computes the two verified successor facts of the
// Reconciliation Manifest: the Nodes whose ID, kind, and dependencies are
// unchanged between the old and new compiled bodies, and the Nodes whose
// Task Branch/Worktree HEAD, status, and Dirty Fingerprint still match
// the interruption Checkpoint (with no Branch Quarantine).
func (a *Application) manifestFacts(ctx context.Context, wf model.WorkflowID, st model.State, oldNodes, newNodes compile.Workflow) (unchanged, resumable map[model.NodeID]bool) {
	unchanged = map[model.NodeID]bool{}
	deps := func(nodes compile.Workflow) map[string][]string {
		out := map[string][]string{}
		for _, e := range nodes.Edges {
			out[e.To] = append(out[e.To], e.From)
		}
		return out
	}
	oldDeps, newDeps := deps(oldNodes), deps(newNodes)
	oldByID := map[string]compile.WorkflowNode{}
	for _, n := range oldNodes.Nodes {
		oldByID[n.ID] = n
	}
	newByID := map[string]compile.WorkflowNode{}
	for _, n := range newNodes.Nodes {
		newByID[n.ID] = n
	}
	for id, old := range oldByID {
		nw, ok := newByID[id]
		if !ok {
			continue
		}
		if nodeDefinitionEqual(old, nw, oldDeps[id], newDeps[id]) {
			unchanged[model.NodeID(id)] = true
		}
	}
	resumable = map[model.NodeID]bool{}
	for id, n := range st.Nodes {
		if n.Kind != model.NodeAgentTask {
			continue
		}
		quarantined := false
		for _, q := range st.Quarantines {
			if q.Branch == n.Branch {
				quarantined = true
			}
		}
		if quarantined {
			continue
		}
		if a.reuseWorktreeDrift(ctx, wf, id) == "" {
			resumable[id] = true
		}
	}
	return unchanged, resumable
}

// nodeDefinitionEqual compares the definition identity of two compiled
// Nodes: the Node ID, kind/type, Spec, command, and the ordered
// dependencies (PRD 已确认：未污染兄弟 Task 增量恢复 step 3: a changed
// definition must use a new Node id).
func nodeDefinitionEqual(a, b compile.WorkflowNode, aDeps, bDeps []string) bool {
	if a.ID != b.ID || a.Type != b.Type || a.SpecID != b.SpecID || a.CommandID != b.CommandID {
		return false
	}
	if len(aDeps) != len(bDeps) {
		return false
	}
	for i := range aDeps {
		if aDeps[i] != bDeps[i] {
			return false
		}
	}
	return true
}

// latestManifestRevision is the highest persisted Reconciliation
// Manifest Revision of one workflow.
func (a *Application) latestManifestRevision(ctx context.Context, wf model.WorkflowID) (int, error) {
	root, err := a.workflowEvidenceDir(ctx, wf)
	if err != nil || root == "" {
		return 0, nil
	}
	entries, err := os.ReadDir(filepath.Join(root, "reconciliation"))
	if err != nil {
		return 0, nil
	}
	latest := 0
	for _, e := range entries {
		var rev int
		if _, err := fmt.Sscanf(e.Name(), "manifest-%d.json", &rev); err == nil && rev > latest {
			latest = rev
		}
	}
	return latest, nil
}

// readReconciliationManifest reads one persisted manifest body (nil when
// absent).
func (a *Application) readReconciliationManifest(ctx context.Context, wf model.WorkflowID, revision int) ([]byte, error) {
	path, err := a.reconciliationManifestPath(ctx, wf, revision)
	if err != nil {
		return nil, err
	}
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return body, nil
}

// marshalSpecSet re-serializes the successor Spec set canonically: the
// JSON array shape of the Spec Artifact body (a spec object or a
// spec-set sequence, both schema-validated on Put).
func marshalSpecSet(specs []map[string]any) ([]byte, error) {
	return json.Marshal(specs)
}

// queryReplacementPreview projects the unified Replacement Execution
// Approval gate from the persisted facts.
func (a *Application) queryReplacementPreview(ctx context.Context, q ReplacementPreviewQuery) (View, error) {
	wf, err := a.resolveQueryWorkflow(q.Workflow)
	if err != nil {
		return nil, orCtx(ctx, err)
	}
	view, err := a.readAggregate(ctx, wf, store.StoreQuery{})
	if err != nil {
		return nil, orCtx(ctx, err)
	}
	st := view.State
	if st.Workflow.ID == "" {
		return nil, model.InvalidInputFault("no such workflow: " + string(wf))
	}
	facts := st.Workflow.ExecutionFacts
	if facts == nil {
		return nil, model.InvalidInputFault("no execution facts to replace")
	}
	out := ReplacementPreviewView{
		Workflow:             wf,
		Stage:                st.Workflow.Stage,
		Runtime:              st.Workflow.Runtime,
		Quarantines:          append([]model.Quarantine(nil), st.Quarantines...),
		OldRevision:          facts.WorkflowRevision,
		NewRevision:          facts.WorkflowRevision + 1,
		BaselineHead:         st.Workflow.IntegrationHead,
		RoutingHash:          facts.RoutingHash,
		BudgetHash:           facts.BudgetHash,
		OldFingerprint:       driftOldFingerprint(st, facts.Fingerprint),
		NewFingerprint:       facts.Fingerprint,
		Preflight:            &PreflightPreview{Revision: facts.PreflightRevision, EvidenceHash: facts.CommitPolicyHash, Fingerprint: facts.Fingerprint},
		SupersededApprovalID: string(latestExecutionApproval(st)),
		PlanHash:             facts.PlanHash,
		SpecHashes:           append([]string(nil), facts.SpecHashes...),
		CatalogHash:          facts.CatalogHash,
		WorkflowHash:         facts.WorkflowHash,
	}
	if rev, err := a.latestManifestRevision(ctx, wf); err == nil && rev > 0 {
		if body, err := a.readReconciliationManifest(ctx, wf, rev); err == nil && body != nil {
			var m model.ReconciliationManifest
			if jsonUnmarshal(body, &m) == nil {
				out.Manifest = m
			}
		}
	}
	return out, nil
}

// jsonUnmarshal parses one canonical JSON body.
func jsonUnmarshal(body []byte, v any) error {
	return json.Unmarshal(body, v)
}
