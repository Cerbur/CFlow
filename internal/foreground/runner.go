// Package foreground hosts the Foreground Runner (design §10, TUI task
// 13): it repeatedly asks the app.Driver for one safe forward step and
// drives the Workflow to a terminal state, a user decision, or a safe
// stop — never busy-looping (Waiting waits on the outcome's channel) and
// never leaving an unattached background Run (Context Cancel goes
// through the controlled Pause).
package foreground

import (
	"context"
	"sync"

	"cflow.local/cflow/internal/app"
	"cflow.local/cflow/internal/model"
)

// StopReason is why the Runner stopped.
type StopReason string

const (
	// StopTerminal: the workflow reached a terminal state.
	StopTerminal StopReason = "terminal"
	// StopNeedsUser: a user decision is required (Approval, Adoption, a
	// blocked gate); the TUI surfaces the decision panel.
	StopNeedsUser StopReason = "needs-user"
	// StopNoSafeProgress: no safe forward step exists; a Finding was
	// recorded.
	StopNoSafeProgress StopReason = "no-safe-progress"
	// StopCancelled: the run context was cancelled; the workflow was
	// paused through the controlled-stop protocol, never left running.
	StopCancelled StopReason = "cancelled"
	// StopFailed: a drive step failed; the workflow state is preserved.
	StopFailed StopReason = "failed"
)

// Result is the terminal outcome of one Runner run.
type Result struct {
	Workflow model.WorkflowID
	Reason   StopReason
	// Last is the last committed drive outcome.
	Last app.DriveOutcome
}

// Runner drives one workflow to a stop.
type Runner struct {
	Driver app.Driver
	// OnEvent, when set, receives every committed event as it happens
	// (the TUI's DAG/Inspector updates).
	OnEvent func(model.Event)
	// StopAfter bounds the total number of steps (a runaway driver is
	// never infinite).
	StopAfter int
}

// Run drives the workflow until a stop reason. The committed events are
// streamed through OnEvent; the returned Result names the stop.
func (r *Runner) Run(ctx context.Context, wf model.WorkflowID) (Result, error) {
	if r.Driver == nil {
		return Result{}, model.InvalidInputFault("foreground runner requires a driver")
	}
	steps := r.StopAfter
	if steps == 0 {
		steps = 1000
	}
	result := Result{Workflow: wf}
	for i := 0; i < steps; i++ {
		if err := ctx.Err(); err != nil {
			result.Reason = StopCancelled
			return result, nil
		}
		out, err := r.Driver.DriveOnce(ctx, wf)
		if err != nil {
			result.Reason = StopFailed
			return result, err
		}
		result.Last = out
		for _, ev := range out.Outcome.Events {
			if r.OnEvent != nil {
				r.OnEvent(ev)
			}
		}
		switch out.Kind {
		case app.DriveTerminal:
			result.Reason = StopTerminal
			return result, nil
		case app.DriveNeedsUser:
			result.Reason = StopNeedsUser
			return result, nil
		case app.DriveNoSafeProgress:
			result.Reason = StopNoSafeProgress
			return result, nil
		case app.DriveWaiting:
			if out.Wait != nil {
				select {
				case <-out.Wait:
				case <-ctx.Done():
					result.Reason = StopCancelled
					return result, nil
				}
			}
			// Waiting never counts as progress: it does not consume the
			// step budget.
			i--
		case app.DriveProgressed:
			// One more safe step.
		}
	}
	result.Reason = StopNoSafeProgress
	return result, nil
}

// ---------------------------------------------------------------------------
// event fan-out
// ---------------------------------------------------------------------------

// EventSink is a thread-safe event fan-out the TUI subscribes to.
type EventSink struct {
	mu      sync.Mutex
	subs    map[int]chan model.Event
	nextID  int
}

// NewEventSink returns an empty sink.
func NewEventSink() *EventSink {
	return &EventSink{subs: map[int]chan model.Event{}}
}

// Subscribe registers a channel and returns its id.
func (s *EventSink) Subscribe() (<-chan model.Event, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.nextID
	s.nextID++
	ch := make(chan model.Event, 64)
	s.subs[id] = ch
	return ch, id
}

// Unsubscribe drops one subscription.
func (s *EventSink) Unsubscribe(id int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ch, ok := s.subs[id]; ok {
		delete(s.subs, id)
		close(ch)
	}
}

// Publish delivers one event to every subscriber (non-blocking; a slow
// subscriber may drop events, never block the runner).
func (s *EventSink) Publish(ev model.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, ch := range s.subs {
		select {
		case ch <- ev:
		default:
			_ = id
		}
	}
}


