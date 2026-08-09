package native

// Native Session Bridge tests (TUI task 12): the Bridge launches the
// provider's native interactive resume of a Session in the Workspace
// through the supervised interactive seam and returns the exit facts.

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"

	"cflow.local/cflow/internal/agent"
	"cflow.local/cflow/internal/agent/fake"
	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/process"
)

// TestBridgeRestoresAndReconciles is the TUI task 12 failure test: the
// Bridge runs the provider's native interactive resume of an existing
// Session in the Workspace and returns the reconciled exit facts.
func TestBridgeRestoresAndReconciles(t *testing.T) {
	reg, err := agent.LoadProviderRegistry()
	if err != nil {
		t.Fatal(err)
	}
	ad := fake.New(reg)
	sup := process.NewSupervisor(process.NewOSAdapter())
	session := model.SessionID("sess-1")
	providerSession := agent.ProviderSessionID("provider-sess-1")
	// The fake's interactive resume needs its executable on PATH.
	dir := t.TempDir()
	stubExecutable(t, dir)
	t.Setenv("PATH", dir)
	// PATH is also how the adapter's LookPath resolves "cflow-fake-agent".
	// The test terminal round-trips a fake interactive exchange: the
	// "provider" is the fake adapter's scripted run, but the interactive
	// seam launches a real stub process that echoes and exits 0.
	stubInteractiveProvider(t, dir)

	var inR io.Reader = strings.NewReader("")
	var outW io.Writer = io.Discard
	result, err := (Bridge{}).Run(context.Background(), Request{
		Workflow:        model.WorkflowID("wf-1"),
		Session:         session,
		Provider:        "fake",
		ProviderSession: providerSession,
		Worktree:        dir,
		Terminal:        process.Terminal{In: inR, Out: outW, Err: outW},
		Adapter:         ad,
		Supervisor:      sup,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Session != session || !result.Reconciled {
		t.Fatalf("%+v", result)
	}
	// The result echoes the exact Provider Session the turn resumed, so the
	// Application can revalidate the binding on return (design §9.2).
	if result.ProviderSession != providerSession || result.Provider != "fake" {
		t.Fatalf("bridge result binding = %+v, want %q/%q", result, providerSession, "fake")
	}
}

// TestBridgeRefusesWithoutCapability: a provider without the native
// interactive capability is refused with the protocol fault.
func TestBridgeRefusesWithoutCapability(t *testing.T) {
	sup := process.NewSupervisor(process.NewOSAdapter())
	_, err := (Bridge{}).Run(context.Background(), Request{
		Session: model.SessionID("s"), Provider: "fake",
		ProviderSession: "p", Worktree: "/w",
		Terminal:   process.Terminal{In: strings.NewReader(""), Out: io.Discard, Err: io.Discard},
		Adapter:    noInteractiveAdapter{},
		Supervisor: sup,
	})
	if err == nil {
		t.Fatal("a provider without the interactive capability was accepted")
	}
	if code, ok := model.CodeOf(err); !ok || code != model.CodeProviderProtocolUnsupported {
		t.Fatalf("fault = %v, want PROVIDER_PROTOCOL_UNSUPPORTED", err)
	}
}

// noInteractiveAdapter is an Adapter without the interactive seam.
type noInteractiveAdapter struct{}

func (noInteractiveAdapter) Detect(context.Context) (agent.Installation, error) {
	return agent.Installation{}, nil
}
func (noInteractiveAdapter) Start(context.Context, agent.StartRequest) (agent.Run, error) {
	return nil, model.NewFault(model.CodeNotYetAvailable, "not implemented")
}
func (noInteractiveAdapter) Resume(context.Context, agent.ResumeRequest) (agent.Run, error) {
	return nil, model.NewFault(model.CodeNotYetAvailable, "not implemented")
}
func (noInteractiveAdapter) Cancel(context.Context, agent.RunHandle) error {
	return nil
}
func (noInteractiveAdapter) Inspect(context.Context, agent.ProviderSessionID) (agent.SessionFact, error) {
	return agent.SessionFact{}, nil
}

func stubExecutable(t *testing.T, dir string) {
	t.Helper()
	if err := writeStub(dir, "cflow-fake-agent"); err != nil {
		t.Fatal(err)
	}
}

// stubInteractiveProvider creates a real "cflow-fake-agent" on PATH that
// exits 0 (the fake's interactive resume launches it; the supervisor
// reaps it and the Bridge reports the reconciled exit).
func stubInteractiveProvider(t *testing.T, dir string) {
	t.Helper()
	if err := writeStub(dir, "cflow-fake-agent"); err != nil {
		t.Fatal(err)
	}
}

func writeStub(dir, name string) error {
	path := dir + "/" + name
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		return err
	}
	return nil
}
