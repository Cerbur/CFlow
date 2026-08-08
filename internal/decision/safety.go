package decision

// The Commit Policy Safety Stop, Quarantine, and replacement decisions
// (Task 17, PRD 已确认：Commit Policy 漂移立即安全停止 / 漂移窗口 Commit 的
// 隔离与替代执行 / Replacement Execution Approval 吸收 Policy 确认 / 未污
// 染兄弟 Task 增量恢复): the post-stop settle input records either the
// exact new Preflight confirmation gate (no window Commit) or the
// immutable Branch Quarantines of the drift-window Commits; the
// append-only COMMIT_POLICY Approval binds the exact new Preflight; and
// the unified Replacement Execution Approval binds the Quarantine Set,
// the superseded approval, every successor reference, the current
// Preflight, and the fixed Reconciliation Manifest, then reopens dispatch
// on a fresh Run. The Reconciliation Manifest classification is a pure
// function of the aggregate and the verified successor facts — never of
// an Agent claim.
//
// Same-package split of the decision package: no public seam added beyond
// ClassifyManifest (the Application's replacement preview computes it
// against the persisted facts).

import (
	"encoding/json"
	"fmt"
	"sort"

	"cflow.local/cflow/internal/model"
)

// decidePolicyDriftSettle is the Kernel's post-Safety-Stop settle: with a
// window Commit the Branch(es) receive an immutable Quarantine Record
// and the Workflow Blocks; without one the freshly observed Commit
// Preflight Revision is recorded and the Workflow pauses at the
// COMMIT_POLICY_CONFIRMATION_REQUIRED gate (PRD steps 6-7).
func decidePolicyDriftSettle(state model.State, in model.PolicyDriftSettleInput) (model.Decision, error) {
	if state.Workflow.ID == "" {
		return model.Decision{}, model.InvalidInputFault("no workflow for a policy drift settle")
	}
	if state.Workflow.Runtime.IsTerminal() {
		return model.Decision{}, model.InvalidInputFault("a terminal workflow cannot settle a policy drift")
	}
	// The settle only follows a persisted Policy Safety Stop: the Run's
	// recorded stop reason (a terminal INTERRUPTED Run keeps it).
	stopped := false
	for i := range state.Runs {
		if state.Runs[i].StopReason == model.CodeCommitPolicyDrift {
			stopped = true
			break
		}
	}
	if !stopped {
		return model.Decision{}, model.InvalidInputFault(
			"a policy drift settle requires the persisted COMMIT_POLICY_DRIFT stop")
	}
	if len(in.WindowCommits) > 0 {
		return decideDriftWindowQuarantine(state, in)
	}
	return decideDriftConfirmation(state, in)
}

// decideDriftConfirmation records the exact new Commit Preflight Revision
// and pauses the Workflow at the COMMIT_POLICY_CONFIRMATION_REQUIRED gate
// (PRD 已确认：执行期间 Commit Policy 漂移确认 steps 1-2). The gate is a
// Kernel-level resume guard, never a third regular approval gate; the
// append-only COMMIT_POLICY Approval binds the exact new Preflight and no
// later action re-asks for the same fingerprint.
func decideDriftConfirmation(state model.State, in model.PolicyDriftSettleInput) (model.Decision, error) {
	p := in.Preflight
	if p == nil || p.Fingerprint == "" || p.EvidenceHash == "" || (p.ProbeRequired && !p.ProbeSuccess) {
		return model.Decision{}, model.InvalidInputFault(
			"a successful new commit preflight is required before the confirmation gate")
	}
	facts := state.Workflow.ExecutionFacts
	if facts == nil {
		return model.Decision{}, model.InvalidInputFault("no execution facts to confirm against")
	}
	b := &builder{state: state}
	next := facts.PreflightRevision + 1
	b.mutate(model.PreflightRecordMutation{
		Revision:          next,
		RepositoryContext: p.RepositoryContext,
		GitVersion:        p.GitVersion,
		Fingerprint:       p.Fingerprint,
		IdentityJSON:      p.IdentityJSON,
		SigningPolicyJSON: p.SigningPolicyJSON,
		ProbeStatus:       p.ProbeStatus,
		ArtifactPath:      p.ArtifactPath,
		ArtifactHash:      p.EvidenceHash,
	})
	b.mutate(model.FindingAppendMutation{Finding: model.Finding{
		ID:       model.FindingID(fmt.Sprintf("finding-%d", len(state.Findings)+1)),
		Code:     model.CodeCommitPolicyConfirmationRequired,
		Scope:    model.ScopeWorkflow,
		Subject:  p.Fingerprint,
		Blocking: false,
		Text:     "commit policy drifted; confirm the exact new preflight before commit-capable actions",
		Seq:      state.NextEventSeq,
	}})
	b.event(model.EventFindingOpened, "", model.AttemptKey{}, model.CodeCommitPolicyConfirmationRequired,
		"commit policy confirmation required")
	// The workflow pauses at the gate — unless a blocking Finding already
	// exists (it stays BLOCKED, and every commit-capable action still
	// requires the confirmation first; PRD 已确认 step 4).
	if !hasBlockingFinding(state) {
		b.mutate(wfMutStatus(state, model.RuntimePaused))
		b.event(model.EventWorkflowPaused, "", model.AttemptKey{}, "", "workflow paused at the commit policy confirmation gate")
	}
	return b.decision(), nil
}

// decideDriftWindowQuarantine records one immutable Branch Quarantine per
// window Commit with a unique audit Ref, marks the owning Node FAILED
// (the interrupted Attempt stays INTERRUPTED and is never rewritten as an
// ordinary failure), Blocks the Workflow, and persists the blocking
// COMMIT_DURING_POLICY_DRIFT_WINDOW Finding. The app creates the unique
// audit Refs before feeding the settle, so the quarantine rows and the
// refs commit together (PRD 已确认：漂移窗口 Commit 的隔离与替代执行).
func decideDriftWindowQuarantine(state model.State, in model.PolicyDriftSettleInput) (model.Decision, error) {
	b := &builder{state: state}
	quarantined := map[string]bool{}
	for _, q := range state.Quarantines {
		quarantined[q.Branch] = true
	}
	firstBranch := ""
	for i, wc := range in.WindowCommits {
		if wc.Branch == "" || wc.FromHead == "" || wc.ToHead == "" {
			return model.Decision{}, model.InvalidInputFault(
				"a window commit requires the branch and the head range")
		}
		if quarantined[wc.Branch] {
			continue
		}
		quarantined[wc.Branch] = true
		if firstBranch == "" {
			firstBranch = wc.Branch
		}
		id := fmt.Sprintf("quarantine-%d", len(state.Quarantines)+i+1)
		b.mutate(model.QuarantineAppendMutation{Quarantine: model.Quarantine{
			ID:       id,
			AuditRef: fmt.Sprintf("refs/cflow/%s/quarantine/%s", state.Workflow.ID, id),
			Branch:   wc.Branch,
			FromHead: wc.FromHead,
			ToHead:   wc.ToHead,
			Code:     model.CodeCommitDuringPolicyDriftWindow,
			Reason:   "a commit was created inside the policy drift window",
			Seq:      state.NextEventSeq + uint64(len(b.d.Events)),
		}})
		b.event(model.EventQuarantineRecorded, wc.Node, model.AttemptKey{},
			model.CodeCommitDuringPolicyDriftWindow, "branch quarantined")
		if wc.Node != "" {
			if n := state.Nodes[wc.Node]; n != nil && !n.Status.IsTerminal() {
				b.mutate(model.NodeStatusMutation{Node: wc.Node, Status: model.NodeFailed, RetryCharged: n.RetryCharged})
				b.event(model.EventNodeFailed, wc.Node, model.AttemptKey{},
					model.CodeCommitDuringPolicyDriftWindow, "node failed: branch quarantined")
			}
		}
	}
	if len(b.d.Mutations) == 0 {
		return model.Decision{}, model.InvalidInputFault(
			"every window commit belongs to an already-quarantined branch")
	}
	b.mutate(model.FindingAppendMutation{Finding: model.Finding{
		ID:       model.FindingID(fmt.Sprintf("finding-%d", len(state.Findings)+1)),
		Code:     model.CodeCommitDuringPolicyDriftWindow,
		Scope:    model.ScopeBranch,
		Subject:  firstBranch,
		Blocking: true,
		Text:     "a commit was created inside the policy drift window; the branch is quarantined",
		Seq:      state.NextEventSeq,
	}})
	b.event(model.EventFindingOpened, "", model.AttemptKey{}, model.CodeCommitDuringPolicyDriftWindow,
		"blocking drift-window finding")
	b.mutate(wfMutStatus(state, model.RuntimeBlocked))
	b.event(model.EventWorkflowBlocked, "", model.AttemptKey{}, "", "workflow blocked by the drift-window commit")
	return b.decision(), nil
}

// decideCommitPolicyApproval is the append-only user decision binding the
// exact new Commit Policy Preflight Revision, hash, and fingerprint after
// a drift Safety Stop (PRD 已确认：执行期间 Commit Policy 漂移确认 step 4).
// Any reference change since the recorded Preflight is
// COMMIT_POLICY_INPUT_CHANGED with no mutation; the confirmation is valid
// only while a confirmation is actually pending (the latest Preflight is
// not yet bound), so one approval is never generalised to other
// identities, keys, formats, or programs.
func decideCommitPolicyApproval(state model.State, in model.CommitPolicyApprovalInput) (model.Decision, error) {
	if state.Workflow.ID == "" {
		return model.Decision{}, model.InvalidInputFault("no workflow to confirm")
	}
	if state.Workflow.Runtime.IsTerminal() {
		return model.Decision{}, model.InvalidInputFault("a terminal workflow cannot confirm a commit policy")
	}
	facts := state.Workflow.ExecutionFacts
	if facts == nil || facts.PreflightRevision != in.PreflightRevision ||
		facts.CommitPolicyHash != in.PreflightHash || facts.Fingerprint != in.Fingerprint {
		return model.Decision{}, model.NewFault(model.CodeCommitPolicyInputChanged,
			"commit policy facts changed since the drift confirmation; re-observe the preflight")
	}
	if policyConfirmed(state) {
		return model.Decision{}, model.InvalidInputFault(
			"no pending commit policy confirmation: the latest preflight is already bound")
	}
	b := &builder{state: state}
	b.mutate(model.ApprovalAppendMutation{Approval: model.Approval{
		ID:                model.ApprovalID(fmt.Sprintf("approval-%d", len(state.Approvals)+1)),
		Kind:              model.ApprovalCommitPolicy,
		Seq:               state.NextEventSeq,
		Refs:              []model.ArtifactRef{{Workflow: state.Workflow.ID, Type: model.ArtifactReport, Revision: facts.PreflightRevision, Hash: facts.CommitPolicyHash}},
		Fingerprint:       facts.Fingerprint,
		PreflightRevision: facts.PreflightRevision,
	}})
	b.event(model.EventCommitPolicyConfirmed, "", model.AttemptKey{}, "", "commit policy confirmed")
	return b.decision(), nil
}

// decideReplacementApproval is the user's unified Replacement Execution
// Approval (PRD 已确认：Replacement Execution Approval 吸收 Policy 确认): a
// single append-only EXECUTION Approval binding the Quarantine Set, the
// superseded Execution Approval, every successor execution reference, the
// current Preflight, and the fixed Reconciliation Manifest Revision/Hash,
// with decision_context reason=COMMIT_POLICY_DRIFT_REPLACEMENT and
// absorbs_commit_policy_confirmation=true. It reopens dispatch on a fresh
// Run; the drift-window blocker is contained by the approval itself.
func decideReplacementApproval(state model.State, in model.ReplacementApprovalInput) (model.Decision, error) {
	if state.Workflow.ID == "" {
		return model.Decision{}, model.InvalidInputFault("no workflow for a replacement approval")
	}
	facts := state.Workflow.ExecutionFacts
	if facts == nil {
		return model.Decision{}, model.InvalidInputFault("no execution facts to replace")
	}
	if !facts.Matches(in.PlanHash, in.SpecHashes, in.CatalogHash, in.WorkflowHash,
		in.RoutingHash, in.BudgetHash, in.PreflightHash, facts.ChangeSetHash) {
		return model.Decision{}, model.NewFault(model.CodeApprovalInputChanged,
			"the successor execution artifacts changed since the replacement preview; re-generate it")
	}
	if facts.PreflightRevision != in.PreflightRevision || facts.CommitPolicyHash != in.PreflightHash ||
		facts.Fingerprint != in.Fingerprint {
		return model.Decision{}, model.NewFault(model.CodeApprovalInputChanged,
			"the commit policy facts changed since the replacement preview; re-observe the preflight")
	}
	quarantineIDs := map[string]bool{}
	for _, q := range state.Quarantines {
		quarantineIDs[q.ID] = true
	}
	if len(in.QuarantineIDs) == 0 {
		return model.Decision{}, model.InvalidInputFault("a replacement requires the quarantine set")
	}
	for _, id := range in.QuarantineIDs {
		if !quarantineIDs[id] {
			return model.Decision{}, model.NewFault(model.CodeApprovalInputChanged,
				"the quarantine set changed since the replacement preview")
		}
	}
	superseded := false
	for _, ap := range state.Approvals {
		if ap.ID == model.ApprovalID(in.SupersededApprovalID) && ap.Kind == model.ApprovalExecution {
			superseded = true
			break
		}
	}
	if !superseded {
		return model.Decision{}, model.InvalidInputFault(
			"the replacement must supersede the recorded execution approval")
	}
	if in.ManifestRevision < 1 || in.ManifestHash == "" {
		return model.Decision{}, model.InvalidInputFault(
			"the replacement requires the fixed reconciliation manifest revision and hash")
	}
	switch state.Workflow.Runtime {
	case model.RuntimeBlocked, model.RuntimePaused:
	default:
		return model.Decision{}, model.InvalidInputFault(
			"a replacement approval requires the blocked or paused workflow at the drift gate")
	}
	ctx, err := json.Marshal(map[string]any{
		"reason":                             "COMMIT_POLICY_DRIFT_REPLACEMENT",
		"superseded_approval_id":             in.SupersededApprovalID,
		"quarantine_ids":                     in.QuarantineIDs,
		"reconciliation_manifest":            map[string]any{"revision": in.ManifestRevision, "hash": in.ManifestHash},
		"absorbs_commit_policy_confirmation": true,
	})
	if err != nil {
		return model.Decision{}, model.InvariantFault(fmt.Errorf("replacement decision context cannot be serialized"))
	}
	refs := []model.ArtifactRef{
		{Workflow: state.Workflow.ID, Type: model.ArtifactPlan, Revision: planRevisionOf(state), Hash: facts.PlanHash},
		{Workflow: state.Workflow.ID, Type: model.ArtifactSpec, Revision: facts.SpecRevision, Hash: facts.SpecHashes[0]},
		{Workflow: state.Workflow.ID, Type: model.ArtifactCatalog, Revision: facts.CatalogRevision, Hash: facts.CatalogHash},
		{Workflow: state.Workflow.ID, Type: model.ArtifactWorkflow, Revision: facts.WorkflowRevision, Hash: facts.WorkflowHash},
	}
	if facts.ChangeSetHash != "" && facts.ChangeSetRevision > 0 {
		refs = append(refs, model.ArtifactRef{Workflow: state.Workflow.ID, Type: model.ArtifactChangeSet,
			Revision: facts.ChangeSetRevision, Hash: facts.ChangeSetHash})
	}
	b := &builder{state: state}
	b.mutate(model.ApprovalAppendMutation{Approval: model.Approval{
		ID:   model.ApprovalID(fmt.Sprintf("approval-%d", len(state.Approvals)+1)),
		Kind: model.ApprovalExecution,
		Seq:  state.NextEventSeq,
		Refs: refs,
		Fingerprint:       facts.Fingerprint,
		PreflightRevision: facts.PreflightRevision,
		DecisionContext:   string(ctx),
	}})
	b.event(model.EventExecutionApproved, "", model.AttemptKey{}, "", "replacement execution approved")
	// The approval establishes the new trusted execution path: a fresh Run
	// with the dispatch gate open.
	b.mutate(model.RunAppendMutation{Run: newRun(state, model.RunRunning, true)})
	b.mutate(wfMutStatus(state, model.RuntimeRunning))
	b.event(model.EventRunStarted, "", model.AttemptKey{}, "", "replacement run started")
	return b.decision(), nil
}

// planRevisionOf is the current Plan Revision of the aggregate ("" when
// no Plan exists).
func planRevisionOf(state model.State) int {
	if state.Plan != nil {
		return state.Plan.Revision
	}
	return 0
}

// policyConfirmed reports whether an append-only Approval (EXECUTION or
// COMMIT_POLICY) binds the latest Commit Preflight Revision — the
// latestConfirmedCommitPolicy of the design (PRD 已确认：Replacement
// Execution Approval 吸收 Policy 确认 item 4: a valid EXECUTION Approval
// and a standalone COMMIT_POLICY Approval are both auditable confirmation
// sources; the specific Approval identity is returned to the caller
// through the aggregate). A workflow with no Preflight has no gate.
func policyConfirmed(state model.State) bool {
	facts := state.Workflow.ExecutionFacts
	if facts == nil || facts.PreflightRevision == 0 {
		return true
	}
	for _, ap := range state.Approvals {
		if ap.PreflightRevision == facts.PreflightRevision {
			return true
		}
	}
	return false
}

// replacementApproved reports whether the workflow carries a Replacement
// Execution Approval (decision context reason COMMIT_POLICY_DRIFT_REPLACEMENT).
// It contains the drift-window blocker: dispatch reopens on the fresh Run
// and the blocking window Finding no longer defers the eligible Nodes.
func replacementApproved(state model.State) bool {
	for _, ap := range state.Approvals {
		if ap.Kind != model.ApprovalExecution || ap.DecisionContext == "" {
			continue
		}
		var ctx struct {
			Reason string `json:"reason"`
		}
		if json.Unmarshal([]byte(ap.DecisionContext), &ctx) == nil &&
			ctx.Reason == "COMMIT_POLICY_DRIFT_REPLACEMENT" {
			return true
		}
	}
	return false
}

// ClassifyManifest computes the deterministic per-Node Reconciliation
// actions of a Policy Safety Stop (design 15.6, PRD 已确认：未污染兄弟 Task
// 增量恢复). unchanged carries the Node identities whose definition and
// dependencies are identical in the successor Dynamic Workflow Revision
// (the Runtime compared the old and new compiled bodies); resumable
// carries the Nodes whose Task Branch/Worktree HEAD, status, and Dirty
// Fingerprint still match the interruption Checkpoint and whose
// dependencies remain on the trusted baseline. The classification is
// computed from Git, Attempt, Session, and evidence facts — never from an
// Agent claim; a drifted fact never widens the reuse set.
func ClassifyManifest(state model.State, unchanged map[model.NodeID]bool, resumable map[model.NodeID]bool) []model.ManifestAction {
	ids := make([]model.NodeID, 0, len(state.Nodes))
	for id := range state.Nodes {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	action := map[model.NodeID]model.ManifestActionKind{}
	reason := map[model.NodeID]string{}
	// Pass 1: quarantine and failure facts dominate every other signal.
	for _, id := range ids {
		n := state.Nodes[id]
		if n == nil {
			continue
		}
		if branchQuarantined(state, n.Branch) {
			action[id] = model.ManifestReplaceContaminated
			reason[id] = "branch quarantined inside the drift window"
			continue
		}
		if n.Status == model.NodeFailed {
			action[id] = model.ManifestReplaceContaminated
			reason[id] = "node failed inside the drift window"
		}
	}
	// Pass 2: the remaining nodes classify from their persisted state and
	// the verified successor facts.
	for _, id := range ids {
		n := state.Nodes[id]
		if n == nil {
			continue
		}
		if _, done := action[id]; done {
			continue
		}
		if nodePathReplaced(state, id, action) {
			// A Verify/Merge path of a replaced Task re-runs against the
			// new path (PRD 已确认 step 3).
			if n.Kind == model.NodeVerify {
				action[id] = model.ManifestRerunVerification
				reason[id] = "the verification path of a replaced task must re-run"
			} else {
				action[id] = model.ManifestReplaceContaminated
				reason[id] = "the merge path of a replaced task must be re-created"
			}
			continue
		}
		switch n.Status {
		case model.NodeSucceeded:
			if unchanged[id] {
				action[id] = model.ManifestReuseSucceeded
				reason[id] = "succeeded with intact evidence and an unchanged definition"
			} else {
				action[id] = model.ManifestReplaceContaminated
				reason[id] = "the successor revision changed the node's definition"
			}
		case model.NodeReady, model.NodePending:
			if unchanged[id] && resumable[id] {
				action[id] = model.ManifestResumeInterrupted
				reason[id] = "facts match the interruption checkpoint; resume on the same path"
			} else {
				action[id] = model.ManifestReplaceContaminated
				reason[id] = "the path cannot be reused: facts drifted or the definition changed"
			}
		default:
			// Terminal CANCELLED/SKIPPED or RUNNING Nodes of a stopped run
			// never re-execute and are not part of the successor schedule.
			if n.Status == model.NodeCancelled || n.Status == model.NodeSkipped {
				continue
			}
			action[id] = model.ManifestReplaceContaminated
			reason[id] = "the node cannot be resumed from its persisted state"
		}
	}
	out := make([]model.ManifestAction, 0, len(action))
	for _, id := range ids {
		a, ok := action[id]
		if !ok {
			continue
		}
		out = append(out, model.ManifestAction{Node: id, Action: a, Reason: reason[id]})
	}
	return out
}

// nodePathReplaced reports whether any dependency path of one Node leads
// to a replaced Node (the skeleton edges of the persisted graph: verify
// and merge Nodes depend on their Task Node).
func nodePathReplaced(state model.State, id model.NodeID, action map[model.NodeID]model.ManifestActionKind) bool {
	n := state.Nodes[id]
	if n == nil {
		return false
	}
	for _, d := range n.Dependencies {
		if action[d] == model.ManifestReplaceContaminated {
			return true
		}
		if dn := state.Nodes[d]; dn != nil && dn.Kind == model.NodeAgentTask &&
			branchQuarantined(state, dn.Branch) {
			return true
		}
	}
	return false
}

// decideReplacementPreview records the successor execution references of
// a drift-window quarantine (PRD 已确认：漂移窗口 Commit 的隔离与替代执行):
// the successor Spec and Dynamic Workflow Revisions, the fresh Commit
// Preflight, and the fixed Reconciliation Manifest reference. The
// Workflow stays at the unified Replacement Execution Approval gate; the
// successor references are exactly what the Replacement Execution
// Approval compares-and-swaps against.
func decideReplacementPreview(state model.State, in model.ReplacementPreviewInput) (model.Decision, error) {
	if state.Workflow.ID == "" {
		return model.Decision{}, model.InvalidInputFault("no workflow for a replacement preview")
	}
	if state.Workflow.Runtime.IsTerminal() {
		return model.Decision{}, model.InvalidInputFault("a terminal workflow cannot be replaced")
	}
	if len(state.Quarantines) == 0 {
		return model.Decision{}, model.InvalidInputFault("no quarantined branch to replace")
	}
	facts := state.Workflow.ExecutionFacts
	if facts == nil {
		return model.Decision{}, model.InvalidInputFault("no execution facts to replace")
	}
	if in.PlanHash == "" || len(in.SpecHashes) == 0 || in.CatalogHash == "" ||
		in.WorkflowHash == "" || in.ManifestRevision < 1 || in.ManifestHash == "" {
		return model.Decision{}, model.InvalidInputFault("the replacement preview requires the complete successor references")
	}
	p := in.Preflight
	if p.Fingerprint == "" || p.EvidenceHash == "" || (p.ProbeRequired && !p.ProbeSuccess) {
		return model.Decision{}, model.InvalidInputFault("a successful commit preflight is required for the replacement preview")
	}
	b := &builder{state: state}
	b.mutate(model.ArtifactRefMutation{
		Type: model.ArtifactSpec, Revision: in.SpecRevision, Path: artifactPath(in.SpecRevision, in.SpecHashes[0]), Hash: in.SpecHashes[0],
	})
	b.mutate(model.ArtifactRefMutation{
		Type: model.ArtifactWorkflow, Revision: in.WorkflowRevision, Path: artifactPath(in.WorkflowRevision, in.WorkflowHash), Hash: in.WorkflowHash,
	})
	b.mutate(model.PreflightRecordMutation{
		Revision:          facts.PreflightRevision + 1,
		RepositoryContext: p.RepositoryContext,
		GitVersion:        p.GitVersion,
		Fingerprint:       p.Fingerprint,
		IdentityJSON:      p.IdentityJSON,
		SigningPolicyJSON: p.SigningPolicyJSON,
		ProbeStatus:       p.ProbeStatus,
		ArtifactPath:      p.ArtifactPath,
		ArtifactHash:      p.EvidenceHash,
	})
	// The Workflow stays at the drift gate (BLOCKED with the drift-window
	// blocker, or PAUSED); the Replacement Execution Approval reopens
	// dispatch.
	if !hasBlockingFinding(state) {
		b.mutate(wfMutStatus(state, model.RuntimePaused))
		b.event(model.EventWorkflowPaused, "", model.AttemptKey{}, "", "workflow paused at the replacement approval gate")
	}
	return b.decision(), nil
}

// artifactPath is the deterministic Artifact Store path of one revision
// (the executor's derived shape).
func artifactPath(revision int, hash string) string {
	return fmt.Sprintf("%d/%s", revision, hash)
}
