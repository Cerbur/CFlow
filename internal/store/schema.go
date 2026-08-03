package store

// The embedded forward-only migration registry and the migration protocol
// (design 9.3, PRD 决策 1-9): shared version read -> exclusive Schema Lock
// request -> re-read -> 0600 consistent backup + hashed immutable Manifest
// -> one BEGIN IMMEDIATE chain -> integrity/foreign-key verification ->
// current Reader reopen. Migration never runs Provider, Git, Verification,
// or Artifact rewrite Effects.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"cflow.local/cflow/internal/model"
	migrationfs "cflow.local/cflow/migrations"
)

// Migration is one embedded, immutable, forward-only schema migration.
// Version, ID, and SHA-256 are pinned at build time and never change
// (PRD 决策 1).
type Migration struct {
	Version int
	ID      string
	File    string
	SHA256  string // hex SHA-256 of the embedded SQL content
	SQL     string
}

// migrations returns the registry in ascending version order, validating
// contiguity and completeness at first use. A registry defect is a build
// error: the Store must never guess a migration chain.
func migrations() []Migration {
	reg := []Migration{
		{Version: 1, ID: "cflow-001-initial", File: "001_initial.sql"},
		{Version: 2, ID: "cflow-002-cleanup-apply", File: "002_cleanup_apply.sql"},
		{Version: 3, ID: "cflow-003-integration-head", File: "003_integration_head.sql"},
	}
	for i, m := range reg {
		body, err := migrationfs.FS.ReadFile(m.File)
		if err != nil {
			panic(fmt.Sprintf("store: embedded migration %s missing: %v", m.File, err))
		}
		reg[i].SQL = string(body)
		sum := sha256.Sum256(body)
		reg[i].SHA256 = hex.EncodeToString(sum[:])
		if m.ID == "" || len(reg[i].SQL) == 0 {
			panic(fmt.Sprintf("store: migration %d (%s) incomplete", m.Version, m.File))
		}
	}
	if reg[0].Version != 1 {
		panic("store: migration chain must start at version 1")
	}
	for i := 1; i < len(reg); i++ {
		if reg[i].Version != reg[i-1].Version+1 {
			panic(fmt.Sprintf("store: migration chain broken between %d and %d", reg[i-1].Version, reg[i].Version))
		}
	}
	return reg
}

// appliedMigration is one row of the authoritative schema_migrations
// record (PRD 最低数据库记录).
type appliedMigration struct {
	Version        int
	ID             string
	SHA256         string
	ManifestPath   string
	ManifestSHA256 string
}

// schemaState is the version facts read under the shared Schema Lock.
type schemaState struct {
	userVersion int
	applied     []appliedMigration
	hasTable    bool
}

// readSchemaState reads PRAGMA user_version (consistency guard only) and
// the authoritative schema_migrations record.
func (s *Store) readSchemaState(ctx context.Context, q querier) (schemaState, error) {
	var st schemaState
	var has int
	if err := q.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'schema_migrations'`).Scan(&has); err != nil {
		return st, fmt.Errorf("schema_migrations probe: %w", err)
	}
	st.hasTable = has != 0
	if st.hasTable {
		rows, err := q.QueryContext(ctx, `SELECT version, migration_id, migration_sha256,
			COALESCE(backup_manifest_path, ''), COALESCE(backup_manifest_sha256, '')
			FROM schema_migrations ORDER BY version`)
		if err != nil {
			return st, fmt.Errorf("schema_migrations read: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var a appliedMigration
			if err := rows.Scan(&a.Version, &a.ID, &a.SHA256, &a.ManifestPath, &a.ManifestSHA256); err != nil {
				return st, fmt.Errorf("schema_migrations scan: %w", err)
			}
			st.applied = append(st.applied, a)
		}
		if err := rows.Err(); err != nil {
			return st, fmt.Errorf("schema_migrations rows: %w", err)
		}
	}
	if err := q.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&st.userVersion); err != nil {
		return st, fmt.Errorf("user_version: %w", err)
	}
	return st, nil
}

// classify fails closed on every state that cannot be determinately
// reconciled (PRD 决策 2, 8, 9): newer/unknown schema, checksum mismatch,
// broken chain, or an inconsistent version guard. On success it returns
// the pending forward chain.
func classify(st schemaState, reg []Migration) ([]Migration, error) {
	maxEmbedded := reg[len(reg)-1].Version
	if !st.hasTable {
		// Fresh database: the guard must be untouched and there is no
		// authoritative record yet.
		if st.userVersion > maxEmbedded {
			return nil, model.NewFault(model.CodeDatabaseSchemaTooNew,
				fmt.Sprintf("database schema version %d is newer than this binary supports (%d)", st.userVersion, maxEmbedded))
		}
		if st.userVersion != 0 {
			return nil, model.NewFault(model.CodeDatabaseMigrationIncomplete,
				fmt.Sprintf("database user_version %d conflicts with the missing schema_migrations record", st.userVersion))
		}
		return reg, nil
	}
	maxApplied := 0
	if len(st.applied) > 0 {
		maxApplied = st.applied[len(st.applied)-1].Version
	}
	if maxApplied > maxEmbedded {
		return nil, model.NewFault(model.CodeDatabaseSchemaTooNew,
			fmt.Sprintf("database schema version %d is newer than this binary supports (%d)", maxApplied, maxEmbedded))
	}
	if st.userVersion != maxApplied {
		return nil, model.NewFault(model.CodeDatabaseMigrationIncomplete,
			fmt.Sprintf("PRAGMA user_version (%d) disagrees with schema_migrations (%d)", st.userVersion, maxApplied))
	}
	for i, a := range st.applied {
		if a.Version != reg[i].Version {
			return nil, model.NewFault(model.CodeDatabaseMigrationPathMissing,
				fmt.Sprintf("no continuous migration path: applied version %d at position %d (registry expects %d)", a.Version, i+1, reg[i].Version))
		}
		if a.ID != reg[i].ID || a.SHA256 != reg[i].SHA256 {
			return nil, model.NewFault(model.CodeMigrationChecksumMismatch,
				fmt.Sprintf("applied migration %d identity/checksum %s/%s does not match the embedded registry %s/%s",
					a.Version, a.ID, a.SHA256, reg[i].ID, reg[i].SHA256))
		}
	}
	return reg[len(st.applied):], nil
}

// sha256Of returns the hex SHA-256 digest of s.
func sha256Of(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// splitStatements splits a SQL script into individual statements on
// top-level semicolons, respecting single-quoted strings (with ” escapes),
// double-quoted identifiers, and -- line comments.
func splitStatements(src string) []string {
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

// ---------------------------------------------------------------------------
// backup and immutable manifest (PRD 决策 5-6, 8)
// ---------------------------------------------------------------------------

type manifestMigration struct {
	ID     string `json:"migration_id"`
	SHA256 string `json:"migration_sha256"`
}

// backupManifest is the immutable record fixed before any migration runs:
// source/target Schema Versions, CFlow Build Version, database Hash/Size,
// the Migration ID/Checksum chain, timestamps, and the backup paths.
type backupManifest struct {
	SourceVersion int                 `json:"source_version"`
	TargetVersion int                 `json:"target_version"`
	CflowVersion  string              `json:"cflow_version"`
	DatabaseHash  string              `json:"database_hash"`
	DatabaseSize  int64               `json:"database_size"`
	Migrations    []manifestMigration `json:"migrations"`
	BackupPath    string              `json:"backup_path"`
	ManifestPath  string              `json:"manifest_path"`
	CreatedAt     string              `json:"created_at"`
}

// backupResult names the verified backup artifacts of one migration run.
type backupResult struct {
	dir          string
	backupPath   string
	manifestPath string
	manifestSHA  string
}

// verifyBackup re-reads and verifies an existing backup directory against
// the pending chain: the manifest must parse, its source/target versions
// and chain must match the pending run, and the backup file must match the
// recorded hash and size (PRD 决策 6, 8).
func (s *Store) verifyBackup(dir string, from, to int, pending []Migration) (backupResult, bool) {
	body, err := os.ReadFile(filepath.Join(dir, "backup-manifest.json"))
	if err != nil {
		return backupResult{}, false
	}
	var mf backupManifest
	if err := json.Unmarshal(body, &mf); err != nil {
		return backupResult{}, false
	}
	if mf.SourceVersion != from || mf.TargetVersion != to {
		return backupResult{}, false
	}
	if len(mf.Migrations) != len(pending) {
		return backupResult{}, false
	}
	for i, p := range pending {
		if mf.Migrations[i].ID != p.ID || mf.Migrations[i].SHA256 != p.SHA256 {
			return backupResult{}, false
		}
	}
	fi, err := os.Stat(filepath.Join(dir, "cflow.db"))
	if err != nil {
		return backupResult{}, false
	}
	if fi.Size() != mf.DatabaseSize {
		return backupResult{}, false
	}
	buf, err := os.ReadFile(filepath.Join(dir, "cflow.db"))
	if err != nil {
		return backupResult{}, false
	}
	if sha256Of(string(buf)) != mf.DatabaseHash {
		return backupResult{}, false
	}
	return backupResult{
		dir:          dir,
		backupPath:   filepath.Join(dir, "cflow.db"),
		manifestPath: filepath.Join(dir, "backup-manifest.json"),
		manifestSHA:  sha256Of(string(body)),
	}, true
}

// createBackup produces the 0600 consistent backup and its immutable
// 0600 manifest (atomic temp-file rename), then re-reads and verifies
// both. It returns the fault injection points FailBeforeBackupManifest
// and FailAfterBackupManifest.
func (s *Store) createBackup(ctx context.Context, dir string, from, to int, pending []Migration) (backupResult, error) {
	backupPath := filepath.Join(dir, "cflow.db")
	manifestPath := filepath.Join(dir, "backup-manifest.json")

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return backupResult{}, s.migrationFailedFault(fmt.Errorf("create backup dir: %w", err), backupPath)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return backupResult{}, s.migrationFailedFault(fmt.Errorf("backup dir mode: %w", err), backupPath)
	}
	// The consistent backup: VACUUM INTO snapshots the database including
	// WAL state; a raw copy of a WAL-mode file would not (PRD 决策 5).
	if _, err := s.db.ExecContext(ctx,
		"VACUUM INTO '"+strings.ReplaceAll(backupPath, "'", "''")+"'"); err != nil {
		return backupResult{}, s.migrationFailedFault(fmt.Errorf("consistent backup: %w", err), backupPath)
	}
	if err := os.Chmod(backupPath, 0o600); err != nil {
		return backupResult{}, s.migrationFailedFault(fmt.Errorf("backup file mode: %w", err), backupPath)
	}
	buf, err := os.ReadFile(backupPath)
	if err != nil {
		return backupResult{}, s.migrationFailedFault(fmt.Errorf("read backup: %w", err), backupPath)
	}
	mf := backupManifest{
		SourceVersion: from,
		TargetVersion: to,
		CflowVersion:  s.cflowVersion,
		DatabaseHash:  sha256Of(string(buf)),
		DatabaseSize:  int64(len(buf)),
		BackupPath:    backupPath,
		ManifestPath:  manifestPath,
		CreatedAt:     s.now().UTC().Format(time.RFC3339Nano),
	}
	for _, p := range pending {
		mf.Migrations = append(mf.Migrations, manifestMigration{ID: p.ID, SHA256: p.SHA256})
	}
	body, err := json.MarshalIndent(mf, "", "  ")
	if err != nil {
		return backupResult{}, s.migrationFailedFault(fmt.Errorf("encode manifest: %w", err), backupPath)
	}
	body = append(body, '\n')
	if err := s.injectFault(FailBeforeBackupManifest); err != nil {
		return backupResult{}, err
	}
	if err := writeFileAtomic(manifestPath, body, 0o600); err != nil {
		return backupResult{}, s.migrationFailedFault(fmt.Errorf("write manifest: %w", err), backupPath)
	}
	if err := s.injectFault(FailAfterBackupManifest); err != nil {
		return backupResult{}, err
	}
	// The manifest must be readable and verifiable before any migration
	// may start (PRD 决策 6).
	res, ok := s.verifyBackup(dir, from, to, pending)
	if !ok {
		return backupResult{}, model.NewFault(model.CodeDatabaseMigrationIncomplete,
			fmt.Sprintf("backup manifest at %s cannot be re-read and verified; refusing to migrate (backup preserved)", manifestPath))
	}
	return res, nil
}

// writeFileAtomic writes body to path with the given mode via a same
// directory temp file and rename.
func writeFileAtomic(path string, body []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".manifest-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// ---------------------------------------------------------------------------
// the migration run
// ---------------------------------------------------------------------------

// migrate detects the database version under the shared Schema Lock,
// requests the exclusive Schema Lock, re-reads, backs up, and applies the
// complete pending chain in one BEGIN IMMEDIATE transaction (design 9.3).
// The exclusive lock is a flock next to the database; the formal lock API
// and the lifetime shared hold arrive with Task 6 (internal/platform).
func (s *Store) migrate(ctx context.Context) error {
	s.migrateMu.Lock()
	defer s.migrateMu.Unlock()

	// 1. Shared version read.
	st, err := s.readSchemaState(ctx, s.db)
	if err != nil {
		return err
	}
	reg := migrations()
	pending, err := classify(st, reg)
	if err != nil {
		return err
	}
	if len(pending) == 0 {
		return nil
	}

	// 2. Exclusive Schema Lock request, then re-read version and registry
	// checksums before any backup (PRD 决策 3). The re-read after the
	// exclusive acquisition is what makes concurrent migration exactly
	// once: a migrator that read before another committed observes the
	// committed chain here and stops.
	lock, err := acquireSchemaLock(s.path + ".schema-lock")
	if err != nil {
		return fmt.Errorf("store: exclusive schema lock: %w", err)
	}
	defer lock.Close()
	st, err = s.readSchemaState(ctx, s.db)
	if err != nil {
		return err
	}
	pending, err = classify(st, reg)
	if err != nil {
		return err
	}
	if len(pending) == 0 {
		return nil
	}
	from := 0
	if len(st.applied) > 0 {
		from = st.applied[len(st.applied)-1].Version
	}
	to := pending[len(pending)-1].Version

	// 3. Source integrity must hold before the backup is trusted.
	if ok, err := s.integrityOK(ctx, s.db); err != nil {
		return s.migrationFailedFault(err, "")
	} else if !ok {
		return s.migrationFailedFault(errors.New("source database failed integrity_check"), "")
	}

	// 4. 0600 consistent backup + immutable Manifest. A fresh baseline
	// needs no backup (PRD 决策 5); any other upgrade verifies or creates
	// one and never auto-restores it (PRD 决策 6, 8).
	var backup backupResult
	backupDir := filepath.Join(filepath.Dir(s.path), "backups", "db", pending[len(pending)-1].ID)
	if from == 0 {
		backup = backupResult{dir: backupDir}
	} else if res, ok := s.verifyBackup(backupDir, from, to, pending); ok {
		backup = res
	} else if _, err := os.Stat(backupDir); err == nil {
		return model.NewFault(model.CodeDatabaseMigrationIncomplete,
			fmt.Sprintf("backup directory %s exists but cannot be verified (manifest missing or corrupt); refusing to migrate, backup preserved", backupDir))
	} else {
		res, err := s.createBackup(ctx, backupDir, from, to, pending)
		if err != nil {
			return err
		}
		backup = res
	}

	// 5. One BEGIN IMMEDIATE chain: every pending migration plus its
	// schema_migrations row commits atomically; any failure rolls the
	// whole chain back and the next start retries idempotently.
	if err := s.injectFault(FailBeforeMigrate); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil) // _txlock=immediate
	if err != nil {
		return s.mapSQLError(err)
	}
	rollback := func(err error) error {
		_ = tx.Rollback()
		return err
	}
	now := s.now().UTC().Format(time.RFC3339Nano)
	for _, m := range pending {
		for _, stmt := range splitStatements(m.SQL) {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return rollback(s.migrationFailedFault(fmt.Errorf("migration %s: %w", m.ID, err), backup.backupPath))
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations
			(version, migration_id, migration_sha256, cflow_version,
			 backup_manifest_path, backup_manifest_sha256, applied_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			m.Version, m.ID, m.SHA256, s.cflowVersion, backup.manifestPath, backup.manifestSHA, now); err != nil {
			return rollback(s.migrationFailedFault(fmt.Errorf("schema_migrations row %s: %w", m.ID, err), backup.backupPath))
		}
	}
	// 6. Integrity and foreign-key verification before Commit (PRD 决策 7).
	if ok, err := s.integrityOK(ctx, tx); err != nil || !ok {
		return rollback(s.migrationFailedFault(errors.New("integrity_check failed after migration chain"), backup.backupPath))
	}
	if fk, err := s.foreignKeyViolations(ctx, tx); err != nil {
		return rollback(s.migrationFailedFault(err, backup.backupPath))
	} else if len(fk) > 0 {
		return rollback(s.migrationFailedFault(fmt.Errorf("foreign_key_check after migration: %s", strings.Join(fk, "; ")), backup.backupPath))
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, to)); err != nil {
		return rollback(s.migrationFailedFault(err, backup.backupPath))
	}
	if err := tx.Commit(); err != nil {
		return s.mapSQLError(err)
	}
	if err := s.injectFault(FailAfterMigrate); err != nil {
		return err
	}

	// 7. Reopen through the current Reader: verify the committed facts
	// before the Store serves any aggregate.
	st, err = s.readSchemaState(ctx, s.db)
	if err != nil {
		return err
	}
	if _, err := classify(st, reg); err != nil {
		return model.NewFault(model.CodeDatabaseMigrationIncomplete,
			fmt.Sprintf("post-migration verification failed: %v", err))
	}
	if ok, err := s.integrityOK(ctx, s.db); err != nil {
		return s.migrationFailedFault(err, backup.backupPath)
	} else if !ok {
		return model.NewFault(model.CodeDatabaseMigrationIncomplete,
			fmt.Sprintf("post-migration integrity_check failed (backup preserved at %s)", backup.backupPath))
	}
	return nil
}

func (s *Store) integrityOK(ctx context.Context, q querier) (bool, error) {
	var v string
	if err := q.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&v); err != nil {
		return false, err
	}
	return v == "ok", nil
}

func (s *Store) foreignKeyViolations(ctx context.Context, q querier) ([]string, error) {
	rows, err := q.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var table, parent, fkid string
		var rowid int64
		if err := rows.Scan(&table, &rowid, &parent, &fkid); err != nil {
			return nil, err
		}
		out = append(out, fmt.Sprintf("%s.%s(row %d)", table, fkid, rowid))
	}
	return out, rows.Err()
}

func (s *Store) migrationFailedFault(err error, backupPath string) error {
	text := fmt.Sprintf("migration failed: %v", err)
	if backupPath != "" {
		text += fmt.Sprintf("; verified backup preserved at %s (never auto-restored)", backupPath)
	}
	return model.NewFault(model.CodeDatabaseMigrationFailed, text)
}

// cleanupBindingHash is the canonical SHA-256 of one Cleanup Manifest
// binding record (002 capability): the immutable binding of an Attempt to
// its confirmed Manifest identity.
func cleanupBindingHash(attemptID, manifestPath, manifestSHA string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%s\n", attemptID, manifestPath, manifestSHA)))
	return hex.EncodeToString(sum[:])
}
