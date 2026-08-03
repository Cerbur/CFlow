// One supervised claude run (design 14.1/14.3): the run reads the
// supervisor's bounded framed pipeline, decodes each stdout frame through
// the stream-json dialect, and drops stderr (bounded in the supervisor,
// never surfaced). A process that ends without a validated terminal
// structured event is a *ProcessCrash whose exit code is a fact that can
// never complete the run (PRD 约束 43). A fail-closed stream (protocol
// violation, output overflow) stops the affected process before the
// error is returned (design 14.3).
package claude

import (
	"context"
	"errors"
	"fmt"
	"io"

	"cflow.local/cflow/internal/agent"
	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/process"
)

// run is one supervised claude process event stream. established and
// terminalSeen are written only by the draining goroutine; stopped and
// reaped are shared with Cancel and Inspect and are guarded by ad.mu.
type run struct {
	ad  *Adapter
	sup process.Supervisor
	h   process.Handle
	ch  process.Events

	parser       streamParser
	terminalSeen bool
	stopped      bool
	reaped       bool
}

// Next yields the next unified event: io.EOF at the stream end, a
// *ProtocolError on a dialect or budget failure, a *ProcessCrash when
// the process ended before its terminal event, or the context error.
func (r *run) Next(ctx context.Context) (agent.Event, error) {
	for {
		select {
		case ev, ok := <-r.ch:
			if !ok {
				return r.afterReap(ctx)
			}
			switch ev.Kind {
			case process.EventFrameOut:
				frame, skip, err := r.decodeFrame(ev.Frame)
				if err != nil {
					return agent.Event{}, err
				}
				if skip {
					continue // known unmapped claude frames pass over silently
				}
				if frame.Type == agent.EventCompleted || frame.Type == agent.EventFailed {
					r.terminalSeen = true
				}
				return frame, nil
			case process.EventOverflowOut:
				r.stopProcess()
				return agent.Event{}, &agent.ProtocolError{
					Code:    model.CodeProviderProtocolViolation,
					Message: "claude stdout exceeded the bounded stream budget",
				}
			default:
				// EventStarted, stderr frames, EOF facts, and overflow
				// facts of the dropped stderr stream never surface.
			}
		case <-ctx.Done():
			return agent.Event{}, ctx.Err()
		}
	}
}

// decodeFrame parses one stdout frame through the dialect and registers
// the run under the established provider session id so Cancel and
// Inspect can find it. A dialect failure stops the affected process and
// returns the fail-closed protocol error carrying the raw frame; an init
// frame with no session id carries the binding's missing-id code
// (PROVIDER_SESSION_ID_MISSING), every other violation is
// PROVIDER_PROTOCOL_VIOLATION.
func (r *run) decodeFrame(raw []byte) (agent.Event, bool, error) {
	ev, skip, err := r.parser.parse(raw)
	if err != nil {
		r.stopProcess()
		code := model.CodeProviderProtocolViolation
		if errors.Is(err, errSessionIDMissing) {
			code = model.CodeProviderSessionIDMissing
		}
		return agent.Event{}, false, &agent.ProtocolError{
			Code:    code,
			Frame:   raw,
			Message: "claude: " + err.Error(),
		}
	}
	if skip {
		return agent.Event{}, true, nil
	}
	if ev.Type == agent.EventSessionStarted && ev.SessionID != "" {
		r.ad.mu.Lock()
		r.ad.runs[ev.SessionID] = r
		r.ad.mu.Unlock()
	}
	return ev, false, nil
}

// afterReap reports the stream end once the process has been fully
// reaped. A run stopped by Cancel returns io.EOF so the Runtime settles
// it as CANCELLED; a run that saw its validated terminal event returns
// io.EOF (the exit code can never override a terminal event); anything
// else is a *ProcessCrash carrying the exit fact.
func (r *run) afterReap(ctx context.Context) (agent.Event, error) {
	exit, err := r.sup.Wait(ctx, r.h)
	if err != nil {
		return agent.Event{}, err
	}
	r.ad.mu.Lock()
	stopped := r.stopped
	terminal := r.terminalSeen
	r.reaped = true
	delete(r.ad.runs, r.parser.established)
	r.ad.mu.Unlock()
	if stopped || terminal {
		return agent.Event{}, io.EOF
	}
	return agent.Event{}, &agent.ProcessCrash{
		ExitCode: exit.Code,
		Message:  fmt.Sprintf("claude %s (code %d) without a terminal structured event", factText(exit.Fact), exit.Code),
	}
}

// stop marks the controlled stop: after the process is reaped, Next
// returns io.EOF instead of a crash, so the Runtime settles the run as
// CANCELLED and preserves the partial redacted events it already read.
func (r *run) stop() {
	r.ad.mu.Lock()
	r.stopped = true
	r.ad.mu.Unlock()
}

// stopProcess stops the affected process after a fail-closed stream
// (design 14.3: unknown events and malformed frames stop the affected
// process). The supervisor keeps supervising and reaps it; the stop is
// best-effort and never kills an already-reaped run.
func (r *run) stopProcess() {
	r.ad.mu.Lock()
	reaped := r.reaped
	r.ad.mu.Unlock()
	if reaped {
		return
	}
	_ = r.sup.Signal(context.Background(), r.h, process.Terminate)
}

// isReaped reports whether the process was fully reaped.
func (r *run) isReaped() bool {
	r.ad.mu.Lock()
	defer r.ad.mu.Unlock()
	return r.reaped
}

// factText renders the supervisor exit fact for crash messages.
func factText(f process.ExitFact) string {
	switch f {
	case process.FactProcessExit:
		return "exited"
	case process.FactSignaled:
		return "was signalled"
	case process.FactTimeout:
		return "timed out"
	case process.FactCancelled:
		return "was cancelled"
	}
	return "ended"
}
