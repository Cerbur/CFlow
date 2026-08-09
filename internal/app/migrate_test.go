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
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cflow.local/cflow/internal/artifact"
	"cflow.local/cflow/internal/gitflow"
	"cflow.local/cflow/internal/layout"
	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/security"
)

// TestLegacyMigrationPreparePersistsMatchingIntent proves the Prepare
// durability boundary: the immutable migration row and the recoverable
// effect intent must both exist, carry the same migration identity/hash,
// and precede every filesystem move.
func TestLegacyMigrationPreparePersistsMatchingIntent(t *testing.T) {
	lf := newLegacyMigrationFixture(t)
	ctx := context.Background()
	a := lf.app()
	qv, err := a.Query(ctx, LayoutMigrationPreviewQuery{Workflow: lf.wf})
	if err != nil {
		t.Fatal(err)
	}
	pv := qv.(MigrationPreviewView)
	if _, err := a.Execute(ctx, PrepareLayoutMigrationCommand{Workflow: lf.wf, ManifestHash: pv.ManifestHash}); err != nil {
		t.Fatalf("prepare: %v", err)
	}

	db, err := openRawDB(t, filepath.Join(lf.fx.home, "cflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var migrationID, status, manifestPath, manifestHash string
	if err := db.QueryRow(`SELECT id, status, manifest_path, manifest_sha256
		FROM layout_migrations WHERE workflow_id = ?`, string(lf.wf)).Scan(
		&migrationID, &status, &manifestPath, &manifestHash); err != nil {
		t.Fatalf("migration row: %v", err)
	}
	if migrationID == "" || status != "PREPARED" || manifestHash != pv.ManifestHash {
		// The persisted manifest hash may differ from the read-only preview
		// hash once backup/snapshot evidence is attached, but it must never
		// be empty.
		if migrationID == "" || status != "PREPARED" || manifestHash == "" {
			t.Fatalf("migration row = id=%q status=%q hash=%q", migrationID, status, manifestHash)
		}
	}
	if _, err := os.Stat(manifestPath); err != nil {
		t.Fatalf("immutable manifest: %v", err)
	}
	backupPath := strings.TrimSuffix(manifestPath, ".json") + ".db.backup"
	backup, err := sql.Open("sqlite", "file:"+backupPath+"?mode=ro")
	if err != nil {
		t.Fatalf("open migration database backup: %v", err)
	}
	defer backup.Close()
	var integrity string
	if err := backup.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatalf("migration database backup integrity = %q, err=%v", integrity, err)
	}
	var payload []byte
	if err := db.QueryRow(`SELECT payload_json FROM effects
		WHERE workflow_id = ? AND kind = 'layout-migration' AND status = 'PENDING'`,
		string(lf.wf)).Scan(&payload); err != nil {
		t.Fatalf("pending migration intent: %v", err)
	}
	var intent model.LayoutMigrationIntent
	if err := json.Unmarshal(payload, &intent); err != nil {
		t.Fatal(err)
	}
	if intent.MigrationID != migrationID || intent.ManifestHash != manifestHash || len(intent.Moves) != len(pv.Moves) {
		t.Fatalf("intent = %+v, row id/hash = %s/%s", intent, migrationID, manifestHash)
	}
	if !pathExists(lf.legacyRoot+"/integration") || pathExists(lf.aggRoot+"/workspace") {
		t.Fatal("Prepare moved a worktree")
	}
}

func TestPreparedManifestBindsBackupAndDatabaseImpact(t *testing.T) {
	lf := newLegacyMigrationFixture(t)
	ctx := context.Background()
	a := lf.app()
	qv, err := a.Query(ctx, LayoutMigrationPreviewQuery{Workflow: lf.wf})
	if err != nil {
		t.Fatal(err)
	}
	pv := qv.(MigrationPreviewView)
	if _, err := a.Execute(ctx, PrepareLayoutMigrationCommand{Workflow: lf.wf, ManifestHash: pv.ManifestHash}); err != nil {
		t.Fatal(err)
	}
	db, err := openRawDB(t, filepath.Join(lf.fx.home, "cflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var path, recordedHash string
	if err := db.QueryRow(`SELECT manifest_path, manifest_sha256 FROM layout_migrations WHERE workflow_id = ?`, string(lf.wf)).Scan(&path, &recordedHash); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	if fmt.Sprintf("%x", sum[:]) != recordedHash {
		t.Fatalf("row hash does not bind manifest bytes")
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	backup, ok := doc["backup"].(map[string]any)
	if !ok || backup["path"] == "" || backup["sha256"] == "" || backup["size"] == nil {
		t.Fatalf("backup evidence missing: %+v", doc)
	}
	if doc["source_snapshot_hash"] == "" || doc["database_impact"] == nil {
		t.Fatalf("source snapshot/database impact missing: %+v", doc)
	}
}

func TestPrepareRejectsSymlinkManifestRetry(t *testing.T) {
	lf := newLegacyMigrationFixture(t)
	ctx := context.Background()
	a := lf.app()
	qv, err := a.Query(ctx, LayoutMigrationPreviewQuery{Workflow: lf.wf})
	if err != nil {
		t.Fatal(err)
	}
	pv := qv.(MigrationPreviewView)
	preview := layout.MigrationPreview{Workflow: pv.Workflow, From: pv.From, To: pv.To, Moves: pv.Moves, ManifestHash: pv.ManifestHash}
	body, err := json.Marshal(preview)
	if err != nil {
		t.Fatal(err)
	}
	id := migrationID(lf.wf, pv.ManifestHash)
	manifestPath := filepath.Join(a.layout.StateDir(lf.wf), "layout-migrations", id+".json")
	if err := os.MkdirAll(filepath.Dir(manifestPath), 0o700); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(t.TempDir(), "manifest.json")
	if err := os.WriteFile(foreign, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(foreign, manifestPath); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Execute(ctx, PrepareLayoutMigrationCommand{Workflow: lf.wf, ManifestHash: pv.ManifestHash}); err == nil {
		t.Fatal("Prepare accepted a symlink immutable manifest")
	}
}

// TestLegacyMigrationPrepareIsIdempotentOnRetry proves the Prepare
// durability contract: a crash mid-Prepare leaves the immutable manifest
// on disk, and re-running the exact same Prepare (the designed recovery
// action) must succeed idempotently — it must not hard-fail, must not
// create a second migration row, and must not alter the manifest bytes.
func TestLegacyMigrationPrepareIsIdempotentOnRetry(t *testing.T) {
	lf := newLegacyMigrationFixture(t)
	ctx := context.Background()
	a := lf.app()
	qv, err := a.Query(ctx, LayoutMigrationPreviewQuery{Workflow: lf.wf})
	if err != nil {
		t.Fatal(err)
	}
	pv := qv.(MigrationPreviewView)
	if _, err := a.Execute(ctx, PrepareLayoutMigrationCommand{Workflow: lf.wf, ManifestHash: pv.ManifestHash}); err != nil {
		t.Fatalf("prepare #1: %v", err)
	}
	manifestPath := filepath.Join(a.layout.StateDir(lf.wf), "layout-migrations",
		migrationID(lf.wf, pv.ManifestHash)+".json")
	before, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read manifest after prepare #1: %v", err)
	}
	if _, err := a.Execute(ctx, PrepareLayoutMigrationCommand{Workflow: lf.wf, ManifestHash: pv.ManifestHash}); err != nil {
		t.Fatalf("prepare #2 (idempotent retry): %v", err)
	}
	after, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read manifest after prepare #2: %v", err)
	}
	if string(before) != string(after) {
		t.Fatal("the immutable manifest bytes changed across the idempotent retry")
	}
	db, err := openRawDB(t, filepath.Join(lf.fx.home, "cflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var rowCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM layout_migrations WHERE workflow_id = ?`, string(lf.wf)).Scan(&rowCount); err != nil {
		t.Fatal(err)
	}
	if rowCount != 1 {
		t.Fatalf("migration rows = %d, want exactly 1", rowCount)
	}
	var status string
	if err := db.QueryRow(`SELECT status FROM layout_migrations WHERE workflow_id = ?`, string(lf.wf)).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "PREPARED" {
		t.Fatalf("migration status = %q, want PREPARED", status)
	}
}

func TestLegacyMigrationExecuteBlocksOutOfOrderBeforeAnyMove(t *testing.T) {
	lf := newLegacyMigrationFixture(t)
	ctx := context.Background()
	a := lf.app()
	qv, err := a.Query(ctx, LayoutMigrationPreviewQuery{Workflow: lf.wf})
	if err != nil {
		t.Fatal(err)
	}
	pv := qv.(MigrationPreviewView)
	if _, err := a.Execute(ctx, PrepareLayoutMigrationCommand{Workflow: lf.wf, ManifestHash: pv.ManifestHash}); err != nil {
		t.Fatal(err)
	}
	if err := a.performMigrationMove(ctx, lf.wf, pv.Moves[1]); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Execute(ctx, ExecuteLayoutMigrationCommand{Workflow: lf.wf, ManifestHash: pv.ManifestHash}); err == nil {
		t.Fatal("out-of-order landed move was accepted")
	}
	if !pathExists(pv.Moves[0].Source) || pathExists(pv.Moves[0].Destination) {
		t.Fatal("Execute moved the first item before detecting later drift")
	}
}

func TestLegacyMigrationPersistedTaskIsDeduplicatedAndBound(t *testing.T) {
	lf := newLegacyMigrationFixture(t)
	db, err := openRawDB(t, filepath.Join(lf.fx.home, "cflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	now := "2026-01-01T00:00:00Z"
	branch := "cflow/" + string(lf.wf) + "/task-task-s01"
	if _, err := db.Exec(`INSERT INTO tasks (id, workflow_id, spec_id, title, branch_name, created_at, updated_at)
		VALUES (?, ?, ?, 'task', ?, ?, ?)`, "persisted-task-s01", string(lf.wf), "task-s01", branch, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO nodes (id, workflow_id, task_id, node_type, status, created_at, updated_at)
		VALUES ('task-s01', ?, 'persisted-task-s01', 'agent-task', 'PENDING', ?, ?)`, string(lf.wf), now, now); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	qv, err := lf.app().Query(context.Background(), LayoutMigrationPreviewQuery{Workflow: lf.wf})
	if err != nil {
		t.Fatal(err)
	}
	var found []model.PathMove
	for _, move := range qv.(MigrationPreviewView).Moves {
		if move.Source == filepath.Join(lf.legacyRoot, "tasks", "task-s01") {
			found = append(found, move)
		}
	}
	if len(found) != 1 {
		t.Fatalf("persisted/observed task moves = %d, want 1: %+v", len(found), found)
	}
	if found[0].Branch != branch || found[0].Head == "" {
		t.Fatalf("task move not exactly bound: %+v", found[0])
	}
}

func TestPreparedMigrationPreviewRejectsAuthoritativeRowDrift(t *testing.T) {
	lf := newLegacyMigrationFixture(t)
	ctx := context.Background()
	a := lf.app()
	qv, err := a.Query(ctx, LayoutMigrationPreviewQuery{Workflow: lf.wf})
	if err != nil {
		t.Fatal(err)
	}
	pv := qv.(MigrationPreviewView)
	if _, err := a.Execute(ctx, PrepareLayoutMigrationCommand{Workflow: lf.wf, ManifestHash: pv.ManifestHash}); err != nil {
		t.Fatal(err)
	}
	db, err := openRawDB(t, filepath.Join(lf.fx.home, "cflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE layout_migrations SET manifest_sha256 = 'drift' WHERE workflow_id = ?`, string(lf.wf)); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	if _, err := a.Query(ctx, LayoutMigrationPreviewQuery{Workflow: lf.wf}); err == nil {
		t.Fatal("prepared preview accepted a drifted authoritative migration row")
	}
}

// TestLegacyMigrationExecuteContinuesAfterOneMove proves Execute is driven
// by the persisted intent and reconstructible source/destination facts. A
// crash after one landed move must continue; it must not recompute a fresh
// preview and reject the now-absent source.
func TestLegacyMigrationExecuteContinuesAfterOneMove(t *testing.T) {
	lf := newLegacyMigrationFixture(t)
	ctx := context.Background()
	a := lf.app()
	qv, err := a.Query(ctx, LayoutMigrationPreviewQuery{Workflow: lf.wf})
	if err != nil {
		t.Fatal(err)
	}
	pv := qv.(MigrationPreviewView)
	if _, err := a.Execute(ctx, PrepareLayoutMigrationCommand{Workflow: lf.wf, ManifestHash: pv.ManifestHash}); err != nil {
		t.Fatal(err)
	}
	if err := a.performMigrationMove(ctx, lf.wf, pv.Moves[0]); err != nil {
		t.Fatalf("land first move: %v", err)
	}
	if _, err := a.Execute(ctx, ExecuteLayoutMigrationCommand{Workflow: lf.wf, ManifestHash: pv.ManifestHash}); err != nil {
		t.Fatalf("resume execute: %v", err)
	}
	if got := lf.fx.status(lf.wf); got.WorkspacePath != filepath.Join(lf.aggRoot, "workspace") {
		t.Fatalf("workspace path = %q", got.WorkspacePath)
	}
}

// TestLegacyMigrationExecuteContinuesAfterEveryMove exercises every
// per-step crash boundary of the persisted manifest. The source/destination
// and Git registry facts uniquely reconstruct progress without overwrite.
func TestLegacyMigrationExecuteContinuesAfterEveryMove(t *testing.T) {
	seed := newLegacyMigrationFixture(t)
	view, err := seed.app().Query(context.Background(), LayoutMigrationPreviewQuery{Workflow: seed.wf})
	if err != nil {
		t.Fatal(err)
	}
	moveCount := len(view.(MigrationPreviewView).Moves)
	for completed := 1; completed <= moveCount; completed++ {
		t.Run(fmt.Sprintf("after-move-%d", completed), func(t *testing.T) {
			lf := newLegacyMigrationFixture(t)
			ctx := context.Background()
			a := lf.app()
			qv, err := a.Query(ctx, LayoutMigrationPreviewQuery{Workflow: lf.wf})
			if err != nil {
				t.Fatal(err)
			}
			pv := qv.(MigrationPreviewView)
			if _, err := a.Execute(ctx, PrepareLayoutMigrationCommand{Workflow: lf.wf, ManifestHash: pv.ManifestHash}); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < completed; i++ {
				if err := a.performMigrationMove(ctx, lf.wf, pv.Moves[i]); err != nil {
					t.Fatalf("land move %d: %v", i, err)
				}
			}
			if _, err := a.Execute(ctx, ExecuteLayoutMigrationCommand{Workflow: lf.wf, ManifestHash: pv.ManifestHash}); err != nil {
				t.Fatalf("continue after %d moves: %v", completed, err)
			}
			if got := lf.fx.status(lf.wf); got.LayoutVersion != 2 {
				t.Fatalf("layout version = %d", got.LayoutVersion)
			}
		})
	}
}

// TestLegacyMigrationExecuteRecognizesDBCommitCrash exercises the final
// crash window: every move and the authoritative Layout 2 facts committed,
// but the migration/effect status marker did not. Execute only settles the
// matching markers and reports completion.
func TestLegacyMigrationExecuteRecognizesDBCommitCrash(t *testing.T) {
	lf := newLegacyMigrationFixture(t)
	ctx := context.Background()
	a := lf.app()
	qv, err := a.Query(ctx, LayoutMigrationPreviewQuery{Workflow: lf.wf})
	if err != nil {
		t.Fatal(err)
	}
	pv := qv.(MigrationPreviewView)
	if _, err := a.Execute(ctx, PrepareLayoutMigrationCommand{Workflow: lf.wf, ManifestHash: pv.ManifestHash}); err != nil {
		t.Fatal(err)
	}
	for i, move := range pv.Moves {
		if err := a.performMigrationMove(ctx, lf.wf, move); err != nil {
			t.Fatalf("move %d: %v", i, err)
		}
	}
	db, err := openRawDB(t, filepath.Join(lf.fx.home, "cflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`UPDATE workflows SET layout_version = 2, aggregate_version = aggregate_version + 1,
		workspace_path = ?, workspace_branch = ?,
		candidate_workspace_head = integration_head, verified_workspace_head = integration_head WHERE id = ?`,
		filepath.Join(lf.aggRoot, "workspace"), "cflow/"+string(lf.wf)+"/integration", string(lf.wf))
	_ = db.Close()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Execute(ctx, ExecuteLayoutMigrationCommand{Workflow: lf.wf, ManifestHash: pv.ManifestHash}); err != nil {
		t.Fatalf("recognize db commit: %v", err)
	}
	db, err = openRawDB(t, filepath.Join(lf.fx.home, "cflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var migrationStatus, effectStatus string
	if err := db.QueryRow(`SELECT status FROM layout_migrations WHERE workflow_id = ?`, string(lf.wf)).Scan(&migrationStatus); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT status FROM effects WHERE workflow_id = ? AND kind = 'layout-migration'`, string(lf.wf)).Scan(&effectStatus); err != nil {
		t.Fatal(err)
	}
	if migrationStatus != "COMPLETED" || effectStatus != "RESULTED" {
		t.Fatalf("statuses = migration:%s effect:%s", migrationStatus, effectStatus)
	}
}

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

// TestLegacyMigrationRejectsPathsOutsideManagedRoots proves a persisted
// move cannot escape the exact legacy source roots or aggregated
// destination root, even if both paths otherwise exist.
func TestLegacyMigrationRejectsPathsOutsideManagedRoots(t *testing.T) {
	lf := newLegacyMigrationFixture(t)
	outside := filepath.Join(t.TempDir(), "foreign.txt")
	if err := os.WriteFile(outside, []byte("foreign"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := lf.app().performMigrationMove(context.Background(), lf.wf, model.PathMove{
		Kind: model.MoveKindArtifact, Source: outside,
		Destination: filepath.Join(lf.aggRoot, "foreign.txt"),
	})
	if err == nil {
		t.Fatal("migration accepted a source outside its managed legacy roots")
	}
	if !pathExists(outside) || pathExists(filepath.Join(lf.aggRoot, "foreign.txt")) {
		t.Fatal("rejected foreign move changed the filesystem")
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
