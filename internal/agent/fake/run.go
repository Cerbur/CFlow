// The scripted run: one deterministic event stream that can stop at
// every event boundary (design 14.1: the Fake can deterministically stop
// at every event boundary). The run holds a copy of its script so later
// LoadScript calls can never move it.
package fake

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"

	"cflow.local/cflow/internal/agent"
	"cflow.local/cflow/internal/model"
)

// ---------------------------------------------------------------------------
// Scripted run (deterministic stop at every event boundary)
// ---------------------------------------------------------------------------

// run is one scripted event stream. It holds a copy of its script so
// later LoadScript calls can never move it. established tracks the
// session id claimed by the first session_started frame so compact
// frames can inherit it.
type run struct {
	ad          *Adapter
	script      Script
	key         agent.ProviderSessionID
	idx         int
	established agent.ProviderSessionID
	stopCh      chan struct{}
	stopOne     sync.Once
	ended       bool
}

func newRun(ad *Adapter, sc Script, key agent.ProviderSessionID) *run {
	return &run{ad: ad, script: sc, key: key, stopCh: make(chan struct{})}
}

// Next yields the next unified event, io.EOF at the stream end, a
// *ProtocolError on a malformed or unknown frame, or a *ProcessCrash at
// the declared crash point. When the script declares a stop point
// (stop_after), Next blocks at that boundary until Cancel or the context
// ends: the Fake can deterministically stop at every event boundary.
func (r *run) Next(ctx context.Context) (agent.Event, error) {
	for {
		r.ad.mu.Lock()
		if r.ended {
			r.ad.mu.Unlock()
			return agent.Event{}, io.EOF
		}
		if r.script.StopAfter > 0 && r.idx >= r.script.StopAfter {
			r.ad.mu.Unlock()
			select {
			case <-r.stopCh:
				r.end()
				return agent.Event{}, io.EOF
			case <-ctx.Done():
				return agent.Event{}, ctx.Err()
			}
		}
		if r.script.CrashAfter > 0 && r.idx >= r.script.CrashAfter {
			r.ad.mu.Unlock()
			r.end()
			return agent.Event{}, &agent.ProcessCrash{
				ExitCode: r.script.ExitCode,
				Message:  fmt.Sprintf("fake: process crashed at its declared crash point after %d frames", r.script.CrashAfter),
			}
		}
		if r.idx >= len(r.script.rawLines) {
			r.ad.mu.Unlock()
			r.end()
			return agent.Event{}, io.EOF
		}
		line := r.script.rawLines[r.idx]
		r.idx++
		r.ad.mu.Unlock()
		if len(bytes.TrimSpace(line)) == 0 {
			continue // a decoder skips empty lines silently
		}
		wf, compact, err := parseFrameLine(line)
		if err != nil {
			return agent.Event{}, &agent.ProtocolError{
				Code:    model.CodeProviderProtocolViolation,
				Frame:   line,
				Message: "fake: " + err.Error(),
			}
		}
		// The compact shorthand omits the session id after the start
		// frame; the established id is inherited. The faithful JSON wire
		// form always carries it explicitly, so the Runtime's missing-id
		// rule is exercised through JSON frames.
		if compact && wf.SessionID == "" && r.established != "" {
			wf.SessionID = string(r.established)
		}
		ev, err := wireToEvent(wf, line)
		if err != nil {
			return agent.Event{}, err
		}
		if ev.Type == agent.EventSessionStarted && ev.SessionID != "" {
			r.established = ev.SessionID
		}
		return ev, nil
	}
}

// end unregisters the run exactly once.
func (r *run) end() {
	r.ad.mu.Lock()
	if !r.ended {
		r.ended = true
		delete(r.ad.runs, r.key)
	}
	r.ad.mu.Unlock()
}

// stop signals the controlled stop; the blocked Next returns io.EOF at
// the next boundary.
func (r *run) stop() {
	r.stopOne.Do(func() { close(r.stopCh) })
}
