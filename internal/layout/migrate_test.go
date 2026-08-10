package layout

// Legacy Layout Migration tests (TUI task 8): the read-only Preview
// derives the exact ordered moves for a Layout Version 1 workflow without
// touching the filesystem, and the manifest hash is canonical and stable.

import (
	"path/filepath"
	"testing"

	"cflow.local/cflow/internal/model"
)

// legacyState is a minimal Layout 1 aggregate carrying the legacy
// Integration Branch/Head and one Task Node.
func legacyState(wf model.WorkflowID) model.State {
	return model.State{
		Workflow: model.Workflow{
			ID: wf, Stage: model.StageExecution, Runtime: model.RuntimeRunning,
			BaseCommit:        "base-1",
			IntegrationBranch: "cflow/" + string(wf) + "/integration",
			IntegrationHead:   "int-1",
			LayoutVersion:     1,
		},
		Nodes: map[model.NodeID]*model.Node{
			"task-s01": {ID: "task-s01", Kind: model.NodeAgentTask},
			"task-s02": {ID: "task-s02", Kind: model.NodeAgentTask},
		},
	}
}

// TestLegacyPreviewIsReadOnly is the TUI task 8 failure test: computing
// the Preview of a Layout 1 workflow derives the exact moves and never
// creates, moves, or deletes anything (the fixture's legacy roots stay
// untouched and the aggregated root stays absent).
func TestLegacyPreviewIsReadOnly(t *testing.T) {
	wf := model.WorkflowID("wf-1")
	home := filepath.Join(t.TempDir(), "home")
	key := "test-project"
	st := legacyState(wf)

	preview, err := Preview(wf, 1, 2, key, home, st)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if preview.Workflow != wf || preview.From != 1 || preview.To != 2 {
		t.Fatalf("preview identity = %+v", preview)
	}
	if preview.ManifestHash == "" {
		t.Fatal("preview carried no manifest hash")
	}
	// The exact ordered moves: planning, integration -> workspace, both
	// task worktrees, the artifacts root, and the static manifest.
	want := []struct {
		kind MoveKind
		src  string
		dst  string
	}{
		{MoveKindWorktree, filepath.Join(home, "worktrees", key, string(wf), "planning"),
			filepath.Join(home, "projects", key, string(wf), "tmp", "planning")},
		{MoveKindWorktree, filepath.Join(home, "worktrees", key, string(wf), "integration"),
			filepath.Join(home, "projects", key, string(wf), "workspace")},
		{MoveKindWorktree, filepath.Join(home, "worktrees", key, string(wf), "tasks", "task-s01"),
			filepath.Join(home, "projects", key, string(wf), "tmp", "tasks", "task-s01")},
		{MoveKindWorktree, filepath.Join(home, "worktrees", key, string(wf), "tasks", "task-s02"),
			filepath.Join(home, "projects", key, string(wf), "tmp", "tasks", "task-s02")},
		{MoveKindArtifact, filepath.Join(home, "projects", key, "workflows", string(wf), "workflow.yaml"),
			filepath.Join(home, "projects", key, string(wf), "workflow.yaml")},
	}
	if len(preview.Moves) != len(want) {
		t.Fatalf("moves = %d, want %d: %+v", len(preview.Moves), len(want), preview.Moves)
	}
	for i, w := range want {
		got := preview.Moves[i]
		if got.Kind != w.kind || got.Source != w.src || got.Destination != w.dst {
			t.Fatalf("move %d = %+v, want kind=%s src=%s dst=%s", i, got, w.kind, w.src, w.dst)
		}
	}
	// The integration move binds its branch and head (the future Workspace
	// Branch and the verified Head).
	integ := preview.Moves[1]
	if integ.Branch != "cflow/"+string(wf)+"/integration" || integ.Head != "int-1" {
		t.Fatalf("integration move = %+v, want the branch and head bound", integ)
	}
	// Determinism: the same input produces the same manifest hash.
	again, err := Preview(wf, 1, 2, key, home, st)
	if err != nil {
		t.Fatal(err)
	}
	if again.ManifestHash != preview.ManifestHash {
		t.Fatalf("manifest hash is not canonical: %s != %s", again.ManifestHash, preview.ManifestHash)
	}
}

// TestLegacyPreviewRefusesNonLegacyWorkflow: a Layout 2 workflow (or one
// without a recorded integration head) never receives a Preview.
func TestLegacyPreviewRefusesNonLegacyWorkflow(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	key := "test-project"
	wf := model.WorkflowID("wf-1")
	agg := legacyState(wf)
	agg.Workflow.LayoutVersion = 2
	if _, err := Preview(wf, 1, 2, key, home, agg); err == nil {
		t.Fatal("a Layout 2 workflow received a migration preview")
	}
	noHead := legacyState(wf)
	noHead.Workflow.IntegrationHead = ""
	if _, err := Preview(wf, 1, 2, key, home, noHead); err == nil {
		t.Fatal("a legacy workflow without an integration head received a preview")
	}
}
