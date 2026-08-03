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
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"sync"
	"time"

	sqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"cflow.local/cflow/internal/model"
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
	return view, nil
}

// ---------------------------------------------------------------------------
// Transact
// ---------------------------------------------------------------------------

// Transact applies one Decision to the bound aggregate atomically: the
// state mutations, the authoritative Events, and the at most one Effect
// Intent commit in one transaction (design 9.2). expected must equal the
// current aggregate version; a stale writer is rejected. Event sequences
// are assigned by the database transaction, strictly increasing, and
// returned as the half-open EventRange [From, To).
func (s *Store) Transact(ctx context.Context, expected model.AggregateVersion, fn func(model.State) (model.Decision, error)) (model.CommittedDecision, error) {
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
		if _, err := tx.ExecContext(ctx, `INSERT INTO effects
			(id, workflow_id, kind, payload_json, status, decision_version, created_at)
			VALUES (?, ?, ?, ?, 'PENDING', ?, ?)`,
			fmt.Sprintf("effect-%d", effectID+1), workflow, kind, string(payload),
			expected+1, now.Format(time.RFC3339Nano)); err != nil {
			return model.CommittedDecision{}, s.mapSQLError(err)
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
