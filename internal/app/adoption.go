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
	// and its candidate facts must match the re-observed Workspace exactly
	// (Approval 后漂移 closes the gate before any Session starts).
	changeSet, err := a.readChangeSetBody(ctx, wf, facts.ChangeSetRevision, facts.ChangeSetHash)
	if err != nil {
		return nil, "", model.NewFault(model.CodeEvidenceSubjectChanged,
			"the frozen change set no longer resolves; the workspace drifted after the approval")
	}
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
	if !status.Clean() {
		return nil, "", model.NewFault(model.CodeDirtyWorktreeDrifted,
			"the workspace is not git-clean; commit or discard the native changes before adopting")
	}
	if err := a.verifyAdoptionScope(ctx, wf, changeSet); err != nil {
		return nil, "", err
	}
	// The fixed Verification Catalog runs over the full candidate range
	// base..candidate inside the Workspace.
	if err := a.verifyAdoptionCatalog(ctx, wf, facts, view.State.Workflow.BaseCommit, status.Head); err != nil {
		return nil, "", err
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
// recorded CFlow-owned Workspace Branch (design 8.2).
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
			if path == scope || strings.HasPrefix(path, strings.TrimSuffix(scope, "/")+"/") {
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
	facts := view.State.Workflow.ExecutionFacts
	changeSet, err := a.readChangeSetBody(ctx, wf, facts.ChangeSetRevision, facts.ChangeSetHash)
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
		ChangeSet:     string(readArtifact(ctx, store, wf, model.ArtifactChangeSet)),
		Plan:          string(readArtifact(ctx, store, wf, model.ArtifactPlan)),
		Spec:          string(readArtifact(ctx, store, wf, model.ArtifactSpec)),
		Catalog:       string(readArtifact(ctx, store, wf, model.ArtifactCatalog)),
		Workflow:      string(readArtifact(ctx, store, wf, model.ArtifactWorkflow)),
		Workspace:     cwd,
		CommitRange:   base + ".." + changeSet.CandidateHead,
		Diff:          a.gitDiff(ctx, cwd, base+".."+changeSet.CandidateHead),
		CandidateHead: changeSet.CandidateHead,
	}, nil
}
