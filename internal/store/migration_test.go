package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"cflow.local/cflow/internal/model"

	// The raw driver builds the v1 fixture database independently of the
	// Store's own Open path.
	_ "modernc.org/sqlite"
)

// ---------------------------------------------------------------------------
// fixtures
// ---------------------------------------------------------------------------

// v1FixturePath builds a database that represents a v1 database created by
// the previous binary: it executes tests/testdata/db/v001.sql with the raw
// driver, records the authoritative baseline row (PRD 最低数据库记录), and
// pins PRAGMA user_version as the consistency guard.
func v1FixturePath(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "cflow.db")
	db, err := sql.Open("sqlite", fileDSN(path, false, time.Second))
	if err != nil {
		t.Fatalf("open fixture db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`PRAGMA journal_mode = WAL`); err != nil {
		t.Fatalf("fixture wal: %v", err)
	}
	fixture, err := readV1Fixture()
	if err != nil {
		t.Fatalf("read v001 fixture: %v", err)
	}
	for _, stmt := range splitStatements(fixture) {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("execute fixture statement: %v\n%s", err, stmt)
		}
	}
	sha := sha256Of(fixture)
	if _, err := db.Exec(`INSERT INTO schema_migrations
		(version, migration_id, migration_sha256, cflow_version, backup_manifest_path, backup_manifest_sha256, applied_at)
		VALUES (1, 'cflow-001-initial', ?, '0.0.0-dev', NULL, NULL, ?)`, sha, nowText()); err != nil {
		t.Fatalf("seed baseline row: %v", err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 1`); err != nil {
		t.Fatalf("set user_version: %v", err)
	}
	return path
}

func readV1Fixture() (string, error) {
	// go test runs with the package directory as the working directory.
	body, err := os.ReadFile(filepath.Join("..", "..", "tests", "testdata", "db", "v001.sql"))
	if err != nil {
		return "", fmt.Errorf("read v001.sql fixture: %w", err)
	}
	return string(body), nil
}

func nowText() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

// fixtureWithVersionMutator opens the raw fixture DB, applies mut to the
// schema_migrations rows and user_version, and returns the path.
func fixtureWithVersionMutator(t *testing.T, mut func(tx *sql.Tx, db *sql.DB)) string {
	t.Helper()
	path := v1FixturePath(t)
	db, err := sql.Open("sqlite", fileDSN(path, false, time.Second))
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	mut(tx, db)
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit mutation: %v", err)
	}
	return path
}

func schemaRows(t *testing.T, s *Store) []appliedMigration {
	t.Helper()
	rows, err := s.db.Query(`SELECT version, migration_id, migration_sha256,
		COALESCE(backup_manifest_path, ''), COALESCE(backup_manifest_sha256, '')
		FROM schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatalf("query schema_migrations: %v", err)
	}
	defer rows.Close()
	var out []appliedMigration
	for rows.Next() {
		var a appliedMigration
		if err := rows.Scan(&a.Version, &a.ID, &a.SHA256, &a.ManifestPath, &a.ManifestSHA256); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, a)
	}
	return out
}

func userVersion(t *testing.T, s *Store) int {
	t.Helper()
	var v int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatalf("user_version: %v", err)
	}
	return v
}

func tableNames(t *testing.T, s *Store) []string {
	t.Helper()
	rows, err := s.db.Query(`SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name`)
	if err != nil {
		t.Fatalf("query tables: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, n)
	}
	return out
}

func integrityOK(t *testing.T, s *Store) {
	t.Helper()
	var v string
	if err := s.db.QueryRow(`PRAGMA integrity_check`).Scan(&v); err != nil {
		t.Fatalf("integrity_check: %v", err)
	}
	if v != "ok" {
		t.Fatalf("integrity_check = %q", v)
	}
}

// ---------------------------------------------------------------------------
// the registry itself
// ---------------------------------------------------------------------------

func TestMigrationRegistryCompleteAndImmutable(t *testing.T) {
	reg := migrations()
	if len(reg) < 2 {
		t.Fatalf("registry = %d migrations, want >= 2", len(reg))
	}
	for i, m := range reg {
		if m.Version != i+1 {
			t.Fatalf("migration %d version = %d: versions must be contiguous from 1", i, m.Version)
		}
		if m.ID == "" || m.SHA256 == "" || m.SQL == "" {
			t.Fatalf("migration %d incomplete: %+v", m.Version, m)
		}
		if len(m.SHA256) != 64 {
			t.Fatalf("migration %d sha256 = %q", m.Version, m.SHA256)
		}
	}
	// The v001 fixture must represent exactly the released 001 migration:
	// a database created by the previous binary has this byte-identical
	// schema (migrations are immutable).
	fixture, err := readV1Fixture()
	if err != nil {
		t.Fatalf("read v001 fixture: %v", err)
	}
	if sha256Of(fixture) != reg[0].SHA256 {
		t.Fatalf("tests/testdata/db/v001.sql drifted from migrations/001_initial.sql")
	}
}

// ---------------------------------------------------------------------------
// fresh and forward migration
// ---------------------------------------------------------------------------

func TestMigrationFromFreshDatabase(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(context.Background(), OpenOptions{Path: filepath.Join(dir, "cflow.db"), CflowVersion: "1.2.3"})
	if err != nil {
		t.Fatalf("open fresh: %v", err)
	}
	defer s.Close()
	rows := schemaRows(t, s)
	if len(rows) != 4 {
		t.Fatalf("schema_migrations = %d rows, want 4", len(rows))
	}
	// Baseline rows need no backup: manifest fields stay empty (PRD 决策 5).
	for i, r := range rows {
		if r.ManifestPath != "" || r.ManifestSHA256 != "" {
			t.Fatalf("row %d has backup fields on a fresh baseline: %+v", i, r)
		}
	}
	if rows[0].ID != "cflow-001-initial" || rows[1].ID != "cflow-002-cleanup-apply" ||
		rows[2].ID != "cflow-003-integration-head" || rows[3].ID != "cflow-004-apply-staging-head" {
		t.Fatalf("rows = %+v", rows)
	}
	if got := userVersion(t, s); got != 4 {
		t.Fatalf("user_version = %d, want 4", got)
	}
	integrityOK(t, s)
	// No backup debris on a fresh baseline.
	if entries, err := os.ReadDir(filepath.Join(dir, "backups")); err == nil && len(entries) != 0 {
		t.Fatalf("fresh baseline created backups: %v", entries)
	}
}

func TestMigrationAppliesForwardChainFromV1(t *testing.T) {
	path := v1FixturePath(t)
	s, err := Open(context.Background(), OpenOptions{Path: path, CflowVersion: "2.0.0"})
	if err != nil {
		t.Fatalf("open v1 database: %v", err)
	}
	defer s.Close()
	rows := schemaRows(t, s)
	if len(rows) != 4 {
		t.Fatalf("rows = %d, want 4: %+v", len(rows), rows)
	}
	if got := userVersion(t, s); got != 4 {
		t.Fatalf("user_version = %d, want 4", got)
	}
	// The upgrade row records the verified backup manifest.
	if rows[1].ManifestPath == "" || rows[1].ManifestSHA256 == "" {
		t.Fatalf("upgrade row missing manifest: %+v", rows[1])
	}
	// The v2 capability exists and the v1 tables are untouched.
	names := tableNames(t, s)
	if !contains(names, "cleanup_manifest_bindings") || !contains(names, "cleanup_attempts") {
		t.Fatalf("missing tables after migration: %v", names)
	}

	// The 0600 backup + immutable manifest sit under backups/db/.
	backupDir := filepath.Join(filepath.Dir(path), "backups", "db", "cflow-004-apply-staging-head")
	info, err := os.Stat(backupDir)
	if err != nil {
		t.Fatalf("backup dir: %v", err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("backup dir mode = %o, want 0700", info.Mode().Perm())
	}
	backupPath := filepath.Join(backupDir, "cflow.db")
	manifestPath := filepath.Join(backupDir, "backup-manifest.json")
	for _, p := range []string{backupPath, manifestPath} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatalf("backup file %s: %v", p, err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %o, want 0600", p, fi.Mode().Perm())
		}
	}
	// The manifest pins source/target versions, the migration chain, the
	// database hash/size, the cflow version, and the paths; it must re-read
	// and verify (PRD 决策 5-6).
	manifestBody, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if rows[1].ManifestSHA256 != sha256Of(string(manifestBody)) {
		t.Fatalf("recorded manifest sha %s does not match manifest file", rows[1].ManifestSHA256)
	}
	var mf backupManifest
	if err := json.Unmarshal(manifestBody, &mf); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	if mf.SourceVersion != 1 || mf.TargetVersion != 4 || mf.CflowVersion != "2.0.0" {
		t.Fatalf("manifest versions = %+v", mf)
	}
	if mf.BackupPath != backupPath || mf.ManifestPath != manifestPath {
		t.Fatalf("manifest paths = %+v", mf)
	}
	reg := migrations()
	if len(mf.Migrations) != 3 || mf.Migrations[0].ID != reg[1].ID || mf.Migrations[0].SHA256 != reg[1].SHA256 ||
		mf.Migrations[1].ID != reg[2].ID || mf.Migrations[1].SHA256 != reg[2].SHA256 ||
		mf.Migrations[2].ID != reg[3].ID || mf.Migrations[2].SHA256 != reg[3].SHA256 {
		t.Fatalf("manifest chain = %+v", mf.Migrations)
	}
	buf, err := os.ReadFile(backupPath)
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if sha256Of(string(buf)) != mf.DatabaseHash || int64(len(buf)) != mf.DatabaseSize {
		t.Fatalf("backup hash/size does not match manifest: manifest %s/%d, file %s/%d",
			mf.DatabaseHash, mf.DatabaseSize, sha256Of(string(buf)), len(buf))
	}
	integrityOK(t, s)
}

// ---------------------------------------------------------------------------
// fail-closed states
// ---------------------------------------------------------------------------

func TestMigrationChecksumMismatchFailsClosed(t *testing.T) {
	path := fixtureWithVersionMutator(t, func(tx *sql.Tx, _ *sql.DB) {
		if _, err := tx.Exec(`UPDATE schema_migrations SET migration_sha256 = ? WHERE version = 1`, strings.Repeat("ab", 32)); err != nil {
			t.Fatalf("mutate sha: %v", err)
		}
	})
	s, err := Open(context.Background(), OpenOptions{Path: path, CflowVersion: "0.0.0-dev"})
	if s != nil {
		s.Close()
	}
	assertFaultCode(t, err, model.CodeMigrationChecksumMismatch)
	// The database is untouched (checked with the raw driver because the
	// Store itself correctly fails closed on every reopen of this state).
	db, err := sql.Open("sqlite", fileDSN(path, false, time.Second))
	if err != nil {
		t.Fatalf("raw reopen: %v", err)
	}
	defer db.Close()
	var uv int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&uv); err != nil {
		t.Fatalf("user_version: %v", err)
	}
	if uv != 1 {
		t.Fatalf("user_version = %d, want 1 (checksum mismatch must not migrate)", uv)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if n != 1 {
		t.Fatalf("schema_migrations rows = %d, want 1", n)
	}
}

func TestMigrationSchemaTooNewFailsClosed(t *testing.T) {
	path := fixtureWithVersionMutator(t, func(tx *sql.Tx, _ *sql.DB) {
		if _, err := tx.Exec(`INSERT INTO schema_migrations
			(version, migration_id, migration_sha256, cflow_version, applied_at)
			VALUES (99, 'cflow-099-future', ?, '9.9.9', ?)`, strings.Repeat("cd", 32), nowText()); err != nil {
			t.Fatalf("insert future row: %v", err)
		}
		if _, err := tx.Exec(`PRAGMA user_version = 99`); err != nil {
			t.Fatalf("set user_version: %v", err)
		}
	})
	s, err := Open(context.Background(), OpenOptions{Path: path, CflowVersion: "0.0.0-dev"})
	if s != nil {
		s.Close()
	}
	assertFaultCode(t, err, model.CodeDatabaseSchemaTooNew)
	// Read-only commands also fail closed: no safe Reader for a newer
	// schema (PRD 决策 9).
	rs, err := Open(context.Background(), OpenOptions{Path: path, ReadOnly: true, CflowVersion: "0.0.0-dev"})
	if rs != nil {
		rs.Close()
	}
	assertFaultCode(t, err, model.CodeDatabaseSchemaTooNew)
}

func TestMigrationPathMissingFailsClosed(t *testing.T) {
	// A database whose applied set skips version 1 has no continuous
	// forward path; it must block, never guess or jump versions.
	path := fixtureWithVersionMutator(t, func(tx *sql.Tx, _ *sql.DB) {
		if _, err := tx.Exec(`DELETE FROM schema_migrations`); err != nil {
			t.Fatalf("delete rows: %v", err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations
			(version, migration_id, migration_sha256, cflow_version, applied_at)
			VALUES (3, 'cflow-003-integration-head', ?, '0.0.0-dev', ?)`, sha256Of(migrations()[2].SQL), nowText()); err != nil {
			t.Fatalf("insert v2-only row: %v", err)
		}
		// The guard must agree with the authoritative record so the
		// broken-chain classification is what fails closed.
		if _, err := tx.Exec(`PRAGMA user_version = 3`); err != nil {
			t.Fatalf("set user_version: %v", err)
		}
	})
	s, err := Open(context.Background(), OpenOptions{Path: path, CflowVersion: "0.0.0-dev"})
	if s != nil {
		s.Close()
	}
	assertFaultCode(t, err, model.CodeDatabaseMigrationPathMissing)
}

func TestMigrationIncompleteOnVersionGuardMismatch(t *testing.T) {
	// user_version is only a consistency guard; a mismatch with the
	// authoritative schema_migrations record must fail closed.
	path := fixtureWithVersionMutator(t, func(tx *sql.Tx, _ *sql.DB) {
		if _, err := tx.Exec(`PRAGMA user_version = 2`); err != nil {
			t.Fatalf("set user_version: %v", err)
		}
	})
	s, err := Open(context.Background(), OpenOptions{Path: path, CflowVersion: "0.0.0-dev"})
	if s != nil {
		s.Close()
	}
	assertFaultCode(t, err, model.CodeDatabaseMigrationIncomplete)
}

// ---------------------------------------------------------------------------
// crash points (brief Step 1: crash before/after backup manifest)
// ---------------------------------------------------------------------------

func TestCrashBeforeBackupManifestLeavesUnverifiableBackup(t *testing.T) {
	path := v1FixturePath(t)
	s, err := Open(context.Background(), OpenOptions{Path: path, CflowVersion: "0.0.0-dev",
		faults: []FaultPoint{FailBeforeBackupManifest}})
	if s != nil {
		s.Close()
	}
	if !errors.Is(err, errInjected) {
		t.Fatalf("open = %v, want injected failure", err)
	}
	// Crash state: consistent backup exists, manifest was never written.
	backupDir := filepath.Join(filepath.Dir(path), "backups", "db", "cflow-004-apply-staging-head")
	if _, err := os.Stat(filepath.Join(backupDir, "cflow.db")); err != nil {
		t.Fatalf("backup file missing after crash: %v", err)
	}
	if _, err := os.Stat(filepath.Join(backupDir, "backup-manifest.json")); !os.IsNotExist(err) {
		t.Fatalf("manifest exists after crash-before-manifest: %v", err)
	}
	// The database itself was never touched.
	s2, err2 := openReadOnlyRaw(t, path)
	if err2 != nil {
		t.Fatalf("reopen: %v", err2)
	}
	defer s2.Close()
	if got := userVersion(t, s2); got != 1 {
		t.Fatalf("user_version = %d, want 1", got)
	}
	// The next open cannot determinately classify the unverifiable backup:
	// fail closed with DATABASE_MIGRATION_INCOMPLETE, never auto-restore.
	s3, err3 := Open(context.Background(), OpenOptions{Path: path, CflowVersion: "0.0.0-dev"})
	if s3 != nil {
		s3.Close()
	}
	assertFaultCode(t, err3, model.CodeDatabaseMigrationIncomplete)
	if _, err := os.Stat(filepath.Join(backupDir, "cflow.db")); err != nil {
		t.Fatalf("fail-closed open removed the backup: %v", err)
	}
}

func TestCrashAfterBackupManifestRetriesIdempotently(t *testing.T) {
	path := v1FixturePath(t)
	s, err := Open(context.Background(), OpenOptions{Path: path, CflowVersion: "0.0.0-dev",
		faults: []FaultPoint{FailAfterBackupManifest}})
	if s != nil {
		s.Close()
	}
	if !errors.Is(err, errInjected) {
		t.Fatalf("open = %v, want injected failure", err)
	}
	backupDir := filepath.Join(filepath.Dir(path), "backups", "db", "cflow-004-apply-staging-head")
	manifestPath := filepath.Join(backupDir, "backup-manifest.json")
	manifestBody, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("manifest missing after crash-after-manifest: %v", err)
	}
	backupPath := filepath.Join(backupDir, "cflow.db")
	backupBefore, err := os.ReadFile(backupPath)
	if err != nil {
		t.Fatalf("backup missing: %v", err)
	}

	// The next open verifies the backup + manifest and retries the chain
	// idempotently: the recorded manifest hash binds the verified backup.
	s2, err2 := Open(context.Background(), OpenOptions{Path: path, CflowVersion: "0.0.0-dev"})
	if err2 != nil {
		t.Fatalf("reopen after crash: %v", err2)
	}
	defer s2.Close()
	if got := userVersion(t, s2); got != 4 {
		t.Fatalf("user_version = %d, want 4", got)
	}
	rows := schemaRows(t, s2)
	if len(rows) != 4 || rows[3].ManifestSHA256 != sha256Of(string(manifestBody)) {
		t.Fatalf("rows after retry = %+v", rows)
	}
	backupAfter, err := os.ReadFile(backupPath)
	if err != nil {
		t.Fatalf("backup after retry: %v", err)
	}
	if string(backupAfter) != string(backupBefore) {
		t.Fatal("retry re-created the backup; the verified backup must be reused")
	}
}

func TestCrashBeforeMigrationChainLeavesDatabaseUntouched(t *testing.T) {
	path := v1FixturePath(t)
	s, err := Open(context.Background(), OpenOptions{Path: path, CflowVersion: "0.0.0-dev",
		faults: []FaultPoint{FailBeforeMigrate}})
	if s != nil {
		s.Close()
	}
	if !errors.Is(err, errInjected) {
		t.Fatalf("open = %v, want injected failure", err)
	}
	s2, err2 := openReadOnlyRaw(t, path)
	if err2 != nil {
		t.Fatalf("reopen: %v", err2)
	}
	defer s2.Close()
	if got := userVersion(t, s2); got != 1 {
		t.Fatalf("user_version = %d, want 1 (chain never began)", got)
	}
}

func TestCrashAfterMigrationCommitRecognizesCompletion(t *testing.T) {
	path := v1FixturePath(t)
	s, err := Open(context.Background(), OpenOptions{Path: path, CflowVersion: "0.0.0-dev",
		faults: []FaultPoint{FailAfterMigrate}})
	if s != nil {
		s.Close()
	}
	if !errors.Is(err, errInjected) {
		t.Fatalf("open = %v, want injected failure", err)
	}
	// Commit already happened: schema_migrations is the authoritative
	// completion record and the next open must not re-run anything.
	s2, err2 := Open(context.Background(), OpenOptions{Path: path, CflowVersion: "0.0.0-dev"})
	if err2 != nil {
		t.Fatalf("reopen after committed migration: %v", err2)
	}
	defer s2.Close()
	if got := userVersion(t, s2); got != 4 {
		t.Fatalf("user_version = %d, want 4", got)
	}
	if rows := schemaRows(t, s2); len(rows) != 4 {
		t.Fatalf("rows = %d, want 4 (no re-run)", len(rows))
	}
}

func TestMigrationManifestDeletedFailsClosed(t *testing.T) {
	path := v1FixturePath(t)
	s, err := Open(context.Background(), OpenOptions{Path: path, CflowVersion: "0.0.0-dev",
		faults: []FaultPoint{FailAfterBackupManifest}})
	if s != nil {
		s.Close()
	}
	if !errors.Is(err, errInjected) {
		t.Fatalf("open = %v, want injected failure", err)
	}
	backupDir := filepath.Join(filepath.Dir(path), "backups", "db", "cflow-004-apply-staging-head")
	if err := os.Remove(filepath.Join(backupDir, "backup-manifest.json")); err != nil {
		t.Fatalf("remove manifest: %v", err)
	}
	s2, err2 := Open(context.Background(), OpenOptions{Path: path, CflowVersion: "0.0.0-dev"})
	if s2 != nil {
		s2.Close()
	}
	assertFaultCode(t, err2, model.CodeDatabaseMigrationIncomplete)
}

// ---------------------------------------------------------------------------
// read-only opens never migrate (PRD 决策 4)
// ---------------------------------------------------------------------------

func TestReadOnlyOpenFromV1DoesNotMigrate(t *testing.T) {
	path := v1FixturePath(t)
	s, err := Open(context.Background(), OpenOptions{Path: path, ReadOnly: true, CflowVersion: "0.0.0-dev"})
	if err != nil {
		t.Fatalf("read-only open: %v", err)
	}
	defer s.Close()
	if got := userVersion(t, s); got != 1 {
		t.Fatalf("user_version = %d, want 1 (read commands never migrate)", got)
	}
	if rows := schemaRows(t, s); len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), "backups")); !os.IsNotExist(err) {
		t.Fatalf("read-only open created backups: %v", err)
	}
	view := mustView(t, s)
	if view.State.Workflow.ID != "" {
		t.Fatalf("v1 database read-only view = %#v", view.State)
	}
}

// ---------------------------------------------------------------------------
// migration isolation and concurrency
// ---------------------------------------------------------------------------

func TestMigrationPerformsNoExternalEffects(t *testing.T) {
	path := v1FixturePath(t)
	s, err := Open(context.Background(), OpenOptions{Path: path, CflowVersion: "0.0.0-dev"})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	// The backup directory contains exactly the consistent backup and its
	// immutable manifest: no Artifact, Git, Verification, or Provider
	// outputs are produced by migration (PRD 决策 7).
	backupDir := filepath.Join(filepath.Dir(path), "backups", "db", "cflow-004-apply-staging-head")
	entries, err := os.ReadDir(backupDir)
	if err != nil {
		t.Fatalf("read backup dir: %v", err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	want := []string{"backup-manifest.json", "cflow.db"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("backup dir = %v, want exactly %v", names, want)
	}
}

func TestMigrationConcurrentOpensMigrateExactlyOnce(t *testing.T) {
	path := v1FixturePath(t)
	const n = 4
	start := make(chan struct{})
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			s, err := Open(context.Background(), OpenOptions{Path: path, CflowVersion: "0.0.0-dev"})
			if s != nil {
				err = errors.Join(err, s.Close())
			}
			errs[i] = err
		}(i)
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent open %d: %v", i, err)
		}
	}
	s, err := Open(context.Background(), OpenOptions{Path: path, ReadOnly: true, CflowVersion: "0.0.0-dev"})
	if err != nil {
		t.Fatalf("final open: %v", err)
	}
	defer s.Close()
	rows := schemaRows(t, s)
	if len(rows) != 4 {
		t.Fatalf("rows = %d, want 4 (migrated exactly once)", len(rows))
	}
	if got := userVersion(t, s); got != 4 {
		t.Fatalf("user_version = %d, want 4", got)
	}
}

// ---------------------------------------------------------------------------
// Cleanup manifest bindings (002 capability)
// ---------------------------------------------------------------------------

func TestCleanupAppendCommitsImmutableManifestBinding(t *testing.T) {
	s := openTestStore(t)
	mustTransact(t, s, 0, fixtureDecision)
	items := []model.CleanupItem{
		{Index: 0, Kind: model.CleanupWorktree, CanonicalPath: "/abs/task-a", Branch: "task/a",
			ExpectedHead: "head-a", Fingerprint: "fp-a", Status: model.CleanupItemPending},
		{Index: 1, Kind: model.CleanupScratch, CanonicalPath: "/abs/scratch", Branch: "",
			ExpectedHead: "", Fingerprint: "", Status: model.CleanupItemPending},
	}
	mustTransact(t, s, 1, func(state model.State) (model.Decision, error) {
		return model.Decision{
			Mutations: []model.Mutation{model.CleanupAppendMutation{CleanupAttempt: model.CleanupAttempt{
				ID: "cleanup-1", Status: model.CleanupStatusDryRun,
				Manifest: model.ArtifactRef{Workflow: "wf-1", Type: model.ArtifactCleanupManifest,
					Revision: 0, Hash: model.CleanupManifestHash(items)},
				Items: items, StartedAt: state.Now,
			}}},
			Events: []model.Event{{Seq: state.NextEventSeq, Kind: model.EventCleanupAttemptCreated,
				Workflow: "wf-1", Text: "cleanup dry run", At: state.Now}},
		}, nil
	})
	view := mustView(t, s)
	if len(view.State.CleanupAttempts) != 1 {
		t.Fatalf("cleanup attempts = %d, want 1", len(view.State.CleanupAttempts))
	}
	att := view.State.CleanupAttempts[0]
	if att.Manifest.Hash != model.CleanupManifestHash(items) || len(att.Items) != 2 {
		t.Fatalf("cleanup attempt = %#v", att)
	}
	if att.Items[0].Kind != model.CleanupWorktree || att.Items[0].CanonicalPath != "/abs/task-a" {
		t.Fatalf("cleanup item 0 = %#v", att.Items[0])
	}
	// The 002 binding row pins the manifest identity and hash.
	var bindingSHA string
	err := s.db.QueryRow(`SELECT binding_sha256 FROM cleanup_manifest_bindings WHERE cleanup_attempt_id = 'cleanup-1'`).Scan(&bindingSHA)
	if err != nil {
		t.Fatalf("binding row: %v", err)
	}
	if bindingSHA != cleanupBindingHash("cleanup-1", "cleanup/cleanup-plan-cleanup-1.json", att.Manifest.Hash) {
		t.Fatalf("binding hash = %s", bindingSHA)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func openReadOnlyRaw(t *testing.T, path string) (*Store, error) {
	t.Helper()
	s, err := Open(context.Background(), OpenOptions{Path: path, ReadOnly: true, CflowVersion: "0.0.0-dev"})
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, nil
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// mapSQLError stable classification (Task 21 ledger obligation (a)): the
// model has no dedicated local-contention or stale-writer Code, so
// SQLITE_BUSY maps to DATABASE_MIGRATION_FAILED and a stale aggregate
// version to INVALID_INPUT; every other database failure is the invariant
// default. These tests pin the mapping and the compiled disposition fast
// (in-package, a bounded busy timeout) — the release matrix rows
// (store_sqlite_busy, store_stale_version, store_db_failure_*) assert the
// same through the real 5s timeout and the fresh-restart facts.
// ---------------------------------------------------------------------------

// openStoreWithBusy opens a store over path with an in-package bounded
// busy timeout (the seam only test constructors reach).
func openStoreWithBusy(t *testing.T, path string, busy time.Duration) *Store {
	t.Helper()
	s, err := Open(context.Background(), OpenOptions{Path: path, Workflow: "wf-1", CflowVersion: "0.0.0-dev", busyTimeout: busy})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	seedProjectRow(t, s)
	return s
}

// assertDisposition checks one Code's compiled disposition (design 8.2).
func assertDisposition(t *testing.T, code model.Code, category model.FaultCategory, closeDispatch bool) {
	t.Helper()
	pol, ok := model.Policy(code)
	if !ok {
		t.Fatalf("no compiled policy for %s", code)
	}
	if pol.Category != category || pol.CloseDispatch != closeDispatch {
		t.Fatalf("policy(%s) = %+v, want category %s closeDispatch %v", code, pol, category, closeDispatch)
	}
	if pol.Retry.ChargesBudget {
		t.Fatalf("policy(%s) charges a retry budget: %+v", code, pol)
	}
}

func TestSQLiteBusyClassifiedAsDatabaseMigrationFailed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cflow.db")
	s := openStoreWithBusy(t, path, 150*time.Millisecond)
	mustTransact(t, s, 0, fixtureDecision)

	// A peer connection holds BEGIN IMMEDIATE: the store's write contends
	// for the bounded busy timeout and returns the stable local-contention
	// Fault — never an unbounded loop (PRD 决策: SQLITE_BUSY 有界).
	raw, err := sql.Open("sqlite", fileDSN(path, false, time.Second))
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	defer raw.Close()
	hold, err := raw.Begin()
	if err != nil {
		t.Fatalf("raw begin: %v", err)
	}
	start := time.Now()
	_, err = s.Transact(context.Background(), 1, func(state model.State) (model.Decision, error) {
		return model.Decision{Mutations: []model.Mutation{model.NodeAppendMutation{Node: model.Node{
			ID: "n1", Kind: model.NodeAgentTask, Status: model.NodeReady,
		}}}}, nil
	})
	elapsed := time.Since(start)
	_ = hold.Rollback()
	assertFaultCode(t, err, model.CodeDatabaseMigrationFailed)
	assertDisposition(t, model.CodeDatabaseMigrationFailed, model.CatInvariantFailure, true)
	if elapsed < 100*time.Millisecond {
		t.Fatalf("busy contention returned in %v, want a bounded wait", elapsed)
	}
	// The failed write committed nothing.
	view := mustView(t, s)
	if len(view.State.Nodes) != 0 || view.AggregateVersion != 1 {
		t.Fatalf("busy failure left partial state: %+v", view.State.Nodes)
	}
}

func TestStaleAggregateVersionClassifiedInvalidInput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cflow.db")
	a := openStoreWithBusy(t, path, time.Second)
	mustTransact(t, a, 0, fixtureDecision)
	b := openStoreWithBusy(t, path, time.Second)

	// A commits version 1 -> 2 (a node) while B still holds the stale
	// expectation and tries to write a second node.
	mustTransact(t, a, 1, func(state model.State) (model.Decision, error) {
		return model.Decision{Mutations: []model.Mutation{model.NodeAppendMutation{Node: model.Node{
			ID: "n1", Kind: model.NodeAgentTask, Status: model.NodeReady,
		}}}}, nil
	})
	_, err := b.Transact(context.Background(), 1, func(state model.State) (model.Decision, error) {
		return model.Decision{Mutations: []model.Mutation{model.NodeAppendMutation{Node: model.Node{
			ID: "n2", Kind: model.NodeAgentTask, Status: model.NodeReady,
		}}}}, nil
	})
	assertFaultCode(t, err, model.CodeInvalidInput)
	assertDisposition(t, model.CodeInvalidInput, model.CatInvalidInput, false)
	// The stale writer added nothing: exactly A's node remains.
	view := mustView(t, a)
	if len(view.State.Nodes) != 1 || view.AggregateVersion != 2 {
		t.Fatalf("stale writer mutated the aggregate: %+v", view.State.Nodes)
	}
}

func TestOtherDatabaseFailureClassifiedInvariant(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cflow.db")
	s := openStoreWithBusy(t, path, time.Second)
	mustTransact(t, s, 0, fixtureDecision)

	// A database failure that is neither contention nor a constraint (a
	// trigger raises on the nodes insert) reaches the mapSQLError default
	// path and is classified coherently as the invariant default; the
	// failed transaction commits nothing.
	raw, err := sql.Open("sqlite", fileDSN(path, false, time.Second))
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	if _, err := raw.Exec(`CREATE TRIGGER cflow_probe_block_insert BEFORE INSERT ON nodes
		BEGIN SELECT RAISE(FAIL, 'probe: nodes insert blocked'); END`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	raw.Close()

	_, err = s.Transact(context.Background(), 1, func(state model.State) (model.Decision, error) {
		return model.Decision{Mutations: []model.Mutation{model.NodeAppendMutation{Node: model.Node{
			ID: "n1", Kind: model.NodeAgentTask, Status: model.NodeReady,
		}}}}, nil
	})
	assertFaultCode(t, err, model.CodeStateInvariantViolation)
	assertDisposition(t, model.CodeStateInvariantViolation, model.CatInvariantFailure, true)
	view := mustView(t, s)
	if len(view.State.Nodes) != 0 || view.AggregateVersion != 1 {
		t.Fatalf("database failure left partial state: %+v", view.State.Nodes)
	}
}
