// Package native hosts the Native Session Bridge (design §9, TUI task
// 12): the Runtime launches one Provider's native interactive resume of
// an existing CFlow Session in the Workflow's Workspace — the terminal
// is inherited directly through the supervised interactive process seam
// (Task 11) — and returns the exit facts. internal/native never imports
// internal/tui or Bubble Tea; the TUI drives the Bridge inside its
// blocking-exec callback.
package native

import (
	"context"
	"fmt"

	"cflow.local/cflow/internal/agent"
	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/process"
)

// Request is everything the Bridge needs to run one native interactive
// turn of an existing CFlow Session in the Workflow's Workspace.
type Request struct {
	Workflow model.WorkflowID
	// Session is the exact CFlow Session to resume.
	Session  model.SessionID
	Provider string
	// ProviderSession is the Provider's own session id the CFlow Session
	// bound at start (the interactive resume targets it).
	ProviderSession agent.ProviderSessionID
	Worktree        string
	Terminal        process.Terminal
	// Adapter is the Provider Adapter the Runtime resolved (a real
	// dialect or the Fake). It must implement agent.InteractiveAdapter.
	Adapter agent.Adapter
	// Supervisor is the supervised-process seam (Task 11).
	Supervisor process.Supervisor
}

// Result is the outcome of one native interactive turn.
type Result struct {
	Session    model.SessionID
	Provider   string
	// ProviderSession is the Provider's own session identity the turn
	// resumed (echoed for the Application's binding revalidation).
	ProviderSession agent.ProviderSessionID
	Exit            process.Exit
	Reconciled      bool
}

// Bridge is the native interactive session runner.
type Bridge struct{}

// Run launches the provider's native interactive resume of the Session in
// the Workspace and waits for the terminal turn to end. It returns the
// exact exit facts; the Kernel judges them, never a claim.
func (Bridge) Run(ctx context.Context, req Request) (Result, error) {
	if req.Session == "" || req.Provider == "" || req.Worktree == "" {
		return Result{}, model.InvalidInputFault("native session requires the session, provider, and workspace")
	}
	if req.ProviderSession == "" {
		return Result{}, model.InvalidInputFault("native session requires the provider session id")
	}
	if req.Adapter == nil || req.Supervisor == nil {
		return Result{}, model.InvalidInputFault("native session requires the adapter and supervisor seams")
	}
	ia, ok := req.Adapter.(agent.InteractiveAdapter)
	if !ok {
		return Result{}, model.NewFault(model.CodeProviderProtocolUnsupported,
			"provider "+req.Provider+" has no native interactive resume capability")
	}
	spec, err := ia.InteractiveResume(ctx, req.ProviderSession, req.Worktree)
	if err != nil {
		return Result{}, err
	}
	if !spec.Capability {
		return Result{}, model.NewFault(model.CodeProviderProtocolUnsupported,
			"provider "+req.Provider+" lacks the native interactive resume capability")
	}
	h, err := req.Supervisor.StartInteractive(ctx, process.InteractiveSpec{
		Executable: spec.Executable,
		Args:       spec.Args,
		Dir:        spec.Dir,
		Env:        spec.Env,
		Terminal:   req.Terminal,
	})
	if err != nil {
		return Result{}, fmt.Errorf("native session: %w", err)
	}
	exit, err := req.Supervisor.Wait(ctx, h.Handle)
	if err != nil {
		// The context was cancelled or the wait failed: the interactive
		// process stays supervised; the caller decides the stop protocol.
		return Result{Session: req.Session, Provider: req.Provider, Exit: exit, Reconciled: false}, err
	}
	return Result{
		Session:         req.Session,
		Provider:        req.Provider,
		ProviderSession: req.ProviderSession,
		Exit:            exit,
		Reconciled:      exit.Fact == process.FactProcessExit && exit.Code == 0,
	}, nil
}
