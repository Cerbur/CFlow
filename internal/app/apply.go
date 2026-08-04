package app

// The protected Apply executors (Task 19, PRD 已确认：显式受保护 Apply,
// design 15.5): the staging executor (isolated Apply Branch/Worktree,
// Commit Policy revalidation, --no-ff merge with the ONE restricted Merge
// Resolution Attempt, Merge Commit Preflight match, deterministic apply
// verification), and the explicit delivery (the pre-delivery rechecks
// and the compare-and-swap fast-forward). The deterministic verification
// and the independent Apply Verification Session live in the same-package
// apply_verify.go split; the command builders in apply_prepare.go.
//
// Invariants: the user's working tree is never operated on until the
// final fast-forward compare-and-swap; the delivery argv is only ever
// `git update-ref <target> <staging-head> <expected-target>` (no
// force-update form exists); a failure leaves the Target exactly old or
// exactly the verified new head; the completed Workflow is never
// altered.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"

	"cflow.local/cflow/internal/agent"
	"cflow.local/cflow/internal/gitflow"
	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/verify"
)

// ---------------------------------------------------------------------------
// Apply staging executor
// ---------------------------------------------------------------------------

// applyStagingCreate runs the isolated Apply staging (PRD steps 1-4):
// the workspace gate, the Apply Branch/Worktree from the recorded Target
// HEAD, the Commit Policy revalidation, the --no-ff merge (with the ONE
// restricted Merge Resolution Attempt on a text conflict), the Merge
// Commit identity match against the Preflight, and the full deterministic
// apply verification inside the Apply Worktree. The user's working tree
// is never touched.
func (a *Application) applyStagingCreate(ctx context.Context, wf model.WorkflowID, intent model.ApplyStagingCreateIntent, rt *agent.Runtime) (model.EffectResultInput, error) {
	fail := func(code model.Code, reason string) model.EffectResultInput {
		return model.EffectResultInput{Kind: model.ApplyStagingFailed, ApplyAttempt: intent.Apply, FailureCode: code, Reason: reason}
	}
	if a.git == nil {
		return model.EffectResultInput{}, model.InvariantFault(fmt.Errorf("git seam is not configured for this application"))
	}
	view, err := a.writeStoreView(ctx, wf)
	if err != nil {
		return model.EffectResultInput{}, err
	}
	att := findApplyAttemptOf(view.State, intent.Apply)
	if att == nil {
		return model.EffectResultInput{}, model.InvariantFault(fmt.Errorf("apply attempt %s is missing", intent.Apply))
	}
	targetBranch := view.State.Workflow.TargetBranch
	integrationBranch := view.State.Workflow.IntegrationBranch

	// 1. The workspace gate (PRD step 1: clean and attached to the
	// expected Target Branch at the recorded head).
	if err := a.applyWorkspaceGate(ctx, targetBranch, att.TargetHead); err != nil {
		code, _ := model.CodeOf(err)
		return fail(code, err.Error()), nil
	}

	// 2. The isolated Apply Branch/Worktree from the recorded Target
	// HEAD (never the user's working tree).
	path, err := a.applyWorktreeEnsure(ctx, wf, att)
	if err != nil {
		code, _ := model.CodeOf(err)
		return fail(code, err.Error()), nil
	}

	// 3. The Commit Identity/Signing Policy revalidation (PRD step 3).
	fingerprint, err := a.observePolicyFingerprint(ctx, "apply-"+string(intent.Apply))
	if err != nil {
		code, _ := model.CodeOf(err)
		return fail(code, err.Error()), nil
	}
	if fingerprint != att.Fingerprint {
		return fail(model.CodeCommitPolicyConfirmationRequired,
			"the commit policy changed since the apply attempt was recorded; confirm the exact new preflight first"), nil
	}

	// 4. The --no-ff merge of the Integration Branch (or the conflicted
	// continuation after the ONE restricted resolution attempt).
	stagingHead, _, err := a.applyMergeEnsure(ctx, wf, att, path, integrationBranch, intent.ResolutionSession, rt)
	if err != nil {
		code, _ := model.CodeOf(err)
		return fail(code, err.Error()), nil
	}

	// 5. The Merge Commit must match the Preflight (PRD step 3: the Apply
	// Merge Commit created must match the Preflight).
	if err := a.verifyApplyCommit(ctx, wf, att, stagingHead); err != nil {
		code, _ := model.CodeOf(err)
		return fail(code, err.Error()), nil
	}

	// 6. The full deterministic apply verification on the combined
	// result, inside the Apply Worktree (PRD step 4). The Catalog
	// identity and the wrapper/manifest/executable hashes are
	// revalidated here; an identity drift is COMMAND_IDENTITY_CHANGED and
	// the drifted tool never runs.
	manifest, err := a.applyVerificationRun(ctx, wf, att, stagingHead, path)
	if err != nil {
		if code, ok := model.CodeOf(err); ok && code == model.CodeEvidenceSubjectChanged {
			return fail(model.CodeCommandIdentityChanged,
				"the apply verification identity changed since the approval; approve the newly discovered catalog revision first"), nil
		}
		code, _ := model.CodeOf(err)
		return fail(code, err.Error()), nil
	}
	if !manifest.Passed {
		return fail(model.CodeIntegrationVerificationFailed,
			"the combined apply result failed the deterministic verification: "+manifest.Reason), nil
	}
	return model.EffectResultInput{
		Kind:         model.ApplyStagingSucceeded,
		ApplyAttempt: intent.Apply,
		EndHead:      stagingHead,
		ManifestHash: manifest.Hash,
		Passed:       true,
	}, nil
}

// applyWorkspaceGate is the authoritative workspace gate of the staging
// and the delivery: clean, attached to the Target Branch, and at the
// recorded head ("" skips the head comparison).
func (a *Application) applyWorkspaceGate(ctx context.Context, targetBranch, targetHead string) error {
	head, err := a.applyWorkspaceHead(ctx, targetBranch)
	if err != nil {
		return err
	}
	if targetHead != "" && head != targetHead {
		return model.NewFault(model.CodeTargetHeadChanged,
			"the target head no longer matches the recorded head")
	}
	return nil
}

// applyWorktreePath is the deterministic Apply Worktree location of one
// attempt (PRD 全局目录结构; each attempt stages in its own isolated
// Worktree, preserved for inspection and retry).
func (a *Application) applyWorktreePath(wf model.WorkflowID, number int) string {
	return filepath.Join(a.home, "worktrees", a.project.Key, string(wf),
		fmt.Sprintf("apply-%d", number))
}

// applyBranchName is the deterministic Apply Branch of one attempt.
func (a *Application) applyBranchName(wf model.WorkflowID, number int) string {
	return fmt.Sprintf("cflow/%s/apply-%d", wf, number)
}

// applyWorktreeEnsure creates the Apply Branch/Worktree from the
// recorded Target HEAD or reuses the existing one of the same attempt
// (a re-run after a block or an interruption), verifying the registry
// entry matches the branch.
func (a *Application) applyWorktreeEnsure(ctx context.Context, wf model.WorkflowID, att *model.ApplyAttempt) (string, error) {
	path := a.applyWorktreePath(wf, att.Number)
	branch := a.applyBranchName(wf, att.Number)
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
			if e.Detached || e.Branch != branch {
				return "", model.NewFault(model.CodeStateInvariantViolation,
					"the apply worktree no longer matches its branch")
			}
			return path, nil
		}
	}
	res, err := a.git.Execute(ctx, gitflow.CreateApply{Branch: branch, BaseHead: att.TargetHead, Path: path})
	if err != nil {
		return "", err
	}
	ar, ok := res.(gitflow.ApplyWorktreeResult)
	if !ok {
		return "", model.InvariantFault(fmt.Errorf("apply worktree creation has an unexpected result"))
	}
	return ar.Worktree, nil
}

// applyMergeEnsure brings the Integration Branch into the Apply Worktree
// with --no-ff (design 15.5, PRD step 3). A merge already complete is
// skipped; a conflicted merge in progress runs the ONE restricted Merge
// Resolution Session (when one was allocated) and completes the merge
// through the recorded parents; a conflict without an allocated
// resolution blocks with MERGE_CONFLICT and the conflicted state stays
// preserved. Returns the staging head and the conflict file set.
func (a *Application) applyMergeEnsure(ctx context.Context, wf model.WorkflowID, att *model.ApplyAttempt, path, integrationBranch string, resolutionSession model.SessionID, rt *agent.Runtime) (string, []string, error) {
	conflictFiles, err := a.unmergedPathsOf(ctx, path)
	if err != nil {
		return "", nil, err
	}
	if len(conflictFiles) > 0 {
		// A conflicted merge in progress: the ONE restricted resolution.
		if resolutionSession == "" {
			return "", nil, model.NewFault(model.CodeMergeConflict,
				"text conflict in the apply worktree; the attempt blocks with the conflicted state preserved for the one restricted resolution attempt")
		}
		if err := a.runApplyResolution(ctx, wf, att, path, conflictFiles, resolutionSession, rt); err != nil {
			return "", nil, err
		}
		res, err := a.git.Execute(ctx, gitflow.CompleteMerge{
			Path: path, ConflictFiles: conflictFiles, Message: "cflow: apply merge " + integrationBranch,
		})
		if err != nil {
			return "", nil, err
		}
		mr, ok := res.(gitflow.MergeResult)
		if !ok {
			return "", nil, model.InvariantFault(fmt.Errorf("merge continuation has an unexpected result"))
		}
		return mr.Head, conflictFiles, nil
	}
	// The integration output is already contained: the merge is a no-op.
	integrationHead, err := a.observedRefHead(ctx, "refs/heads/"+integrationBranch)
	if err != nil {
		return "", nil, err
	}
	contained, err := a.refContained(ctx, path, integrationHead)
	if err != nil {
		return "", nil, err
	}
	if contained {
		head, err := a.worktreeHead(ctx, path)
		return head, nil, err
	}
	res, err := a.git.Execute(ctx, gitflow.MergeIntegration{
		Path: path, Branch: integrationBranch, Message: "cflow: apply merge " + integrationBranch,
	})
	if err != nil {
		return "", nil, err
	}
	switch r := res.(type) {
	case gitflow.MergeResult:
		return r.Head, nil, nil
	case gitflow.MergeConflictResult:
		return "", nil, model.NewFault(model.CodeMergeConflict,
			"text conflict in the apply worktree; the attempt blocks with the conflicted state preserved for the one restricted resolution attempt")
	default:
		return "", nil, model.InvariantFault(fmt.Errorf("apply merge has an unexpected result"))
	}
}

// refContained reports whether head is an ancestor of the worktree HEAD
// (the merge already happened).
func (a *Application) refContained(ctx context.Context, dir, head string) (bool, error) {
	current, err := a.worktreeHead(ctx, dir)
	if err != nil {
		return false, err
	}
	facts, err := a.git.Observe(ctx, gitflow.HistoryRange{From: current, To: head})
	if err != nil {
		return false, err
	}
	rf, ok := facts.(gitflow.RangeFacts)
	if !ok {
		return false, model.InvariantFault(fmt.Errorf("history range observation has an unexpected type"))
	}
	return len(rf.Commits) == 0, nil
}

// worktreeHead resolves the HEAD of one managed worktree.
func (a *Application) worktreeHead(ctx context.Context, dir string) (string, error) {
	status, err := a.observeSnapshot(ctx, dir)
	if err != nil {
		return "", err
	}
	if status.Head == "" {
		return "", model.NewFault(model.CodeStateInvariantViolation, "the managed worktree has no head")
	}
	return status.Head, nil
}

// runApplyResolution runs the ONE restricted Merge Resolution Session
// inside the Apply Worktree (PRD step 3: write scope = the conflict files
// plus the union of the related Specs write scopes). The resolution
// audit Ref is pinned before the session (expected-absent), so a second
// resolution is never allowed.
func (a *Application) runApplyResolution(ctx context.Context, wf model.WorkflowID, att *model.ApplyAttempt, path string, conflictFiles []string, resolutionSession model.SessionID, rt *agent.Runtime) error {
	if rt == nil {
		return model.InvariantFault(fmt.Errorf("agent runtime is not configured for this application"))
	}
	ref := fmt.Sprintf("refs/cflow/%s/apply/%s/resolution", wf, att.ID)
	if _, err := a.git.Execute(ctx, gitflow.CreateAuditRef{Ref: ref, Head: att.TargetHead}); err != nil {
		return model.NewFault(model.CodeMergeConflict,
			"the one restricted merge resolution attempt was already used")
	}
	prompt, ok := a.promptForPurpose(model.PurposeRepair)
	if !ok {
		return model.InvalidInputFault("no embedded prompt for the merge resolution purpose")
	}
	provider := a.sessionProviderOf(wf, resolutionSession)
	if provider == "" {
		return model.InvariantFault(fmt.Errorf("the resolution session has no recorded provider"))
	}
	writeScope := a.applyResolutionWriteScope(ctx, wf, conflictFiles)
	input := map[string]any{
		"conflict_files": conflictFiles,
		"write_scope":    writeScope,
		"message":        "resolve the apply merge conflict inside the declared conflict files only",
	}
	pre, err := a.observeSnapshot(ctx, path)
	if err != nil {
		return err
	}
	res, err := rt.Start(ctx, agent.StartRequest{
		Purpose:   model.PurposeRepair,
		Provider:  provider,
		Prompt:    prompt.Body,
		Input:     a.providerTypedInput(ctx, rt, model.PurposeRepair, provider, input),
		CWD:       path,
		SessionID: resolutionSession,
	})
	if err != nil {
		return err
	}
	if res.Terminal != nil && res.Terminal.Type == agent.EventFailed {
		code := model.Code(res.Terminal.Code)
		if code == "" {
			code = model.CodeAgentProcessCrashed
		}
		return model.NewFault(code, "the merge resolution session failed")
	}
	if err := a.verifySnapshotUnchanged(ctx, path, pre); err != nil {
		return err
	}
	// The resolution may only change the declared conflict files: the
	// merge continuation stages exactly those and fails closed on any
	// change outside them (the post-merge clean check).
	return nil
}

// sessionProviderOf resolves the recorded Provider of one Session.
func (a *Application) sessionProviderOf(wf model.WorkflowID, session model.SessionID) string {
	view, err := a.writeStoreView(context.Background(), wf)
	if err != nil {
		return ""
	}
	for _, s := range view.State.Sessions {
		if s.ID == session {
			return s.Provider
		}
	}
	return ""
}

// applyResolutionWriteScope is the restricted write scope of the ONE
// resolution attempt: the conflict files plus the union of the approved
// Specs' write scopes.
func (a *Application) applyResolutionWriteScope(ctx context.Context, wf model.WorkflowID, conflictFiles []string) []string {
	store, err := a.artifactStore(wf)
	if err != nil {
		return conflictFiles
	}
	body := readArtifact(ctx, store, wf, model.ArtifactSpec)
	specs, err := a.parseSpecSet(body)
	if err != nil {
		return conflictFiles
	}
	seen := map[string]struct{}{}
	for _, c := range conflictFiles {
		seen[c] = struct{}{}
	}
	for _, s := range specs {
		for _, p := range s.WriteScope {
			seen[p] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// verifyApplyCommit verifies the Apply Merge Commit's actual author,
// committer, and signing evidence against the attempt's bound Preflight
// (PRD step 3: the Apply Merge Commit created must match the Preflight).
func (a *Application) verifyApplyCommit(ctx context.Context, wf model.WorkflowID, att *model.ApplyAttempt, head string) error {
	ev, ok := a.readApplyPreflightEvidence(ctx, wf, att)
	if !ok {
		return model.NewFault(model.CodeEvidenceSubjectChanged,
			"the apply preflight evidence cannot be read")
	}
	_, err := a.git.Execute(ctx, gitflow.VerifyCommit{
		Ref:               head,
		ExpectedAuthor:    ev.Author,
		ExpectedCommitter: ev.Committer,
		ExpectedSigning:   ev.Signing,
	})
	return err
}

// readApplyPreflightEvidence reads the Preflight evidence the Apply
// Attempt bound (its recorded Revision/hash — after a confirmation this
// is the fresh Preflight).
func (a *Application) readApplyPreflightEvidence(ctx context.Context, wf model.WorkflowID, att *model.ApplyAttempt) (gitflow.PreflightEvidence, bool) {
	if att.PreflightHash == "" || att.Preflight.Revision < 1 {
		return gitflow.PreflightEvidence{}, false
	}
	store, err := a.artifactStore(wf)
	if err != nil {
		return gitflow.PreflightEvidence{}, false
	}
	body, err := store.Get(ctx, model.ArtifactRef{
		Workflow: wf, Type: model.ArtifactReport,
		Revision: att.Preflight.Revision, Hash: att.PreflightHash,
	})
	if err != nil {
		return gitflow.PreflightEvidence{}, false
	}
	var raw string
	if err := json.Unmarshal(body, &raw); err != nil {
		return gitflow.PreflightEvidence{}, false
	}
	var ev gitflow.PreflightEvidence
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		return gitflow.PreflightEvidence{}, false
	}
	return ev, true
}

// ---------------------------------------------------------------------------
// The explicit delivery (the final compare-and-swap)
// ---------------------------------------------------------------------------

// applyFastForward is the explicit delivery (PRD steps 5-6): immediately
// before the delivery every fact is rechecked — the user workspace clean
// and attached to the Target Branch, the Target HEAD and the Integration
// HEAD equal to the recorded heads, the Commit Policy fingerprint and
// the evidence subjects unchanged — and the Target Branch is updated
// only through `git update-ref <target> <staging-head> <expected-target>`
// when the result is a fast-forward. The outcome is reported after
// observing the actual ref: a delivery that already committed (a crash
// after the compare-and-swap) settles SUCCEEDED from the observation
// without a second update; a ref that moved anywhere else blocks with
// TARGET_HEAD_DRIFTED. No force-update argv exists anywhere in this
// path.
func (a *Application) applyFastForward(ctx context.Context, wf model.WorkflowID, intent model.ApplyFastForwardIntent) (model.EffectResultInput, error) {
	fail := func(code model.Code, reason string) model.EffectResultInput {
		return model.EffectResultInput{Kind: model.ApplyFastForwardFailed, ApplyAttempt: intent.Apply, FailureCode: code, Reason: reason}
	}
	if a.git == nil {
		return model.EffectResultInput{}, model.InvariantFault(fmt.Errorf("git seam is not configured for this application"))
	}
	view, err := a.writeStoreView(ctx, wf)
	if err != nil {
		return model.EffectResultInput{}, err
	}
	att := findApplyAttemptOf(view.State, intent.Apply)
	if att == nil {
		return model.EffectResultInput{}, model.InvariantFault(fmt.Errorf("apply attempt %s is missing", intent.Apply))
	}
	targetBranch := view.State.Workflow.TargetBranch
	integrationBranch := view.State.Workflow.IntegrationBranch
	targetRef := "refs/heads/" + targetBranch

	// The staging head is the Apply Branch ref (the authoritative git
	// fact; never persisted).
	stagingHead, err := a.observedRefHead(ctx, "refs/heads/"+a.applyBranchName(wf, att.Number))
	if err != nil {
		code, _ := model.CodeOf(err)
		return fail(code, err.Error()), nil
	}

	// The observed Target decides the crash recovery first: a Target
	// already at the verified staging head is the delivered outcome (a
	// crash after the compare-and-swap) — never ambiguous, never
	// re-swapped.
	observedTarget, err := a.observedRefHead(ctx, targetRef)
	if err != nil {
		code, _ := model.CodeOf(err)
		return fail(code, err.Error()), nil
	}
	switch {
	case observedTarget == stagingHead:
		return model.EffectResultInput{
			Kind: model.ApplyFastForwardSucceeded, ApplyAttempt: intent.Apply, ObservedHead: observedTarget,
		}, nil
	case observedTarget != att.TargetHead:
		return fail(model.CodeTargetHeadChanged,
			"the target branch no longer matches the recorded head"), nil
	}

	// The pre-delivery rechecks (PRD step 5).
	if err := a.applyWorkspaceGate(ctx, targetBranch, att.TargetHead); err != nil {
		code, _ := model.CodeOf(err)
		return fail(code, err.Error()), nil
	}
	if got, err := a.observedRefHead(ctx, "refs/heads/"+integrationBranch); err != nil || got != att.IntegrationHead {
		if err != nil {
			code, _ := model.CodeOf(err)
			return fail(code, err.Error()), nil
		}
		return fail(model.CodeTargetHeadChanged,
			"the integration branch no longer matches the recorded head"), nil
	}
	fingerprint, err := a.observePolicyFingerprint(ctx, "apply-"+string(intent.Apply)+"-deliver")
	if err != nil {
		code, _ := model.CodeOf(err)
		return fail(code, err.Error()), nil
	}
	if fingerprint != att.Fingerprint {
		return fail(model.CodeCommitPolicyConfirmationRequired,
			"the commit policy changed before the delivery; confirm the exact new preflight first"), nil
	}
	// The evidence subjects: the approved Catalog Revision revalidates
	// and the deterministic apply verification manifest re-reads
	// unchanged.
	catalog, err := a.applyCatalogRef(ctx, wf, att, view.State)
	if err != nil {
		code, _ := model.CodeOf(err)
		return fail(code, err.Error()), nil
	}
	engine, err := verify.NewEngine(verify.EngineOptions{
		Supervisor: a.supervisor, GitFlow: a.git, Redaction: a.redaction,
		LoadCatalog: func(ctx context.Context, ref model.CatalogRef) ([]byte, error) {
			return a.readCatalogBody(ctx, wf, ref)
		},
	})
	if err != nil {
		return model.EffectResultInput{}, err
	}
	if _, err := engine.ValidateCatalog(ctx, catalog); err != nil {
		return fail(model.CodeCommandIdentityChanged,
			"the apply verification catalog identity changed before the delivery"), nil
	}
	if a.applyVerificationManifestHash(wf, att) == "" {
		return fail(model.CodeEvidenceSubjectChanged,
			"the apply verification evidence is missing before the delivery"), nil
	}

	// The final compare-and-swap fast-forward. The gitflow operation
	// re-observes the expected head, verifies the fast-forward, and
	// updates only through the expected-value argv; the outcome is the
	// observed actual ref. A typed TARGET_HEAD_DRIFTED is a blocked
	// delivery; any other failure is the unsettled crash path (the
	// attempt stays RUNNING and the retry observes).
	res, err := a.git.Execute(ctx, gitflow.UpdateRef{Ref: targetRef, New: stagingHead, Expected: att.TargetHead})
	if err != nil {
		code, ok := model.CodeOf(err)
		if ok && code == model.CodeTargetHeadChanged {
			return fail(code, err.Error()), nil
		}
		return model.EffectResultInput{}, err
	}
	ur, ok := res.(gitflow.UpdateRefResult)
	if !ok {
		return model.EffectResultInput{}, model.InvariantFault(fmt.Errorf("target update has an unexpected result"))
	}
	if ur.Observed != stagingHead {
		return fail(model.CodeTargetHeadChanged,
			"the observed target ref does not match the delivered head"), nil
	}
	return model.EffectResultInput{
		Kind: model.ApplyFastForwardSucceeded, ApplyAttempt: intent.Apply, ObservedHead: ur.Observed,
	}, nil
}

// ---------------------------------------------------------------------------
// small helpers
// ---------------------------------------------------------------------------

// applyAttemptStagingOf returns the Apply Attempt currently in the
// staging phase (the review Session runs only while the attempt stages).
func applyAttemptStagingOf(st model.State) *model.ApplyAttempt {
	for i := range st.ApplyAttempts {
		if st.ApplyAttempts[i].Status == model.ApplyStaging {
			return &st.ApplyAttempts[i]
		}
	}
	return nil
}

func lastApplyAttemptOf(st model.State) *model.ApplyAttempt {
	if len(st.ApplyAttempts) == 0 {
		return nil
	}
	return &st.ApplyAttempts[len(st.ApplyAttempts)-1]
}

func findApplyAttemptOf(st model.State, id model.ApplyAttemptID) *model.ApplyAttempt {
	for i := range st.ApplyAttempts {
		if st.ApplyAttempts[i].ID == id {
			return &st.ApplyAttempts[i]
		}
	}
	return nil
}

func sha256HexString(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// lastApplyBlockedCode returns the Code of the last APPLY_BLOCKED event
// of one command's committed events ("" when none).
func lastApplyBlockedCode(events []model.Event) model.Code {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Kind == model.EventApplyBlocked {
			return events[i].Code
		}
	}
	return ""
}
