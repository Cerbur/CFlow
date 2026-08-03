package app

// The Application-side controlled-stop machinery (Task 17, design 13.3):
// the staged two-phase budget, the stop context the second Ctrl+C
// cancels, and the identity facts the executor inspects for the orphan
// path. Same-package split of the Application seam: no public seam added.

import (
	"context"

	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/process"
)

// StopPolicy is the staged budget of the two-phase controlled stop
// (design 13.3, PRD 已确认：Ctrl+C 两阶段有限停止): the Adapter-cancel
// grace, the post-Terminate wait, and the post-ForceKill wait. Production
// uses the PRD 10s + 2s + 2s budget; tests inject tiny values.
type StopPolicy = process.StopPolicy

// stopPolicy is the internal alias the Application field carries.
type stopPolicy = process.StopPolicy

// defaultStopPolicy applies the PRD budget when Options carries none.
func defaultStopPolicy(p *StopPolicy) stopPolicy {
	if p != nil {
		return *p
	}
	return process.DefaultStopPolicy()
}

// stopContext returns the Application's controlled-stop context. It is
// independent of the command context (the first Ctrl+C cancels the
// command so the sessions abort; the stop itself keeps its grace) and is
// cancelled by EscalateStop — the second Ctrl+C — so the two-phase stop
// skips the remaining grace and escalates to ForceKill (PRD step 3).
func (a *Application) stopContext(ctx context.Context) (context.Context, context.CancelFunc) {
	a.stopMu.Lock()
	defer a.stopMu.Unlock()
	if a.stopCtx == nil {
		a.stopCtx, a.stopCancel = context.WithCancel(ctx)
	}
	return a.stopCtx, a.stopCancel
}

// EscalateStop jumps a running controlled stop to the force-kill phase
// (the second Ctrl+C; PRD step 3). It is safe to call concurrently and
// is a no-op when no stop is in flight.
func (a *Application) EscalateStop() {
	a.stopMu.Lock()
	defer a.stopMu.Unlock()
	if a.stopCancel != nil {
		a.stopCancel()
	}
}

// bindProcess records the supervised handle facts of one managed Process
// identity: the handle and the exact PID/start-token identity the
// EventStarted event carried (design 13.2: PID alone is never trusted).
func (a *Application) bindProcess(id model.ProcessID, session model.SessionID, handle process.Handle, identity process.ProcessIdentity) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.procs[id] = handle
	a.processIdentities[id] = identity
	a.processSessions[id] = session
}

// unbindProcess drops the managed-process facts once the process settled.
func (a *Application) unbindProcess(id model.ProcessID) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.procs, id)
	delete(a.processIdentities, id)
	delete(a.processSessions, id)
}
