package app

// The Recovery-before-mutation hook and the protocol helpers of the
// decision/effect loop (design 6, 17). Same-package split of the
// Application seam: no public seam added.

import (
	"context"
	"errors"

	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/platform"
	"cflow.local/cflow/internal/recovery"
	"cflow.local/cflow/internal/store"
)

// recoveryHook is the full Recovery-before-mutation hook (Task 13,
// design 17): the Recovery Engine evaluates the facts in the design's
// order and returns typed dispositions; the hook converts blocked_drift
// and fatal_invariant faults into typed Faults that block the mutation
// (user action required / quarantine), and lets already_completed and
// safe_to_retry dispositions proceed (the re-execution semantics of
// safe intents arrive with Task 17).
type recoveryHook struct {
	engine *recovery.RecoveryEngine
}

func (h *recoveryHook) Reconcile(ctx context.Context) error {
	return h.reconcile(ctx, "")
}

// ReconcileWorkflow reconciles one workflow's aggregate, Git, Artifact,
// and evidence facts before a mutation of that workflow.
func (h *recoveryHook) ReconcileWorkflow(ctx context.Context, wf model.WorkflowID) error {
	return h.reconcile(ctx, wf)
}

func (h *recoveryHook) reconcile(ctx context.Context, wf model.WorkflowID) error {
	out, err := h.engine.Reconcile(ctx, recovery.Scope{Workflow: wf})
	if err != nil {
		return err
	}
	if len(out.Faults) > 0 {
		return &out.Faults[0]
	}
	return nil
}

// workflowAwareRecoverer is the optional Recoverer extension that
// reconciles the command's workflow scope; the plain seam stays for
// project-level hooks and the app tests.
type workflowAwareRecoverer interface {
	ReconcileWorkflow(ctx context.Context, wf model.WorkflowID) error
}

// reconcileSweep completes the unfinished recovery protocols a crash may
// have left behind — a persisted Cancel intent, a QUIESCING or STOPPING
// Run whose snapshot Attempts and processes settled, or a RUNNING
// Workflow carrying a FAILED Node with nothing in flight (design 17,
// PRD 已确认：Cancel 逻辑终止 step 4). It runs before every mutation with
// the mutation locks held, after the Recovery Engine accepted the facts,
// and never reopens dispatch: Recovery of a Stop, Cancel, Quiesce, or
// Safety Stop can only finish the persisted protocol.
func (a *Application) reconcileSweep(ctx context.Context, st *store.Store, wf model.WorkflowID) error {
	view, err := st.View(ctx, store.StoreQuery{})
	if err != nil {
		return err
	}
	if !needsReconcile(view.State) {
		return nil
	}
	_, err = a.runDecisionLoop(ctx, st, wf, ReconcileCommand{Workflow: wf}, model.ReconcileInput{}, false)
	return err
}

// needsReconcile reports whether the aggregate carries an unfinished
// recovery protocol the sweep can complete.
func needsReconcile(st model.State) bool {
	if st.Workflow.CancelIntent != nil && !st.Workflow.Runtime.IsTerminal() {
		return true
	}
	if run := activeRunOfState(st); run != nil {
		if run.Status == model.RunQuiescing || run.Status == model.RunStopping {
			return true
		}
	}
	if st.Workflow.Runtime == model.RuntimeRunning {
		for _, n := range st.Nodes {
			if n.Status == model.NodeFailed {
				return true
			}
		}
	}
	return false
}

// activeRunOfState returns the first non-terminal Run of one aggregate.
func activeRunOfState(st model.State) *model.Run {
	for i := range st.Runs {
		if !st.Runs[i].Status.IsTerminal() {
			return &st.Runs[i]
		}
	}
	return nil
}

// safetyPathAllowed reports whether a failed Recovery hook may route
// pause/cancel through the restricted safety-command path (design 6.1):
// posture, compatibility, and quarantine faults still allow stopping and
// reconciling already managed processes. An uninterpretable schema never
// does: CFlow cannot persist a trustworthy stop result.
func safetyPathAllowed(cmd Command, code model.Code) bool {
	switch cmd.(type) {
	case PauseWorkflowCommand, CancelWorkflowCommand:
	default:
		return false
	}
	switch code {
	case model.CodeInsecureCFLOWHomePermissions,
		model.CodeProviderProtocolUnsupported,
		model.CodeProviderBindingChanged,
		model.CodeSensitiveDataRedactionFailed,
		model.CodeCommitDuringPolicyDriftWindow:
		return true
	}
	return false
}

// effectBudget bounds one command's effect loop (design 6.2): at most one
// Intent per aggregate entity that can request an external Effect (running
// processes, running Attempts, in-flight Apply Attempts, pending Cleanup
// items), plus the planning chain (a planning command requests a Provider
// run and then the Artifact write it produced), plus the persisted
// pending Intent ledger and the final no-effect Decision. A Kernel that
// requests more is broken.
func effectBudget(st model.State, pending int, cmd model.Input) int {
	n := 1
	for _, p := range st.Processes {
		if p.Status == model.ProcessStatusRunning {
			n++
		}
	}
	for _, a := range st.Attempts {
		if a.Status == model.AttemptRunning {
			n++
		}
	}
	for _, at := range st.ApplyAttempts {
		if at.Status == model.ApplyStaging || at.Status == model.ApplyAwaitingConfirmation || at.Status == model.ApplyRunning {
			n++
		}
	}
	for _, c := range st.CleanupAttempts {
		for _, it := range c.Items {
			if !it.Status.IsTerminal() {
				n++
			}
		}
	}
	// A planning Session requests its Provider run and then the Artifact
	// write; the per-command chain is two Effects. Non-terminal Sessions
	// already in the aggregate add the same chain for the run they are
	// still owed from a previous command. Spec generation and workflow
	// compilation chain three Effects (run, then the compile or the Spec
	// write, then the Workflow write); the Execution Approval chains the
	// Integration Worktree creation after the approval decision.
	switch cmd.(type) {
	case model.DiscussRequirementInput, model.GeneratePlanInput, model.CheckPlanInput,
		model.SpecGenerationInput:
		n += 2
	case model.WorkflowCompilationInput:
		n += 3
	case model.ExecutionApprovalInput:
		n += 2
	case model.DispatchInput:
		// One allocation chains the Task Worktree creation and the coding
		// Session run (two Effects) before the settle Decision.
		n += 2
	}
	for _, s := range st.Sessions {
		if !s.Status.IsTerminal() {
			n += 2
		}
	}
	return n + pending + 1
}

// lockFault normalizes lock-acquisition failures into the Fault contract.
// Local contention (another Runtime holding the lock) is the same stable
// local-contention class the Store uses for SQLITE_BUSY.
func lockFault(err error) error {
	if errors.Is(err, platform.ErrLockBusy) {
		return model.NewFault(model.CodeDatabaseMigrationFailed,
			"project is busy: another runtime holds a required lock")
	}
	return err
}

// orCtx prefers a cancelled command context over any other error: a user
// interruption is the authoritative outcome (design 20, exit class 130).
func orCtx(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

func releaseHolds(holds []*platform.Hold) {
	for _, h := range holds {
		h.Release()
	}
}
