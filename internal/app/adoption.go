package app

// Workspace Adoption Gate (TUI task 6, design 8.4): the Execution
// Approval of an aggregated workspace Workflow binds the frozen Change Set
// of the discussion; before any normal Task may be scheduled from the
// candidate, the user runs AdoptWorkspaceCommand, which re-verifies the
// Workspace chain (Change Set re-observation, Commit Policy, Identity/
// Signing, Clean/Scope, Catalog Verification) and records an independent
// Adoption Review Session. The Kernel advances verified_workspace_head to
// the exact verified Candidate Head on a PASS verdict; any failure Blocks
// the Workflow and preserves the Workspace, the Change Set, and the Target
// Branch.

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"cflow.local/cflow/internal/agent"
	"cflow.local/cflow/internal/artifact"
	"cflow.local/cflow/internal/gitflow"
	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/store"
	"cflow.local/cflow/internal/verify"
)

// latestChangeSetRef resolves the latest frozen ArtifactChangeSet Revision
// of one workflow (zero ref when none exists yet).
func (a *Application) latestChangeSetRef(ctx context.Context, wf model.WorkflowID) (model.ArtifactRef, error) {
	store, err := a.artifactStore(wf)
	if err != nil {
		return model.ArtifactRef{}, err
	}
	ref, err := store.Resolve(ctx, artifact.ResolveRequest{WorkflowID: wf, Type: model.ArtifactChangeSet})
	if err != nil {
		return model.ArtifactRef{}, nil
	}
	return ref, nil
}

// prepareAdoption resolves the workflow and re-verifies the Workspace
// Adoption Gate chain before the Kernel records the independent Adoption
// Review Session:
//
//  1. the aggregated Workspace exists with the recorded Branch;
//  2. the Execution Approval bound a Change Set (facts.ChangeSetHash);
//  3. the frozen Change Set Revision resolves and its candidate facts
//     match the re-observed Workspace (Branch, candidate Head, Dirty
//     Fingerprint) — a drift is the "Approval 后漂移" closure;
//  4. the Commit Policy re-verification (Identity/Signing preflight);
//  5. the Workspace is Git-clean and within the approved scope;
//  6. the fixed Verification Catalog runs over base..candidate;
//  7. the approved independent-review route of the routing policy.
//
// Every failure is a typed fault with no mutation: the Workspace, the
// Change Set, and the Target Branch stay untouched.
//
// When the Workspace is NOT Git-clean (Task 4, design 8.4 step 2), the
// gate first runs a managed adoption/coding Session that organizes and
// commits the dirty native changes inside the Workspace; the full gate
// chain then runs against the NEW post-adoption candidate Head and the
// independent Review follows. The dirty case does not reject: it returns
// the managed adoption input, and a failed adoption Session Blocks the
// Workflow through the Kernel.
func (a *Application) prepareAdoption(ctx context.Context, c AdoptWorkspaceCommand) (model.Input, model.WorkflowID, error) {
	wf, err := a.resolveMutationWorkflow(c.Workflow)
	if err != nil {
		return nil, "", err
	}
	view, err := a.readAggregate(ctx, wf, store.StoreQuery{})
	if err != nil {
		return nil, "", err
	}
	if view.State.Workflow.ID != wf {
		return nil, "", model.InvalidInputFault("the workflow does not exist")
	}
	if view.State.Workflow.LayoutVersion < 2 || view.State.Workflow.WorkspacePath == "" || view.State.Workflow.WorkspaceBranch == "" {
		return nil, "", model.InvalidInputFault("workspace adoption requires the aggregated workspace layout")
	}
	if view.State.Workflow.Stage != model.StageExecution {
		return nil, "", model.InvalidInputFault("workspace adoption requires the EXECUTION stage")
	}
	if view.State.Workflow.Runtime != model.RuntimeRunning {
		return nil, "", model.InvalidInputFault("workspace adoption requires a running workflow")
	}
	if view.State.Workflow.VerifiedWorkspaceHead != "" {
		return nil, "", model.InvalidInputFault("the workspace is already adopted")
	}
	facts := view.State.Workflow.ExecutionFacts
	if facts == nil || facts.ChangeSetHash == "" || facts.ChangeSetRevision < 1 {
		return nil, "", model.InvalidInputFault("workspace adoption requires an execution approval bound to a change set")
	}
	cwd, err := a.planningCWD(ctx, wf)
	if err != nil {
		return nil, "", err
	}
	status, err := a.observeChangeSetStatus(ctx, cwd)
	if err != nil {
		return nil, "", err
	}
	// The frozen Change Set Revision the approval bound: it must resolve
	// (the adoption Session and the gate chain read it; a missing body is a
	// closed fault before any Session starts).
	changeSet, err := a.readChangeSetBody(ctx, wf, facts.ChangeSetRevision, facts.ChangeSetHash)
	if err != nil {
		return nil, "", model.NewFault(model.CodeEvidenceSubjectChanged,
			"the frozen change set no longer resolves; the workspace drifted after the approval")
	}
	routing, err := a.approvedRoutingPolicy(ctx, wf)
	if err != nil {
		return nil, "", err
	}
	review, ok := routingPrimaryBinding(routing, model.PurposeReview)
	if !ok || review.Provider == "" {
		return nil, "", model.NewFault(model.CodeApprovalInputChanged,
			"the execution approval bound no independent review route for the adoption")
	}
	if !status.Clean() {
		// DIRTY native Workspace (Task 4, design 8.4 step 2): a managed
		// adoption/coding Session organizes and commits the changes; the
		// gate chain runs afterwards against the NEW candidate Head. The
		// candidate facts recorded here are the observed dirty facts; the
		// Kernel replaces them with the post-adoption evidence.
		//
		// The BASE-COMMIT invariant still holds on the dirty path: for a
		// dirty Workspace the candidate Head/fingerprint mismatch is
		// expected, but a frozen Change Set whose BaseCommit differs from
		// the recorded Workflow BaseCommit is an approval drift that must
		// block BEFORE any adoption Session starts.
		if changeSet.BaseCommit != view.State.Workflow.BaseCommit {
			return nil, "", model.NewFault(model.CodeEvidenceSubjectChanged,
				"the workspace drifted after the execution approval; re-freeze before adopting")
		}
		adoption, ok := routingPrimaryBinding(routing, model.PurposeImplementation)
		if !ok || adoption.Provider == "" {
			return nil, "", model.NewFault(model.CodeApprovalInputChanged,
				"the execution approval bound no coding route for the adoption session")
		}
		return model.AdoptWorkspaceInput{
			Session:          model.SessionID(a.ids(model.IDSession)),
			Route:            review.Provider,
			AdoptionSession:  model.SessionID(a.ids(model.IDSession)),
			AdoptionRoute:    adoption.Provider,
			ChangeSetHash:    facts.ChangeSetHash,
			CandidateHead:    status.Head,
			DirtyFingerprint: status.Dirty.Combined,
		}, wf, nil
	}
	// The frozen Change Set Revision the approval bound: it must resolve
	// and its candidate facts must match the re-observed Workspace exactly
	// (Approval 后漂移 closes the gate before any Session starts).
	if changeSet.BaseCommit != view.State.Workflow.BaseCommit ||
		changeSet.CandidateHead != status.Head ||
		changeSet.DirtyFingerprint != status.Dirty.Combined {
		return nil, "", model.NewFault(model.CodeEvidenceSubjectChanged,
			"the workspace drifted after the execution approval; re-freeze before adopting")
	}
	if err := a.verifyWorkspaceBranch(ctx, cwd, view.State.Workflow.WorkspaceBranch); err != nil {
		return nil, "", err
	}
	// Commit Policy / Identity / Signing re-verification: the recorded
	// preflight the approval bound still holds for the exact candidate.
	if err := a.verifyAdoptionPreflight(ctx, wf, facts); err != nil {
		return nil, "", err
	}
	// Clean/Scope: the Workspace must be Git-clean (every native change
	// committed) and within the approved write scope.
	if err := a.verifyAdoptionScope(ctx, wf, changeSet); err != nil {
		return nil, "", err
	}
	// The fixed Verification Catalog runs over the full candidate range
	// base..candidate inside the Workspace.
	if err := a.verifyAdoptionCatalog(ctx, wf, facts, view.State.Workflow.BaseCommit, status.Head); err != nil {
		return nil, "", err
	}
	return model.AdoptWorkspaceInput{
		Session:          model.SessionID(a.ids(model.IDSession)),
		Route:            review.Provider,
		ChangeSetHash:    facts.ChangeSetHash,
		CandidateHead:    status.Head,
		DirtyFingerprint: status.Dirty.Combined,
	}, wf, nil
}

// readChangeSetBody reads and validates the frozen Change Set body of one
// exact Revision/Hash (the immutable Artifact Store verifies the content
// hash before returning any byte).
func (a *Application) readChangeSetBody(ctx context.Context, wf model.WorkflowID, revision int, hash string) (model.ChangeSet, error) {
	store, err := a.artifactStore(wf)
	if err != nil {
		return model.ChangeSet{}, err
	}
	body, err := store.Get(ctx, model.ArtifactRef{
		Workflow: wf, Type: model.ArtifactChangeSet, Revision: revision, Hash: hash,
	})
	if err != nil {
		return model.ChangeSet{}, err
	}
	var cs model.ChangeSet
	if err := jsonUnmarshal(body, &cs); err != nil {
		return model.ChangeSet{}, model.InvariantFault(fmt.Errorf("the frozen change set body is not canonical"))
	}
	return cs, nil
}

// verifyWorkspaceBranch asserts the workspace worktree is attached to the
// recorded CFlow-owned Workspace Branch (design 8.2): the branch ref must
// exist AND the worktree's observed HEAD must be attached to that exact
// branch. An adoption Session that switched branches fails closed.
func (a *Application) verifyWorkspaceBranch(ctx context.Context, cwd, want string) error {
	if a.git == nil {
		return model.InvariantFault(fmt.Errorf("git seam is not configured for this application"))
	}
	facts, err := a.git.Observe(ctx, gitflow.RefLookup{Ref: "refs/heads/" + want})
	if err != nil {
		return err
	}
	ref, ok := facts.(gitflow.RefFacts)
	if !ok {
		return model.InvariantFault(fmt.Errorf("ref lookup observation has an unexpected type"))
	}
	if !ref.Exists || ref.Value == "" {
		return model.NewFault(model.CodeEvidenceSubjectChanged,
			"the workspace branch no longer exists")
	}
	// The Worktree must be attached to the recorded Workspace Branch
	// (design 8.2): a Session that switched branches fails closed.
	bf, err := a.git.Observe(ctx, gitflow.BranchInspect{Dir: cwd})
	if err != nil {
		return err
	}
	branch, ok := bf.(gitflow.BranchFacts)
	if !ok {
		return model.InvariantFault(fmt.Errorf("branch inspection observation has an unexpected type"))
	}
	if branch.Detached || branch.Branch != want || branch.Head == "" || branch.Head != ref.Value {
		return model.NewFault(model.CodeEvidenceSubjectChanged,
			"the workspace worktree is not attached to the workspace branch")
	}
	return nil
}

// verifyAdoptionAncestry asserts the pre-adoption HEAD is an ancestor of
// the post-adoption HEAD (the adoption's new-commit closure, design 8.4
// step 2): the adoption may only append commits on top of the recorded
// candidate. A misbehaving session that moved the Workspace to a foreign
// head (e.g. `git reset --hard` to an unrelated or past commit) fails
// closed with EVIDENCE_SUBJECT_CHANGED before any gate runs.
func (a *Application) verifyAdoptionAncestry(ctx context.Context, pre, post string) error {
	if a.git == nil {
		return model.InvariantFault(fmt.Errorf("git seam is not configured for this application"))
	}
	facts, err := a.git.Observe(ctx, gitflow.AncestryCheck{Ancestor: pre, Descendant: post})
	if err != nil {
		return err
	}
	ac, ok := facts.(gitflow.AncestryFacts)
	if !ok {
		return model.InvariantFault(fmt.Errorf("ancestry observation has an unexpected type"))
	}
	if !ac.AncestorOf {
		return model.NewFault(model.CodeEvidenceSubjectChanged,
			"the adoption session moved the workspace to a head that does not descend from the recorded candidate")
	}
	return nil
}

// verifyAdoptionPreflight re-runs the Commit Policy preflight and
// requires the bound fingerprint to hold for the exact candidate (design
// 8.4 step 3: Identity, Signing, Append-only, Evidence).
func (a *Application) verifyAdoptionPreflight(ctx context.Context, wf model.WorkflowID, facts *model.ExecutionFacts) error {
	if a.git == nil {
		return model.InvariantFault(fmt.Errorf("git seam is not configured for this application"))
	}
	res, err := a.git.Execute(ctx, gitflow.CommitPreflight{Revision: fmt.Sprintf("adoption-%d", facts.PreflightRevision)})
	if err != nil {
		return err
	}
	ev, ok := res.(gitflow.PreflightEvidence)
	if !ok {
		return model.InvariantFault(fmt.Errorf("adoption preflight result has an unexpected type"))
	}
	if facts.Fingerprint != "" && ev.PolicyFingerprint != facts.Fingerprint {
		return model.NewFault(model.CodeCommitPolicyMismatch,
			"the commit policy fingerprint changed since the execution approval")
	}
	return nil
}

// verifyAdoptionScope asserts every tracked and untracked path of the
// frozen Change Set stays within the union of the approved Spec write
// scopes (the adoption never adopts out-of-scope native changes).
func (a *Application) verifyAdoptionScope(ctx context.Context, wf model.WorkflowID, cs model.ChangeSet) error {
	specs, err := a.approvedSpecs(ctx, wf)
	if err != nil {
		return err
	}
	var scopes []string
	for _, s := range specs {
		scopes = append(scopes, s.WriteScope...)
	}
	if len(scopes) == 0 {
		return nil // no declared write scope: nothing to enforce
	}
	check := func(path string) error {
		for _, scope := range scopes {
			// The approved Spec write scope may carry a `/**` glob marker
			// (the same normalization the scheduler's static conflict
			// judgment uses): `src/divide/**` covers every path below
			// `src/divide/`.
			base := normalizeScope(scope)
			if path == base || strings.HasPrefix(path, strings.TrimSuffix(base, "/")+"/") {
				return nil
			}
		}
		return model.NewFault(model.CodeScopeViolation,
			"the candidate change set contains the out-of-scope path "+path)
	}
	for _, e := range cs.TrackedDiff {
		if err := check(e.Path); err != nil {
			return err
		}
	}
	for _, e := range cs.Untracked {
		if err := check(e.Path); err != nil {
			return err
		}
	}
	return nil
}

// verifyAdoptionCatalog runs the fixed Verification Catalog over the full
// candidate range base..candidate inside the Workspace (design 8.4 step 4):
// the approved task-verify entry is validated by identity and executed; a
// failing Manifest fails the adoption closed.
func (a *Application) verifyAdoptionCatalog(ctx context.Context, wf model.WorkflowID, facts *model.ExecutionFacts, base, head string) error {
	engine, err := verify.NewEngine(verify.EngineOptions{
		Supervisor: a.supervisor, GitFlow: a.git, Redaction: a.redaction,
		LoadCatalog: func(ctx context.Context, ref model.CatalogRef) ([]byte, error) {
			return a.readCatalogBody(ctx, wf, ref)
		},
	})
	if err != nil {
		return err
	}
	cat := model.CatalogRef{Revision: facts.CatalogRevision, Hash: facts.CatalogHash}
	validated, err := engine.ValidateCatalog(ctx, cat)
	if err != nil {
		return err
	}
	commandID := ""
	for _, id := range sortedCatalogIDs(validated.Entries) {
		entry := validated.Entries[id]
		if entry.Purpose == string(verify.PurposeTaskVerify) &&
			entry.ExecutableKind == verify.KindProjectRelative {
			commandID = id
			break
		}
	}
	if commandID == "" {
		return model.NewFault(model.CodeEvidenceSubjectChanged,
			"the approved catalog has no task-verify entry for the adoption")
	}
	cwd, err := a.planningCWD(ctx, wf)
	if err != nil {
		return err
	}
	manifest, err := engine.Run(ctx, verify.VerificationRequest{
		Node:        "workspace-adoption",
		Catalog:     cat,
		CommandID:   commandID,
		Purpose:     verify.PurposeTaskVerify,
		Worktree:    cwd,
		CommitRange: base + ".." + head,
	})
	if err != nil {
		return err
	}
	if !manifest.Passed {
		return model.NewFault(model.CodeCommandFailed,
			"the adoption verification catalog rejected the candidate change set")
	}
	return nil
}

// sortedCatalogIDs returns the catalog entry ids in deterministic order.
func sortedCatalogIDs(entries map[string]verify.ValidatedEntry) []string {
	ids := make([]string, 0, len(entries))
	for id := range entries {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// adoptionReviewProviderStart runs the independent Adoption Review Session
// (design 8.4 step 5): a non-coding Session inside the Workspace, bound to
// the exact frozen Change Set, the approved Plan/Spec/Catalog, and the
// candidate Diff. The Workspace's HEAD and Git-visible state must be
// unchanged (UNEXPECTED_AGENT_MUTATION otherwise); the result carries the
// observed clean Dirty Fingerprint so the Kernel records it with the
// verified head.
func (a *Application) adoptionReviewProviderStart(ctx context.Context, wf model.WorkflowID, intent model.ProviderStartIntent, cmd model.Input, rt *agent.Runtime) (model.EffectResultInput, error) {
	if rt == nil {
		return model.EffectResultInput{}, model.InvariantFault(fmt.Errorf("agent runtime is not configured for this application"))
	}
	prompt, ok := a.promptForPurpose(model.PurposeReview)
	if !ok {
		return model.EffectResultInput{}, model.InvalidInputFault("no embedded prompt for the review purpose")
	}
	cwd, err := a.planningCWD(ctx, wf)
	if err != nil {
		return model.EffectResultInput{}, err
	}
	pre, err := a.observeSnapshot(ctx, cwd)
	if err != nil {
		return model.EffectResultInput{}, err
	}
	input, err := a.adoptionReviewSessionInput(ctx, wf, cwd)
	if err != nil {
		return model.EffectResultInput{}, err
	}
	res, err := rt.Start(ctx, agent.StartRequest{
		Purpose:   intent.Purpose,
		Provider:  intent.Route,
		Prompt:    renderPrompt(prompt.Body, input),
		Input:     a.providerTypedInput(ctx, rt, intent.Purpose, intent.Route, input),
		CWD:       cwd,
		SessionID: intent.Session,
	})
	if err != nil {
		return model.EffectResultInput{}, err
	}
	post, err := a.observeSnapshot(ctx, cwd)
	if err != nil {
		return model.EffectResultInput{}, err
	}
	if post.Head != pre.Head || post.Dirty != pre.Dirty {
		return model.EffectResultInput{}, model.NewFault(model.CodeUnexpectedAgentMutation,
			"the workspace changed during the adoption review; its output is invalid")
	}
	out, err := a.runOutcome(cmd, res)
	if err != nil {
		return model.EffectResultInput{}, err
	}
	if res.Terminal != nil {
		out.Body = []byte(res.Terminal.Result)
	}
	// The clean Dirty Fingerprint at the adopted Head: the Kernel records
	// it with the verified head on a PASS verdict (design 8.4 step 6).
	out.EndDirtyFingerprint = post.Dirty.Combined
	return out, nil
}

// adoptionCodingProviderStart runs the managed adoption/coding Session
// (Task 4, design 8.4 step 2): a coding Session inside the Workspace that
// organizes and commits the dirty native changes to the Workspace Branch.
// The adoption output is judged by evidence, never by a claim: the result
// carries the Runtime-observed Workspace facts (EndHead and the Dirty
// Fingerprint), the Kernel re-judges them, and the gate chain (Change Set
// re-freeze against the NEW candidate Head, Identity/Signing, Clean/Scope,
// the fixed Verification Catalog over base..new-candidate) runs right here
// after the Session. Any gate failure is reported as a failed Session with
// the typed code; the Kernel Blocks the Workflow and preserves the
// Workspace, the Change Set, and the Target Branch.
func (a *Application) adoptionCodingProviderStart(ctx context.Context, wf model.WorkflowID, intent model.ProviderStartIntent, cmd model.Input, rt *agent.Runtime) (model.EffectResultInput, error) {
	if rt == nil {
		return model.EffectResultInput{}, model.InvariantFault(fmt.Errorf("agent runtime is not configured for this application"))
	}
	prompt, ok := a.promptForPurpose(model.PurposeAdoption)
	if !ok {
		return model.EffectResultInput{}, model.InvalidInputFault("no embedded prompt for the adoption purpose")
	}
	cwd, err := a.planningCWD(ctx, wf)
	if err != nil {
		return model.EffectResultInput{}, err
	}
	pre, err := a.observeSnapshot(ctx, cwd)
	if err != nil {
		return model.EffectResultInput{}, err
	}
	input, err := a.adoptionCodingSessionInput(ctx, wf, cwd)
	if err != nil {
		return model.EffectResultInput{}, err
	}
	res, err := rt.Start(ctx, agent.StartRequest{
		Purpose:   intent.Purpose,
		Provider:  intent.Route,
		Prompt:    renderPrompt(prompt.Body, input),
		Input:     a.providerTypedInput(ctx, rt, intent.Purpose, intent.Route, input),
		CWD:       cwd,
		SessionID: intent.Session,
	})
	if err != nil {
		return model.EffectResultInput{}, err
	}
	// The Workspace facts after the adoption Session are the evidence the
	// gate judges (a new Commit, a clean Workspace, the candidate HEAD
	// advanced). EndDirtyFingerprint follows the AttemptEnded convention:
	// "" when the Workspace is clean, the fingerprint when dirty (gitflow's
	// DirtyFingerprint.Combined is a deterministic hash of the state and is
	// non-empty even for a clean tree, so the clean signal is the empty
	// string, never the combined hash).
	post, err := a.observeSnapshot(ctx, cwd)
	if err != nil {
		return model.EffectResultInput{}, err
	}
	out, err := a.runOutcome(cmd, res)
	if err != nil {
		return model.EffectResultInput{}, err
	}
	out.EndHead = post.Head
	if !post.Clean() {
		out.EndDirtyFingerprint = dirtyFingerprint(post.Dirty)
	}
	// A crashed or failed adoption Session: report the observed facts; the
	// Kernel Blocks the gate.
	if res.Terminal != nil && res.Terminal.Type == agent.EventFailed {
		return out, nil
	}
	// The adoption evidence (design 8.4 step 2): a new Commit exists (the
	// candidate HEAD advanced) and the Workspace is clean. The Kernel
	// re-judges the same facts from this Result; the executor only stops
	// here to avoid re-freezing a Change Set the gate will reject.
	if !post.Clean() || post.Head == "" || post.Head == pre.Head {
		return out, nil
	}
	// The adoption evidence is an ancestry closure, never a bare head-string
	// inequality: the pre-adoption HEAD must be an ancestor of the
	// post-adoption HEAD (the adoption may only append commits on top of the
	// recorded candidate). A session that moved the workspace to a foreign
	// head (e.g. `git reset --hard` to an unrelated or past commit) fails
	// closed with EVIDENCE_SUBJECT_CHANGED and the Kernel Blocks the
	// Workflow before any gate runs.
	if err := a.verifyAdoptionAncestry(ctx, pre.Head, post.Head); err != nil {
		out.Session.Status = model.SessionFailed
		out.FailureCode = model.CodeEvidenceSubjectChanged
		return out, nil
	}
	view, err := a.writeStoreView(ctx, wf)
	if err != nil {
		return model.EffectResultInput{}, err
	}
	base := view.State.Workflow.BaseCommit
	facts := view.State.Workflow.ExecutionFacts
	if facts == nil {
		return model.EffectResultInput{}, model.InvariantFault(fmt.Errorf("the adoption gate lost the execution facts"))
	}
	// Re-freeze the Change Set against the NEW candidate Head: the frozen
	// facts the approval bound are re-bound to the post-adoption revision
	// (the committed native changes now form the candidate).
	rangeFacts, err := a.observeCommitRange(ctx, base, post.Head)
	if err != nil {
		return model.EffectResultInput{}, err
	}
	body, err := assembleChangeSet(base, cwd, post, rangeFacts, string(intent.Session))
	if err != nil {
		return model.EffectResultInput{}, err
	}
	ref, err := a.freezeChangeSet(ctx, wf, intent.Session, body)
	if err != nil {
		return model.EffectResultInput{}, err
	}
	out.Artifact = ref
	reFrozen, err := a.readChangeSetBody(ctx, wf, ref.Revision, ref.Hash)
	if err != nil {
		return model.EffectResultInput{}, err
	}
	failAdoption := func(code model.Code) model.EffectResultInput {
		out.Session.Status = model.SessionFailed
		out.FailureCode = code
		return out
	}
	// The re-frozen Change Set body is the adoption evidence the Kernel
	// re-judges: the Kernel requires it to carry at least one real Commit.
	out.Body = body
	// The adoption evidence requires the re-frozen Change Set to carry at
	// least one real Commit (design 8.4 step 2): a session that moved the
	// workspace to a head with an empty commit range (a reset to a foreign
	// or past head) produces no adoption Commit and Blocks the gate.
	if len(reFrozen.Commits) == 0 {
		return failAdoption(model.CodeMissingImplementationCommit), nil
	}
	// The post-adoption gate chain (design 8.4 steps 3-4): the Workspace
	// Branch, the Commit Policy Identity/Signing preflight, the Clean/Scope
	// check over the re-frozen Change Set, and the fixed Verification
	// Catalog over base..new-candidate.
	if err := a.verifyWorkspaceBranch(ctx, cwd, view.State.Workflow.WorkspaceBranch); err != nil {
		if code, ok := model.CodeOf(err); ok {
			return failAdoption(code), nil
		}
		return model.EffectResultInput{}, err
	}
	if err := a.verifyAdoptionPreflight(ctx, wf, facts); err != nil {
		if code, ok := model.CodeOf(err); ok {
			return failAdoption(code), nil
		}
		return model.EffectResultInput{}, err
	}
	if err := a.verifyAdoptionScope(ctx, wf, reFrozen); err != nil {
		if code, ok := model.CodeOf(err); ok {
			return failAdoption(code), nil
		}
		return model.EffectResultInput{}, err
	}
	if err := a.verifyAdoptionCatalog(ctx, wf, facts, base, post.Head); err != nil {
		if code, ok := model.CodeOf(err); ok {
			return failAdoption(code), nil
		}
		return model.EffectResultInput{}, err
	}
	return out, nil
}

// adoptionCodingSessionInput builds the managed adoption Session's typed
// input block: the frozen Change Set body the approval bound (the candidate
// facts the adoption Session organizes and commits) and the Workspace
// facts.
func (a *Application) adoptionCodingSessionInput(ctx context.Context, wf model.WorkflowID, cwd string) (any, error) {
	store, err := a.artifactStore(wf)
	if err != nil {
		return nil, err
	}
	view, err := a.writeStoreView(ctx, wf)
	if err != nil {
		return nil, err
	}
	facts := view.State.Workflow.ExecutionFacts
	if facts == nil || facts.ChangeSetRevision < 1 || facts.ChangeSetHash == "" {
		return nil, model.InvariantFault(fmt.Errorf("the adoption gate lost the bound change set facts"))
	}
	// The BOUND Change Set Revision the Execution Approval referenced, never
	// the ACTIVE revision (the adoption re-freeze may already have advanced
	// the active reference): the adoption Session organizes the exact
	// approved diff.
	changeSetBody, err := store.Get(ctx, model.ArtifactRef{
		Workflow: wf, Type: model.ArtifactChangeSet,
		Revision: facts.ChangeSetRevision, Hash: facts.ChangeSetHash,
	})
	if err != nil {
		return nil, err
	}
	var changeSet model.ChangeSet
	if err := jsonUnmarshal(changeSetBody, &changeSet); err != nil {
		return nil, model.InvariantFault(fmt.Errorf("the frozen change set body is not canonical"))
	}
	base := view.State.Workflow.BaseCommit
	return struct {
		ChangeSet     string `json:"change_set"`
		Workspace     string `json:"workspace"`
		CommitRange   string `json:"commit_range"`
		Diff          string `json:"diff"`
		CandidateHead string `json:"candidate_head"`
	}{
		ChangeSet:     string(changeSetBody),
		Workspace:     cwd,
		CommitRange:   base + ".." + changeSet.CandidateHead,
		Diff:          a.gitDiff(ctx, cwd, base+".."+changeSet.CandidateHead),
		CandidateHead: changeSet.CandidateHead,
	}, nil
}

// adoptionReviewSessionInput builds the Adoption Reviewer's typed input
// block: the frozen Change Set body, the approved Plan/Spec/Catalog, the
// Workspace facts, and the candidate Diff.
func (a *Application) adoptionReviewSessionInput(ctx context.Context, wf model.WorkflowID, cwd string) (any, error) {
	store, err := a.artifactStore(wf)
	if err != nil {
		return nil, err
	}
	view, err := a.writeStoreView(ctx, wf)
	if err != nil {
		return nil, err
	}
	// The ACTIVE Change Set Revision: after a managed adoption the Runtime
	// re-froze the Change Set against the post-adoption candidate Head, so
	// the review judges the latest frozen Revision (the re-bound facts),
	// never the stale revision the approval bound before the adoption ran.
	changeSetBody, err := readRequiredArtifact(ctx, store, wf, model.ArtifactChangeSet)
	if err != nil {
		return nil, err
	}
	var changeSet model.ChangeSet
	if err := jsonUnmarshal(changeSetBody, &changeSet); err != nil {
		return nil, model.InvariantFault(fmt.Errorf("the change set body is not canonical"))
	}
	plan, err := readRequiredArtifact(ctx, store, wf, model.ArtifactPlan)
	if err != nil {
		return nil, err
	}
	spec, err := readRequiredArtifact(ctx, store, wf, model.ArtifactSpec)
	if err != nil {
		return nil, err
	}
	catalog, err := readRequiredArtifact(ctx, store, wf, model.ArtifactCatalog)
	if err != nil {
		return nil, err
	}
	workflow, err := readRequiredArtifact(ctx, store, wf, model.ArtifactWorkflow)
	if err != nil {
		return nil, err
	}
	base := view.State.Workflow.BaseCommit
	return struct {
		ChangeSet     string `json:"change_set"`
		Plan          string `json:"plan"`
		Spec          string `json:"spec"`
		Catalog       string `json:"catalog"`
		Workflow      string `json:"workflow"`
		Workspace     string `json:"workspace"`
		CommitRange   string `json:"commit_range"`
		Diff          string `json:"diff"`
		CandidateHead string `json:"candidate_head"`
	}{
		ChangeSet:     string(changeSetBody),
		Plan:          string(plan),
		Spec:          string(spec),
		Catalog:       string(catalog),
		Workflow:      string(workflow),
		Workspace:     cwd,
		CommitRange:   base + ".." + changeSet.CandidateHead,
		Diff:          a.gitDiff(ctx, cwd, base+".."+changeSet.CandidateHead),
		CandidateHead: changeSet.CandidateHead,
	}, nil
}
