package cli

import (
	"testing"

	"cflow.local/cflow/internal/agent"
	"cflow.local/cflow/internal/agent/claude"
	"cflow.local/cflow/internal/agent/codex"
	"cflow.local/cflow/internal/process"
)

// TestOpenAdaptersDefault: without CFLOW_PROVIDERS the CLI binds no
// real Provider Adapter — the deterministic Fake Adapter stays the only
// registered Adapter and no Provider executable is ever probed here
// (detection happens later at the Execution Dry Run).
func TestOpenAdaptersDefault(t *testing.T) {
	sup := process.NewSupervisor(process.NewOSAdapter())
	reg, err := agent.LoadProviderRegistry()
	if err != nil {
		t.Fatal(err)
	}
	adapters, err := openAdapters(sup, reg, "")
	if err != nil {
		t.Fatalf("openAdapters: %v", err)
	}
	if len(adapters) != 0 {
		t.Fatalf("adapters = %v, want none without CFLOW_PROVIDERS", adapters)
	}
}

// TestOpenAdaptersRegistersRealProviders: CFLOW_PROVIDERS=codex,claude
// binds the real Adapters from the embedded registry bindings (the same
// Adapters the approval-gated E2E and self-Dogfood drive).
func TestOpenAdaptersRegistersRealProviders(t *testing.T) {
	sup := process.NewSupervisor(process.NewOSAdapter())
	reg, err := agent.LoadProviderRegistry()
	if err != nil {
		t.Fatal(err)
	}
	adapters, err := openAdapters(sup, reg, "codex, claude")
	if err != nil {
		t.Fatalf("openAdapters: %v", err)
	}
	if _, ok := adapters["codex"].(*codex.Adapter); !ok {
		t.Fatalf("codex adapter has unexpected type %T", adapters["codex"])
	}
	if _, ok := adapters["claude"].(*claude.Adapter); !ok {
		t.Fatalf("claude adapter has unexpected type %T", adapters["claude"])
	}
}

// TestOpenAdaptersUnknownProvider: a name the embedded registry does not
// know fails the open (never guessed, never silently skipped).
func TestOpenAdaptersUnknownProvider(t *testing.T) {
	sup := process.NewSupervisor(process.NewOSAdapter())
	reg, err := agent.LoadProviderRegistry()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := openAdapters(sup, reg, "bogus"); err == nil {
		t.Fatal("openAdapters with an unknown provider must fail")
	}
}
