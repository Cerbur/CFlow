package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/security"
)

// ---------------------------------------------------------------------------
// fixtures
// ---------------------------------------------------------------------------

// fixtureProjectID is the opaque Project the fixture database registers.
const fixtureProjectID = "p-1"

// fixtureDecision creates the fixture Workflow. It mirrors the Kernel's
// create decision (design 6.2): identity comes from the input, the Event
// sequence claims State.NextEventSeq.
func fixtureDecision(state model.State) (model.Decision, error) {
	if state.Workflow.ID != "" {
		return model.Decision{}, errors.New("workflow already exists")
	}
	return model.Decision{
		Mutations: []model.Mutation{model.WorkflowMutation{
			ID: "wf-1", Project: fixtureProjectID,
			Stage: model.StageRequirementDiscussion, Runtime: model.RuntimePending,
			TargetBranch: "main", BaseCommit: "base-1",
		}},
		Events: []model.Event{{
			Seq: state.NextEventSeq, Kind: model.EventWorkflowCreated,
			Workflow: "wf-1", Text: "workflow created", At: state.Now,
		}},
	}, nil
}

// startDecision advances the fixture Workflow to RUNNING with a new Run,
// mirroring the Kernel's start decision.
func startDecision(state model.State) (model.Decision, error) {
	if state.Workflow.ID != "wf-1" || state.Workflow.Runtime != model.RuntimePending {
		return model.Decision{}, errors.New("workflow not pending")
	}
	return model.Decision{
		Mutations: []model.Mutation{
			model.WorkflowMutation{
				ID: state.Workflow.ID, Project: state.Workflow.Project,
				Stage: model.StageRequirementDiscussion, Runtime: model.RuntimeRunning,
				TargetBranch: state.Workflow.TargetBranch, BaseCommit: state.Workflow.BaseCommit,
				CancelIntent: state.Workflow.CancelIntent,
			},
			model.RunAppendMutation{Run: model.Run{ID: "run-1", Status: model.RunRunning,
				DispatchGate: true, StartedAt: state.Now}},
		},
		Events: []model.Event{
			{Seq: state.NextEventSeq, Kind: model.EventRunStarted, Workflow: "wf-1", Text: "run started", At: state.Now},
			{Seq: state.NextEventSeq + 1, Kind: model.EventWorkflowStarted, Workflow: "wf-1", Text: "workflow started", At: state.Now},
		},
	}, nil
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	return openTestStoreAt(t, filepath.Join(t.TempDir(), "cflow.db"))
}

func openTestStoreAt(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(context.Background(), OpenOptions{
		Path:         path,
		Workflow:     "wf-1",
		CflowVersion: "0.0.0-dev",
		busyTimeout:  2 * time.Second,
	})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	seedProjectRow(t, s)
	return s
}

// seedProjectRow registers the fixture Project exactly once; several
// tests open multiple Stores over one database file.
func seedProjectRow(t *testing.T, s *Store) {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM projects WHERE id = ?`, fixtureProjectID).Scan(&n); err != nil {
		t.Fatalf("project check: %v", err)
	}
	if n > 0 {
		return
	}
	seedProject(t, s)
}

// seedProject registers the fixture Project; the Store requires the Project
// row before any Event may reference it (events.project_id NOT NULL, PRD
// 核心数据库表).
func seedProject(t *testing.T, s *Store) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := s.db.Exec(`INSERT INTO projects
		(id, project_key, canonical_path, display_name, git_root, created_at, updated_at, last_opened_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		fixtureProjectID, fixtureProjectID, "/"+fixtureProjectID, fixtureProjectID, "/"+fixtureProjectID,
		now, now, now); err != nil {
		t.Fatalf("seed project: %v", err)
	}
}

// seedNodeAfter registers a Node row (the Decision Kernel has no Node
// append Mutation yet; Node insertion arrives with the Workflow Compiler).
// The Workflow row must exist first: nodes.workflow_id is a foreign key.
func seedNodeAfter(t *testing.T, s *Store, id string, kind model.NodeKind, status model.NodeStatus, budget int) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := s.db.Exec(`INSERT INTO nodes
		(id, workflow_id, node_type, status, max_retry_budget, created_at, updated_at)
		VALUES (?, 'wf-1', ?, ?, ?, ?, ?)`, id, kind, status, budget, now, now); err != nil {
		t.Fatalf("seed node: %v", err)
	}
}

func injectFailure(t *testing.T, s *Store, p FaultPoint) {
	t.Helper()
	if s.inject == nil {
		s.inject = map[FaultPoint]struct{}{}
	}
	s.inject[p] = struct{}{}
}

func mustView(t *testing.T, s *Store) StoreView {
	t.Helper()
	v, err := s.View(context.Background(), StoreQuery{})
	if err != nil {
		t.Fatalf("view: %v", err)
	}
	return v
}

func mustTransact(t *testing.T, s *Store, expected model.AggregateVersion, fn func(model.State) (model.Decision, error)) model.CommittedDecision {
	t.Helper()
	cd, err := s.Transact(context.Background(), expected, fn)
	if err != nil {
		t.Fatalf("transact: %v", err)
	}
	return cd
}

func assertFaultCode(t *testing.T, err error, want model.Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected fault %s, got nil error", want)
	}
	code, ok := model.CodeOf(err)
	if !ok {
		t.Fatalf("expected model fault %s, got %T: %v", want, err, err)
	}
	if code != want {
		t.Fatalf("fault code = %s, want %s (text: %v)", code, want, err)
	}
}

// ---------------------------------------------------------------------------
// atomicity and the transaction contract (brief Step 1)
// ---------------------------------------------------------------------------

func TestTransactCommitsStateAndEventsAtomically(t *testing.T) {
	s := openTestStore(t)
	injectFailure(t, s, FailBeforeCommit)
	_, err := s.Transact(context.Background(), 0, fixtureDecision)
	if err == nil {
		t.Fatal("expected injected failure")
	}
	view := mustView(t, s)
	if view.AggregateVersion != 0 || len(view.Events) != 0 {
		t.Fatalf("partial commit: %#v", view)
	}
}

func TestTransactCommitsStateAndEvents(t *testing.T) {
	s := openTestStore(t)
	cd := mustTransact(t, s, 0, fixtureDecision)
	if cd.Version != 1 {
		t.Fatalf("version = %d, want 1", cd.Version)
	}
	if cd.EventRange.From != 1 || cd.EventRange.To != 2 {
		t.Fatalf("event range = %v, want [1,2)", cd.EventRange)
	}
	view := mustView(t, s)
	if view.AggregateVersion != 1 {
		t.Fatalf("aggregate version = %d, want 1", view.AggregateVersion)
	}
	if view.State.Workflow.ID != "wf-1" || view.State.Workflow.Stage != model.StageRequirementDiscussion {
		t.Fatalf("state workflow = %#v", view.State.Workflow)
	}
	if len(view.Events) != 1 {
		t.Fatalf("events = %d, want 1", len(view.Events))
	}
	if view.Events[0].Seq != 1 || view.Events[0].Kind != model.EventWorkflowCreated {
		t.Fatalf("event = %#v", view.Events[0])
	}
	if view.NextEventSeq != 2 {
		t.Fatalf("next event seq = %d, want 2", view.NextEventSeq)
	}

	// A second decision applies on the returned version.
	cd2 := mustTransact(t, s, cd.Version, startDecision)
	if cd2.Version != 2 {
		t.Fatalf("version = %d, want 2", cd2.Version)
	}
	view = mustView(t, s)
	if view.State.Workflow.Runtime != model.RuntimeRunning {
		t.Fatalf("runtime = %s, want RUNNING", view.State.Workflow.Runtime)
	}
	if len(view.State.Runs) != 1 || view.State.Runs[0].Status != model.RunRunning || !view.State.Runs[0].DispatchGate {
		t.Fatalf("runs = %#v", view.State.Runs)
	}
	if len(view.Events) != 3 {
		t.Fatalf("events = %d, want 3", len(view.Events))
	}
	for i, e := range view.Events {
		if e.Seq != uint64(i+1) {
			t.Fatalf("event %d seq = %d, want %d (strictly increasing from 1)", i, e.Seq, i+1)
		}
	}
}

func TestTransactRollsBackWhenDecisionFails(t *testing.T) {
	s := openTestStore(t)
	_, err := s.Transact(context.Background(), 0, func(model.State) (model.Decision, error) {
		return model.Decision{}, errors.New("kernel rejected")
	})
	if err == nil || !strings.Contains(err.Error(), "kernel rejected") {
		t.Fatalf("err = %v, want kernel rejection", err)
	}
	view := mustView(t, s)
	if view.AggregateVersion != 0 || len(view.Events) != 0 || view.State.Workflow.ID != "" {
		t.Fatalf("decision failure left partial state: %#v", view)
	}
}

func TestTransactRejectsStaleAggregateVersion(t *testing.T) {
	s := openTestStore(t)
	mustTransact(t, s, 0, fixtureDecision)
	// A stale writer whose snapshot predates the commit must be rejected.
	_, err := s.Transact(context.Background(), 0, startDecision)
	assertFaultCode(t, err, model.CodeInvalidInput)
	view := mustView(t, s)
	if view.AggregateVersion != 1 {
		t.Fatalf("stale write changed version: %#v", view)
	}
	// The current version applies.
	mustTransact(t, s, 1, startDecision)
}

func TestTransactRejectsInvalidAggregateOutcome(t *testing.T) {
	s := openTestStore(t)
	// An Attempt that references a Node that does not exist violates the
	// model invariant "attempt has no Node"; the Store must not commit it.
	_, err := s.Transact(context.Background(), 0, func(state model.State) (model.Decision, error) {
		d, err := fixtureDecision(state)
		if err != nil {
			return model.Decision{}, err
		}
		d.Mutations = append(d.Mutations, model.AttemptAppendMutation{Attempt: model.Attempt{
			Key: model.AttemptKey{Node: "n-ghost", Number: 1}, Status: model.AttemptReady, StartedAt: state.Now,
		}})
		return d, nil
	})
	assertFaultCode(t, err, model.CodeStateInvariantViolation)
	view := mustView(t, s)
	if view.AggregateVersion != 0 || len(view.Events) != 0 {
		t.Fatalf("invalid aggregate committed: %#v", view)
	}
}

// ---------------------------------------------------------------------------
// Events and sequence authority (design 9.2)
// ---------------------------------------------------------------------------

func TestEventSequenceStrictlyIncreasingAcrossDecisions(t *testing.T) {
	s := openTestStore(t)
	var expected model.AggregateVersion
	for i := 0; i < 5; i++ {
		if i == 0 {
			expected = mustTransact(t, s, 0, fixtureDecision).Version
			continue
		}
		expected = mustTransact(t, s, expected, func(state model.State) (model.Decision, error) {
			d, err := startDecision(state)
			if err != nil {
				return model.Decision{}, err
			}
			// Duplicate start is rejected by this fixture, so mutate a
			// distinct field per step instead: append one finding.
			d = model.Decision{
				Mutations: []model.Mutation{model.FindingAppendMutation{Finding: model.Finding{
					ID: model.FindingID("f-" + string(rune('a'+i))), Code: model.CodePlanDrift,
					Scope: model.ScopePlan, Subject: "plan", Blocking: false,
					Text: "drift", Seq: state.NextEventSeq,
				}}},
				Events: []model.Event{{Seq: state.NextEventSeq, Kind: model.EventFindingOpened,
					Workflow: "wf-1", Text: "finding opened", At: state.Now}},
			}
			return d, nil
		}).Version
	}
	rows, err := s.db.Query(`SELECT sequence FROM events ORDER BY sequence`)
	if err != nil {
		t.Fatalf("query events: %v", err)
	}
	defer rows.Close()
	var prev uint64
	n := 0
	for rows.Next() {
		var seq uint64
		if err := rows.Scan(&seq); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if seq != prev+1 {
			t.Fatalf("event sequence gap: %d after %d", seq, prev)
		}
		prev, n = seq, n+1
	}
	if n != 5 { // 1 create + 4 findings
		t.Fatalf("event count = %d, want 5", n)
	}
}

func TestEventSequenceDatabaseAssignedAcrossWorkflows(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cflow.db")
	// Two stores bound to different aggregates share one database; the
	// authoritative Event sequence stays strictly increasing across both.
	sA := openTestStoreAt(t, path)
	sB := openTestStoreAt(t, path)
	// openTestStoreAt binds Workflow wf-1; open a store bound to wf-b.
	seedProjectRow(t, sB)
	sB.workflow = "wf-b"
	cdA := mustTransact(t, sA, 0, fixtureDecision)
	if cdA.EventRange.From != 1 {
		t.Fatalf("workflow A first event seq = %d, want 1", cdA.EventRange.From)
	}
	cdB := mustTransact(t, sB, 0, func(state model.State) (model.Decision, error) {
		if state.Workflow.ID != "" {
			return model.Decision{}, errors.New("workflow already exists")
		}
		return model.Decision{
			Mutations: []model.Mutation{model.WorkflowMutation{
				ID: "wf-b", Project: fixtureProjectID,
				Stage: model.StageRequirementDiscussion, Runtime: model.RuntimePending,
				TargetBranch: "main", BaseCommit: "base-1",
			}},
			Events: []model.Event{{Seq: state.NextEventSeq, Kind: model.EventWorkflowCreated,
				Workflow: "wf-b", Text: "workflow created", At: state.Now}},
		}, nil
	})
	// Workflow B claimed sequence 2 from its hydration; the database is the
	// authority and must hand out 2 as well (single writer serialization).
	if cdB.EventRange.From != 2 {
		t.Fatalf("workflow B first event seq = %d, want 2", cdB.EventRange.From)
	}
	cdA2 := mustTransact(t, sA, cdA.Version, startDecision)
	if cdA2.EventRange.From != 3 {
		t.Fatalf("workflow A second decision first seq = %d, want 3", cdA2.EventRange.From)
	}
	// The hydrated view reflects the database-assigned sequences.
	if len(mustView(t, sB).Events) != 1 || mustView(t, sB).Events[0].Seq != 2 {
		t.Fatalf("workflow B view = %#v", mustView(t, sB).Events)
	}
}

func TestAttemptIdentityNeverReused(t *testing.T) {
	s := openTestStore(t)
	mustTransact(t, s, 0, fixtureDecision)
	seedNodeAfter(t, s, "n-1", model.NodeAgentTask, model.NodePending, 2)
	appendAttempt := func(n int) error {
		view := mustView(t, s)
		_, err := s.Transact(context.Background(), view.AggregateVersion, func(state model.State) (model.Decision, error) {
			return model.Decision{
				Mutations: []model.Mutation{model.AttemptAppendMutation{Attempt: model.Attempt{
					Key:    model.AttemptKey{Node: "n-1", Number: model.AttemptNumber(n)},
					Status: model.AttemptReady, StartedAt: state.Now,
				}}},
				Events: []model.Event{{Seq: state.NextEventSeq, Kind: model.EventAttemptCreated,
					Workflow: "wf-1", Text: "attempt created", At: state.Now}},
			}, nil
		})
		return err
	}
	if err := appendAttempt(1); err != nil {
		t.Fatalf("append attempt 1: %v", err)
	}
	// The unique (node_id, attempt_number) constraint must reject a reused
	// number and leave the aggregate untouched.
	err := appendAttempt(1)
	if err == nil {
		t.Fatal("expected duplicate attempt number rejection")
	}
	if code, ok := model.CodeOf(err); !ok || code != model.CodeStateInvariantViolation {
		t.Fatalf("duplicate attempt error = %v, want invariant fault", err)
	}
	view := mustView(t, s)
	if len(view.State.Attempts) != 1 {
		t.Fatalf("attempts = %d, want 1 (duplicate committed?)", len(view.State.Attempts))
	}
	if len(view.Events) != 2 {
		t.Fatalf("events = %d, want 2 (duplicate committed?)", len(view.Events))
	}
}

// ---------------------------------------------------------------------------
// foreign keys and orphan references
// ---------------------------------------------------------------------------

func TestWorkflowRequiresRegisteredProject(t *testing.T) {
	s := openTestStore(t)
	_, err := s.Transact(context.Background(), 0, func(state model.State) (model.Decision, error) {
		d, err := fixtureDecision(state)
		if err != nil {
			return model.Decision{}, err
		}
		wm := d.Mutations[0].(model.WorkflowMutation)
		wm.Project = "p-missing"
		d.Mutations[0] = wm
		return d, nil
	})
	if err == nil {
		t.Fatal("expected unregistered-project rejection")
	}
	view := mustView(t, s)
	if view.AggregateVersion != 0 || len(view.Events) != 0 {
		t.Fatalf("unregistered project committed: %#v", view)
	}
}

// ---------------------------------------------------------------------------
// managed process rows must stay visible to hydration (review fix)
// ---------------------------------------------------------------------------

// seedSessionAfter registers a Session row once the Workflow row exists.
func seedSessionAfter(t *testing.T, s *Store, id string, workflow string, purpose model.AgentPurpose) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := s.db.Exec(`INSERT INTO sessions
		(id, workflow_id, purpose, status, started_at)
		VALUES (?, ?, ?, 'ACTIVE', ?)`, id, workflow, purpose, now); err != nil {
		t.Fatalf("seed session: %v", err)
	}
}

// TestProcessAppendRequiresSession: the Store rejects a session-less
// process at commit — the persistence layer must never accept a shape its
// own hydration (queryProcesses resolves rows through the Session's
// workflow) cannot return.
func TestProcessAppendRequiresSession(t *testing.T) {
	s := openTestStore(t)
	mustTransact(t, s, 0, fixtureDecision)
	_, err := s.Transact(context.Background(), 1, func(state model.State) (model.Decision, error) {
		return model.Decision{
			Mutations: []model.Mutation{model.ProcessAppendMutation{Process: model.ProcessRecord{
				ID: "proc-1", Status: model.ProcessStatusRunning, StartedAt: state.Now,
			}}},
			Events: []model.Event{{Seq: state.NextEventSeq, Kind: model.EventRunStarted,
				Workflow: "wf-1", Text: "process started", At: state.Now}},
		}, nil
	})
	assertFaultCode(t, err, model.CodeStateInvariantViolation)
	view := mustView(t, s)
	if len(view.State.Processes) != 0 || len(view.Events) != 1 || view.AggregateVersion != 1 {
		t.Fatalf("session-less process committed: %#v", view)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM managed_processes`).Scan(&n); err != nil {
		t.Fatalf("process rows: %v", err)
	}
	if n != 0 {
		t.Fatalf("session-less process row persisted: %d", n)
	}
}

// TestProcessAppendRejectsCrossWorkflowSession: a process bound to another
// workflow's Session would commit and then never hydrate into this
// aggregate; the Store rejects it.
func TestProcessAppendRejectsCrossWorkflowSession(t *testing.T) {
	s := openTestStore(t)
	mustTransact(t, s, 0, fixtureDecision)
	// A sibling Workflow (row + Session) the process must not bind to.
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := s.db.Exec(`INSERT INTO workflows
		(id, project_id, stage, runtime_status, created_at, updated_at)
		VALUES ('wf-other', 'p-1', 'REQUIREMENT_DISCUSSION', 'PENDING', ?, ?)`, now, now); err != nil {
		t.Fatalf("seed sibling workflow: %v", err)
	}
	seedSessionAfter(t, s, "sess-other", "wf-other", model.PurposePlanning)
	_, err := s.Transact(context.Background(), 1, func(state model.State) (model.Decision, error) {
		return model.Decision{
			Mutations: []model.Mutation{model.ProcessAppendMutation{Process: model.ProcessRecord{
				ID: "proc-1", Session: "sess-other", Status: model.ProcessStatusRunning, StartedAt: state.Now,
			}}},
			Events: []model.Event{{Seq: state.NextEventSeq, Kind: model.EventRunStarted,
				Workflow: "wf-1", Text: "process started", At: state.Now}},
		}, nil
	})
	assertFaultCode(t, err, model.CodeStateInvariantViolation)
	view := mustView(t, s)
	if len(view.State.Processes) != 0 || len(view.Events) != 1 {
		t.Fatalf("cross-workflow process committed: %#v", view)
	}
}

// TestProcessAppendRoundTripsThroughView: a session-bound process commits
// and hydrates back into the aggregate on the next View.
func TestProcessAppendRoundTripsThroughView(t *testing.T) {
	s := openTestStore(t)
	mustTransact(t, s, 0, fixtureDecision)
	seedSessionAfter(t, s, "sess-1", "wf-1", model.PurposeImplementation)
	mustTransact(t, s, 1, func(state model.State) (model.Decision, error) {
		return model.Decision{
			Mutations: []model.Mutation{model.ProcessAppendMutation{Process: model.ProcessRecord{
				ID: "proc-1", Session: "sess-1", Purpose: model.PurposeImplementation,
				Status: model.ProcessStatusRunning, StartedAt: state.Now,
			}}},
			Events: []model.Event{{Seq: state.NextEventSeq, Kind: model.EventRunStarted,
				Workflow: "wf-1", Text: "process started", At: state.Now}},
		}, nil
	})
	view := mustView(t, s)
	if len(view.State.Processes) != 1 {
		t.Fatalf("processes = %d, want 1 (committed process invisible to hydration)", len(view.State.Processes))
	}
	p := view.State.Processes[0]
	if p.ID != "proc-1" || p.Session != "sess-1" || p.Status != model.ProcessStatusRunning ||
		p.Purpose != model.PurposeImplementation {
		t.Fatalf("process = %#v", p)
	}
}

func TestOrphanSessionReferenceRejected(t *testing.T) {
	s := openTestStore(t)
	mustTransact(t, s, 0, fixtureDecision)
	seedNodeAfter(t, s, "n-1", model.NodeAgentTask, model.NodePending, 2)
	// An Attempt bound to a Session that was never persisted is an orphan
	// reference; the foreign key must reject it and roll back everything.
	_, err := s.Transact(context.Background(), 1, func(state model.State) (model.Decision, error) {
		return model.Decision{
			Mutations: []model.Mutation{model.AttemptAppendMutation{Attempt: model.Attempt{
				Key: model.AttemptKey{Node: "n-1", Number: 1}, Session: "session-orphan",
				Status: model.AttemptReady, StartedAt: state.Now,
			}}},
			Events: []model.Event{{Seq: state.NextEventSeq, Kind: model.EventAttemptCreated,
				Workflow: "wf-1", Text: "attempt created", At: state.Now}},
		}, nil
	})
	if err == nil {
		t.Fatal("expected foreign-key rejection")
	}
	view := mustView(t, s)
	if view.AggregateVersion != 1 || len(view.State.Attempts) != 0 {
		t.Fatalf("orphan reference committed: %#v", view)
	}
}

// ---------------------------------------------------------------------------
// busy handling (design 9.2: bounded, stable local contention Fault)
// ---------------------------------------------------------------------------

func TestBusyTimeoutReturnsBoundedContentionFault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cflow.db")
	s := openTestStoreAt(t, path)
	// A second writer that must not wait long: pin the pool to one
	// connection so the pragma below reaches the connection Transact uses.
	sBusy := openTestStoreAt(t, path)
	sBusy.db.SetMaxOpenConns(1)
	if _, err := sBusy.db.Exec(`PRAGMA busy_timeout = 100`); err != nil {
		t.Fatalf("set busy timeout: %v", err)
	}

	holder := mustConn(t, s)
	if _, err := holder.ExecContext(context.Background(), `BEGIN IMMEDIATE`); err != nil {
		t.Fatalf("begin holder tx: %v", err)
	}
	defer holder.ExecContext(context.Background(), `ROLLBACK`)
	defer holder.Close()

	start := time.Now()
	_, err := sBusy.Transact(context.Background(), 0, fixtureDecision)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected contention fault")
	}
	code, ok := model.CodeOf(err)
	if !ok {
		t.Fatalf("expected model fault, got %T: %v", err, err)
	}
	if code != model.CodeDatabaseMigrationFailed {
		t.Fatalf("contention fault code = %s, want %s (stable local contention Fault)", code, model.CodeDatabaseMigrationFailed)
	}
	if elapsed < 80*time.Millisecond || elapsed > 2*time.Second {
		t.Fatalf("busy wait = %v, want bounded by the busy timeout (no unbounded loop)", elapsed)
	}
	// After the holder releases, the same store works.
	if _, err := holder.ExecContext(context.Background(), `COMMIT`); err != nil {
		t.Fatalf("release holder tx: %v", err)
	}
	mustTransact(t, sBusy, 0, fixtureDecision)
}

func mustConn(t *testing.T, s *Store) *sql.Conn {
	t.Helper()
	conn, err := s.db.Conn(context.Background())
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	return conn
}

// ---------------------------------------------------------------------------
// read-only opens (PRD: read commands never migrate)
// ---------------------------------------------------------------------------

func TestReadOnlyOpenOfMissingDatabaseIsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.db")
	s, err := Open(context.Background(), OpenOptions{Path: path, ReadOnly: true, CflowVersion: "0.0.0-dev"})
	if err != nil {
		t.Fatalf("read-only open of missing database: %v", err)
	}
	defer s.Close()
	if s.exists {
		t.Fatal("read-only open must not create the database file")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("database file created by read-only open: %v", err)
	}
	view := mustView(t, s)
	if view.AggregateVersion != 0 || len(view.Events) != 0 || view.State.Workflow.ID != "" {
		t.Fatalf("empty view expected: %#v", view)
	}
	_, err = s.Transact(context.Background(), 0, fixtureDecision)
	if err == nil {
		t.Fatal("read-only store must reject Transact")
	}
}

// ---------------------------------------------------------------------------
// Effect Intents (design 6.2: committed atomically, never executed inline)
// ---------------------------------------------------------------------------

func TestEffectIntentCommitsWithDecisionAndSurfacesAsPending(t *testing.T) {
	s := openTestStore(t)
	ref := model.ArtifactRef{Workflow: "wf-1", Type: model.ArtifactPlan, Revision: 1, Hash: "h1"}
	mustTransact(t, s, 0, func(state model.State) (model.Decision, error) {
		d, err := fixtureDecision(state)
		if err != nil {
			return model.Decision{}, err
		}
		d.Effect = model.ArtifactWriteIntent{Ref: ref}
		return d, nil
	})
	view := mustView(t, s)
	if len(view.PendingEffects) != 1 {
		t.Fatalf("pending effects = %d, want 1", len(view.PendingEffects))
	}
	got, ok := view.PendingEffects[0].Intent.(model.ArtifactWriteIntent)
	if !ok || got.Ref != ref {
		t.Fatalf("pending effect = %#v, want ArtifactWriteIntent{%v}", view.PendingEffects[0].Intent, ref)
	}
	if view.PendingEffects[0].DecisionVersion != 1 {
		t.Fatalf("pending effect decision version = %d, want 1", view.PendingEffects[0].DecisionVersion)
	}
}

func TestFailedEffectIntentCommitLeavesNoPendingEffect(t *testing.T) {
	s := openTestStore(t)
	injectFailure(t, s, FailBeforeCommit)
	_, err := s.Transact(context.Background(), 0, func(state model.State) (model.Decision, error) {
		d, err := fixtureDecision(state)
		if err != nil {
			return model.Decision{}, err
		}
		d.Effect = model.ArtifactWriteIntent{Ref: model.ArtifactRef{Workflow: "wf-1", Type: model.ArtifactPlan, Revision: 1, Hash: "h1"}}
		return d, nil
	})
	if err == nil {
		t.Fatal("expected injected failure")
	}
	view := mustView(t, s)
	if len(view.PendingEffects) != 0 || len(view.Events) != 0 || view.AggregateVersion != 0 {
		t.Fatalf("partial effect commit: %#v", view)
	}
}

// ---------------------------------------------------------------------------
// hydration round-trip
// ---------------------------------------------------------------------------

func TestHydrationRoundTripsAggregate(t *testing.T) {
	s := openTestStore(t)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	mustTransact(t, s, 0, fixtureDecision)

	// Seed the remaining aggregate rows directly: the Kernel has no Node
	// append Mutation yet, so nodes enter the database with the Workflow
	// Compiler; hydration must still reconstruct them faithfully.
	seed := []string{
		`INSERT INTO tasks (id, workflow_id, spec_id, title, branch_name, created_at, updated_at) VALUES ('t-1','wf-1','s-1','task','task/n-1','` + now + `','` + now + `')`,
		`INSERT INTO nodes (id, workflow_id, task_id, node_type, status, max_retry_budget, created_at, updated_at) VALUES ('n-1','wf-1','t-1','agent-task','RUNNING',3,'` + now + `','` + now + `')`,
		`INSERT INTO sessions (id, workflow_id, purpose, status, started_at) VALUES ('sess-1','wf-1','planning','ACTIVE','` + now + `')`,
		`INSERT INTO node_attempts (id, node_id, attempt_number, status, session_id, start_head_commit, start_dirty_fingerprint, started_at, evidence_manifest_json, retry_budget_charged) VALUES ('n-1#1','n-1',1,'RUNNING','sess-1','head-1','fp-1','` + now + `','[{"kind":"commit","hash":"abc123","subject":"n-1"}]',0)`,
		`INSERT INTO runs (id, workflow_id, status, dispatch_gate, quiesce_snapshot_json, started_at) VALUES ('run-9','wf-1','RUNNING',1,'[{"Node":"n-1","Number":1}]','` + now + `')`,
		`INSERT INTO findings (id, project_id, workflow_id, code, severity, status, scope, subject, finding_text, seq, created_at) VALUES ('f-1','p-1','wf-1','PLAN_DRIFT','BLOCKING','OPEN','PLAN','plan','drifted',2,'` + now + `')`,
		`INSERT INTO approvals (id, workflow_id, gate_type, decision, seq, plan_revision, plan_sha256, created_at) VALUES ('approval-1','wf-1','PLAN','APPROVE',2,1,'plan-hash','` + now + `')`,
		`INSERT INTO managed_processes (id, session_id, process_type, status, started_at) VALUES ('proc-1','sess-1','implementation','RUNNING','` + now + `')`,
		`INSERT INTO branch_quarantines (id, workflow_id, branch_name, head_commit, audit_ref, reason_code, created_at) VALUES ('q-1','wf-1','task/bad','bad-head','refs/cflow/quarantine/q-1','DIRTY_TASK_WORKTREE','` + now + `')`,
		`INSERT INTO workflow_artifact_refs (workflow_id, artifact_type, active_revision, artifact_path, artifact_sha256, updated_at) VALUES ('wf-1','plan',1,'plan/plan-1.md','plan-hash','` + now + `')`,
		`INSERT INTO git_commit_preflights (id, workflow_id, revision, repository_context, git_version, commit_policy_fingerprint, identity_json, signing_policy_json, probe_status, artifact_path, artifact_sha256, created_at) VALUES ('pre-1','wf-1',1,'repo','git-x','fp-pre','{}','{}','PASS','preflight/pre-1.json','pre-hash','` + now + `')`,
		`INSERT INTO apply_attempts (id, workflow_id, attempt_number, status, target_head_at_start, integration_head, git_commit_preflight_revision, git_commit_preflight_sha256, git_commit_policy_fingerprint, started_at) VALUES ('apply-1','wf-1',1,'SUCCEEDED','tgt-1','int-1',1,'pre-hash','fp-pre','` + now + `')`,
		`INSERT INTO cleanup_attempts (id, workflow_id, status, plan_path, plan_sha256, started_at) VALUES ('cleanup-1','wf-1','SUCCEEDED','cleanup/cleanup-plan-cleanup-1.json','m-hash','` + now + `')`,
		`INSERT INTO cleanup_items (id, cleanup_attempt_id, ordinal, target_type, canonical_path, expected_branch, expected_head_commit, expected_fingerprint, status, error_code) VALUES ('cleanup-1-0','cleanup-1',0,'worktree','/abs/task-a','task/a','head-a','fp-a','COMPLETED','')`,
	}
	for _, stmt := range seed {
		if _, err := s.db.Exec(stmt); err != nil {
			t.Fatalf("seed: %v (%s)", err, stmt)
		}
	}

	view := mustView(t, s)
	st := view.State
	if len(st.Nodes) != 1 || st.Nodes["n-1"].Kind != model.NodeAgentTask || st.Nodes["n-1"].RetryBudget != 3 {
		t.Fatalf("nodes = %#v", st.Nodes)
	}
	if st.Nodes["n-1"].Branch != "task/n-1" {
		t.Fatalf("node branch = %q, want task/n-1 (from task row)", st.Nodes["n-1"].Branch)
	}
	att := st.Attempts[model.AttemptKey{Node: "n-1", Number: 1}]
	if att == nil || att.Session != "sess-1" || att.StartHead != "head-1" || att.Status != model.AttemptRunning {
		t.Fatalf("attempt = %#v", att)
	}
	if len(att.Evidence) != 1 || att.Evidence[0].Hash != "abc123" {
		t.Fatalf("attempt evidence = %#v", att.Evidence)
	}
	if len(st.Runs) != 1 || st.Runs[0].ID != "run-9" || !st.Runs[0].DispatchGate {
		t.Fatalf("runs = %#v", st.Runs)
	}
	if len(st.Runs[0].QuiesceSnapshot) != 1 || st.Runs[0].QuiesceSnapshot[0] != (model.AttemptKey{Node: "n-1", Number: 1}) {
		t.Fatalf("quiesce snapshot = %#v", st.Runs[0].QuiesceSnapshot)
	}
	if len(st.Findings) != 1 || !st.Findings[0].Blocking || st.Findings[0].Code != model.CodePlanDrift {
		t.Fatalf("findings = %#v", st.Findings)
	}
	if len(st.Approvals) != 1 || st.Approvals[0].Kind != model.ApprovalPlan ||
		len(st.Approvals[0].Refs) != 1 || st.Approvals[0].Refs[0].Hash != "plan-hash" {
		t.Fatalf("approvals = %#v", st.Approvals)
	}
	if len(st.Processes) != 1 || st.Processes[0].ID != "proc-1" || st.Processes[0].Purpose != model.PurposeImplementation {
		t.Fatalf("processes = %#v", st.Processes)
	}
	if len(st.Quarantines) != 1 || st.Quarantines[0].Branch != "task/bad" || st.Quarantines[0].Code != model.CodeDirtyTaskWorktree {
		t.Fatalf("quarantines = %#v", st.Quarantines)
	}
	if st.Plan == nil || st.Plan.Revision != 1 || st.Plan.Hash != "plan-hash" || st.Plan.Artifact.Type != model.ArtifactPlan {
		t.Fatalf("plan = %#v", st.Plan)
	}
	if st.Workflow.ExecutionFacts == nil || st.Workflow.ExecutionFacts.PlanHash != "plan-hash" ||
		st.Workflow.ExecutionFacts.CommitPolicyHash != "pre-hash" || st.Workflow.ExecutionFacts.Fingerprint != "fp-pre" {
		t.Fatalf("execution facts = %#v", st.Workflow.ExecutionFacts)
	}
	if len(st.ApplyAttempts) != 1 || st.ApplyAttempts[0].TargetHead != "tgt-1" ||
		st.ApplyAttempts[0].PreflightHash != "pre-hash" {
		t.Fatalf("apply attempts = %#v", st.ApplyAttempts)
	}
	if len(st.CleanupAttempts) != 1 || st.CleanupAttempts[0].Manifest.Hash != "m-hash" ||
		len(st.CleanupAttempts[0].Items) != 1 || st.CleanupAttempts[0].Items[0].Kind != model.CleanupWorktree {
		t.Fatalf("cleanup attempts = %#v", st.CleanupAttempts)
	}
}

// ---------------------------------------------------------------------------
// concurrent writers (brief Step 5: 20 repeated writers)
// ---------------------------------------------------------------------------

func TestConcurrentWritersProduceNoDuplicatesOrPartialTransitions(t *testing.T) {
	s := openTestStore(t)
	mustTransact(t, s, 0, fixtureDecision)
	seedNodeAfter(t, s, "n-1", model.NodeAgentTask, model.NodePending, 100)

	const writers = 20
	var wg sync.WaitGroup
	errs := make([]error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Each writer appends one uniquely numbered Attempt, retrying
			// against the freshest committed aggregate version.
			for {
				view, err := s.View(context.Background(), StoreQuery{})
				if err != nil {
					errs[i] = err
					return
				}
				_, err = s.Transact(context.Background(), view.AggregateVersion, func(state model.State) (model.Decision, error) {
					return model.Decision{
						Mutations: []model.Mutation{model.AttemptAppendMutation{Attempt: model.Attempt{
							Key:    model.AttemptKey{Node: "n-1", Number: model.AttemptNumber(i + 1)},
							Status: model.AttemptReady, StartedAt: state.Now,
						}}},
						Events: []model.Event{{Seq: state.NextEventSeq, Kind: model.EventAttemptCreated,
							Workflow: "wf-1", Text: "attempt created", At: state.Now}},
					}, nil
				})
				if err == nil {
					return
				}
				if code, ok := model.CodeOf(err); ok && code == model.CodeInvalidInput {
					continue // stale version: re-snapshot and retry
				}
				errs[i] = err
				return
			}
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("writer %d: %v", i, err)
		}
	}

	view := mustView(t, s)
	if len(view.State.Attempts) != writers {
		t.Fatalf("attempts = %d, want %d", len(view.State.Attempts), writers)
	}
	numbers := map[int]bool{}
	for key := range view.State.Attempts {
		if key.Node != "n-1" || numbers[int(key.Number)] {
			t.Fatalf("duplicate or foreign attempt key %v", key)
		}
		numbers[int(key.Number)] = true
	}
	if len(view.Events) != 1+writers {
		t.Fatalf("events = %d, want %d (no duplicates, no partial transitions)", len(view.Events), 1+writers)
	}
	for i, e := range view.Events {
		if e.Seq != uint64(i+1) {
			t.Fatalf("event %d seq = %d, want %d", i, e.Seq, i+1)
		}
	}
	if view.AggregateVersion != model.AggregateVersion(1+writers) {
		t.Fatalf("aggregate version = %d, want %d", view.AggregateVersion, 1+writers)
	}
}

// ---------------------------------------------------------------------------
// store hygiene
// ---------------------------------------------------------------------------

func TestStoreRejectsMutationOnReadOnlyStore(t *testing.T) {
	s := openTestStore(t)
	s.readOnly = true
	_, err := s.Transact(context.Background(), 0, fixtureDecision)
	if err == nil {
		t.Fatal("expected read-only rejection")
	}
}

// TestBackupLayoutMigrationSelfHealsStaleZeroByteScratch proves a failed
// online backup cannot wedge the migration retry forever: BackupLayoutMigration
// creates the owner-only target before the online backup, so a failure leaves
// a 0-byte file that passes PRAGMA integrity_check as an empty database and
// would otherwise record Size 0 evidence that readMigrationManifest rejects.
// The next Prepare must recognize the 0-byte file as CFlow's own incomplete
// scratch, remove it, and regenerate a real backup with Size > 0.
func TestBackupLayoutMigrationSelfHealsStaleZeroByteScratch(t *testing.T) {
	s := openTestStore(t)
	mustTransact(t, s, 0, fixtureDecision)
	// The scratch path must not resolve through a symlink (macOS temp roots
	// resolve /var -> /private/var) and its parent must be owner-only, like
	// every guarded CFLOW_HOME path.
	tempRoot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp root: %v", err)
	}
	parent := filepath.Join(tempRoot, "managed")
	if err := os.MkdirAll(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(parent, "migration.db.backup")
	// A failed attempt left the owner-only scratch file before the online
	// backup streamed any pages (CreateSensitiveFile succeeded, the backup
	// never did).
	f, err := security.CreateSensitiveFile(path)
	if err != nil {
		t.Fatalf("create stale scratch: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 0 {
		t.Fatalf("fixture scratch size = %d, want 0", info.Size())
	}
	// The next Prepare must self-heal: the backup is regenerated with real
	// content and the evidence carries a positive size and a real hash.
	backup, err := s.BackupLayoutMigration(context.Background(), path)
	if err != nil {
		t.Fatalf("backup after stale 0-byte scratch: %v", err)
	}
	if backup.Size <= 0 {
		t.Fatalf("regenerated backup size = %d, want > 0", backup.Size)
	}
	if backup.SHA256 == "" {
		t.Fatal("regenerated backup carries no sha256")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(body)) != backup.Size {
		t.Fatalf("backup file size = %d, evidence = %d", len(body), backup.Size)
	}
}
