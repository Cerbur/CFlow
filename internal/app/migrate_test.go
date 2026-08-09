package app

// The explicit Legacy Layout Migration (TUI task 8, design §7.4):
// Preview is read-only, Prepare persists the migration row and the bound
// manifest, and Execute moves the legacy Worktrees and Artifacts into
// the aggregated workflow root and advances the persisted Layout facts
// to Version 2. The fixture builds a Layout Version 1 workflow directly
// (legacy planning/integration/task Worktrees and the legacy Artifacts
// root), mirrors the workflow.yaml, and drives the three explicit steps.

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"cflow.local/cflow/internal/artifact"
	"cflow.local/cflow/internal/gitflow"
	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/security"
)

// legacyMigrationFixture builds one Layout Version 1 workflow over the
// real repository with its legacy roots.
type legacyMigrationFixture struct {
	t   *testing.T
	fx  *planningFixture
	wf  model.WorkflowID
	key string

	legacyRoot string // worktrees/<key>/<wf>
	aggRoot    string // projects/<key>/<wf>
	legacyRef  model.ArtifactRef
}

// newLegacyMigrationFixture creates a workflow through the real pipeline
// and rewinds it to the legacy layout: the DB row is set to
// layout_version=1, the artifacts and workflow.yaml stay in the legacy
// projects/<key>/workflows/<wf> root, and legacy planning/integration/
// task Worktrees are created under worktrees/<key>/<wf>.
func newLegacyMigrationFixture(t *testing.T) *legacyMigrationFixture {
	t.Helper()
	fx := newExecutionFixture(t)
	wf, err := fx.create("legacy-demo", false)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// The aggregated workspace was created by the create; remove the
	// registered Worktree (git worktree remove, never a bare directory
	// delete) and rewind the row so the fixture is a true Layout 1
	// workflow.
	ws := fx.workspacePath(wf)
	cmd := execGit(fx.root, "worktree", "remove", ws)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("remove workspace worktree: %v\n%s", err, out)
	}
	key := ProjectFor(fx.root).Key
	dbPath := filepath.Join(fx.home, "cflow.db")
	if err := setLayoutVersion(t, dbPath, wf, 1); err != nil {
		t.Fatalf("rewind layout: %v", err)
	}
	// Create the legacy planning/integration/task Worktrees.
	flow, err := gitflow.NewGitFlow(fx.sup, fx.root)
	if err != nil {
		t.Fatalf("new gitflow: %v", err)
	}
	legacyRoot := filepath.Join(fx.home, "worktrees", key, string(wf))
	planning := filepath.Join(legacyRoot, "planning")
	integration := filepath.Join(legacyRoot, "integration")
	taskWT := filepath.Join(legacyRoot, "tasks", "task-s01")
	for _, dir := range []string{filepath.Join(legacyRoot, "tasks")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	base := gitOut(t, fx.root, "rev-parse", "HEAD")
	if _, err := flow.Execute(context.Background(), gitflow.CreatePlanningSnapshot{
		BaseCommit: base, Path: planning,
	}); err != nil {
		t.Fatalf("create planning snapshot: %v", err)
	}
	if _, err := flow.Execute(context.Background(), gitflow.CreateIntegration{
		Branch: "cflow/" + string(wf) + "/integration", BaseCommit: base, Path: integration,
	}); err != nil {
		t.Fatalf("create integration: %v", err)
	}
	if _, err := flow.Execute(context.Background(), gitflow.CreateTask{
		Branch: "cflow/" + string(wf) + "/task-task-s01", BaseHead: base, Path: taskWT,
	}); err != nil {
		t.Fatalf("create task: %v", err)
	}
	// Record the legacy integration head in the aggregate.
	if err := setIntegrationHead(t, dbPath, wf, base); err != nil {
		t.Fatalf("record integration head: %v", err)
	}
	// Build the legacy non-code root explicitly. Move the fixture manifest
	// from its original Layout 2 destination and create the historical
	// artifacts directory; Layout 2 writes must never populate that legacy
	// directory as a side effect.
	legacyWFYaml := filepath.Join(fx.home, "projects", key, "workflows", string(wf), "workflow.yaml")
	if err := os.MkdirAll(filepath.Dir(legacyWFYaml), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(filepath.Dir(legacyWFYaml), "artifacts"), 0o700); err != nil {
		t.Fatal(err)
	}
	legacyStore, err := artifact.New(filepath.Join(filepath.Dir(legacyWFYaml), "artifacts"), security.Registry{})
	if err != nil {
		t.Fatal(err)
	}
	legacyRef, err := legacyStore.Put(context.Background(), artifact.PutRequest{
		WorkflowID: wf, Type: model.ArtifactReport, Revision: 1,
		SchemaVersion: "1.0.0", CreatedAt: "2026-01-01T00:00:00Z",
		Body: []byte("legacy report"),
	})
	if err != nil {
		t.Fatal(err)
	}
	aggregatedWFYaml := filepath.Join(fx.home, "projects", key, string(wf), "workflow.yaml")
	if err := os.Rename(aggregatedWFYaml, legacyWFYaml); err != nil {
		t.Fatalf("move fixture manifest to legacy root: %v", err)
	}
	return &legacyMigrationFixture{
		t: t, fx: fx, wf: wf, key: key,
		legacyRoot: legacyRoot,
		aggRoot:    filepath.Join(fx.home, "projects", key, string(wf)),
		legacyRef:  legacyRef,
	}
}

func (lf *legacyMigrationFixture) app() *Application {
	return lf.fx.app()
}

// TestLegacyMigrationPreviewPrepareExecute drives the three explicit
// steps and asserts the aggregated layout after Execute.
func TestLegacyMigrationPreviewPrepareExecute(t *testing.T) {
	lf := newLegacyMigrationFixture(t)
	ctx := context.Background()
	a := lf.app()

	// Preview: read-only — no file moved, manifest hash stable.
	qv, err := a.Query(ctx, LayoutMigrationPreviewQuery{Workflow: lf.wf})
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	pv := qv.(MigrationPreviewView)
	if pv.From != 1 || pv.To != 2 || pv.ManifestHash == "" || len(pv.Moves) < 4 {
		t.Fatalf("preview = %+v", pv)
	}
	if !pathExists(lf.legacyRoot+"/integration") || pathExists(lf.aggRoot+"/workspace") {
		t.Fatal("the preview moved files")
	}

	// Prepare: the migration row is persisted; still nothing moves.
	if _, err := a.Execute(ctx, PrepareLayoutMigrationCommand{Workflow: lf.wf, ManifestHash: pv.ManifestHash}); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if !pathExists(lf.legacyRoot+"/integration") || pathExists(lf.aggRoot+"/workspace") {
		t.Fatal("the prepare moved files")
	}

	// Execute: every move lands and the layout facts advance to 2.
	if _, err := a.Execute(ctx, ExecuteLayoutMigrationCommand{Workflow: lf.wf, ManifestHash: pv.ManifestHash}); err != nil {
		t.Fatalf("execute: %v", err)
	}
	// The integration Worktree became the aggregated Workspace.
	if !pathExists(lf.aggRoot + "/workspace") {
		t.Fatal("the workspace was not created by the migration")
	}
	if pathExists(lf.legacyRoot + "/integration") {
		t.Fatal("the legacy integration worktree still exists")
	}
	// The task worktree moved into tmp/tasks.
	if !pathExists(filepath.Join(lf.aggRoot, "tmp", "tasks", "task-s01")) {
		t.Fatal("the task worktree was not migrated")
	}
	// Every legacy revision is readable through the Layout 2 Store after
	// migration; the historical artifacts/<wf>/<type> shape is gone.
	workflowStore, err := artifact.NewWorkflow(lf.aggRoot, lf.wf, security.Registry{})
	if err != nil {
		t.Fatal(err)
	}
	ref, err := workflowStore.Resolve(ctx, artifact.ResolveRequest{
		WorkflowID: lf.wf, Type: model.ArtifactReport, Revision: lf.legacyRef.Revision,
	})
	if err != nil {
		t.Fatalf("resolve migrated artifact: %v", err)
	}
	if ref.Hash != lf.legacyRef.Hash {
		t.Fatalf("migrated artifact hash = %s, want %s", ref.Hash, lf.legacyRef.Hash)
	}
	// The workflow is now Layout 2 with the workspace facts.
	st := lf.fx.status(lf.wf)
	if st.WorkspaceBranch != "cflow/"+string(lf.wf)+"/integration" || st.VerifiedWorkspaceHead == "" {
		t.Fatalf("migrated workspace facts = %+v", st)
	}
}

// TestLegacyMigrationPrepareRejectsStaleManifest: a manifest that no
// longer matches the current Preview is refused with no mutation.
func TestLegacyMigrationPrepareRejectsStaleManifest(t *testing.T) {
	lf := newLegacyMigrationFixture(t)
	ctx := context.Background()
	a := lf.app()
	_, err := a.Execute(ctx, PrepareLayoutMigrationCommand{Workflow: lf.wf, ManifestHash: "stale"})
	if err == nil {
		t.Fatal("a stale manifest was accepted by Prepare")
	}
	if code, ok := model.CodeOf(err); !ok || code != model.CodeApprovalInputChanged {
		t.Fatalf("stale manifest fault = %v", err)
	}
}

// setLayoutVersion rewinds the persisted Layout Version of one workflow
// (the migration fixture's legacy construction).
func setLayoutVersion(t *testing.T, dbPath string, wf model.WorkflowID, version int) error {
	t.Helper()
	db, err := openRawDB(t, dbPath)
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.Exec(`UPDATE workflows SET layout_version = ?, workspace_path = '', workspace_branch = '' WHERE id = ?`, version, string(wf))
	return err
}

// setIntegrationHead records the legacy Integration Head of one workflow.
func setIntegrationHead(t *testing.T, dbPath string, wf model.WorkflowID, head string) error {
	t.Helper()
	db, err := openRawDB(t, dbPath)
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.Exec(`UPDATE workflows SET integration_branch = ?, integration_head = ? WHERE id = ?`,
		"cflow/"+string(wf)+"/integration", head, string(wf))
	return err
}

// openRawDB opens the workflow database read-write for the fixture's
// legacy rewinds.
func openRawDB(t *testing.T, dbPath string) (*sql.DB, error) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}
	return db, nil
}
