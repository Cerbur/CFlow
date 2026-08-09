package app

// The PrepareApply / ExecuteApply / ConfirmApplyPolicy command builders
// (Task 19, PRD 已确认：显式受保护 Apply): the workspace gate (clean and
// attached to the recorded Target Branch), the fresh Target/Integration
// heads the attempt records, the Commit Policy fingerprint observation,
// the independent Apply Verification Session allocation, and the Apply
// Catalog re-discovery/validation/fixing for the append-only
// APPLY_CATALOG approval. Same-package split of the Application seam: no
// public seam added.

import (
	"context"
	"fmt"
	"sort"

	"cflow.local/cflow/internal/artifact"
	"cflow.local/cflow/internal/gitflow"
	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/store"
)

// ---------------------------------------------------------------------------
// PrepareApply command builder
// ---------------------------------------------------------------------------

// prepareApply builds the ApplyRequest input: the workspace gate (clean
// and attached to the recorded Target Branch; CFlow never stashes,
// WIP-commits, or overwrites user content), the fresh Target/Integration
// heads the attempt records, the Commit Policy fingerprint observation,
// the independent Apply Verification Session allocation, and the ONE
// restricted Merge Resolution Session allocation when the Apply Worktree
// of the last blocked attempt still holds a conflicted merge.
func (a *Application) prepareApply(ctx context.Context, wf model.WorkflowID) (model.Input, model.WorkflowID, error) {
	resolved, err := a.resolveMutationWorkflow(wf)
	if err != nil {
		return nil, "", err
	}
	view, err := a.readAggregate(ctx, resolved, store.StoreQuery{})
	if err != nil {
		return nil, "", err
	}
	st := view.State
	if st.Workflow.Stage != model.StageCompleted || st.Workflow.Runtime != model.RuntimeSucceeded {
		return nil, "", model.InvalidInputFault("apply requires a completed workflow")
	}
	if st.Workflow.ExecutionFacts == nil || st.Workflow.ExecutionFacts.PreflightRevision < 1 ||
		st.Workflow.ExecutionFacts.CommitPolicyHash == "" {
		return nil, "", model.InvalidInputFault("apply requires the approved commit-policy preflight facts")
	}
	// The workspace gate: clean, attached to the recorded Target Branch.
	// The observed head is the Target HEAD the attempt records.
	head, err := a.applyWorkspaceHead(ctx, st.Workflow.TargetBranch)
	if err != nil {
		return nil, "", err
	}
	deliveryBranch, deliveryHead, _, err := a.deliveryFacts(ctx, resolved, st)
	if err != nil {
		return nil, "", err
	}
	if deliveryBranch == "" || deliveryHead == "" {
		return nil, "", model.InvalidInputFault("apply requires the recorded delivery branch and head")
	}
	integrationHead, err := a.observedRefHead(ctx, "refs/heads/"+deliveryBranch)
	if err != nil {
		return nil, "", err
	}
	if integrationHead != deliveryHead {
		return nil, "", model.NewFault(model.CodeTargetHeadChanged,
			"the delivery branch no longer matches the recorded head")
	}
	fingerprint, err := a.observePolicyFingerprint(ctx, "apply-"+string(resolved))
	if err != nil {
		return nil, "", err
	}
	reviewRoute, err := a.applyReviewRoute(ctx, resolved, st.Workflow.ExecutionFacts)
	if err != nil {
		return nil, "", err
	}
	reviewSession := model.SessionID(a.ids(model.IDSession))
	reviewProcess := model.ProcessID(a.ids(model.IDProcess))
	var resolutionSession model.SessionID
	var resolutionProcess model.ProcessID
	resolutionNeeded, err := a.applyResolutionNeeded(ctx, resolved, st)
	if err != nil {
		return nil, "", err
	}
	if resolutionNeeded {
		resolutionSession = model.SessionID(a.ids(model.IDSession))
		resolutionProcess = model.ProcessID(a.ids(model.IDProcess))
	}
	facts := st.Workflow.ExecutionFacts
	return model.ApplyCommandInput{
		Kind:              model.ApplyRequest,
		TargetHead:        head,
		IntegrationHead:   integrationHead,
		Preflight:         model.ArtifactRef{Workflow: resolved, Type: model.ArtifactReport, Revision: facts.PreflightRevision, Hash: facts.CommitPolicyHash},
		PreflightHash:     facts.CommitPolicyHash,
		Fingerprint:       fingerprint,
		ReviewSession:     reviewSession,
		ReviewRoute:       reviewRoute,
		ReviewProcess:     reviewProcess,
		ResolutionSession: resolutionSession,
		ResolutionProcess: resolutionProcess,
	}, resolved, nil
}

// prepareApplyExecute builds the ApplyExecute input: the app re-observes
// the workspace head, the attached Branch, and the Integration ref and
// re-asserts the attempt's Preflight facts, so the Kernel's strict re-bind
// sees fresh observations. The authoritative pre-delivery rechecks run in
// the executor.
func (a *Application) prepareApplyExecute(ctx context.Context, wf model.WorkflowID) (model.Input, model.WorkflowID, error) {
	resolved, err := a.resolveMutationWorkflow(wf)
	if err != nil {
		return nil, "", err
	}
	view, err := a.readAggregate(ctx, resolved, store.StoreQuery{})
	if err != nil {
		return nil, "", err
	}
	st := view.State
	att := lastApplyAttemptOf(st)
	if att == nil {
		return nil, "", model.InvalidInputFault("no apply attempt to execute")
	}
	// The delivery builder only observes: the workspace may already lag
	// the delivered Target (a crash after the compare-and-swap moved the
	// branch under the working tree), so the strict clean gate belongs to
	// the executor's pre-delivery rechecks, never here.
	head, err := a.observeWorkspaceHead(ctx)
	if err != nil {
		return nil, "", err
	}
	deliveryBranch, deliveryHead, _, err := a.deliveryFacts(ctx, resolved, st)
	if err != nil {
		return nil, "", err
	}
	integrationHead, err := a.observedRefHead(ctx, "refs/heads/"+deliveryBranch)
	if err != nil {
		return nil, "", err
	}
	if integrationHead != deliveryHead {
		return nil, "", model.NewFault(model.CodeTargetHeadChanged,
			"the delivery branch no longer matches the recorded head")
	}
	return model.ApplyCommandInput{
		Kind:              model.ApplyExecute,
		TargetHead:        head,
		IntegrationHead:   integrationHead,
		Preflight:         att.Preflight,
		PreflightHash:     att.PreflightHash,
		Fingerprint:       att.Fingerprint,
		ReviewSession:     att.ReviewSession,
		ReviewRoute:       att.ReviewRoute,
		ReviewProcess:     att.ReviewProcess,
		ResolutionSession: "",
		ResolutionProcess: "",
	}, resolved, nil
}

// prepareApplyPolicyConfirm builds the ApplyPolicyConfirmation input:
// the fresh Commit Preflight is observed (with the signing probe — a
// failed probe blocks with GIT_SIGNING_PREFLIGHT_FAILED), and when the
// wrapper/manifest/executable identity of the approved Catalog no longer
// matches the tree the Apply verification runs in, a new Apply
// Verification Catalog Revision is re-discovered, validated, and fixed
// from the Apply Worktree for the append-only APPLY_CATALOG approval.
func (a *Application) prepareApplyPolicyConfirm(ctx context.Context, wf model.WorkflowID) (model.Input, model.WorkflowID, error) {
	resolved, err := a.resolveMutationWorkflow(wf)
	if err != nil {
		return nil, "", err
	}
	view, err := a.readAggregate(ctx, resolved, store.StoreQuery{})
	if err != nil {
		return nil, "", err
	}
	st := view.State
	att := lastApplyAttemptOf(st)
	if att == nil || att.Status != model.ApplyBlocked {
		return nil, "", model.InvalidInputFault("no blocked apply attempt to confirm")
	}
	head, err := a.applyWorkspaceHead(ctx, st.Workflow.TargetBranch)
	if err != nil {
		return nil, "", err
	}
	deliveryBranch, deliveryHead, _, err := a.deliveryFacts(ctx, resolved, st)
	if err != nil {
		return nil, "", err
	}
	integrationHead, err := a.observedRefHead(ctx, "refs/heads/"+deliveryBranch)
	if err != nil {
		return nil, "", err
	}
	if integrationHead != deliveryHead {
		return nil, "", model.NewFault(model.CodeTargetHeadChanged,
			"the delivery branch no longer matches the recorded head")
	}
	if st.Workflow.ExecutionFacts == nil {
		return nil, "", model.InvalidInputFault("the confirmation requires the approved execution facts")
	}
	// The fresh Commit Preflight (PRD 约束 40-41: a new Preflight Revision
	// must succeed before the confirmation can bind it). The report
	// Artifact type is shared with the Final Report, so the next free
	// Revision comes from the Artifact Store, never a stale counter.
	next := st.Workflow.ExecutionFacts.PreflightRevision + 1
	if artStore, err := a.artifactStore(resolved); err == nil {
		if latest, err := artStore.Resolve(ctx, artifact.ResolveRequest{WorkflowID: resolved, Type: model.ArtifactReport}); err == nil && latest.Revision >= next {
			next = latest.Revision + 1
		}
	}
	preflight, err := a.observePreflight(ctx, resolved, next)
	if err != nil {
		return nil, "", err
	}
	// The Catalog identity re-discovery: when the tree the Apply
	// verification runs in no longer matches the pinned wrapper/manifest
	// identities, the newly discovered, validated, and fixed Revision is
	// the APPLY_CATALOG approval's subject.
	catalogRef := model.CatalogRef{}
	identityDrifted, err := a.applyIdentityDrifted(ctx, resolved, att, st)
	if err != nil {
		return nil, "", err
	}
	if identityDrifted {
		ref, err := a.rediscoverApplyCatalog(ctx, resolved, att)
		if err != nil {
			return nil, "", err
		}
		catalogRef = ref
	}
	reviewRoute, err := a.applyReviewRoute(ctx, resolved, st.Workflow.ExecutionFacts)
	if err != nil {
		return nil, "", err
	}
	// The confirmation's staging re-run mirrors the request's ONE
	// restricted Merge Resolution allocation: a worktree that still holds
	// a conflicted merge gets the resolution session, so the confirm
	// itself can complete the conflict without an extra retry.
	var resolutionSession model.SessionID
	var resolutionProcess model.ProcessID
	resolutionNeeded, err := a.applyResolutionNeeded(ctx, resolved, st)
	if err != nil {
		return nil, "", err
	}
	if resolutionNeeded {
		resolutionSession = model.SessionID(a.ids(model.IDSession))
		resolutionProcess = model.ProcessID(a.ids(model.IDProcess))
	}
	return model.ApplyPolicyConfirmationInput{
		Attempt:           att.ID,
		TargetHead:        head,
		IntegrationHead:   integrationHead,
		Preflight:         model.ArtifactRef{Workflow: resolved, Type: model.ArtifactReport, Revision: next, Hash: preflight.EvidenceHash},
		PreflightHash:     preflight.EvidenceHash,
		Fingerprint:       preflight.Fingerprint,
		CatalogRef:        catalogRef,
		ReviewSession:     model.SessionID(a.ids(model.IDSession)),
		ReviewRoute:       reviewRoute,
		ReviewProcess:     model.ProcessID(a.ids(model.IDProcess)),
		ResolutionSession: resolutionSession,
		ResolutionProcess: resolutionProcess,
	}, resolved, nil
}

// ---------------------------------------------------------------------------
// PrepareApply input helpers
// ---------------------------------------------------------------------------

// applyWorkspaceHead observes the user workspace: it must be clean and
// attached to the expected Target Branch (a detached HEAD or a wrong
// branch is APPLY_TARGET_BRANCH_CHANGED; dirt is APPLY_TARGET_DIRTY).
// Returns the observed workspace HEAD.
func (a *Application) applyWorkspaceHead(ctx context.Context, targetBranch string) (string, error) {
	if a.git == nil {
		return "", model.InvariantFault(fmt.Errorf("git seam is not configured for this application"))
	}
	facts, err := a.git.Observe(ctx, gitflow.GitStatus{Dir: a.project.Root, UntrackedAll: true})
	if err != nil {
		return "", err
	}
	st, ok := facts.(gitflow.StatusFacts)
	if !ok {
		return "", model.InvariantFault(fmt.Errorf("git status observation has an unexpected type"))
	}
	if !st.Clean() {
		return "", model.NewFault(model.CodeApplyTargetDirty,
			"the user workspace is dirty; apply never stashes, WIP-commits, or overwrites user content")
	}
	branch, err := a.attachedBranchOf(ctx, a.project.Root)
	if err != nil {
		return "", err
	}
	if branch != targetBranch {
		return "", model.NewFault(model.CodeApplyTargetBranchChanged,
			"the user workspace is not attached to the target branch "+targetBranch)
	}
	return st.Head, nil
}

// attachedBranchOf resolves the attached local Branch of one worktree
// path from the registry ("" when detached).
func (a *Application) attachedBranchOf(ctx context.Context, path string) (string, error) {
	facts, err := a.git.Observe(ctx, gitflow.WorktreeList{})
	if err != nil {
		return "", err
	}
	registry, ok := facts.(gitflow.WorktreeFacts)
	if !ok {
		return "", model.InvariantFault(fmt.Errorf("worktree list observation has an unexpected type"))
	}
	for _, e := range registry.Entries {
		if e.Path == path {
			return e.Branch, nil
		}
	}
	return "", model.NewFault(model.CodeStateInvariantViolation, "the user workspace is missing from the worktree registry")
}

// observedRefHead resolves the current value of one ref, failing closed
// when it is missing.
func (a *Application) observedRefHead(ctx context.Context, ref string) (string, error) {
	facts, err := a.git.Observe(ctx, gitflow.RefLookup{Ref: ref})
	if err != nil {
		return "", err
	}
	rf, ok := facts.(gitflow.RefFacts)
	if !ok {
		return "", model.InvariantFault(fmt.Errorf("ref observation has an unexpected type"))
	}
	if !rf.Exists || rf.Value == "" {
		return "", model.NewFault(model.CodeStateInvariantViolation, "ref "+ref+" is missing")
	}
	return rf.Value, nil
}

// observeWorkspaceHead observes the user workspace HEAD without the
// clean/attached gates (the delivery builder's observation; the executor
// runs the authoritative gates immediately before the compare-and-swap).
func (a *Application) observeWorkspaceHead(ctx context.Context) (string, error) {
	if a.git == nil {
		return "", model.InvariantFault(fmt.Errorf("git seam is not configured for this application"))
	}
	facts, err := a.git.Observe(ctx, gitflow.GitStatus{Dir: a.project.Root, UntrackedAll: true})
	if err != nil {
		return "", err
	}
	st, ok := facts.(gitflow.StatusFacts)
	if !ok {
		return "", model.InvariantFault(fmt.Errorf("git status observation has an unexpected type"))
	}
	return st.Head, nil
}

// observePolicyFingerprint recomputes the effective Commit Policy
// fingerprint without a signing probe (PRD 已确认：Commit Policy 漂移立即安
// 全停止 step 5).
func (a *Application) observePolicyFingerprint(ctx context.Context, revision string) (string, error) {
	facts, err := a.git.Observe(ctx, gitflow.FingerprintObserve{Revision: revision})
	if err != nil {
		return "", err
	}
	ff, ok := facts.(gitflow.FingerprintFacts)
	if !ok {
		return "", model.InvariantFault(fmt.Errorf("fingerprint observation has an unexpected type"))
	}
	return ff.PolicyFingerprint, nil
}

// applyReviewRoute resolves the independent Apply Verification Session's
// route from the approved routing policy: the same approved binding the
// Execution Approval fixed for the final-verification purpose (PRD: the
// Apply Verification can use the same Provider but never a Session of
// the Workflow's history).
func (a *Application) applyReviewRoute(ctx context.Context, wf model.WorkflowID, facts *model.ExecutionFacts) (string, error) {
	routing, err := a.verifyApprovedRouting(ctx, wf, facts)
	if err != nil {
		return "", err
	}
	rb, ok := routingPrimaryBinding(routing, model.PurposeFinalVerification)
	if !ok || rb.Provider == "" {
		return "", model.NewFault(model.CodeEvidenceSubjectChanged,
			"the approved routing policy carries no final-verification binding for the apply verification session")
	}
	return rb.Provider, nil
}

// applyResolutionNeeded reports whether the ONE restricted Merge
// Resolution Session must be allocated: the last attempt is BLOCKED and
// its Apply Worktree still holds an unresolved conflicted merge.
func (a *Application) applyResolutionNeeded(ctx context.Context, wf model.WorkflowID, st model.State) (bool, error) {
	att := lastApplyAttemptOf(st)
	if att == nil || att.Status != model.ApplyBlocked || att.Number < 1 {
		return false, nil
	}
	path, err := a.applyWorktreePath(ctx, wf, att.Number)
	if err != nil {
		return false, err
	}
	unmerged, err := a.unmergedPathsOf(ctx, path)
	if err != nil || len(unmerged) == 0 {
		// A blocked policy attempt may not have created its Apply Worktree
		// yet. Path-selection errors have already been propagated above; an
		// absent/unreadable worktree means no resolution session is owed.
		return false, nil
	}
	return true, nil
}

// unmergedPathsOf returns the worktree-relative paths of the unmerged
// entries of one managed worktree (empty when no merge is in progress).
func (a *Application) unmergedPathsOf(ctx context.Context, dir string) ([]string, error) {
	facts, err := a.git.Observe(ctx, gitflow.GitStatus{Dir: dir, UntrackedAll: true})
	if err != nil {
		return nil, err
	}
	st, ok := facts.(gitflow.StatusFacts)
	if !ok {
		return nil, model.InvariantFault(fmt.Errorf("git status observation has an unexpected type"))
	}
	seen := map[string]struct{}{}
	var paths []string
	for _, p := range st.Staged {
		if _, dup := seen[p.Path]; !dup {
			seen[p.Path] = struct{}{}
			paths = append(paths, p.Path)
		}
	}
	for _, p := range st.Unstaged {
		if _, dup := seen[p.Path]; !dup {
			seen[p.Path] = struct{}{}
			paths = append(paths, p.Path)
		}
	}
	sort.Strings(paths)
	return paths, nil
}
