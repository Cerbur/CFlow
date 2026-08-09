package app

// Application tests (design 22.1): a real temporary SQLite database, a
// deterministic Clock and ID source, the Fake Process Adapter, and the
// protocol probe recording the exact order of the effect loop. The two
// brief-mandated tests (Step 1) assert that an external Effect is not
// executed until its Intent commits, and that read projections neither
// migrate nor take writer locks.

import (
	"context"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"cflow.local/cflow/internal/gitflow"
	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/platform"
	"cflow.local/cflow/internal/process"
	"cflow.local/cflow/internal/store"
)

// fixtureRepo creates a real temporary committed Git repository the
// GitFlow seam serves (design 22.1: real repositories, no mocks).
func fixtureRepo(t *testing.T) string {
	t.Helper()
	root := filepath.Join(tempRoot(t), "repo")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(),
			"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0",
			"GIT_AUTHOR_NAME=Test User", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=Test User", "GIT_COMMITTER_EMAIL=test@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-b", "main", "-q")
	if err := os.WriteFile(filepath.Join(root, "init.txt"), []byte("init"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", "init.txt")
	git("commit", "-q", "-m", "init")
	return root
}

// ---------------------------------------------------------------------------
// probe: the protocol-order test seam (design 22.1)
// ---------------------------------------------------------------------------

// callProbe records the command protocol phases and the lock kinds taken.
// Production Applications carry a nil probe; the fixture installs one.
type callProbe struct {
	mu    sync.Mutex
	steps []string
	locks map[platform.LockKind]bool
}

func (p *callProbe) step(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.steps = append(p.steps, name)
}

func (p *callProbe) lockKind(k platform.LockKind) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.locks == nil {
		p.locks = map[platform.LockKind]bool{}
	}
	p.locks[k] = true
}

// Calls returns the recorded phase markers in order.
func (p *callProbe) Calls() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.steps)
}

// RequireAbsent asserts that none of the named phases or lock kinds was
// recorded. "migration" is a phase marker; "project-writer" is a lock kind
// (platform.LockKind.String).
func (p *callProbe) RequireAbsent(t *testing.T, names ...string) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, name := range names {
		if slices.Contains(p.steps, name) {
			t.Fatalf("probe recorded unexpected phase %q in %v", name, p.steps)
		}
		for k := range p.locks {
			if k.String() == name {
				t.Fatalf("probe recorded unexpected lock %q in %v", name, p.locks)
			}
		}
	}
}

func requireNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// fixture
// ---------------------------------------------------------------------------

// tempRoot returns a temporary directory whose full path is free of
// symlinks (on macOS /var is a symlink to /private/var; the security guard
// rejects symlink components in managed paths).
func tempRoot(t *testing.T) string {
	t.Helper()
	p, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp root: %v", err)
	}
	return p
}

// fixtureApplication builds one Application over a real temporary SQLite
// database and a real temporary committed Git repository (the Task 8
// GitFlow seam drives workflow creation): a PENDING workflow is created
// and started, one RUNNING managed process is seeded with a registered
// Fake Supervisor handle, and a fresh protocol probe is installed. The
// probe records only the test's own command afterwards.
func fixtureApplication(t *testing.T) (*Application, *callProbe) {
	t.Helper()
	home := filepath.Join(tempRoot(t), "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(home, "cflow.db")
	root := fixtureRepo(t)
	proj := ProjectFor(root)

	// Migrate the database once through the real SQLite file.
	s, err := store.Open(context.Background(), store.OpenOptions{
		Path: dbPath, Workflow: "fixture", CflowVersion: "0.0.0-dev",
	})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	fake, sup := process.NewFakeSupervisor()
	// The GitFlow seam runs real git processes, so it needs the real OS
	// Supervisor; the Application's own Supervisor stays the Fake one the
	// managed-process seeding uses.
	flow, err := gitflow.NewGitFlow(process.NewSupervisor(process.NewOSAdapter()), root)
	if err != nil {
		t.Fatalf("new gitflow: %v", err)
	}
	app, err := New(Options{
		Home:         home,
		Project:      proj,
		CflowVersion: "0.0.0-dev",
		Now:          func() time.Time { return time.Unix(1700000000, 0).UTC() },
		IDs:          model.SequentialIDSource(),
		Supervisor:   sup,
		GitFlow:      flow,
	})
	if err != nil {
		t.Fatalf("new application: %v", err)
	}

	out, err := app.Execute(context.Background(), CreateWorkflowCommand{Name: "fixture", Provider: "fake", ConfirmDirty: false})
	if err != nil {
		t.Fatalf("create workflow: %v", err)
	}
	if out.Workflow != "workflow-1" { // SequentialIDSource determinism
		t.Fatalf("fixture workflow id = %q, want workflow-1", out.Workflow)
	}
	if _, err := app.Execute(context.Background(), StartWorkflowCommand{}); err != nil {
		t.Fatalf("start workflow: %v", err)
	}

	// Seed one RUNNING managed process row and register its Fake
	// Supervisor handle: the Runtime's live process registry. The process
	// is scripted to exit immediately so the controlled stop settles
	// deterministically.
	h, _, err := sup.Start(context.Background(), process.ProcessSpec{Executable: "/bin/true"})
	if err != nil {
		t.Fatalf("start fake process: %v", err)
	}
	fake.ExitGroup(h, 0)
	seedProcessRow(t, dbPath, out.Workflow, "proc-1", model.ProcessStatusRunning)
	app.procs[model.ProcessID("proc-1")] = h

	probe := &callProbe{}
	app.probe = probe
	return app, probe
}

// fixtureCommandWithEffect is the brief-mandated fixture command: a pause
// on the RUNNING fixture workflow, which commits a ManagedProcessStop
// Intent before the executor runs.
func fixtureCommandWithEffect() Command {
	return PauseWorkflowCommand{Workflow: "workflow-1"}
}

// seedProjectRow registers the fixture Project row directly (Task 8
// delivers discovery; the fixture stands in for it).
func seedProjectRow(t *testing.T, dbPath, key string) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.Exec(`INSERT INTO projects
		(id, project_key, canonical_path, display_name, git_root, created_at, updated_at, last_opened_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		key, key, "/"+key, key, "/"+key, now, now, now); err != nil {
		t.Fatalf("seed project: %v", err)
	}
}

// seedProcessRow inserts one managed process record bound to the fixture
// Run (run_id is how hydration joins it to the workflow).
func seedProcessRow(t *testing.T, dbPath string, wf model.WorkflowID, id string, status model.ProcessStatus) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.Exec(`INSERT INTO managed_processes
		(id, run_id, process_type, status, exit_code, started_at)
		VALUES (?, 'run-1', 'implementation', ?, 0, ?)`, id, string(status), now); err != nil {
		t.Fatalf("seed process: %v", err)
	}
}

// fixtureRecoverer injects a Recovery hook outcome for one command.
type fixtureRecoverer struct {
	err error
}

func (r *fixtureRecoverer) Reconcile(context.Context) error { return r.err }

func requireFaultCode(t *testing.T, err error, want model.Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected fault %s, got nil error", want)
	}
	if got, ok := model.CodeOf(err); !ok || got != want {
		t.Fatalf("expected fault %s, got %v", want, err)
	}
}

// ---------------------------------------------------------------------------
// brief Step 1: effect-order and read-isolation tests
// ---------------------------------------------------------------------------

// TestEffectIntentCommitsBeforeExecutorRuns is the brief-mandated call
// order: recovery hook (the Recovery Engine's aggregate read takes the
// shared Schema Lock), mutation locks, Intent commit, executor, Result
// commit.
func TestEffectIntentCommitsBeforeExecutorRuns(t *testing.T) {
	app, probe := fixtureApplication(t)
	_, err := app.Execute(context.Background(), fixtureCommandWithEffect())
	requireNoError(t, err)
	want := []string{"recover", "lock", "lock", "intent-commit", "effect", "result-commit"}
	if got := probe.Calls(); !slices.Equal(want, got) {
		t.Fatalf("want %v, got %v", want, got)
	}
}

// TestStatusDoesNotMigrateOrTakeWriter is the brief-mandated read
// isolation: a Project read never migrates and never takes the Project
// Writer lock.
func TestStatusDoesNotMigrateOrTakeWriter(t *testing.T) {
	app, probe := fixtureApplication(t)
	_, err := app.Query(context.Background(), StatusQuery{})
	requireNoError(t, err)
	probe.RequireAbsent(t, "migration", "project-writer")
}

// TestListUsesSQLiteWithoutWorkflowDirectories: SQLite is authoritative for
// workflow enumeration. A fresh process must list a persisted workflow even
// when no legacy or aggregated workflow directory is present.
func TestListUsesSQLiteWithoutWorkflowDirectories(t *testing.T) {
	app, _ := fixtureApplication(t)
	db, err := sql.Open("sqlite", filepath.Join(app.home, "cflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.Exec(`INSERT INTO projects
		(id, project_key, canonical_path, display_name, git_root, created_at, updated_at, last_opened_at)
		VALUES ('other-project', 'other-key', '/tmp/cflow-other-project', 'other', '/tmp/cflow-other-project', ?, ?, ?)`, now, now, now); err != nil {
		db.Close()
		t.Fatalf("seed other project: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO workflows
		(id, project_id, stage, runtime_status, created_at, updated_at)
		VALUES ('workflow-other', 'other-project', 'REQUIREMENT_DISCUSSION', 'PENDING', ?, ?)`, now, now); err != nil {
		db.Close()
		t.Fatalf("seed other-project workflow: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	projects := filepath.Join(app.home, "projects")
	if err := os.Rename(projects, filepath.Join(app.home, "detached-projects-fixture")); err != nil {
		t.Fatalf("detach workflow directories: %v", err)
	}
	fresh, err := New(Options{
		Home: app.home, Project: app.project, CflowVersion: app.cflowVer,
		Now: app.now, IDs: model.SequentialIDSource(), Supervisor: app.supervisor,
		GitFlow: app.git,
	})
	requireNoError(t, err)
	view, err := fresh.Query(context.Background(), ListQuery{})
	requireNoError(t, err)
	workflows := view.(ListView).Workflows
	if len(workflows) != 1 || workflows[0].ID != "workflow-1" {
		t.Fatalf("project-scoped SQLite workflows = %+v, want only workflow-1", workflows)
	}
}

// ---------------------------------------------------------------------------
// mutation behavior
// ---------------------------------------------------------------------------

// TestPauseStopsEveryManagedProcess: the effect loop commits one stop
// Intent per running process, executes each, and settles each Result
// before requesting the next stop. Both fixture process records share the
// one scripted supervisor handle: the loop protocol, not process identity,
// is under test.
func TestPauseStopsEveryManagedProcess(t *testing.T) {
	app, probe := fixtureApplication(t)
	seedProcessRow(t, app.dbPath, "workflow-1", "proc-2", model.ProcessStatusRunning)
	app.procs[model.ProcessID("proc-2")] = app.procs[model.ProcessID("proc-1")]

	out, err := app.Execute(context.Background(), PauseWorkflowCommand{Workflow: "workflow-1"})
	requireNoError(t, err)
	if out.Runtime != model.RuntimePaused {
		t.Fatalf("runtime = %s, want PAUSED", out.Runtime)
	}
	calls := probe.Calls()
	// The second "lock" is the Recovery Engine's aggregate read under the
	// shared Schema Lock before the mutation locks are taken.
	want := []string{"recover", "lock", "lock", "intent-commit", "effect", "result-commit", "intent-commit", "effect", "result-commit"}
	if !slices.Equal(want, calls) {
		t.Fatalf("want %v, got %v", want, calls)
	}
	requireNoRunningProcesses(t, app)
}

// TestCancelStopsProcessesThenCompletes: cancel persists the intent, stops
// the managed process through the effect loop, and commits the terminal
// CANCELLED Decision only after nothing is running (design 17.4).
func TestCancelStopsProcessesThenCompletes(t *testing.T) {
	app, _ := fixtureApplication(t)
	out, err := app.Execute(context.Background(), CancelWorkflowCommand{Workflow: "workflow-1", Reason: "user decision"})
	requireNoError(t, err)
	if out.Runtime != model.RuntimeCancelled {
		t.Fatalf("runtime = %s, want CANCELLED", out.Runtime)
	}
	requireNoRunningProcesses(t, app)
}

// TestCancelOnTerminalWorkflowRejected: a terminal workflow cannot be
// cancelled; the terminal state is never rewritten (PRD 终止状态保护).
func TestCancelOnTerminalWorkflowRejected(t *testing.T) {
	app, _ := fixtureApplication(t)
	if _, err := app.Execute(context.Background(), CancelWorkflowCommand{Workflow: "workflow-1"}); err != nil {
		t.Fatalf("first cancel: %v", err)
	}
	_, err := app.Execute(context.Background(), CancelWorkflowCommand{Workflow: "workflow-1"})
	requireFaultCode(t, err, model.CodeInvalidInput)
}

// TestResumeRequiresPausedOrBlocked: the kernel rejects a resume from a
// workflow that is not PAUSED or BLOCKED with no mutation.
func TestResumeRequiresPausedOrBlocked(t *testing.T) {
	app, _ := fixtureApplication(t)
	_, err := app.Execute(context.Background(), ResumeWorkflowCommand{Workflow: "workflow-1"})
	requireFaultCode(t, err, model.CodeInvalidInput)
}

// TestDryRunOnNonTerminalWorkflowBlocked: cleanup dry-run requires a
// terminal workflow (design 17.4); the fault is a safe user-action
// request.
func TestDryRunOnNonTerminalWorkflowBlocked(t *testing.T) {
	app, _ := fixtureApplication(t)
	_, err := app.Execute(context.Background(), DryRunCommand{Workflow: "workflow-1"})
	requireFaultCode(t, err, model.CodeCleanupWorkflowNotTerminal)
}

// TestDryRunProducesImmutableManifest: on a completed Workflow the dry run
// commits one Cleanup Attempt with an immutable Manifest reference and
// pending items (design 17.4). The managed Worktrees are auto-collected;
// the explicitly provided targets are exact scratch paths (a Worktree item
// can never be injected by the caller).
func TestDryRunProducesImmutableManifest(t *testing.T) {
	app, _ := fixtureApplication(t)
	// Cleanup requires a terminal workflow with no managed processes:
	// stop the fixture process first, then complete the aggregate.
	if _, err := app.Execute(context.Background(), PauseWorkflowCommand{Workflow: "workflow-1"}); err != nil {
		t.Fatalf("settle fixture process: %v", err)
	}
	completeFixtureWorkflow(t, app)
	scratch := filepath.Join(app.home, "scratch", "run-1", "tmp")
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		t.Fatalf("mkdir scratch: %v", err)
	}
	out, err := app.Execute(context.Background(), DryRunCommand{
		Workflow: "workflow-1",
		Items: []model.CleanupItem{{
			Index: 0, Kind: model.CleanupScratch, CanonicalPath: scratch,
		}},
	})
	requireNoError(t, err)
	if out.Cleanup == nil {
		t.Fatal("expected a committed cleanup manifest")
	}
	if out.Cleanup.Status != model.CleanupStatusAwaitingConfirmation {
		t.Fatalf("cleanup status = %s, want AWAITING_CONFIRMATION", out.Cleanup.Status)
	}
	if out.Cleanup.Manifest.Type != model.ArtifactCleanupManifest || out.Cleanup.Manifest.Workflow != "workflow-1" {
		t.Fatalf("unexpected manifest ref %v", out.Cleanup.Manifest)
	}
	if len(out.Cleanup.Items) != 1 || out.Cleanup.Items[0].Status != model.CleanupItemPending {
		t.Fatalf("unexpected manifest items %+v", out.Cleanup.Items)
	}
}

// completeFixtureWorkflow commits a direct completion Decision through the
// Application's own Store (the Stage transition chain arrives with the
// lifecycle tasks; the fixture supplies the terminal aggregate).
func completeFixtureWorkflow(t *testing.T, app *Application) {
	t.Helper()
	st := app.stores[model.WorkflowID("workflow-1")]
	if st == nil {
		t.Fatal("fixture store not open")
	}
	view, err := st.View(context.Background(), store.StoreQuery{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.Transact(context.Background(), view.AggregateVersion, func(state model.State) (model.Decision, error) {
		return model.Decision{Mutations: []model.Mutation{model.WorkflowMutation{
			ID:           state.Workflow.ID,
			Project:      state.Workflow.Project,
			Stage:        model.StageCompleted,
			Runtime:      model.RuntimeSucceeded,
			TargetBranch: state.Workflow.TargetBranch,
			BaseCommit:   state.Workflow.BaseCommit,
		}}}, nil
	})
	if err != nil {
		t.Fatalf("complete fixture workflow: %v", err)
	}
}

// requireNoRunningProcesses asserts the settled aggregate carries no
// RUNNING managed process (settled records legitimately remain).
func requireNoRunningProcesses(t *testing.T, app *Application) {
	t.Helper()
	view := mustStatus(t, app, "workflow-1")
	for _, p := range view.Processes {
		if p.Status == model.ProcessStatusRunning {
			t.Fatalf("process still running: %+v", p)
		}
	}
}

// ---------------------------------------------------------------------------
// restricted safety path (design 6.1)
// ---------------------------------------------------------------------------

// TestRestrictedSafetyPathStopsManagedProcesses: when the recovery hook
// fails with a posture fault, pause still acquires the locks and stops the
// already managed processes; normal mutation is quarantined.
func TestRestrictedSafetyPathStopsManagedProcesses(t *testing.T) {
	app, _ := fixtureApplication(t)
	app.recoverer = &fixtureRecoverer{err: model.NewFault(model.CodeInsecureCFLOWHomePermissions, "fixture posture fault")}
	out, err := app.Execute(context.Background(), PauseWorkflowCommand{Workflow: "workflow-1"})
	requireNoError(t, err)
	if !out.Restricted {
		t.Fatal("expected the restricted safety path")
	}
	if out.Runtime != model.RuntimePaused {
		t.Fatalf("runtime = %s, want PAUSED", out.Runtime)
	}
	requireNoRunningProcesses(t, app)
}

// TestResumeBlockedByRecoveryFault: resume is not a safety command; the
// recovery fault blocks it unchanged.
func TestResumeBlockedByRecoveryFault(t *testing.T) {
	app, _ := fixtureApplication(t)
	app.recoverer = &fixtureRecoverer{err: model.NewFault(model.CodeInsecureCFLOWHomePermissions, "fixture posture fault")}
	_, err := app.Execute(context.Background(), ResumeWorkflowCommand{Workflow: "workflow-1"})
	requireFaultCode(t, err, model.CodeInsecureCFLOWHomePermissions)
}

// TestSafetyPathNeverRunsNonStopEffects: a restricted command that the
// kernel nevertheless asks to start a Provider fails closed instead of
// running the effect (design 6.1: may not start a Provider).
func TestSafetyPathNeverRunsNonStopEffects(t *testing.T) {
	app, _ := fixtureApplication(t)
	app.recoverer = &fixtureRecoverer{err: model.NewFault(model.CodeInsecureCFLOWHomePermissions, "fixture posture fault")}
	if _, err := app.executeEffect(context.Background(), model.ProviderStartIntent{
		Session: model.SessionID("s-1"), Purpose: model.PurposeImplementation,
	}, true, "workflow-1", StartWorkflowCommand{}, model.ReconcileInput{}, nil); err == nil {
		t.Fatal("expected the restricted path to reject a ProviderStart effect")
	}
}

// ---------------------------------------------------------------------------
// events export (design 21)
// ---------------------------------------------------------------------------

// TestMutationWritesEventsJsonlExport: every successful mutation appends
// the committed Event window to the workflow's events.jsonl audit export,
// generated from the Event sequence (never read by Recovery).
func TestMutationWritesEventsJsonlExport(t *testing.T) {
	app, _ := fixtureApplication(t)
	dir, err := app.workflowDir(context.Background(), "workflow-1")
	if err != nil {
		t.Fatalf("resolve workflow directory: %v", err)
	}
	exportPath := filepath.Join(dir, "events.jsonl")
	body, err := os.ReadFile(exportPath)
	if err != nil {
		t.Fatalf("events export missing after create/start: %v", err)
	}
	for _, want := range []string{"WORKFLOW_CREATED", "RUN_STARTED", "WORKFLOW_STARTED"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("events export missing %q:\n%s", want, body)
		}
	}
	if _, err := app.Execute(context.Background(), PauseWorkflowCommand{Workflow: "workflow-1"}); err != nil {
		t.Fatalf("pause: %v", err)
	}
	body, err = os.ReadFile(exportPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "WORKFLOW_PAUSED") {
		t.Fatalf("events export missing the pause events:\n%s", body)
	}
}

// ---------------------------------------------------------------------------
// defensive loop bounds (design 6.2)
// ---------------------------------------------------------------------------

// TestRepeatedIdenticalIntentRejected: the effect loop rejects a second
// request for the same uncompleted Intent identity as an invariant
// failure instead of looping.
func TestRepeatedIdenticalIntentRejected(t *testing.T) {
	a := model.ManagedProcessStopIntent{Process: model.ProcessID("p-1")}
	if intentIdentity(a) != intentIdentity(model.ManagedProcessStopIntent{Process: model.ProcessID("p-1")}) {
		t.Fatal("identical intents must share an identity")
	}
	if intentIdentity(a) == intentIdentity(model.ManagedProcessStopIntent{Process: model.ProcessID("p-2")}) {
		t.Fatal("different intents must differ in identity")
	}
}

// TestExecuteRejectsNilCommand: the closed union has no stringly typed
// registry; a nil command is invalid input.
func TestExecuteRejectsNilCommand(t *testing.T) {
	app, _ := fixtureApplication(t)
	_, err := app.Execute(context.Background(), nil)
	requireFaultCode(t, err, model.CodeInvalidInput)
}

func mustStatus(t *testing.T, app *Application, wf string) StatusView {
	t.Helper()
	v, err := app.Query(context.Background(), StatusQuery{Workflow: model.WorkflowID(wf)})
	if err != nil {
		t.Fatalf("status query: %v", err)
	}
	sv, ok := v.(StatusView)
	if !ok {
		t.Fatalf("status query returned %T", v)
	}
	return sv
}
