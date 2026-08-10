// Package store implements the concrete SQLite aggregate transaction seam
// (design 9): the authoritative State Store and Event log behind a
// per-Workflow aggregate. Every state transition and its Events commit in
// one transaction; the Event sequence is database-assigned and strictly
// increasing; Attempt identity (node_id, attempt_number) is never reused;
// and the aggregate version compare-and-swap rejects stale writers.
//
// Migrations are embedded, forward-only, version-numbered, and
// checksum-pinned (see schema.go); read-only opens never migrate, and a
// database this binary cannot read fails closed. The Store never runs
// Provider, Git, Verification, or Artifact rewrite Effects: a Decision's
// Effect Intent commits atomically and is executed by the Application.
//
// Tests use real temporary on-disk SQLite databases, never an in-memory
// mock (design 22.1). Fault points (FailBeforeCommit, the migration crash
// points) are unexported injection seams settable only inside this package.
package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	sqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/security"
)

// errInjected is the sentinel every injected fault point returns.
var errInjected = errors.New("store: injected failure")

// FaultPoint names one crash/injection point inside the Store. The points
// are test seams: they are set through OpenOptions or the in-package
// injectFailure helper and are never reachable from release code paths.
type FaultPoint string

const (
	// FailBeforeCommit aborts a Transact immediately before COMMIT,
	// leaving the transaction to roll back (no partial commit).
	FailBeforeCommit FaultPoint = "fail-before-commit"
	// FailBeforeBackupManifest aborts migration after the consistent
	// backup exists but before the immutable Manifest is written.
	FailBeforeBackupManifest FaultPoint = "fail-before-backup-manifest"
	// FailAfterBackupManifest aborts migration after the Manifest is
	// written and verified, before the migration chain begins.
	FailAfterBackupManifest FaultPoint = "fail-after-backup-manifest"
	// FailBeforeMigrate aborts migration after the backup, before the
	// BEGIN IMMEDIATE chain.
	FailBeforeMigrate FaultPoint = "fail-before-migrate"
	// FailAfterMigrate aborts the open after the chain committed.
	FailAfterMigrate FaultPoint = "fail-after-migrate"
)

// Store is the concrete SQLite aggregate transaction seam. It is bound to
// one Workflow aggregate at Open: View and Transact hydrate that aggregate,
// and the Event sequence is database-assigned per database (design 9).
type Store struct {
	db       *sql.DB
	path     string
	exists   bool // false only for a read-only open of a missing database
	readOnly bool

	wfMu     sync.RWMutex
	workflow model.WorkflowID // bound aggregate; "" = not created yet

	cflowVersion string
	now          func() time.Time
	busyTimeout  time.Duration

	migrateMu sync.Mutex // in-process migration serialization
	inject    map[FaultPoint]struct{}
}

// OpenOptions configures one Store open.
type OpenOptions struct {
	// Path is the SQLite database file. The parent directory must exist.
	Path string
	// Workflow is the aggregate the Store is bound to. The zero value
	// denotes a not-yet-created Workflow: the first Transact must carry a
	// WorkflowMutation that establishes the identity.
	Workflow model.WorkflowID
	// ReadOnly opens never migrate and reject Transact (PRD 决策 4). A
	// read-only open of a missing database succeeds as an empty store and
	// never creates the file.
	ReadOnly bool
	// CflowVersion is recorded in schema_migrations and backup manifests.
	// Defaults to "0.0.0-dev".
	CflowVersion string
	// Now is the injected clock for database bookkeeping timestamps.
	// Defaults to time.Now.
	Now func() time.Time

	busyTimeout time.Duration // test-only; defaults to 5s (PRD init)
	faults      []FaultPoint  // test-only injection points for migration
}

// StoreQuery selects the Event window a View returns.
type StoreQuery struct {
	// From is the first Event sequence to include (inclusive). 0 starts
	// at the beginning.
	From uint64
	// Limit caps the number of Events returned. 0 is unlimited.
	Limit int
}

// StoreView is one read of the bound aggregate.
type StoreView struct {
	// AggregateVersion is the committed aggregate version.
	AggregateVersion model.AggregateVersion
	// State is the hydrated aggregate (design 7.1, 8.1).
	State model.State
	// Events are the bound Workflow's authoritative Events in sequence
	// order within the requested window.
	Events []model.Event
	// NextEventSeq is the sequence the next authoritative Event will
	// receive (database-assigned, global).
	NextEventSeq uint64
	// PendingEffects are the committed, not-yet-resolved Effect Intents
	// of the bound Workflow (design 6.2).
	PendingEffects []PendingEffect
	// LayoutMigration is the authoritative migration row for the bound
	// Workflow, when one has been explicitly prepared.
	LayoutMigration *LayoutMigrationRecord
}

// PendingEffect is one committed Effect Intent awaiting execution.
type PendingEffect struct {
	ID              string
	Intent          model.EffectIntent
	DecisionVersion model.AggregateVersion
}

// Open opens the database at Path, applies the PRD initialization pragmas,
// and runs the forward-only migration protocol for write opens (schema.go).
func Open(ctx context.Context, opts OpenOptions) (*Store, error) {
	if opts.Path == "" {
		return nil, model.InvalidInputFault("store open requires a database path")
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	cflowVersion := opts.CflowVersion
	if cflowVersion == "" {
		cflowVersion = "0.0.0-dev"
	}
	busy := opts.busyTimeout
	if busy <= 0 {
		busy = 5 * time.Second
	}
	s := &Store{
		path: opts.Path, workflow: opts.Workflow, readOnly: opts.ReadOnly,
		cflowVersion: cflowVersion, now: now, busyTimeout: busy,
	}
	if len(opts.faults) > 0 {
		s.inject = make(map[FaultPoint]struct{}, len(opts.faults))
		for _, f := range opts.faults {
			s.inject[f] = struct{}{}
		}
	}
	if opts.ReadOnly {
		return s.openReadOnly(ctx)
	}
	db, err := sql.Open("sqlite", fileDSN(opts.Path, false, busy))
	if err != nil {
		return nil, fmt.Errorf("store: open sqlite: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: connect: %w", err)
	}
	s.db = db
	s.exists = true
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// openReadOnly performs no migration (PRD 决策 4). A database this binary
// cannot read fails closed (PRD 决策 9); a missing database is an empty
// store and the file is never created.
func (s *Store) openReadOnly(ctx context.Context) (*Store, error) {
	if _, err := os.Stat(s.path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return s, nil
		}
		return nil, fmt.Errorf("store: stat %s: %w", s.path, err)
	}
	db, err := sql.Open("sqlite", fileDSN(s.path, true, s.busyTimeout))
	if err != nil {
		return nil, fmt.Errorf("store: open sqlite: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: connect: %w", err)
	}
	s.db = db
	s.exists = true
	st, err := s.readSchemaState(ctx, db)
	if err != nil {
		db.Close()
		return nil, err
	}
	if _, err := classify(st, migrations()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// fileDSN builds the modernc.org/sqlite DSN: URI form, the PRD init
// pragmas (WAL, foreign keys, bounded busy timeout, synchronous NORMAL),
// and _txlock=immediate so BeginTx opens a write transaction immediately.
func fileDSN(path string, readOnly bool, busyTimeout time.Duration) string {
	dsn := (&url.URL{Scheme: "file", Path: path}).String()
	dsn += "?_pragma=busy_timeout(" + strconv.FormatInt(busyTimeout.Milliseconds(), 10) + ")"
	dsn += "&_pragma=foreign_keys(1)"
	if readOnly {
		dsn += "&mode=ro"
		return dsn
	}
	dsn += "&_pragma=synchronous(NORMAL)"
	dsn += "&_pragma=journal_mode(WAL)"
	dsn += "&_txlock=immediate"
	return dsn
}

// Close releases the database.
func (s *Store) Close() error {
	if s.db == nil {
		return nil
	}
	return s.db.Close()
}

// workflowID returns the aggregate the Store is bound to.
func (s *Store) workflowID() model.WorkflowID {
	s.wfMu.RLock()
	defer s.wfMu.RUnlock()
	return s.workflow
}

// bindWorkflow pins the Store to the aggregate a create Decision
// established. Writes happen only when the binding changes.
func (s *Store) bindWorkflow(id model.WorkflowID) {
	s.wfMu.Lock()
	defer s.wfMu.Unlock()
	if s.workflow != id {
		s.workflow = id
	}
}

func (s *Store) injectFault(p FaultPoint) error {
	if s.inject != nil {
		if _, ok := s.inject[p]; ok {
			return errInjected
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Migration posture (the Final Report's State Compatibility section)
// ---------------------------------------------------------------------------

// AppliedMigration is one applied schema_migrations row: the version, the
// pinned ID, and the recorded SHA-256 of the applied SQL (design 9).
type AppliedMigration struct {
	Version int
	ID      string
	SHA256  string
}

// AppliedMigrations returns the applied schema_migrations rows of the
// open database in version order (""/zero when the database does not
// exist yet). The Final Report reads it to report the migration
// compatibility posture; reads never migrate (design 6.1).
func (s *Store) AppliedMigrations(ctx context.Context) ([]AppliedMigration, error) {
	if !s.exists {
		return nil, nil
	}
	st, err := s.readSchemaState(ctx, s.db)
	if err != nil {
		return nil, err
	}
	out := make([]AppliedMigration, 0, len(st.applied))
	for _, a := range st.applied {
		out = append(out, AppliedMigration{Version: a.Version, ID: a.ID, SHA256: a.SHA256})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// View
// ---------------------------------------------------------------------------

// View hydrates the bound aggregate and its Events without migrating or
// acquiring any write lock (design 9.1).
func (s *Store) View(ctx context.Context, q StoreQuery) (StoreView, error) {
	var view StoreView
	if !s.exists {
		return view, nil
	}
	st, err := hydrate(ctx, s.db, s.workflowID(), s.now)
	if err != nil {
		return view, err
	}
	view.AggregateVersion = st.Version
	view.State = st
	view.NextEventSeq = st.NextEventSeq

	limit := -1
	if q.Limit > 0 {
		limit = q.Limit
	}
	if err := forEachRow(ctx, s.db, queryEvents, []any{s.workflowID(), q.From, limit}, func(row rowScanner) error {
		var e model.Event
		var seq uint64
		var kind, workflow, payload, createdAt string
		if err := row.Scan(&seq, &kind, &workflow, &payload, &createdAt); err != nil {
			return fmt.Errorf("scan event: %w", err)
		}
		e.Seq = seq
		e.Kind = model.EventKind(kind)
		e.Workflow = model.WorkflowID(workflow)
		e.At = parseTime(createdAt)
		var p eventPayload
		if err := json.Unmarshal([]byte(payload), &p); err != nil {
			return fmt.Errorf("event %d payload: %w", seq, err)
		}
		e.Node, e.Code, e.Text = p.Node, model.Code(p.Code), p.Text
		if p.Attempt != nil {
			e.Attempt = *p.Attempt
		}
		view.Events = append(view.Events, e)
		return nil
	}); err != nil {
		return view, err
	}
	if err := forEachRow(ctx, s.db, queryPendingEffects, []any{s.workflowID()}, func(row rowScanner) error {
		var pe PendingEffect
		var kind, payload string
		if err := row.Scan(&pe.ID, &kind, &payload, &pe.DecisionVersion); err != nil {
			return fmt.Errorf("scan effect: %w", err)
		}
		intent, err := effectFromKind(kind, []byte(payload))
		if err != nil {
			return err
		}
		pe.Intent = intent
		view.PendingEffects = append(view.PendingEffects, pe)
		return nil
	}); err != nil {
		return view, err
	}
	var hasLayoutMigrations int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='layout_migrations'`).Scan(&hasLayoutMigrations); err != nil {
		return view, fmt.Errorf("probe layout migration table: %w", s.mapSQLError(err))
	}
	if hasLayoutMigrations == 1 {
		var migration LayoutMigrationRecord
		var migrationCount int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM layout_migrations WHERE workflow_id = ?`, string(s.workflowID())).Scan(&migrationCount); err != nil {
			return view, fmt.Errorf("count layout migration rows: %w", s.mapSQLError(err))
		}
		if migrationCount > 1 {
			return view, model.InvariantFault(fmt.Errorf("workflow %s has multiple authoritative layout migration rows", s.workflowID()))
		}
		err = s.db.QueryRowContext(ctx, `SELECT id, workflow_id, status, manifest_path, manifest_sha256
			FROM layout_migrations WHERE workflow_id = ?`, string(s.workflowID())).Scan(
			&migration.ID, &migration.Workflow, &migration.Status, &migration.ManifestPath, &migration.ManifestHash)
		if err == nil {
			view.LayoutMigration = &migration
		} else if !errors.Is(err, sql.ErrNoRows) {
			return view, fmt.Errorf("read layout migration view: %w", s.mapSQLError(err))
		}
	}
	return view, nil
}

// ---------------------------------------------------------------------------
// Transact
// ---------------------------------------------------------------------------

// Transact applies one Decision to the bound aggregate atomically.
func (s *Store) Transact(ctx context.Context, expected model.AggregateVersion, fn func(model.State) (model.Decision, error)) (model.CommittedDecision, error) {
	return s.transact(ctx, expected, "", fn)
}

// TransactResult applies the Result Decision for one previously committed
// Effect atomically. The effect must still be pending and belong to this
// workflow; otherwise the transaction fails closed.
func (s *Store) TransactResult(ctx context.Context, expected model.AggregateVersion, effectID string, fn func(model.State) (model.Decision, error)) (model.CommittedDecision, error) {
	if effectID == "" {
		return model.CommittedDecision{}, model.InvalidInputFault("result transaction requires an effect identity")
	}
	return s.transact(ctx, expected, effectID, fn)
}

// transact applies one Decision and, when effectID is non-empty, settles the
// matching pending Effect in the same SQLite transaction.
func (s *Store) transact(ctx context.Context, expected model.AggregateVersion, effectID string, fn func(model.State) (model.Decision, error)) (model.CommittedDecision, error) {
	if s.readOnly || !s.exists {
		return model.CommittedDecision{}, model.InvalidInputFault("store is read-only")
	}
	if fn == nil {
		return model.CommittedDecision{}, model.InvalidInputFault("decision function is nil")
	}
	tx, err := s.db.BeginTx(ctx, nil) // _txlock=immediate
	if err != nil {
		return model.CommittedDecision{}, s.mapSQLError(err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit

	// Hydrate and compare-and-swap under the write lock: the aggregate
	// version observed here is current because BEGIN IMMEDIATE excludes
	// every other writer.
	state, err := hydrate(ctx, tx, s.workflowID(), s.now)
	if err != nil {
		return model.CommittedDecision{}, err
	}
	if state.Version != expected {
		return model.CommittedDecision{}, model.NewFault(model.CodeInvalidInput,
			fmt.Sprintf("stale aggregate version: expected %d, database has %d", expected, state.Version))
	}
	decision, err := fn(state)
	if err != nil {
		return model.CommittedDecision{}, err
	}

	// Apply to the aggregate in memory first; an outcome that violates
	// the model invariants is rejected before any row is written.
	newState := state
	for _, m := range decision.Mutations {
		if err := applyMutation(&newState, m); err != nil {
			return model.CommittedDecision{}, model.InvariantFault(err)
		}
	}
	if err := model.ValidateState(newState); err != nil {
		return model.CommittedDecision{}, model.InvariantFault(err)
	}

	// The effective Workflow identity: the bound aggregate, or the
	// identity a create Decision establishes.
	workflow := s.workflowID()
	for _, m := range decision.Mutations {
		if wm, ok := m.(model.WorkflowMutation); ok {
			if wm.ID == "" || wm.Project == "" {
				return model.CommittedDecision{}, model.InvalidInputFault("workflow mutation without identity")
			}
			workflow = wm.ID
		}
	}
	if workflow == "" {
		return model.CommittedDecision{}, model.InvalidInputFault("decision on an empty aggregate must establish the workflow")
	}
	if newState.Workflow.Project == "" {
		return model.CommittedDecision{}, model.InvalidInputFault("decision leaves the workflow without a project")
	}
	// events.project_id is NOT NULL and references a registered Project;
	// the Store never fabricates Project metadata (PRD 核心数据库表).
	if err := ensureProject(ctx, tx, newState.Workflow.Project); err != nil {
		return model.CommittedDecision{}, err
	}

	now := s.now().UTC()
	existed := state.Workflow.ID != ""
	for _, m := range decision.Mutations {
		if err := persistMutation(ctx, tx, newState, existed, m, now); err != nil {
			return model.CommittedDecision{}, s.mapSQLError(err)
		}
	}

	// The authoritative Event log: sequences are assigned by this
	// transaction from the database's own maximum (design 9.2), so they
	// are strictly increasing and never reused even across Workflows.
	next, err := nextEventSeq(ctx, tx)
	if err != nil {
		return model.CommittedDecision{}, err
	}
	for i, e := range decision.Events {
		seq := next + uint64(i)
		payload, err := json.Marshal(eventPayload{Node: e.Node, Attempt: attemptKeyPtr(e.Attempt), Code: string(e.Code), Text: e.Text})
		if err != nil {
			return model.CommittedDecision{}, fmt.Errorf("encode event payload: %w", err)
		}
		createdAt := e.At.UTC().Format(time.RFC3339Nano)
		if e.At.IsZero() {
			createdAt = now.Format(time.RFC3339Nano)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO events
			(sequence, event_id, project_id, workflow_id, event_type, payload_json, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			seq, fmt.Sprintf("event-%d", seq), newState.Workflow.Project, workflow,
			string(e.Kind), string(payload), createdAt); err != nil {
			return model.CommittedDecision{}, s.mapSQLError(err)
		}
	}

	// At most one Effect Intent, committed atomically: an external Effect
	// is not executed until its Intent and expected facts commit. The
	// ledger id comes from the effects table's own counter: a Decision
	// without Events must still receive a unique intent identity (the
	// planning chain requests a Provider run and then the Artifact write
	// it produced, and the result Decisions carry no Events).
	effectIDCommitted := ""
	if decision.Effect != nil {
		kind, err := effectKindOf(decision.Effect)
		if err != nil {
			return model.CommittedDecision{}, model.InvariantFault(err)
		}
		payload, err := json.Marshal(decision.Effect)
		if err != nil {
			return model.CommittedDecision{}, fmt.Errorf("encode effect intent: %w", err)
		}
		var effectID uint64
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM effects`).Scan(&effectID); err != nil {
			return model.CommittedDecision{}, fmt.Errorf("effect ledger count: %w", err)
		}
		effectIDCommitted = fmt.Sprintf("effect-%d", effectID+1)
		if _, err := tx.ExecContext(ctx, `INSERT INTO effects
			(id, workflow_id, kind, payload_json, status, decision_version, created_at)
			VALUES (?, ?, ?, ?, 'PENDING', ?, ?)`,
			effectIDCommitted, workflow, kind, string(payload),
			expected+1, now.Format(time.RFC3339Nano)); err != nil {
			return model.CommittedDecision{}, s.mapSQLError(err)
		}
	}
	if effectID != "" {
		res, err := tx.ExecContext(ctx, `UPDATE effects SET status = 'RESULTED'
			WHERE id = ? AND workflow_id = ? AND status = 'PENDING'`,
			effectID, workflow)
		if err != nil {
			return model.CommittedDecision{}, s.mapSQLError(err)
		}
		if n, err := res.RowsAffected(); err != nil {
			return model.CommittedDecision{}, fmt.Errorf("settle effect: %w", err)
		} else if n != 1 {
			return model.CommittedDecision{}, model.InvariantFault(
				fmt.Errorf("pending effect %s was not found for workflow %s", effectID, workflow))
		}
	}

	// The aggregate version bump is the compare-and-swap commit point.
	res, err := tx.ExecContext(ctx, `UPDATE workflows
		SET aggregate_version = aggregate_version + 1, updated_at = ?
		WHERE id = ? AND aggregate_version = ?`,
		now.Format(time.RFC3339Nano), workflow, uint64(expected))
	if err != nil {
		return model.CommittedDecision{}, s.mapSQLError(err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return model.CommittedDecision{}, fmt.Errorf("version bump: %w", err)
	} else if n != 1 {
		return model.CommittedDecision{}, model.NewFault(model.CodeInvalidInput,
			fmt.Sprintf("stale aggregate version: expected %d", expected))
	}

	if err := s.injectFault(FailBeforeCommit); err != nil {
		return model.CommittedDecision{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.CommittedDecision{}, s.mapSQLError(err)
	}
	s.bindWorkflow(workflow)
	return model.CommittedDecision{
		Decision:   decision,
		Version:    expected + 1,
		EventRange: model.EventRange{From: next, To: next + uint64(len(decision.Events))},
		EffectID:   effectIDCommitted,
	}, nil
}

// ensureProject verifies the Project row exists before any Event may
// reference it. The Store never invents Project metadata; Project
// registration belongs to project discovery.
// RegisterProject records the Project identity row (PRD 核心数据库表) so
// a Workflow creation can reference it. Idempotent: an existing row is
// left untouched; the display name is the repository directory name.
func (s *Store) RegisterProject(ctx context.Context, id model.ProjectID, root, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !id.Valid() {
		return model.InvalidInputFault("project id is required")
	}
	if root == "" {
		return model.InvalidInputFault("project canonical path is required")
	}
	now := s.now().UTC().Format(time.RFC3339Nano)
	if _, err := s.db.ExecContext(ctx, `INSERT INTO projects
		(id, project_key, canonical_path, display_name, git_root, created_at, updated_at, last_opened_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO NOTHING`,
		string(id), string(id), root, name, root, now, now, now); err != nil {
		return fmt.Errorf("register project: %w", s.mapSQLError(err))
	}
	return nil
}

// LayoutMigrationRecord is the authoritative SQLite identity/status of an
// explicit legacy-layout migration.
type LayoutMigrationRecord struct {
	ID           string
	Workflow     model.WorkflowID
	Status       string
	ManifestPath string
	ManifestHash string
}

// BackupFileEvidence binds the consistent SQLite snapshot created before
// a layout migration intent: exact path, file SHA-256, and size.
type BackupFileEvidence struct {
	Path   string
	SHA256 string
	Size   int64
}

// BackupLayoutMigration creates a consistent, owner-only SQLite snapshot
// before a layout migration intent is committed. It never overwrites an
// existing path; an existing retry target must already be a valid SQLite
// backup or Prepare blocks. A stale 0-byte file (a failed attempt that
// created the guarded scratch file but never completed the online backup)
// holds no database pages, so the retry self-heals by regenerating the
// backup; a non-empty file that fails verification still fails closed.
func (s *Store) BackupLayoutMigration(ctx context.Context, path string) (BackupFileEvidence, error) {
	if path == "" {
		return BackupFileEvidence{}, model.InvalidInputFault("layout migration backup path is required")
	}
	if _, err := os.Lstat(path); err != nil {
		if !os.IsNotExist(err) {
			return BackupFileEvidence{}, err
		}
		if err := s.createLayoutMigrationBackup(ctx, path); err != nil {
			return BackupFileEvidence{}, err
		}
	} else {
		info, err := os.Lstat(path)
		if err != nil {
			return BackupFileEvidence{}, err
		}
		if info.Mode().IsRegular() && info.Size() == 0 {
			// A stale failed attempt: CFlow created the owner-only scratch
			// file but the online backup never streamed any pages. A 0-byte
			// file passes PRAGMA integrity_check as an empty database and
			// would otherwise record Size 0 evidence that readMigrationManifest
			// rejects — permanently wedging the migration retry. Remove the
			// incomplete scratch (it carries no data) and regenerate through
			// the guarded create+online-backup path.
			if err := os.Remove(path); err != nil {
				return BackupFileEvidence{}, fmt.Errorf("remove stale layout migration backup: %w", err)
			}
			if err := s.createLayoutMigrationBackup(ctx, path); err != nil {
				return BackupFileEvidence{}, err
			}
		} else {
			if _, err := security.CheckPath(security.PathRequest{Path: path, Kind: security.KindFile}); err != nil {
				return BackupFileEvidence{}, err
			}
		}
	}
	backup, err := sql.Open("sqlite", fileDSN(path, true, s.busyTimeout))
	if err != nil {
		return BackupFileEvidence{}, fmt.Errorf("open layout migration backup: %w", err)
	}
	defer backup.Close()
	var integrity string
	if err := backup.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
		return BackupFileEvidence{}, model.NewFault(model.CodeEvidenceSubjectChanged,
			"the immutable layout migration database backup failed integrity verification")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return BackupFileEvidence{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return BackupFileEvidence{}, err
	}
	sum := sha256.Sum256(body)
	return BackupFileEvidence{Path: path, SHA256: fmt.Sprintf("%x", sum[:]), Size: info.Size()}, nil
}

// createLayoutMigrationBackup performs the guarded create+online-backup of
// the consistent SQLite snapshot: the owner-only target is created first and
// the online backup streams the live database into it. A failure leaves a
// 0-byte scratch file behind; the next Prepare self-heals it.
func (s *Store) createLayoutMigrationBackup(ctx context.Context, path string) error {
	f, err := security.CreateSensitiveFile(path)
	if err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	err = conn.Raw(func(driverConn any) error {
		backuper, ok := driverConn.(interface {
			NewBackup(string) (*sqlite.Backup, error)
		})
		if !ok {
			return fmt.Errorf("sqlite connection does not support online backup")
		}
		backup, err := backuper.NewBackup(path)
		if err != nil {
			return err
		}
		if _, err := backup.Step(-1); err != nil {
			_ = backup.Finish()
			return err
		}
		return backup.Finish()
	})
	_ = conn.Close()
	if err != nil {
		return fmt.Errorf("create layout migration backup: %w", err)
	}
	// The destination connection opened by modernc's online-backup API may
	// leave copied pages in its journal until the connection is closed. Close
	// that journal before recording durable file evidence.
	dst, err := sql.Open("sqlite", fileDSN(path, false, s.busyTimeout))
	if err != nil {
		return fmt.Errorf("open completed layout migration backup: %w", err)
	}
	if _, err := dst.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		_ = dst.Close()
		return fmt.Errorf("checkpoint layout migration backup: %w", err)
	}
	if err := dst.Close(); err != nil {
		return fmt.Errorf("close layout migration backup: %w", err)
	}
	return syncFileAndParent(path)
}

func syncFileAndParent(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	if err := dir.Sync(); err != nil {
		_ = dir.Close()
		return err
	}
	return dir.Close()
}

// RecordLayoutMigration inserts the immutable identity of one explicit
// Legacy Layout Migration. Repeating the exact same Prepare is idempotent;
// an attempt to reuse the identity with different manifest facts blocks.
func (s *Store) RecordLayoutMigration(ctx context.Context, wf model.WorkflowID, id, manifestPath, manifestHash string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if wf == "" || id == "" || manifestPath == "" || manifestHash == "" {
		return model.InvalidInputFault("layout migration identity, manifest path, and hash are required")
	}
	now := s.now().UTC().Format(time.RFC3339Nano)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("record layout migration: %w", s.mapSQLError(err))
	}
	defer func() { _ = tx.Rollback() }()
	var existing LayoutMigrationRecord
	err = tx.QueryRowContext(ctx, `SELECT id, workflow_id, status, manifest_path, manifest_sha256
		FROM layout_migrations WHERE workflow_id = ? ORDER BY created_at, id LIMIT 1`, string(wf)).Scan(
		&existing.ID, &existing.Workflow, &existing.Status, &existing.ManifestPath, &existing.ManifestHash)
	switch {
	case err == nil:
		if existing.ID != id || existing.ManifestPath != manifestPath || existing.ManifestHash != manifestHash ||
			(existing.Status != "PREPARED" && existing.Status != "COMPLETED") {
			return model.NewFault(model.CodeEvidenceSubjectChanged,
				"layout migration identity or immutable manifest facts changed")
		}
		return nil
	case !errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("read layout migration: %w", s.mapSQLError(err))
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO layout_migrations
		(id, workflow_id, status, manifest_path, manifest_sha256, created_at)
		VALUES (?, ?, 'PREPARED', ?, ?, ?)`, id, string(wf), manifestPath, manifestHash, now)
	if err != nil {
		return fmt.Errorf("record layout migration: %w", s.mapSQLError(err))
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("record layout migration: %w", s.mapSQLError(err))
	}
	return nil
}

// LayoutMigration returns the single persisted migration of a Workflow.
// Multiple rows are an invariant violation because only one forward
// Layout 1 -> 2 adoption is legal.
func (s *Store) LayoutMigration(ctx context.Context, wf model.WorkflowID) (LayoutMigrationRecord, error) {
	if wf == "" {
		return LayoutMigrationRecord{}, model.InvalidInputFault("layout migration workflow is required")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, workflow_id, status, manifest_path, manifest_sha256
		FROM layout_migrations WHERE workflow_id = ? ORDER BY created_at, id`, string(wf))
	if err != nil {
		return LayoutMigrationRecord{}, fmt.Errorf("read layout migration: %w", s.mapSQLError(err))
	}
	defer rows.Close()
	var out LayoutMigrationRecord
	for rows.Next() {
		if out.ID != "" {
			return LayoutMigrationRecord{}, model.InvariantFault(fmt.Errorf("workflow %s has multiple layout migrations", wf))
		}
		if err := rows.Scan(&out.ID, &out.Workflow, &out.Status, &out.ManifestPath, &out.ManifestHash); err != nil {
			return LayoutMigrationRecord{}, fmt.Errorf("scan layout migration: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		return LayoutMigrationRecord{}, err
	}
	if out.ID == "" {
		return LayoutMigrationRecord{}, model.InvalidInputFault("layout migration has not been prepared")
	}
	return out, nil
}

// MarkLayoutMigrationCompleted marks the migration row COMPLETED (the
// persisted Layout facts already advanced; the marker is bookkeeping the
// Recovery engine also derives from the Layout facts).
func (s *Store) MarkLayoutMigrationCompleted(ctx context.Context, wf model.WorkflowID, id, effectID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if wf == "" || id == "" || effectID == "" {
		return model.InvalidInputFault("layout migration completion requires matching migration and effect identities")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("mark layout migration: %w", s.mapSQLError(err))
	}
	defer func() { _ = tx.Rollback() }()
	now := s.now().UTC().Format(time.RFC3339Nano)
	res, err := tx.ExecContext(ctx,
		`UPDATE layout_migrations SET status = 'COMPLETED', completed_at = ?
		 WHERE id = ? AND workflow_id = ? AND status IN ('PREPARED','COMPLETED')`,
		now, id, string(wf))
	if err != nil {
		return fmt.Errorf("mark layout migration: %w", s.mapSQLError(err))
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return model.NewFault(model.CodeEvidenceSubjectChanged,
			"the layout migration completion identity/status no longer matches")
	}
	res, err = tx.ExecContext(ctx, `UPDATE effects SET status = 'RESULTED'
		WHERE id = ? AND workflow_id = ? AND kind = 'layout-migration' AND status = 'PENDING'`,
		effectID, string(wf))
	if err != nil {
		return fmt.Errorf("settle layout migration intent: %w", s.mapSQLError(err))
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return model.NewFault(model.CodeEvidenceSubjectChanged,
			"the layout migration effect identity/status no longer matches")
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("mark layout migration: %w", s.mapSQLError(err))
	}
	return nil
}

// ensureProject runs the Project identity check inside one transaction.
func ensureProject(ctx context.Context, q querier, project model.ProjectID) error {
	var n int
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM projects WHERE id = ?`, string(project)).Scan(&n); err != nil {
		return fmt.Errorf("project check: %w", err)
	}
	if n == 0 {
		return model.InvalidInputFault(fmt.Sprintf("project %s is not registered in the database", project))
	}
	return nil
}

func nextEventSeq(ctx context.Context, q querier) (uint64, error) {
	var next uint64
	if err := q.QueryRowContext(ctx, queryNextEventSeq).Scan(&next); err != nil {
		return 0, fmt.Errorf("next event seq: %w", err)
	}
	return next, nil
}

// eventPayload is the redacted, bounded payload of one authoritative Event
// (design 9.2): immutable references and bounded text only.
type eventPayload struct {
	Node    model.NodeID      `json:"node,omitempty"`
	Attempt *model.AttemptKey `json:"attempt,omitempty"`
	Code    string            `json:"code,omitempty"`
	Text    string            `json:"text,omitempty"`
}

func attemptKeyPtr(k model.AttemptKey) *model.AttemptKey {
	if k.Node == "" {
		return nil
	}
	return &k
}

// mapSQLError normalizes database failures into the model Fault contract:
// SQLITE_BUSY is bounded by the busy timeout and becomes a stable local
// contention Fault; constraint violations (unique identities, foreign
// keys) are invariant failures; anything else stays a database fault.
func (s *Store) mapSQLError(err error) error {
	if err == nil {
		return nil
	}
	var se *sqlite.Error
	if errors.As(err, &se) {
		switch se.Code() {
		case sqlite3.SQLITE_BUSY:
			return model.NewFault(model.CodeDatabaseMigrationFailed,
				"database is busy: local contention exceeded the busy timeout (SQLITE_BUSY)")
		case sqlite3.SQLITE_CONSTRAINT:
			return model.InvariantFault(fmt.Errorf("database constraint: %v", err))
		}
	}
	return model.InvariantFault(fmt.Errorf("database failure: %v", err))
}
