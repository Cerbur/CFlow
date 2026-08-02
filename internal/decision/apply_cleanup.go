package decision

import (
	"fmt"

	"cflow.local/cflow/internal/model"
)

// decideApply handles the user Apply interaction: a post-completion,
// user-initiated delivery attempt that revalidates Integration output and
// may fast-forward the Target Branch (CONTEXT.md: Apply). Apply success
// never alters the Workflow's completed state (design 7.3 invariant 12).
func decideApply(state model.State, in model.ApplyCommandInput) (model.Decision, error) {
	switch in.Kind {
	case model.ApplyRequest:
		return applyRequest(state, in)
	case model.ApplyConfirm:
		return applyConfirm(state, in)
	default:
		return model.Decision{}, model.InvalidInputFault("unsupported apply command")
	}
}

// applyRequest opens a new Apply Attempt against a completed Workflow.
// The Integration output may not come from a quarantined Branch, and the
// attempt records the exact Target/Integration HEAD and Commit Policy
// Preflight facts its confirmation must re-bind.
func applyRequest(state model.State, in model.ApplyCommandInput) (model.Decision, error) {
	if state.Workflow.Stage != model.StageCompleted || state.Workflow.Runtime != model.RuntimeSucceeded {
		return model.Decision{}, model.InvalidInputFault("apply requires a completed workflow")
	}
	if branchQuarantined(state, state.Workflow.IntegrationBranch) {
		return model.Decision{}, model.NewFault(model.CodeCommitDuringPolicyDriftWindow,
			"a quarantined integration branch can never re-enter Apply")
	}
	if in.TargetHead == "" || in.IntegrationHead == "" {
		return model.Decision{}, model.InvalidInputFault("apply requires target and integration HEAD values")
	}
	b := &builder{state: state}
	att := model.ApplyAttempt{
		ID:              model.ApplyAttemptID(fmt.Sprintf("apply-%d", len(state.ApplyAttempts)+1)),
		Status:          model.ApplyStaging,
		TargetHead:      in.TargetHead,
		IntegrationHead: in.IntegrationHead,
		Preflight:       in.Preflight,
		PreflightHash:   in.PreflightHash,
		Fingerprint:     in.Fingerprint,
		StartedAt:       state.Now,
	}
	b.mutate(model.ApplyAppendMutation{ApplyAttempt: att})
	b.event(model.EventApplyAttemptCreated, "", model.AttemptKey{}, "", "apply attempt created")
	b.effect(model.ApplyStagingCreateIntent{Apply: att.ID, TargetHead: in.TargetHead, IntegrationHead: in.IntegrationHead})
	return b.decision(), nil
}

// applyConfirm is the exact compare-and-swap confirmation: it re-binds
// the Apply Attempt, the Target HEAD, the Integration HEAD, and the exact
// Preflight Revision/hash/fingerprint. A drifted Target or Integration
// HEAD is TARGET_HEAD_DRIFTED; a drifted Preflight hash or fingerprint is
// COMMIT_POLICY_INPUT_CHANGED (PRD 约束 40-41, design 15.5).
func applyConfirm(state model.State, in model.ApplyCommandInput) (model.Decision, error) {
	att := lastApplyAttempt(state)
	if att == nil || att.Status != model.ApplyAwaitingConfirmation {
		return model.Decision{}, model.InvalidInputFault("no apply attempt awaiting confirmation")
	}
	if in.TargetHead != att.TargetHead || in.IntegrationHead != att.IntegrationHead {
		return model.Decision{}, model.NewFault(model.CodeTargetHeadChanged,
			"target or integration HEAD drifted since the apply staging")
	}
	if in.PreflightHash != att.PreflightHash || in.Fingerprint != att.Fingerprint {
		return model.Decision{}, model.NewFault(model.CodeCommitPolicyInputChanged,
			"commit-policy preflight facts changed since the apply staging")
	}
	b := &builder{state: state}
	b.mutate(model.ApplyMutation{ID: att.ID, Status: model.ApplyRunning})
	b.effect(model.ApplyFastForwardIntent{Apply: att.ID, TargetHead: att.TargetHead})
	return b.decision(), nil
}

// decideApplyResult settles one Apply Effect Result. Success marks the
// Apply Attempt SUCCEEDED without touching the completed Workflow.
func decideApplyResult(state model.State, in model.EffectResultInput) (model.Decision, error) {
	att := findApplyAttempt(state, in.ApplyAttempt)
	if att == nil {
		return model.Decision{}, model.InvalidInputFault("unknown apply attempt")
	}
	b := &builder{state: state}
	switch in.Kind {
	case model.ApplyStagingSucceeded:
		if att.Status != model.ApplyStaging {
			return model.Decision{}, model.InvalidInputFault("apply attempt is not staging")
		}
		b.mutate(model.ApplyMutation{ID: att.ID, Status: model.ApplyAwaitingConfirmation})
	case model.ApplyFastForwardSucceeded:
		if att.Status != model.ApplyRunning {
			return model.Decision{}, model.InvalidInputFault("apply attempt is not running")
		}
		b.mutate(model.ApplyMutation{ID: att.ID, Status: model.ApplySucceeded, EndedAt: state.Now})
		b.event(model.EventApplySucceeded, "", model.AttemptKey{}, "", "apply succeeded")
	case model.ApplyFastForwardFailed:
		if att.Status != model.ApplyRunning {
			return model.Decision{}, model.InvalidInputFault("apply attempt is not running")
		}
		b.mutate(model.ApplyMutation{ID: att.ID, Status: model.ApplyBlocked, EndedAt: state.Now})
		b.event(model.EventApplyBlocked, "", model.AttemptKey{}, "", "apply blocked")
	}
	return b.decision(), nil
}

func lastApplyAttempt(state model.State) *model.ApplyAttempt {
	if len(state.ApplyAttempts) == 0 {
		return nil
	}
	return &state.ApplyAttempts[len(state.ApplyAttempts)-1]
}

func findApplyAttempt(state model.State, id model.ApplyAttemptID) *model.ApplyAttempt {
	for i := range state.ApplyAttempts {
		if state.ApplyAttempts[i].ID == id {
			return &state.ApplyAttempts[i]
		}
	}
	return nil
}

// decideCancel is the recoverable cancellation protocol: the intent is
// persisted and dispatch closes first; the terminal CANCELLED Decision is
// committed only after all managed processes settle and facts reconcile
// (PRD 状态机与持久化模型, design 17.4). Cancel never allocates a Retry,
// starts a Provider, or generates an Artifact (design 6.1).
func decideCancel(state model.State, in model.WorkflowCommandInput) (model.Decision, error) {
	if state.Workflow.ID == "" {
		return model.Decision{}, model.InvalidInputFault("no workflow to cancel")
	}
	if state.Workflow.Runtime.IsTerminal() {
		return model.Decision{}, model.InvalidInputFault("workflow is already terminal")
	}
	b := &builder{state: state}
	intent := &model.CancelIntent{RequestedSeq: state.NextEventSeq, Reason: in.Reason}
	b.mutate(wfMut(state, state.Workflow.Stage, state.Workflow.Runtime, intent))
	b.event(model.EventWorkflowCancelRequested, "", model.AttemptKey{}, "", "cancel requested")
	if run := activeRun(state); run != nil && !run.Status.IsTerminal() {
		b.mutate(model.RunMutation{ID: run.ID, Status: model.RunStopping, DispatchGate: false})
		b.event(model.EventRunStopped, "", model.AttemptKey{}, "", "run stopping")
	}
	stopRunningProcesses(b, state)
	if !hasRunningAttempt(state) && !hasRunningProcess(state) {
		finishCancel(b, state, intent)
	}
	return b.decision(), nil
}

// decideCleanup handles the user Cleanup interaction: an immutable Dry
// Run Manifest first, then an execution that revalidates every item's
// facts against the exact confirmed Manifest (design 17.4).
func decideCleanup(state model.State, in model.CleanupCommandInput) (model.Decision, error) {
	switch in.Kind {
	case model.CleanupDryRun:
		return cleanupDryRun(state, in)
	case model.CleanupExecute:
		return cleanupExecute(state, in)
	default:
		return model.Decision{}, model.InvalidInputFault("unsupported cleanup command")
	}
}

// cleanupDryRun produces the immutable Manifest over the candidate target
// set. Cleanup targets require terminal Workflow state and no managed
// processes; the Manifest's identity and hash are fixed here and the
// execution confirmation must bind them exactly.
func cleanupDryRun(state model.State, in model.CleanupCommandInput) (model.Decision, error) {
	if !state.Workflow.Runtime.IsTerminal() {
		return model.Decision{}, model.NewFault(model.CodeCleanupWorkflowNotTerminal,
			"cleanup requires a terminal workflow")
	}
	if hasRunningProcess(state) {
		return model.Decision{}, model.NewFault(model.CodeCleanupActiveProcess,
			"cleanup requires no managed processes")
	}
	b := &builder{state: state}
	items := append([]model.CleanupItem(nil), in.Items...)
	for i := range items {
		items[i].Status = model.CleanupItemPending
	}
	att := model.CleanupAttempt{
		ID:     model.CleanupAttemptID(fmt.Sprintf("cleanup-%d", len(state.CleanupAttempts)+1)),
		Status: model.CleanupStatusAwaitingConfirmation,
		Manifest: model.ArtifactRef{
			Workflow: state.Workflow.ID,
			Type:     model.ArtifactCleanupManifest,
			Revision: 1,
			Hash:     model.CleanupManifestHash(items),
		},
		Items:     items,
		StartedAt: state.Now,
	}
	b.mutate(model.CleanupAppendMutation{CleanupAttempt: att})
	b.event(model.EventCleanupAttemptCreated, "", model.AttemptKey{}, "", "cleanup manifest created")
	return b.decision(), nil
}

// cleanupExecute revalidates the freshly observed facts against the exact
// confirmed Manifest before requesting the first pending item. Any drift
// is CLEANUP_FACT_MISMATCH; a dirty target is CLEANUP_TARGET_DIRTY
// (PRD Cleanup Failure Codes, design 17.4).
func cleanupExecute(state model.State, in model.CleanupCommandInput) (model.Decision, error) {
	att := lastCleanupAttempt(state)
	if att == nil || att.Status != model.CleanupStatusAwaitingConfirmation {
		return model.Decision{}, model.InvalidInputFault("no cleanup attempt awaiting confirmation")
	}
	if in.Manifest != att.Manifest {
		return model.Decision{}, model.NewFault(model.CodeCleanupFactsChanged,
			"cleanup manifest identity or hash changed since the dry run")
	}
	if !state.Workflow.Runtime.IsTerminal() {
		return model.Decision{}, model.NewFault(model.CodeCleanupWorkflowNotTerminal,
			"cleanup requires a terminal workflow")
	}
	if hasRunningProcess(state) {
		return model.Decision{}, model.NewFault(model.CodeCleanupActiveProcess,
			"cleanup requires no managed processes")
	}
	if model.CleanupManifestHash(in.Items) != att.Manifest.Hash {
		return model.Decision{}, model.NewFault(model.CodeCleanupFactsChanged,
			"observed cleanup facts no longer match the confirmed manifest")
	}
	manifestItems := map[int]model.CleanupItem{}
	for _, it := range att.Items {
		manifestItems[it.Index] = it
	}
	for _, it := range in.Items {
		mi, ok := manifestItems[it.Index]
		if !ok || it.Kind != mi.Kind || it.CanonicalPath != mi.CanonicalPath ||
			it.Branch != mi.Branch || it.ExpectedHead != mi.ExpectedHead || it.Fingerprint != mi.Fingerprint {
			return model.Decision{}, model.NewFault(model.CodeCleanupFactsChanged,
				"cleanup item facts drifted from the confirmed manifest")
		}
		if it.Dirty {
			return model.Decision{}, model.NewFault(model.CodeCleanupTargetDirty,
				"cleanup target is dirty")
		}
	}
	next := firstPendingItem(att)
	if next == nil {
		return model.Decision{}, model.InvalidInputFault("cleanup manifest has no pending items")
	}
	b := &builder{state: state}
	b.mutate(model.CleanupItemMutation{Attempt: att.ID, Index: next.Index, Status: model.CleanupItemRequested})
	b.event(model.EventCleanupItemRequested, "", model.AttemptKey{}, "", "cleanup item requested")
	requestCleanupEffect(b, att, next)
	return b.decision(), nil
}

// decideCleanupResult settles one item's independent result. A removed
// item completes and the next pending item is requested; when no item
// remains pending the Cleanup Attempt Succeeds. A failed item Blocks the
// Attempt with partial results explicit, and never alters the Workflow's
// terminal state (PRD Cleanup Failure Codes).
func decideCleanupResult(state model.State, in model.EffectResultInput) (model.Decision, error) {
	att := findCleanupAttempt(state, in.CleanupAttempt)
	if att == nil {
		return model.Decision{}, model.InvalidInputFault("unknown cleanup attempt")
	}
	if in.ItemIndex < 0 || in.ItemIndex >= len(att.Items) {
		return model.Decision{}, model.InvalidInputFault("cleanup item index out of range")
	}
	item := &att.Items[in.ItemIndex]
	if item.Status != model.CleanupItemRequested {
		return model.Decision{}, model.InvalidInputFault("cleanup item is not requested")
	}
	b := &builder{state: state}
	switch in.Kind {
	case model.CleanupItemRemovedResult:
		b.mutate(model.CleanupItemMutation{Attempt: att.ID, Index: in.ItemIndex, Status: model.CleanupItemCompleted})
		b.event(model.EventCleanupItemCompleted, "", model.AttemptKey{}, "", "cleanup item completed")
		if next := firstPendingItem(att); next != nil {
			b.mutate(model.CleanupItemMutation{Attempt: att.ID, Index: next.Index, Status: model.CleanupItemRequested})
			b.event(model.EventCleanupItemRequested, "", model.AttemptKey{}, "", "cleanup item requested")
			requestCleanupEffect(b, att, next)
		} else {
			b.mutate(model.CleanupMutation{ID: att.ID, Status: model.CleanupStatusSucceeded, EndedAt: state.Now})
		}
	case model.CleanupItemFailedResult:
		b.mutate(model.CleanupItemMutation{Attempt: att.ID, Index: in.ItemIndex, Status: model.CleanupItemFailed, FailureCode: in.FailureCode})
		b.event(model.EventCleanupItemFailed, "", model.AttemptKey{}, "", "cleanup item failed")
		b.mutate(model.CleanupMutation{ID: att.ID, Status: model.CleanupStatusBlocked, EndedAt: state.Now})
	}
	return b.decision(), nil
}

func lastCleanupAttempt(state model.State) *model.CleanupAttempt {
	if len(state.CleanupAttempts) == 0 {
		return nil
	}
	return &state.CleanupAttempts[len(state.CleanupAttempts)-1]
}

func findCleanupAttempt(state model.State, id model.CleanupAttemptID) *model.CleanupAttempt {
	for i := range state.CleanupAttempts {
		if state.CleanupAttempts[i].ID == id {
			return &state.CleanupAttempts[i]
		}
	}
	return nil
}

func firstPendingItem(att *model.CleanupAttempt) *model.CleanupItem {
	for i := range att.Items {
		if att.Items[i].Status == model.CleanupItemPending {
			return &att.Items[i]
		}
	}
	return nil
}

func requestCleanupEffect(b *builder, att *model.CleanupAttempt, item *model.CleanupItem) {
	if item.Kind == model.CleanupWorktree {
		b.effect(model.CleanupWorktreeRemoveIntent{Cleanup: att.ID, Item: item.Index})
	} else {
		b.effect(model.CleanupScratchRemoveIntent{Cleanup: att.ID, Item: item.Index})
	}
}
