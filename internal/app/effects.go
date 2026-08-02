package app

// The typed Effect dispatcher (design 6.3): the closed union of Effect
// Intents is executed by the Runtime after its Intent and expected facts
// committed (design 6.2 rule 2). Results are immutable evidence inputs to
// another Decision; an executor can never mark a Node, Attempt, Run, or
// Workflow successful (design 6.2 rule 5) — it only reports the typed
// facts, and the Kernel decides.
//
// Effects whose full semantics arrive with later tasks (Provider*: Task 9;
// *Worktree*, GitCommitInspect | GitAuditRefCreate: Task 8; Integration*:
// Task 13; VerificationRun: Task 11; Apply*: Task 19; Cleanup*: Task 20;
// ArtifactWrite: Task 10) have typed executor stubs that fail closed
// without pretending to run the external operation. No Task 7 command
// path can produce them, so a stub firing is an invariant failure.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/process"
)

// stopWaitBudget bounds one controlled stop's graceful Wait. The full
// two-phase Terminate/ForceKill policy with the 10s plus 2s budget
// arrives with Task 17 (design 13.3).
const stopWaitBudget = 12 * time.Second

// executeEffect runs one committed Effect Intent and returns the typed
// Result evidence. restricted commands may only stop and reconcile
// already managed processes (design 6.1).
func (a *Application) executeEffect(ctx context.Context, intent model.EffectIntent, restricted bool) (model.EffectResultInput, error) {
	if restricted && !restrictedAllowed(intent) {
		return model.EffectResultInput{}, model.NewFault(model.CodeStateInvariantViolation,
			"restricted safety path cannot execute effect "+effectName(intent))
	}
	switch e := intent.(type) {
	case model.ManagedProcessStopIntent:
		return a.stopManagedProcess(ctx, e)
	case model.ArtifactWriteIntent:
		// STUB (Task 10): the immutable Artifact Store writes arrive with
		// the lifecycle tasks; no Task 7 command path produces this effect.
		return model.EffectResultInput{}, stubEffect(e)
	case model.ProviderStartIntent, model.ProviderResumeIntent, model.ProviderCancelIntent:
		// STUB (Task 9): the Agent Runtime wires Provider lifecycle.
		return model.EffectResultInput{}, stubEffect(e)
	case model.PlanningWorktreeCreateIntent, model.IntegrationWorktreeCreateIntent, model.TaskWorktreeCreateIntent:
		// STUB (Task 8): GitFlow wires the Worktree primitives.
		return model.EffectResultInput{}, stubEffect(e)
	case model.GitCommitInspectIntent, model.GitAuditRefCreateIntent:
		// STUB (Task 8): GitFlow wires canonical Git facts.
		return model.EffectResultInput{}, stubEffect(e)
	case model.IntegrationMergeIntent, model.IntegrationRollbackIntent:
		// STUB (Task 13): the serial Integration merge protocol.
		return model.EffectResultInput{}, stubEffect(e)
	case model.VerificationRunIntent:
		// STUB (Task 11): the Verification Engine turns the validated
		// Catalog identity into executable plus argv after revalidation.
		return model.EffectResultInput{}, stubEffect(e)
	case model.ApplyStagingCreateIntent, model.ApplyFastForwardIntent:
		// STUB (Task 19): the protected Apply compare-and-swap protocol.
		return model.EffectResultInput{}, stubEffect(e)
	case model.CleanupWorktreeRemoveIntent, model.CleanupScratchRemoveIntent:
		// STUB (Task 20): Cleanup revalidates each item against the
		// confirmed Manifest before removing anything.
		return model.EffectResultInput{}, stubEffect(e)
	default:
		return model.EffectResultInput{}, model.InvariantFault(fmt.Errorf("unknown effect intent %T", intent))
	}
}

// stopManagedProcess runs the controlled stop of one managed process
// (design 13.3 primitives): Terminate the exact process group, then Wait
// within the bounded budget. The Result reports only the typed fact
// (process-stopped); the Kernel settles the aggregate.
func (a *Application) stopManagedProcess(ctx context.Context, intent model.ManagedProcessStopIntent) (model.EffectResultInput, error) {
	handle, ok := a.procs[intent.Process]
	if !ok {
		return model.EffectResultInput{}, model.NewFault(model.CodeStateInvariantViolation,
			"managed process "+string(intent.Process)+" is not supervised by this runtime")
	}
	if err := a.supervisor.Signal(ctx, handle, process.Terminate); err != nil {
		return model.EffectResultInput{}, err
	}
	waitCtx, cancel := context.WithTimeout(ctx, stopWaitBudget)
	defer cancel()
	if _, err := a.supervisor.Wait(waitCtx, handle); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return model.EffectResultInput{}, model.NewFault(model.CodeStateInvariantViolation,
				"managed process "+string(intent.Process)+" did not stop within the bounded budget")
		}
		return model.EffectResultInput{}, err
	}
	delete(a.procs, intent.Process)
	return model.EffectResultInput{Kind: model.ProcessStopped, Process: intent.Process}, nil
}

// restrictedAllowed reports whether one Effect may run on the restricted
// safety path: only stopping already managed processes (design 6.1). The
// path may never start a Provider, allocate a Retry, generate an
// Artifact, Merge, or Apply.
func restrictedAllowed(intent model.EffectIntent) bool {
	switch intent.(type) {
	case model.ManagedProcessStopIntent:
		return true
	}
	return false
}

// validateEffectResult checks the immutable Result evidence against the
// committed Intent before the Result Decision is applied (design 6.2 rule
// 3): the Result must reference exactly the Intent's external target.
func validateEffectResult(intent model.EffectIntent, r model.EffectResultInput) error {
	switch e := intent.(type) {
	case model.ManagedProcessStopIntent:
		if r.Kind != model.ProcessStopped || r.Process != e.Process {
			return model.InvariantFault(fmt.Errorf("process stop result does not match intent for process %s", e.Process))
		}
	}
	return nil
}

// intentIdentity is the stable identity of one Effect Intent: two Intents
// are identical when their external target is identical, so a Decision
// requesting the same uncompleted Intent twice is a kernel invariant
// failure (design 6.2).
func intentIdentity(i model.EffectIntent) string {
	return fmt.Sprintf("%T/%v", i, i)
}

func effectName(i model.EffectIntent) string {
	return fmt.Sprintf("%T", i)
}

// stubEffect is the fail-closed outcome of a typed Effect whose full
// semantics arrive with a later task (design 6.3). The stub never
// pretends to run the external operation.
func stubEffect(intent model.EffectIntent) error {
	return model.InvariantFault(fmt.Errorf("effect %s is not executable by this build", effectName(intent)))
}
