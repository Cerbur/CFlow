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
	"os"
	"path/filepath"
	"strings"
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
// frames can inherit it. cwd is the working directory the scripted
// coding writes materialize into; materialized guards the once-only
// materialization at the terminal Session frame.
type run struct {
	ad           *Adapter
	script       Script
	key          agent.ProviderSessionID
	cwd          string
	idx          int
	established  agent.ProviderSessionID
	stopCh       chan struct{}
	stopOne      sync.Once
	ended        bool
	materialized bool
}

func newRun(ad *Adapter, sc Script, key agent.ProviderSessionID, cwd string) *run {
	return &run{ad: ad, script: sc, key: key, cwd: cwd, stopCh: make(chan struct{})}
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
		// A suffix run (the adapter uniquified the declared id so several
		// parallel Sessions can share one per-purpose fixture) carries its
		// own provider session id on every explicit frame.
		if wf.SessionID != "" && r.key != agent.ProviderSessionID(r.script.SessionID) {
			wf.SessionID = string(r.key)
		}
		// The scripted coding Session materializes its declared writes
		// into the run's working directory when it finishes (Fake coding
		// execution, Task 12): coding output never sets lifecycle state,
		// it writes files in the Task Worktree only.
		if wf.Type == "session_finished" {
			if err := r.materializeWrites(); err != nil {
				return agent.Event{}, &agent.ProtocolError{
					Code:    model.CodeProviderProtocolViolation,
					Frame:   line,
					Message: "fake: " + err.Error(),
				}
			}
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

// materializeWrites writes the script's declared files into the run's
// working directory, exactly once, at the terminal Session frame. A path
// that escapes the working directory fails closed: the fixture never
// writes outside the directory it was given.
func (r *run) materializeWrites() error {
	r.ad.mu.Lock()
	if r.materialized {
		r.ad.mu.Unlock()
		return nil
	}
	r.materialized = true
	writes := append([]FileWrite(nil), r.script.Writes...)
	r.ad.mu.Unlock()
	for _, w := range writes {
		if err := materializeWrite(r.cwd, w); err != nil {
			return err
		}
	}
	return nil
}

// materializeWrite writes one declared file under cwd. Absolute paths and
// ".." escapes are rejected; directories are created 0700 and the file
// 0600 through the security guard so the Fake never creates a world-
// readable artifact.
func materializeWrite(cwd string, w FileWrite) error {
	if cwd == "" {
		return fmt.Errorf("scripted write requires a working directory")
	}
	if w.Path == "" {
		return fmt.Errorf("scripted write carries an empty path")
	}
	clean := filepath.Clean(filepath.FromSlash(w.Path))
	if filepath.IsAbs(clean) || clean == ".." ||
		strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("scripted write path %q escapes the working directory", w.Path)
	}
	dir := filepath.Dir(filepath.Join(cwd, clean))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("scripted write cannot create %s: %w", dir, err)
	}
	return os.WriteFile(filepath.Join(cwd, clean), []byte(w.Content), 0o600)
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
