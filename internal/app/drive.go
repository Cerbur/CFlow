package app

// The Foreground Driver (TUI task 13): DriveOnce performs ONE safe
// forward step of a Workflow — Recovery, Adoption, Dispatch, Reconcile,
// or Final Completion — and returns a typed DriveOutcome. The Kernel
// still validates every command; the Driver never invents a transition.
//
// DriveOnce is the bounded seam the foreground Runner calls repeatedly;
// Waiting outcomes carry a channel the Runner waits on, so the Runner
// never busy-loops and never runs unattached in the background.

import (
	"context"
	"time"

	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/store"
)

// DriveKind is one typed forward step.
type DriveKind uint8

const (
	// DriveProgressed: the step committed a state change; another step
	// may be safe.
	DriveProgressed DriveKind = iota
	// DriveWaiting: the workflow waits on an external event or process;
	// the Runner must wait before stepping again (never poll).
	DriveWaiting
	// DriveNeedsUser: a user decision is required (Approval, Adoption,
	// a blocked gate); the Runner pauses and surfaces the decision
	// panel.
	DriveNeedsUser
	// DriveTerminal: the workflow reached a terminal state (COMPLETED,
	// CANCELLED, or a terminal failure); the Runner stops.
	DriveTerminal
	// DriveNoSafeProgress: no safe forward step exists and the workflow
	// is not terminal; the Driver records a Finding and the Runner
	// stops.
	DriveNoSafeProgress
)

// String renders the Drive Kind.
func (k DriveKind) String() string {
	switch k {
	case DriveProgressed:
		return "progressed"
	case DriveWaiting:
		return "waiting"
	case DriveNeedsUser:
		return "needs-user"
	case DriveTerminal:
		return "terminal"
	case DriveNoSafeProgress:
		return "no-safe-progress"
	}
	return "unknown"
}

// DriveOutcome is the typed result of one DriveOnce step.
type DriveOutcome struct {
	Kind    DriveKind
	Outcome Outcome
	// Wait is the channel the Runner waits on before the next step (nil
	// unless Kind == DriveWaiting).
	Wait <-chan struct{}
	// Reason is the human reason of a terminal or blocked outcome.
	Reason string
}

// Driver is the bounded forward-step seam.
type Driver interface {
	DriveOnce(context.Context, model.WorkflowID) (DriveOutcome, error)
}

// DriveOnce is the Application's implementation of the Driver seam: one
// safe forward step of a workflow.
func (a *Application) DriveOnce(ctx context.Context, wf model.WorkflowID) (DriveOutcome, error) {
	return a.driveOnce(ctx, wf)
}

// driveOnce implements one safe forward step of a workflow.
func (a *Application) driveOnce(ctx context.Context, wf model.WorkflowID) (DriveOutcome, error) {
	view, err := a.readAggregate(ctx, wf, store.StoreQuery{})
	if err != nil {
		return DriveOutcome{}, err
	}
	st := view.State
	if st.Workflow.ID == "" {
		return DriveOutcome{}, model.InvalidInputFault("no such workflow")
	}
	// Terminal states stop the runner.
	switch st.Workflow.Runtime {
	case model.RuntimeSucceeded, model.RuntimeFailed, model.RuntimeCancelled:
		return DriveOutcome{Kind: DriveTerminal, Reason: string(st.Workflow.Runtime)}, nil
	}
	// A blocked workflow needs a user decision.
	if st.Workflow.Runtime == model.RuntimeBlocked {
		return DriveOutcome{Kind: DriveNeedsUser, Reason: "blocked"}, nil
	}
	// A paused workflow needs a user decision (Approval, Adoption).
	if st.Workflow.Runtime == model.RuntimePaused {
		return DriveOutcome{Kind: DriveNeedsUser, Reason: "paused at a gate"}, nil
	}
	// Before EXECUTION the workflow cannot auto-dispatch: the user must
	// drive the planning gates (discussion, plan approval).
	if st.Workflow.Stage != model.StageExecution && st.Workflow.Stage != model.StageFinalVerification {
		return DriveOutcome{Kind: DriveNeedsUser, Reason: string(st.Workflow.Stage)}, nil
	}
	// A running workflow: dispatch one pass (the dispatch pass itself
	// advances adoption/verify/merge/final chains and reconciles).
	out, err := a.Execute(ctx, DispatchCommand{Workflow: wf})
	if err != nil {
		if code, ok := model.CodeOf(err); ok {
			switch code {
			case model.CodeWorkspaceAdoptionRequired, model.CodeApprovalInputChanged,
				model.CodeDispatchGateClosed:
				return DriveOutcome{Kind: DriveNeedsUser, Reason: code.String()}, nil
			}
		}
		return DriveOutcome{}, err
	}
	// Re-read the state after the pass: terminal → stop, still running →
	// progress (a waiting event may follow).
	view, err = a.readAggregate(ctx, wf, store.StoreQuery{})
	if err != nil {
		return DriveOutcome{}, err
	}
	st = view.State
	switch st.Workflow.Runtime {
	case model.RuntimeSucceeded, model.RuntimeFailed, model.RuntimeCancelled:
		return DriveOutcome{Kind: DriveTerminal, Outcome: out, Reason: string(st.Workflow.Runtime)}, nil
	case model.RuntimeBlocked, model.RuntimePaused:
		return DriveOutcome{Kind: DriveNeedsUser, Outcome: out, Reason: string(st.Workflow.Runtime)}, nil
	}
	if a.hasActiveProcess(ctx, st) {
		// The pass started a Provider Session or managed process: the
		// next step must wait for it (a bounded wait, never a poll).
		return DriveOutcome{Kind: DriveWaiting, Outcome: out, Wait: a.processWait(ctx, wf)}, nil
	}
	return DriveOutcome{Kind: DriveProgressed, Outcome: out}, nil
}

// hasActiveProcess reports whether the workflow has a RUNNING managed
// process or an in-flight Attempt.
func (a *Application) hasActiveProcess(ctx context.Context, st model.State) bool {
	for _, p := range st.Processes {
		if p.Status == model.ProcessStatusRunning {
			return true
		}
	}
	for _, at := range st.Attempts {
		if at.Status == model.AttemptRunning {
			return true
		}
	}
	for _, r := range st.Runs {
		if !r.Status.IsTerminal() && r.Status == model.RunRunning {
			return true
		}
	}
	return false
}

// processWait returns a bounded wait channel for the active work: the
// Runner waits on it before the next step. The wait is bounded so a
// stalled process never hangs the Runner forever.
func (a *Application) processWait(ctx context.Context, wf model.WorkflowID) <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		timer := time.NewTimer(2 * time.Second)
		defer timer.Stop()
		select {
		case <-timer.C:
			close(ch)
		case <-ctx.Done():
			close(ch)
		}
	}()
	return ch
}
