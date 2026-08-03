package app

// The Recovery-before-mutation hook and the protocol helpers of the
// decision/effect loop (design 6, 17). Same-package split of the
// Application seam: no public seam added.

import (
	"context"
	"errors"
	"os"

	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/platform"
	"cflow.local/cflow/internal/security"
	"cflow.local/cflow/internal/store"
)

// defaultRecoverer is the Task 7 recovery-before-mutation hook (design
// 17.1 order 1-2): CFLOW_HOME security posture, then store/schema
// compatibility. The full Recovery Engine (Task 13) replaces it; it never
// pretends to reconcile facts it did not collect.
type defaultRecoverer struct {
	home   string
	dbPath string
}

func (r *defaultRecoverer) Reconcile(ctx context.Context) error {
	if err := r.ensureHome(); err != nil {
		return err
	}
	if _, err := security.CheckHome(security.HomeRequest{Path: r.home}); err != nil {
		return err
	}
	return r.checkSchema(ctx)
}

// ensureHome creates CFLOW_HOME 0700 when missing (design 19.1: CFlow-
// created directories use 0700). Mutations may create the home; reads
// never reach this path.
func (r *defaultRecoverer) ensureHome() error {
	if _, err := os.Stat(r.home); err == nil {
		return nil
	}
	return os.MkdirAll(r.home, 0o700)
}

// checkSchema fails closed when the database schema cannot be interpreted
// safely by this binary (PRD 决策 9): the read-only open classifies the
// version without migrating.
func (r *defaultRecoverer) checkSchema(ctx context.Context) error {
	if _, err := os.Stat(r.dbPath); err != nil {
		return nil // no database yet: nothing to interpret
	}
	st, err := store.Open(ctx, store.OpenOptions{Path: r.dbPath, ReadOnly: true})
	if err != nil {
		return err
	}
	return st.Close()
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
