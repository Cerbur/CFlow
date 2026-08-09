package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/store"
)

// TestLayout2RoutesManagedEvidenceBelowWorkflowRoot covers the three
// non-code evidence consumers: provider Session evidence, deterministic
// verification manifests, and reconciliation manifests.
func TestLayout2RoutesManagedEvidenceBelowWorkflowRoot(t *testing.T) {
	fx := newPlanningFixture(t)
	wf, err := fx.create("aggregate evidence", false)
	if err != nil {
		t.Fatal(err)
	}
	a := fx.app()
	view, err := a.readAggregate(context.Background(), wf, store.StoreQuery{})
	if err != nil {
		t.Fatal(err)
	}
	rt, err := a.agentRuntime(context.Background(), view.State)
	if err != nil {
		t.Fatal(err)
	}
	if rt == nil {
		t.Fatal("agent runtime missing")
	}
	defer rt.Close()
	sessionRoot, err := rt.EvidenceDir()
	if err != nil {
		t.Fatal(err)
	}
	wantSessionRoot := a.layout.SessionsDir(wf)
	if sessionRoot != wantSessionRoot {
		t.Fatalf("session evidence root = %q, want %q", sessionRoot, wantSessionRoot)
	}

	manifest := model.EvidenceManifest{Node: "verify-s01", Hash: "manifest-hash"}
	if err := a.writeVerificationManifest(context.Background(), wf, "verify-s01", manifest); err != nil {
		t.Fatal(err)
	}
	wantVerification := filepath.Join(a.layout.EvidenceDir(wf), "verification", "verify-s01.json")
	if _, err := os.Stat(wantVerification); err != nil {
		t.Fatalf("workflow verification evidence missing: %v", err)
	}

	if _, err := a.writeReconciliationManifest(context.Background(), wf, 1, []byte(`{"revision":1}`)); err != nil {
		t.Fatal(err)
	}
	wantReconciliation := filepath.Join(a.layout.EvidenceDir(wf), "reconciliation", "manifest-1.json")
	if _, err := os.Stat(wantReconciliation); err != nil {
		t.Fatalf("workflow reconciliation evidence missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(fx.home, "evidence")); !os.IsNotExist(err) {
		t.Fatalf("layout 2 created global evidence root: %v", err)
	}
}

// TestUnknownLayoutCannotSelectLegacyOutputPath proves version zero or an
// unreadable layout never silently writes new output to the legacy tree.
func TestUnknownLayoutCannotSelectLegacyOutputPath(t *testing.T) {
	fx := newPlanningFixture(t)
	wf, err := fx.create("invalid layout", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := setLayoutVersion(t, filepath.Join(fx.home, "cflow.db"), wf, 0); err != nil {
		t.Fatal(err)
	}
	a := fx.app()
	err = a.exportEvents(context.Background(), wf, []model.Event{{Kind: model.EventWorkflowPaused, Workflow: wf}})
	if err == nil {
		t.Fatal("unknown layout selected a legacy events path")
	}
	legacy := filepath.Join(fx.home, "projects", ProjectFor(fx.root).Key, "workflows", string(wf), "events.jsonl")
	if _, statErr := os.Stat(legacy); !os.IsNotExist(statErr) {
		t.Fatalf("unknown layout wrote legacy output: %v", statErr)
	}
}
