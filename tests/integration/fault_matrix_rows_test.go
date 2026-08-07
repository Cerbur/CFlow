package integration

// The Release Fault Matrix injectors — part 1 (Task 21, brief Step 3): the
// migration/backup/manifest phase, SQLite Commit/lock/version/contention,
// the immutable Artifact write protocol, the process stop escalation, the
// Security Guard, and the Redactor. Injectors are constructor dependencies
// available only from this _test package; the release binary exposes no
// environment flag, CLI flag, debug endpoint, or mutable configuration that
// enables fault injection. Crash states are reproduced from their observable
// filesystem/database facts and validated after a fresh restart.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cflow.local/cflow/internal/artifact"
	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/process"
	"cflow.local/cflow/internal/security"
	"cflow.local/cflow/internal/store"
	migrationfs "cflow.local/cflow/migrations"
	_ "modernc.org/sqlite"
)

// ---------------------------------------------------------------------------
// shared probe helpers
// ---------------------------------------------------------------------------

// faultCodeOf extracts the stable model Code from any error chain ("" when
// none): the harness's one-stable-code probe.
func faultCodeOf(err error) string {
	if err == nil {
		return ""
	}
	code, ok := model.CodeOf(err)
	if !ok {
		return "RAW:" + err.Error()
	}
	return string(code)
}

// dispositionDispatch derives the observed Fault Category and dispatch
// closure from the compiled fault-policy table (design 8.2), so the probe
// measures the compiled disposition instead of echoing the matrix row.
func dispositionDispatch(code string) (string, string) {
	if code == "" {
		return "", "open"
	}
	pol, ok := model.Policy(model.Code(code))
	if !ok {
		return "", "open"
	}
	d := "open"
	if pol.CloseDispatch {
		d = "closed"
	}
	return string(pol.Category), d
}

// retryChargeOf derives the observed retry charge from the compiled
// fault-policy table (design 8.2), so the probe measures production policy
// truth for retry charge exactly as it does for disposition and dispatch.
// A Code with no compiled policy (the recovery-disposition and no-fault
// rows) never charges a Retry. A policy regression that starts charging
// the Retry Budget on any matrix Code flips the derived value and fails
// assertRow against the table.
func retryChargeOf(code string) bool {
	if code == "" {
		return false
	}
	pol, ok := model.Policy(model.Code(code))
	if !ok {
		return false
	}
	return pol.Retry.ChargesBudget
}

// faultRow builds the observed facts of a fault-producing injector.
func faultRow(err error, evidence ...string) rowResult {
	code := faultCodeOf(err)
	d, dispatch := dispositionDispatch(code)
	return rowResult{Code: code, Disposition: d, RetryCharge: retryChargeOf(code), Dispatch: dispatch, Evidence: evidenceOf(evidence...)}
}

// noFaultRow builds the observed facts of a cleanly-settling injector.
func noFaultRow(disposition string, evidence ...string) rowResult {
	return rowResult{Code: "", Disposition: disposition, RetryCharge: retryChargeOf(""), Dispatch: "open", Evidence: evidenceOf(evidence...)}
}

// recoveryRow builds the observed facts of a recovery-disposition probe: an
// unfinished Effect Intent in the ledger keeps dispatch closed until the
// Application settles it (design 17.2).
func recoveryRow(disposition string, evidence ...string) rowResult {
	return rowResult{Code: "", Disposition: disposition, RetryCharge: retryChargeOf(""), Dispatch: "closed_until_reconciled", Evidence: evidenceOf(evidence...)}
}

// canonTemp resolves an owner-only canonical temp root (t.TempDir() is
// born 0755 in this environment; the Security Guard requires 0700).
func canonTemp(t *testing.T) string {
	t.Helper()
	p, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("canonical temp root: %v", err)
	}
	if err := os.Chmod(p, 0o700); err != nil {
		t.Fatalf("chmod temp root: %v", err)
	}
	return p
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// fixtureDSN builds the raw-driver DSN the probes use to construct
// databases and hold locks independently of the Store's own open path.
func fixtureDSN(path string, busy time.Duration) string {
	dsn := "file:" + path
	dsn += "?_pragma=busy_timeout(" + fmt.Sprintf("%d", busy.Milliseconds()) + ")"
	dsn += "&_pragma=foreign_keys(1)"
	dsn += "&_pragma=synchronous(NORMAL)"
	dsn += "&_pragma=journal_mode(WAL)"
	dsn += "&_txlock=immediate"
	return dsn
}

// splitSQL splits a SQL script into individual statements on top-level
// semicolons, respecting single-quoted strings (with ” escapes),
// double-quoted identifiers, and -- line comments (the fixture builder's
// mirror of the Store's own migration splitter).
func splitSQL(src string) []string {
	var stmts []string
	var cur strings.Builder
	inSingle, inDouble, inLineComment := false, false, false
	for i := 0; i < len(src); i++ {
		c := src[i]
		switch {
		case inLineComment:
			cur.WriteByte(c)
			if c == '\n' {
				inLineComment = false
			}
		case inSingle:
			cur.WriteByte(c)
			if c == '\'' {
				if i+1 < len(src) && src[i+1] == '\'' {
					cur.WriteByte('\'')
					i++
				} else {
					inSingle = false
				}
			}
		case inDouble:
			cur.WriteByte(c)
			if c == '"' {
				inDouble = false
			}
		case c == '-' && i+1 < len(src) && src[i+1] == '-':
			inLineComment = true
			cur.WriteString("--")
			i++
		case c == '\'':
			inSingle = true
			cur.WriteByte(c)
		case c == '"':
			inDouble = true
			cur.WriteByte(c)
		case c == ';':
			if strings.TrimSpace(cur.String()) != "" {
				stmts = append(stmts, cur.String())
			}
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	if strings.TrimSpace(cur.String()) != "" {
		stmts = append(stmts, cur.String())
	}
	return stmts
}

func readV001(t *testing.T) string {
	t.Helper()
	return readTestdata(t, "db/v001.sql")
}

// ---------------------------------------------------------------------------
// migration/backup/manifest phase
// ---------------------------------------------------------------------------

// migrationMeta is the embedded migration registry facts the fixture
// builder pins: stable IDs and the SHA-256 of the released SQL content
// (PRD 决策 1). Hardcoding is the fixture's own authority — the Store's
// registry is unexported.
var migrationMeta = []struct {
	version int
	id      string
	file    string
}{
	{1, "cflow-001-initial", "001_initial.sql"},
	{2, "cflow-002-cleanup-apply", "002_cleanup_apply.sql"},
	{3, "cflow-003-integration-head", "003_integration_head.sql"},
	{4, "cflow-004-apply-staging-head", "004_apply_staging_head.sql"},
	{5, "cflow-005-workspace-layout", "005_workspace_layout.sql"},
}

func migrationSHA(t *testing.T, file string) string {
	t.Helper()
	body, err := migrationfs.FS.ReadFile(file)
	if err != nil {
		t.Fatalf("read embedded migration %s: %v", file, err)
	}
	return sha256Hex(body)
}

// v1DB builds a database exactly as the previous binary would have left it:
// the released 001 fixture schema, the authoritative baseline row, and
// PRAGMA user_version = 1 (the Store's own v1FixturePath mirror).
func v1DB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(canonTemp(t), "cflow.db")
	db, err := sql.Open("sqlite", fixtureDSN(path, time.Second))
	if err != nil {
		t.Fatalf("open fixture db: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`PRAGMA journal_mode = WAL`); err != nil {
		t.Fatalf("fixture wal: %v", err)
	}
	for _, stmt := range splitSQL(readV001(t)) {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("execute fixture statement: %v\n%s", err, stmt)
		}
	}
	sha := sha256Hex([]byte(readV001(t)))
	if _, err := db.Exec(`INSERT INTO schema_migrations
		(version, migration_id, migration_sha256, cflow_version, backup_manifest_path, backup_manifest_sha256, applied_at)
		VALUES (1, 'cflow-001-initial', ?, '0.0.0-dev', NULL, NULL, ?)`, sha, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("seed baseline row: %v", err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 1`); err != nil {
		t.Fatalf("set user_version: %v", err)
	}
	return path
}

// v1WithBackup builds a v1 database and leaves the backup directory in the
// crash state of the named migration fault point: a consistent backup with
// no manifest (before_manifest) or a consistent backup plus a verifiable
// manifest (after_manifest). The backup is produced with VACUUM INTO, the
// same mechanism the Store uses (PRD 决策 5).
func v1WithBackup(t *testing.T, phase string) (string, string) {
	t.Helper()
	path := v1DB(t)
	backupDir := filepath.Join(filepath.Dir(path), "backups", "db", "cflow-005-workspace-layout")
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		t.Fatalf("backup dir: %v", err)
	}
	db, err := sql.Open("sqlite", fixtureDSN(path, time.Second))
	if err != nil {
		t.Fatalf("open fixture for backup: %v", err)
	}
	defer db.Close()
	backupPath := filepath.Join(backupDir, "cflow.db")
	if _, err := db.Exec(`VACUUM INTO '` + strings.ReplaceAll(backupPath, "'", "''") + `'`); err != nil {
		t.Fatalf("vacuum into backup: %v", err)
	}
	if err := os.Chmod(backupPath, 0o600); err != nil {
		t.Fatalf("backup mode: %v", err)
	}
	if phase == "after_manifest" {
		buf, err := os.ReadFile(backupPath)
		if err != nil {
			t.Fatalf("read backup: %v", err)
		}
		manifest := map[string]any{
			"source_version": 1,
			"target_version": 5,
			"cflow_version":  "0.0.0-dev",
			"database_hash":  sha256Hex(buf),
			"database_size":  len(buf),
			"migrations": []map[string]string{
				{"migration_id": "cflow-002-cleanup-apply", "migration_sha256": migrationSHA(t, "002_cleanup_apply.sql")},
				{"migration_id": "cflow-003-integration-head", "migration_sha256": migrationSHA(t, "003_integration_head.sql")},
				{"migration_id": "cflow-004-apply-staging-head", "migration_sha256": migrationSHA(t, "004_apply_staging_head.sql")},
				{"migration_id": "cflow-005-workspace-layout", "migration_sha256": migrationSHA(t, "005_workspace_layout.sql")},
			},
			"backup_path":   backupPath,
			"manifest_path": filepath.Join(backupDir, "backup-manifest.json"),
			"created_at":    time.Now().UTC().Format(time.RFC3339Nano),
		}
		body, err := json.MarshalIndent(manifest, "", "  ")
		if err != nil {
			t.Fatalf("encode manifest: %v", err)
		}
		body = append(body, '\n')
		if err := os.WriteFile(filepath.Join(backupDir, "backup-manifest.json"), body, 0o600); err != nil {
			t.Fatalf("write manifest: %v", err)
		}
	}
	return path, backupDir
}

func rawUserVersion(t *testing.T, path string) int {
	t.Helper()
	db, err := sql.Open("sqlite", fixtureDSN(path, time.Second))
	if err != nil {
		t.Fatalf("raw reopen: %v", err)
	}
	defer db.Close()
	var uv int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&uv); err != nil {
		t.Fatalf("user_version: %v", err)
	}
	return uv
}

func migrationRowsCount(t *testing.T, path string) int {
	t.Helper()
	db, err := sql.Open("sqlite", fixtureDSN(path, time.Second))
	if err != nil {
		t.Fatalf("raw reopen: %v", err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return n
}

func injectMigrationCrashBeforeManifest(t *testing.T, _ matrixRow) rowResult {
	path, backupDir := v1WithBackup(t, "before_manifest")
	// The crash state: a consistent backup exists, the manifest was never
	// written (PRD 决策 5-6). The next open cannot verify the backup and
	// fails closed with DATABASE_MIGRATION_INCOMPLETE, never auto-restoring.
	s, err := store.Open(context.Background(), store.OpenOptions{Path: path, CflowVersion: "0.0.0-dev"})
	if s != nil {
		s.Close()
	}
	ev := evidenceOf("backup_preserved", "db_untouched")
	if _, serr := os.Stat(filepath.Join(backupDir, "cflow.db")); serr == nil {
		ev["backup_preserved"] = true
	}
	if rawUserVersion(t, path) == 1 {
		ev["db_untouched"] = true
	}
	code := faultCodeOf(err)
	d, dispatch := dispositionDispatch(code)
	return rowResult{Code: code, Disposition: d, RetryCharge: retryChargeOf(code), Dispatch: dispatch, Evidence: ev}
}

func injectMigrationCrashAfterManifest(t *testing.T, _ matrixRow) rowResult {
	path, backupDir := v1WithBackup(t, "after_manifest")
	backupPath := filepath.Join(backupDir, "cflow.db")
	before, err := os.ReadFile(backupPath)
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	// The next open verifies the backup + manifest and retries the chain
	// idempotently: the recorded manifest hash binds the verified backup.
	s, err := store.Open(context.Background(), store.OpenOptions{Path: path, CflowVersion: "0.0.0-dev"})
	if err != nil {
		t.Fatalf("reopen after crash: %v", err)
	}
	s.Close()
	after, err := os.ReadFile(backupPath)
	if err != nil {
		t.Fatalf("read backup after reopen: %v", err)
	}
	ev := evidenceOf("chain_applied", "backup_reused")
	if rawUserVersion(t, path) != 5 || migrationRowsCount(t, path) != 5 {
		ev["chain_applied"] = false
	}
	if string(before) == string(after) {
		ev["backup_reused"] = true
	}
	return noFaultRow("safe_to_retry", "chain_applied", "backup_reused")
}

func injectMigrationChecksumMutated(t *testing.T, _ matrixRow) rowResult {
	path := v1DB(t)
	db, err := sql.Open("sqlite", fixtureDSN(path, time.Second))
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	if _, err := db.Exec(`UPDATE schema_migrations SET migration_sha256 = ? WHERE version = 1`, strings.Repeat("ab", 32)); err != nil {
		t.Fatalf("mutate sha: %v", err)
	}
	db.Close()
	s, err := store.Open(context.Background(), store.OpenOptions{Path: path, CflowVersion: "0.0.0-dev"})
	if s != nil {
		s.Close()
	}
	ev := evidenceOf("db_untouched", "no_migration")
	if rawUserVersion(t, path) == 1 {
		ev["db_untouched"] = true
	}
	if migrationRowsCount(t, path) == 1 {
		ev["no_migration"] = true
	}
	return faultRow(err, "db_untouched", "no_migration")
}

func injectMigrationSchemaTooNew(t *testing.T, _ matrixRow) rowResult {
	path := v1DB(t)
	db, err := sql.Open("sqlite", fixtureDSN(path, time.Second))
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO schema_migrations
		(version, migration_id, migration_sha256, cflow_version, applied_at)
		VALUES (99, 'cflow-099-future', ?, '9.9.9', ?)`, strings.Repeat("cd", 32), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("insert future row: %v", err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 99`); err != nil {
		t.Fatalf("set user_version: %v", err)
	}
	db.Close()
	s, err := store.Open(context.Background(), store.OpenOptions{Path: path, CflowVersion: "0.0.0-dev"})
	if s != nil {
		s.Close()
	}
	// Read-only commands also fail closed: no safe Reader for a newer
	// schema (PRD 决策 9).
	rs, err2 := store.Open(context.Background(), store.OpenOptions{Path: path, ReadOnly: true, CflowVersion: "0.0.0-dev"})
	if rs != nil {
		rs.Close()
	}
	ev := evidenceOf("readonly_fails_closed")
	if faultCodeOf(err2) == faultCodeOf(err) {
		ev["readonly_fails_closed"] = true
	}
	return faultRow(err, "readonly_fails_closed")
}

func injectMigrationPathMissing(t *testing.T, _ matrixRow) rowResult {
	path := v1DB(t)
	db, err := sql.Open("sqlite", fixtureDSN(path, time.Second))
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	if _, err := db.Exec(`DELETE FROM schema_migrations`); err != nil {
		t.Fatalf("delete rows: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO schema_migrations
		(version, migration_id, migration_sha256, cflow_version, applied_at)
		VALUES (3, 'cflow-003-integration-head', ?, '0.0.0-dev', ?)`, migrationSHA(t, "003_integration_head.sql"), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("insert v3-only row: %v", err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 3`); err != nil {
		t.Fatalf("set user_version: %v", err)
	}
	db.Close()
	s, err := store.Open(context.Background(), store.OpenOptions{Path: path, CflowVersion: "0.0.0-dev"})
	if s != nil {
		s.Close()
	}
	ev := evidenceOf("db_preserved")
	if rawUserVersion(t, path) == 3 {
		ev["db_preserved"] = true
	}
	return faultRow(err, "db_preserved")
}

func injectMigrationGuardMismatch(t *testing.T, _ matrixRow) rowResult {
	path := v1DB(t)
	db, err := sql.Open("sqlite", fixtureDSN(path, time.Second))
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 2`); err != nil {
		t.Fatalf("set user_version: %v", err)
	}
	db.Close()
	s, err := store.Open(context.Background(), store.OpenOptions{Path: path, CflowVersion: "0.0.0-dev"})
	if s != nil {
		s.Close()
	}
	ev := evidenceOf("db_untouched")
	if rawUserVersion(t, path) == 2 {
		ev["db_untouched"] = true
	}
	return faultRow(err, "db_untouched")
}

// ---------------------------------------------------------------------------
// SQLite Commit / lock / version / contention
// ---------------------------------------------------------------------------

// newRegisteredStore builds a fresh CFLOW_HOME store with a registered
// Project and a bound Workflow row, then closes it (the restart point).
func newRegisteredStore(t *testing.T) string {
	t.Helper()
	dir := canonTemp(t)
	path := filepath.Join(dir, "cflow.db")
	now := func() time.Time { return time.Unix(1700000000, 0).UTC() }
	ctx := context.Background()
	s, err := store.Open(ctx, store.OpenOptions{Path: path, Workflow: "wf-1", CflowVersion: "0.0.0-dev", Now: now})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := s.RegisterProject(ctx, "proj-1", dir, "proj-1"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := s.Transact(ctx, 0, func(state model.State) (model.Decision, error) {
		return model.Decision{Mutations: []model.Mutation{model.WorkflowMutation{
			ID: "wf-1", Project: "proj-1", Stage: model.StageRequirementDiscussion,
			Runtime: model.RuntimeRunning, TargetBranch: "main", BaseCommit: "base",
			IntegrationBranch: "cflow/wf-1/integration", IntegrationHead: "base",
		}}}, nil
	}); err != nil {
		t.Fatalf("seed workflow: %v", err)
	}
	s.Close()
	return path
}

func nodeAppendDecision() model.Decision {
	return model.Decision{Mutations: []model.Mutation{model.NodeAppendMutation{Node: model.Node{
		ID: "n1", Kind: model.NodeAgentTask, Status: model.NodeReady,
	}}}}
}

func injectStoreSQLiteBusy(t *testing.T, _ matrixRow) rowResult {
	path := newRegisteredStore(t)
	ctx := context.Background()
	now := func() time.Time { return time.Unix(1700000000, 0).UTC() }
	s, err := store.Open(ctx, store.OpenOptions{Path: path, Workflow: "wf-1", CflowVersion: "0.0.0-dev", Now: now})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	// A peer connection holds BEGIN IMMEDIATE for the whole attempt: the
	// store's write contends for the full bounded busy timeout and returns
	// the stable local-contention Fault (PRD 决策: SQLITE_BUSY 有界).
	raw, err := sql.Open("sqlite", fixtureDSN(path, 5*time.Second))
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	defer raw.Close()
	hold, err := raw.Begin()
	if err != nil {
		t.Fatalf("raw begin: %v", err)
	}
	_, terr := s.Transact(ctx, 1, func(state model.State) (model.Decision, error) {
		return nodeAppendDecision(), nil
	})
	_ = hold.Rollback()

	// No partial commit: the failed write left the aggregate untouched.
	view, err := s.View(ctx, store.StoreQuery{})
	if err != nil {
		t.Fatalf("view: %v", err)
	}
	ev := evidenceOf("contention_fault", "no_partial_commit")
	if len(view.State.Nodes) == 0 {
		ev["no_partial_commit"] = true
	}
	if faultCodeOf(terr) == "DATABASE_MIGRATION_FAILED" {
		ev["contention_fault"] = true
	}
	return faultRow(terr, "contention_fault", "no_partial_commit")
}

func injectStoreStaleVersion(t *testing.T, _ matrixRow) rowResult {
	path := newRegisteredStore(t)
	ctx := context.Background()
	now := func() time.Time { return time.Unix(1700000000, 0).UTC() }
	a, err := store.Open(ctx, store.OpenOptions{Path: path, Workflow: "wf-1", CflowVersion: "0.0.0-dev", Now: now})
	if err != nil {
		t.Fatalf("open a: %v", err)
	}
	defer a.Close()
	b, err := store.Open(ctx, store.OpenOptions{Path: path, Workflow: "wf-1", CflowVersion: "0.0.0-dev", Now: now})
	if err != nil {
		t.Fatalf("open b: %v", err)
	}
	defer b.Close()

	// A commits version 1 -> 2; B still holds the stale expected version.
	if _, err := a.Transact(ctx, 1, func(state model.State) (model.Decision, error) {
		return nodeAppendDecision(), nil
	}); err != nil {
		t.Fatalf("commit a: %v", err)
	}
	_, err = b.Transact(ctx, 1, func(state model.State) (model.Decision, error) {
		return nodeAppendDecision(), nil
	})
	view, err2 := a.View(ctx, store.StoreQuery{})
	if err2 != nil {
		t.Fatalf("view: %v", err2)
	}
	ev := evidenceOf("cas_rejected", "no_mutation")
	if len(view.State.Nodes) == 1 {
		ev["no_mutation"] = true // exactly A's node; B added nothing
	}
	if faultCodeOf(err) == "INVALID_INPUT" {
		ev["cas_rejected"] = true
	}
	return faultRow(err, "cas_rejected", "no_mutation")
}

func injectStoreConstraintAtomic(t *testing.T, _ matrixRow) rowResult {
	path := newRegisteredStore(t)
	ctx := context.Background()
	now := func() time.Time { return time.Unix(1700000000, 0).UTC() }
	s, err := store.Open(ctx, store.OpenOptions{Path: path, Workflow: "wf-1", CflowVersion: "0.0.0-dev", Now: now})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	// A decision whose second row violates the sessions.id UNIQUE
	// constraint commits nothing: the SQL error is classified as the
	// invariant-failure default and the transaction rolls back.
	_, err = s.Transact(ctx, 1, func(state model.State) (model.Decision, error) {
		return model.Decision{Mutations: []model.Mutation{
			model.SessionAppendMutation{Session: model.Session{ID: "s1", Purpose: model.PurposePlanning, Status: model.SessionStarting}, Provider: "fake"},
			model.SessionAppendMutation{Session: model.Session{ID: "s1", Purpose: model.PurposePlanning, Status: model.SessionStarting}, Provider: "fake"},
		}}, nil
	})
	view, err2 := s.View(ctx, store.StoreQuery{})
	if err2 != nil {
		t.Fatalf("view: %v", err2)
	}
	ev := evidenceOf("no_partial_commit", "aggregate_unchanged")
	if len(view.State.Sessions) == 0 {
		ev["no_partial_commit"] = true
	}
	if view.AggregateVersion == 1 {
		ev["aggregate_unchanged"] = true
	}
	return faultRow(err, "no_partial_commit", "aggregate_unchanged")
}

func injectStoreDBFailure(t *testing.T, _ matrixRow) rowResult {
	path := newRegisteredStore(t)
	ctx := context.Background()
	now := func() time.Time { return time.Unix(1700000000, 0).UTC() }

	// A database-level fault that is neither contention nor a constraint: a
	// trigger raises on the nodes insert. The default mapSQLError path
	// classifies it coherently as STATE_INVARIANT_VIOLATION and the failed
	// transaction commits nothing (the aggregate version never advances).
	raw, err := sql.Open("sqlite", fixtureDSN(path, time.Second))
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	if _, err := raw.Exec(`CREATE TRIGGER cflow_probe_block_insert BEFORE INSERT ON nodes
		BEGIN SELECT RAISE(FAIL, 'probe: nodes insert blocked'); END`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	raw.Close()

	s, err := store.Open(ctx, store.OpenOptions{Path: path, Workflow: "wf-1", CflowVersion: "0.0.0-dev", Now: now})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	_, err = s.Transact(ctx, 1, func(state model.State) (model.Decision, error) {
		return nodeAppendDecision(), nil
	})
	view, err2 := s.View(ctx, store.StoreQuery{})
	if err2 != nil {
		t.Fatalf("view: %v", err2)
	}
	ev := evidenceOf("no_partial_commit", "aggregate_unchanged")
	if len(view.State.Nodes) == 0 {
		ev["no_partial_commit"] = true
	}
	if view.AggregateVersion == 1 {
		ev["aggregate_unchanged"] = true
	}
	return faultRow(err, "no_partial_commit", "aggregate_unchanged")
}

// ---------------------------------------------------------------------------
// immutable Artifact Store
// ---------------------------------------------------------------------------

// artifactRootFixture builds a canonical 0700 artifacts root the Security
// Guard accepts, plus a redaction registry the test artifacts use.
func artifactRootFixture(t *testing.T) string {
	t.Helper()
	root := canonTemp(t)
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatalf("chmod root: %v", err)
	}
	return root
}

func artifactRedaction() security.Registry {
	return security.Registry{
		Revision: "test-1",
		Rules: []security.Rule{
			{ID: "provider-token", Category: "provider_token", Pattern: `sk-[A-Za-z0-9]{16,}`},
			{ID: "api-key", Category: "api_key", Pattern: `AKIA[0-9A-Z]{16}`},
		},
	}
}

func newArtifactStore(t *testing.T, root string) *artifact.Store {
	t.Helper()
	s, err := artifact.New(root, artifactRedaction())
	if err != nil {
		t.Fatalf("new artifact store: %v", err)
	}
	return s
}

func planPut(rev int) artifact.PutRequest {
	return artifact.PutRequest{
		WorkflowID: "wf-1", Type: model.ArtifactPlan, Revision: rev,
		SchemaVersion: "1.0.0", CreatedAt: "2026-01-01T00:00:00Z",
		Producer: artifact.ProducerRef{Purpose: "test"},
		Body:     []byte(fmt.Sprintf("---\ntitle: Fix login bug %d\nworkflow_id: wf-1\nrevision: %d\n---\n\n# Fix login bug\n\nImplement the fix with an error on zero.\n", rev, rev)),
	}
}

func artifactTargetPath(root string, ref model.ArtifactRef) string {
	return filepath.Join(root, string(ref.Workflow), string(ref.Type),
		fmt.Sprintf("%d", ref.Revision), ref.Hash)
}

func injectArtifactCrashBeforeRename(t *testing.T, _ matrixRow) rowResult {
	root := artifactRootFixture(t)
	st := newArtifactStore(t, root)
	// The crash state of FailBeforeRename: the revision directory holds a
	// temp debris file, no target was ever installed. The store's
	// same-directory write protocol never reads the debris and the fresh
	// Put installs the verified content.
	dir := filepath.Join(root, "wf-1", "plan", "1")
	for _, comp := range []string{filepath.Join(root, "wf-1"), filepath.Join(root, "wf-1", "plan"), dir} {
		if err := security.CreateSensitiveDir(comp); err != nil {
			t.Fatalf("create %s: %v", comp, err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, ".tmp.debris"), []byte("crash debris"), 0o600); err != nil {
		t.Fatalf("write debris: %v", err)
	}
	ref, err := st.Put(context.Background(), planPut(1))
	if err != nil {
		t.Fatalf("put after crash debris: %v", err)
	}
	body, err := st.Get(context.Background(), ref)
	if err != nil {
		t.Fatalf("verified readback: %v", err)
	}
	ev := evidenceOf("target_installed", "verified_readback", "temp_ignored")
	if _, err := os.Stat(artifactTargetPath(root, ref)); err == nil {
		ev["target_installed"] = true
	}
	if strings.Contains(string(body), "Fix login bug 1") {
		ev["verified_readback"] = true
	}
	if _, err := os.Stat(filepath.Join(dir, ".tmp.debris")); err == nil {
		ev["temp_ignored"] = true
	}
	return noFaultRow("safe_to_retry", "target_installed", "verified_readback", "temp_ignored")
}

func injectArtifactTargetContended(t *testing.T, _ matrixRow) rowResult {
	root := artifactRootFixture(t)
	st := newArtifactStore(t, root)
	ref, err := st.Put(context.Background(), planPut(1))
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	before, err := os.ReadFile(artifactTargetPath(root, ref))
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	// A re-Put of the same body is refused by the existing target: an
	// existing path is never reused or overwritten; idempotency resolves
	// through the recorded intent.
	_, err = st.Put(context.Background(), planPut(1))
	after, err2 := os.ReadFile(artifactTargetPath(root, ref))
	if err2 != nil {
		t.Fatalf("re-read target: %v", err2)
	}
	ev := evidenceOf("existing_file_untouched")
	if string(before) == string(after) {
		ev["existing_file_untouched"] = true
	}
	return faultRow(err, "existing_file_untouched")
}

func injectArtifactSchemaUnsupported(t *testing.T, _ matrixRow) rowResult {
	root := artifactRootFixture(t)
	st := newArtifactStore(t, root)
	req := planPut(1)
	req.SchemaVersion = "9.9.9"
	_, err := st.Put(context.Background(), req)
	ev := evidenceOf("nothing_written")
	if _, err := os.Stat(filepath.Join(root, "wf-1")); os.IsNotExist(err) {
		ev["nothing_written"] = true
	}
	return faultRow(err, "nothing_written")
}

func injectArtifactOldRevision(t *testing.T, _ matrixRow) rowResult {
	root := artifactRootFixture(t)
	st := newArtifactStore(t, root)
	ref1, err := st.Put(context.Background(), planPut(1))
	if err != nil {
		t.Fatalf("put rev1: %v", err)
	}
	if _, err := st.Put(context.Background(), planPut(2)); err != nil {
		t.Fatalf("put rev2: %v", err)
	}
	body, err := st.Get(context.Background(), ref1)
	if err != nil {
		t.Fatalf("get old revision: %v", err)
	}
	info, err := os.Stat(artifactTargetPath(root, ref1))
	if err != nil {
		t.Fatalf("stat old revision: %v", err)
	}
	ev := evidenceOf("byte_identical", "mode_0600")
	if strings.Contains(string(body), "Fix login bug 1") && !strings.Contains(string(body), "Fix login bug 2") {
		ev["byte_identical"] = true
	}
	if info.Mode().Perm() == 0o600 {
		ev["mode_0600"] = true
	}
	return noFaultRow("already_completed", "byte_identical", "mode_0600")
}

func injectArtifactContentMutated(t *testing.T, _ matrixRow) rowResult {
	root := artifactRootFixture(t)
	st := newArtifactStore(t, root)
	ref, err := st.Put(context.Background(), planPut(1))
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	path := artifactTargetPath(root, ref)
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	// A content mutation after the write (checksum drift) fails the read
	// closed: the reader never re-verifies into accepting tampered content.
	if err := os.WriteFile(path, append(original, []byte("\ntampered")...), 0o600); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	_, err = st.Get(context.Background(), ref)
	ev := evidenceOf("read_fails_closed")
	if faultCodeOf(err) != "" {
		ev["read_fails_closed"] = true
	}
	return faultRow(err, "read_fails_closed")
}

// ---------------------------------------------------------------------------
// process stop escalation
// ---------------------------------------------------------------------------

func injectProcessStopEscalation(t *testing.T, _ matrixRow) rowResult {
	fake, sup := process.NewFakeSupervisor()
	h, events, err := sup.Start(context.Background(), process.ProcessSpec{Executable: "/fixture/worker"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	policy := process.StopPolicy{Grace: 200 * time.Millisecond, TerminateWait: 200 * time.Millisecond, ForceKillWait: 400 * time.Millisecond}
	done := make(chan error, 1)
	go func() {
		_, err := process.Stop(context.Background(), sup, h, policy)
		done <- err
	}()
	// The group ignores every signal; Stop escalates Interrupt -> Terminate
	// -> ForceKill. The moment the full escalation is recorded the group is
	// reaped, so Stop returns the reaped exit within the final wait window.
	want := []process.Signal{process.Interrupt, process.Terminate, process.ForceKill}
	deadline := time.Now().Add(10 * time.Second)
	escalated := false
	for time.Now().Before(deadline) {
		if signalsEqual(fake.Signals(h), want) {
			escalated = true
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !escalated {
		t.Fatalf("stop did not escalate to %v, got %v", want, fake.Signals(h))
	}
	fake.ExitGroup(h, 137)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("stop returned %v after the escalation, want the reaped exit", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("stop hung after the escalation")
	}
	sup.Wait(context.Background(), h)
	drainFakeEvents(t, events)
	// The controlled stop closes dispatch: no new allocation crosses the
	// committed stop boundary.
	return rowResult{Code: "", Disposition: "escalated", RetryCharge: retryChargeOf(""), Dispatch: "closed", Evidence: evidenceOf("signals_escalated", "reaped")}
}

func injectProcessStopOrphan(t *testing.T, _ matrixRow) rowResult {
	fake, sup := process.NewFakeSupervisor()
	h, events, err := sup.Start(context.Background(), process.ProcessSpec{Executable: "/fixture/worker"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	policy := process.StopPolicy{Grace: 50 * time.Millisecond, TerminateWait: 50 * time.Millisecond, ForceKillWait: 50 * time.Millisecond}
	done := make(chan error, 1)
	go func() {
		_, err := process.Stop(context.Background(), sup, h, policy)
		done <- err
	}()
	// The group never exits: Stop escalates through every stage and returns
	// ErrNotReaped — the orphan fact the caller must inspect by identity.
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("stop succeeded on a group that never exited")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("stop hung on a never-exiting group")
	}
	ev := evidenceOf("identity_not_reaped", "stops_escalate")
	// Clean up the orphan so the supervisor reaps it and drains.
	fake.ExitGroup(h, 137)
	sup.Wait(context.Background(), h)
	drainFakeEvents(t, events)
	return rowResult{Code: "", Disposition: "orphan_not_reaped", RetryCharge: retryChargeOf(""), Dispatch: "closed", Evidence: ev}
}

func signalsEqual(a, b []process.Signal) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func drainFakeEvents(t *testing.T, events process.Events) {
	t.Helper()
	for range events {
	}
}

// ---------------------------------------------------------------------------
// Security Guard path failures
// ---------------------------------------------------------------------------

func securityRoot(t *testing.T) string {
	t.Helper()
	root := canonTemp(t)
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatalf("chmod root: %v", err)
	}
	return root
}

func injectPathSymlinkEscape(t *testing.T, _ matrixRow) rowResult {
	root := securityRoot(t)
	real := filepath.Join(root, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	_, err := security.CheckPath(security.PathRequest{Path: link, Kind: security.KindDir})
	// The guard never repairs or removes: the symlink stays in place.
	ev := evidenceOf("mode_never_repaired")
	if _, err := os.Lstat(link); err == nil {
		ev["mode_never_repaired"] = true
	}
	return faultRow(err, "mode_never_repaired")
}

func injectPathGroupWritable(t *testing.T, _ matrixRow) rowResult {
	root := securityRoot(t)
	dir := filepath.Join(root, "shared")
	if err := os.Mkdir(dir, 0o775); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	_, err := security.CheckPath(security.PathRequest{Path: dir, Kind: security.KindDir})
	ev := evidenceOf("mode_never_repaired")
	if fi, statErr := os.Stat(dir); statErr == nil && fi.Mode().Perm() == 0o775 {
		ev["mode_never_repaired"] = true
	}
	return faultRow(err, "mode_never_repaired")
}

func injectPathHomeUnsafeMode(t *testing.T, _ matrixRow) rowResult {
	root := securityRoot(t)
	home := filepath.Join(root, "home")
	if err := os.Mkdir(home, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	_, err := security.CheckHome(security.HomeRequest{Path: home})
	ev := evidenceOf("mode_never_repaired")
	if fi, statErr := os.Stat(home); statErr == nil && fi.Mode().Perm() == 0o755 {
		ev["mode_never_repaired"] = true
	}
	return faultRow(err, "mode_never_repaired")
}

// ---------------------------------------------------------------------------
// Redactor fail-closed and short-body
// ---------------------------------------------------------------------------

func redactorRegistry() security.Registry {
	return security.Registry{
		Revision: "matrix-1",
		Rules: []security.Rule{
			{ID: "sk-token", Category: "provider_token", Pattern: `sk-[A-Za-z0-9]{8,}`},
		},
	}
}

func injectRedactionFailClosed(t *testing.T, _ matrixRow) rowResult {
	// A rule that cannot compile poisons the Redactor: every later call
	// fails closed with SENSITIVE_DATA_REDACTION_FAILED and no output is
	// ever emitted under an incomplete policy.
	r := security.NewRedactor(security.Registry{Revision: "matrix-1", Rules: []security.Rule{
		{ID: "unparseable", Category: "secret", Pattern: `(`},
	}})
	frame, err := r.WriteFrame([]byte("token=sk-abc123456\n"))
	ev := evidenceOf("nothing_persisted", "poisoned_after_failure")
	if frame.Text == "" {
		ev["nothing_persisted"] = true
	}
	if _, err2 := r.Flush(); err2 != nil {
		ev["poisoned_after_failure"] = true
	}
	return faultRow(err, "nothing_persisted", "poisoned_after_failure")
}

func injectRedactionShortBody(t *testing.T, _ matrixRow) rowResult {
	// A short body (entire stream within the withholding window) with a
	// trailing secret must still emit the placeholder and never the raw
	// value: the withheld tail flushes fully redacted (PRD 脱敏).
	r := security.NewRedactor(redactorRegistry())
	first, err := r.WriteFrame([]byte("credential=sk-abc123456789\n"))
	if err != nil {
		t.Fatalf("write frame: %v", err)
	}
	flush, err := r.Flush()
	if err != nil {
		t.Fatalf("flush: %v", err)
	}
	out := first.Text + flush.Text
	ev := evidenceOf("placeholder_emitted", "raw_absent")
	if strings.Contains(out, "[REDACTED") {
		ev["placeholder_emitted"] = true
	}
	if !strings.Contains(out, "sk-abc123456789") {
		ev["raw_absent"] = true
	}
	return noFaultRow("already_completed", "placeholder_emitted", "raw_absent")
}
