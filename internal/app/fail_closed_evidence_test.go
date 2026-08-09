package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cflow.local/cflow/internal/artifact"
	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/observe"
	"cflow.local/cflow/internal/store"
	"cflow.local/cflow/internal/verify"
)

func validVerificationManifest(t *testing.T, node model.NodeID) model.EvidenceManifest {
	t.Helper()
	m := model.EvidenceManifest{
		SchemaVersion: "1.0.0", Node: node, Output: "verified output",
		OutputHash: verificationTestHash([]byte("verified output")),
	}
	m.Hash = verificationManifestTestHash(t, m)
	return m
}

func verificationManifestTestHash(t *testing.T, m model.EvidenceManifest) string {
	t.Helper()
	m.Hash = ""
	body, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return verificationTestHash(body)
}

func verificationTestHash(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func putRequiredSpec(t *testing.T, a *Application, wf model.WorkflowID, id string) {
	t.Helper()
	st, err := a.artifactStore(wf)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`[{"id":"` + id + `","goal":"required fixture spec","depends_on":[],"write_scope":["src/**"],"read_scope":[],"locks":[],"acceptance":{"verification_command_ids":["verify"]},"route":{"provider":"fake","model":"default","budget":1},"timeout_seconds":60,"max_retry":0}]`)
	if _, err := st.Put(context.Background(), artifact.PutRequest{
		WorkflowID: wf, Type: model.ArtifactSpec, Revision: 1, SchemaVersion: "1.0.0",
		CreatedAt: "2026-01-01T00:00:00Z", Body: body,
	}); err != nil {
		t.Fatal(err)
	}
}

func putRequiredCatalog(t *testing.T, a *Application, wf model.WorkflowID) {
	putCatalogCommand(t, a, wf, "verify")
}

func putCatalogCommand(t *testing.T, a *Application, wf model.WorkflowID, commandID string) {
	t.Helper()
	body, err := verify.CatalogBody(1, []verify.Candidate{{
		CommandID: commandID, Purpose: verify.PurposeTaskVerify,
		ExecutableKind: verify.KindProjectRelative, Executable: "scripts/verify.sh",
		SHA256: strings.Repeat("a", 64), CWD: ".", TimeoutSeconds: 60,
		ExpectedExitCodes: []int{0}, OutputLimitBytes: 4096, Env: []string{"PATH"},
		Source: "fixture:scripts/verify.sh@sha256:" + strings.Repeat("a", 64),
	}})
	if err != nil {
		t.Fatal(err)
	}
	st, err := a.artifactStore(wf)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Resolve(context.Background(), artifact.ResolveRequest{WorkflowID: wf, Type: model.ArtifactCatalog}); err == nil {
		return
	} else if !artifact.IsNotFound(err) {
		t.Fatal(err)
	}
	if _, err := st.Put(context.Background(), artifact.PutRequest{
		WorkflowID: wf, Type: model.ArtifactCatalog, Revision: 1, SchemaVersion: "1.0.0",
		CreatedAt: "2026-01-01T00:00:00Z", Body: body,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCodingSessionRequiredArtifactsFailClosed(t *testing.T) {
	t.Run("missing spec", func(t *testing.T) {
		fx := newPlanningFixture(t)
		wf, err := fx.create("missing spec", false)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fx.app().codingSessionInput(context.Background(), wf, "task-s01"); err == nil {
			t.Fatal("missing approved Spec did not fail closed")
		}
	})

	t.Run("unmatched spec", func(t *testing.T) {
		fx := newPlanningFixture(t)
		wf, err := fx.create("unmatched spec", false)
		if err != nil {
			t.Fatal(err)
		}
		a := fx.app()
		putRequiredSpec(t, a, wf, "s02")
		seedNodeRow(t, filepath.Join(fx.home, "cflow.db"), wf,
			"task-s01", "s01", string(model.NodeAgentTask), string(model.NodeRunning), "cflow/task-s01")
		if _, err := a.codingSessionInput(context.Background(), wf, "task-s01"); err == nil {
			t.Fatal("Spec set without the dispatched node did not fail closed")
		}
	})

	t.Run("missing catalog", func(t *testing.T) {
		fx := newPlanningFixture(t)
		wf, err := fx.create("missing catalog", false)
		if err != nil {
			t.Fatal(err)
		}
		a := fx.app()
		putRequiredSpec(t, a, wf, "s01")
		seedNodeRow(t, filepath.Join(fx.home, "cflow.db"), wf,
			"task-s01", "s01", string(model.NodeAgentTask), string(model.NodeRunning), "cflow/task-s01")
		if _, err := a.codingSessionInput(context.Background(), wf, "task-s01"); err == nil {
			t.Fatal("missing approved Catalog did not fail closed")
		}
	})

	t.Run("unmatched catalog", func(t *testing.T) {
		fx := newPlanningFixture(t)
		wf, err := fx.create("unmatched catalog", false)
		if err != nil {
			t.Fatal(err)
		}
		a := fx.app()
		putRequiredSpec(t, a, wf, "s01")
		putCatalogCommand(t, a, wf, "other")
		seedNodeRow(t, filepath.Join(fx.home, "cflow.db"), wf,
			"task-s01", "s01", string(model.NodeAgentTask), string(model.NodeRunning), "cflow/task-s01")
		if _, err := a.codingSessionInput(context.Background(), wf, "task-s01"); err == nil {
			t.Fatal("Catalog without the Spec's required command did not fail closed")
		}
	})
}

func TestReviewRequiresVerificationManifest(t *testing.T) {
	fx := newPlanningFixture(t)
	wf, err := fx.create("missing verification", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fx.app().reviewSessionInput(context.Background(), wf, "verify-s01", "task-s01"); err == nil {
		t.Fatal("missing required verification manifest did not fail closed")
	}
}

func TestPolicySnapshotRetainsTaskWhenLayoutCannotBeResolved(t *testing.T) {
	fx := newPlanningFixture(t)
	wf, err := fx.create("policy snapshot", false)
	if err != nil {
		t.Fatal(err)
	}
	seedNodeRow(t, filepath.Join(fx.home, "cflow.db"), wf,
		"task-s01", "s01", string(model.NodeAgentTask), string(model.NodeRunning), "cflow/task-s01")
	if err := setLayoutVersion(t, filepath.Join(fx.home, "cflow.db"), wf, 0); err != nil {
		t.Fatal(err)
	}
	heads := fx.app().activeWorktreeHeads(context.Background(), wf)
	found := false
	for _, head := range heads {
		if head.Node == "task-s01" {
			found = true
		}
	}
	if !found {
		t.Fatal("layout observation failure silently omitted the active task")
	}
}

func TestApplyWorktreeInspectionOnlyAcceptsTrueAbsence(t *testing.T) {
	fx := newPlanningFixture(t)
	wf, err := fx.create("apply inspection", false)
	if err != nil {
		t.Fatal(err)
	}
	a := fx.app()
	view, err := a.readAggregate(context.Background(), wf, store.StoreQuery{})
	if err != nil {
		t.Fatal(err)
	}
	state := view.State
	state.ApplyAttempts = []model.ApplyAttempt{{ID: "apply-1", Number: 1, Status: model.ApplyBlocked}}
	path, err := a.applyWorktreePath(context.Background(), wf, 1)
	if err != nil {
		t.Fatal(err)
	}
	if needed, err := a.applyResolutionNeeded(context.Background(), wf, state); err != nil || needed {
		t.Fatalf("truly absent apply worktree = %v/%v, want false/nil", needed, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("missing-target", path); err != nil {
		t.Fatal(err)
	}
	if _, err := a.applyResolutionNeeded(context.Background(), wf, state); err == nil {
		t.Fatal("dangling apply-worktree symlink did not fail closed")
	}
	state.Workflow.ExecutionFacts = &model.ExecutionFacts{CatalogRevision: 1, CatalogHash: strings.Repeat("a", 64)}
	if _, err := a.applyIdentityDrifted(context.Background(), wf, &state.ApplyAttempts[0], state); err == nil {
		t.Fatal("identity inspection accepted a dangling apply-worktree symlink as absent")
	}
}

func TestReportRejectsCorruptVerificationEvidence(t *testing.T) {
	fx := newPlanningFixture(t)
	wf, err := fx.create("corrupt report evidence", false)
	if err != nil {
		t.Fatal(err)
	}
	a := fx.app()
	dir := filepath.Join(a.layout.EvidenceDir(wf), "verification")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "verify-s01.json"), []byte(`{"Hash":`), 0o600); err != nil {
		t.Fatal(err)
	}
	view, err := a.readAggregate(context.Background(), wf, store.StoreQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.reportInput(context.Background(), observe.BuildInfo{}, view.State, view.NextEventSeq, len(view.Events) == 0); err == nil {
		t.Fatal("report silently omitted corrupt verification evidence")
	}
}

func TestVerificationManifestIntegrityFailsClosed(t *testing.T) {
	t.Run("parseable output tampering at review gate", func(t *testing.T) {
		fx := newPlanningFixture(t)
		wf, err := fx.create("tampered verification output", false)
		if err != nil {
			t.Fatal(err)
		}
		a := fx.app()
		m := validVerificationManifest(t, "verify-s01")
		m.Output = "tampered but parseable output"
		if err := a.writeVerificationManifest(context.Background(), wf, m.Node, m); err != nil {
			t.Fatal(err)
		}
		if _, err := a.readRequiredVerificationManifest(context.Background(), wf, m.Node); err == nil {
			t.Fatal("review evidence accepted an Output value that does not match OutputHash")
		}
	})

	t.Run("parseable self hash tampering in final report", func(t *testing.T) {
		fx := newPlanningFixture(t)
		wf, err := fx.create("tampered verification self hash", false)
		if err != nil {
			t.Fatal(err)
		}
		a := fx.app()
		m := validVerificationManifest(t, "verify-s01")
		m.Hash = strings.Repeat("f", 64)
		if err := a.writeVerificationManifest(context.Background(), wf, m.Node, m); err != nil {
			t.Fatal(err)
		}
		view, err := a.readAggregate(context.Background(), wf, store.StoreQuery{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := a.reportInput(context.Background(), observe.BuildInfo{}, view.State, view.NextEventSeq, len(view.Events) == 0); err == nil {
			t.Fatal("final report accepted a verification manifest with a forged self-hash")
		}
	})
}
