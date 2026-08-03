// The Recovery Engine contract (Task 13, design 17.1-17.2): Reconcile
// evaluates facts in the design's order and, for every unfinished Effect
// Intent in the persisted ledger, produces exactly one disposition —
// already_completed, safe_to_retry, blocked_drift, or fatal_invariant —
// with expected-absent / expected-value compare-and-swap semantics that
// prevent duplicate Worktrees, refs, merges, or Apply updates. The
// fixtures drive real Git repositories and the real Store; a Reconcile
// never mutates anything (the merge count of a completed merge stays 1).
package recovery_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cflow.local/cflow/internal/artifact"
	"cflow.local/cflow/internal/gitflow"
	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/process"
	"cflow.local/cflow/internal/recovery"
	"cflow.local/cflow/internal/security"
	"cflow.local/cflow/internal/store"
)

// ---------------------------------------------------------------------------
// real-git helpers
// ---------------------------------------------------------------------------

type gitRunner struct {
	t   *testing.T
	dir string
}

func (g gitRunner) git(args ...string) string {
	g.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = g.dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		g.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (g gitRunner) write(rel, content string) {
	g.t.Helper()
	path := filepath.Join(g.dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		g.t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		g.t.Fatalf("write %s: %v", path, err)
	}
}

func (g gitRunner) head() string {
	g.t.Helper()
	out, err := exec.Command("git", "-C", g.dir, "rev-parse", "HEAD").CombinedOutput()
	if err != nil {
		g.t.Fatalf("rev-parse HEAD: %v\n%s", err, out)
	}
	return strings.TrimSpace(string(out))
}

// ---------------------------------------------------------------------------
// recoveryFixture: one repository, one Integration Worktree, one Task
// Branch, and one Store with a workflow row and a seeded Intent ledger
// ---------------------------------------------------------------------------

const (
	testWF    = "wf-1"
	testProj  = "project-1"
	taskNode  = "task-s01"
	mergeNode = "merge-s01"
)

// recoveryFixture owns the real repository, the managed worktrees, the
// Store, and the Recovery Engine under test.
type recoveryFixture struct {
	t            *testing.T
	sup          process.Supervisor
	repo         *gitRunner
	home         string
	dbPath       string
	projectKey   string
	integration  string // integration worktree path
	taskWorktree string
	taskBranch   string
	baseHead     string
	taskHead     string
	engine       *recovery.RecoveryEngine
	mergeCount   int
	now          func() time.Time
	pending      []model.EffectIntent // the seeded ledger
}

// newRecoveryFixture builds the repository, the Integration and Task
// Worktrees, the Store with a workflow row, and the Recovery Engine.
func newRecoveryFixture(t *testing.T) *recoveryFixture {
	t.Helper()
	repo := newRepo(t)
	// t.TempDir() may sit behind a symlink (/var -> /private/var); the
	// security guard rejects paths that resolve through a symbolic link,
	// and the CFLOW_HOME must live outside the repository (worktree paths
	// must not be inside the main worktree).
	canonDir, err := filepath.EvalSymlinks(repo.dir)
	if err != nil {
		t.Fatalf("canonical repo dir: %v", err)
	}
	repo.dir = canonDir
	sup := process.NewSupervisor(process.NewOSAdapter())
	flow, err := gitflow.NewGitFlow(sup, repo.dir)
	if err != nil {
		t.Fatalf("new gitflow: %v", err)
	}
	// CFLOW_HOME is created 0700 below the canonical temp root (the
	// posture guard requires an owner-only home).
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("canonical home: %v", err)
	}
	home = filepath.Join(home, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatalf("mkdir home: %v", err)
	}
	key := "test-project"
	worktrees := filepath.Join(home, "worktrees", key, testWF)
	integration := filepath.Join(worktrees, "integration")
	taskWorktree := filepath.Join(worktrees, "tasks", taskNode)
	for _, dir := range []string{worktrees, filepath.Join(worktrees, "tasks")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	base := repo.head()
	if _, err := flow.Execute(context.Background(), gitflow.CreateIntegration{
		Branch: "cflow/" + testWF + "/integration", BaseCommit: base, Path: integration,
	}); err != nil {
		t.Fatalf("create integration: %v", err)
	}
	fx := &recoveryFixture{
		t: t, sup: sup, repo: repo, home: home,
		dbPath: filepath.Join(home, "cflow.db"), projectKey: key,
		integration: integration, taskWorktree: taskWorktree,
		taskBranch: "cflow/" + testWF + "/task-" + taskNode,
		baseHead:   base,
		now:        func() time.Time { return time.Unix(1700000000, 0).UTC() },
	}
	if _, err := flow.Execute(context.Background(), gitflow.CreateTask{
		Branch: fx.taskBranch, BaseHead: base, Path: taskWorktree,
	}); err != nil {
		t.Fatalf("create task: %v", err)
	}
	repo.git("-C", taskWorktree, "commit", "--allow-empty", "-q", "-m", "implement")
	fx.taskHead = repo.git("-C", taskWorktree, "rev-parse", "HEAD")

	// The Store with a workflow row.
	wfStore, err := store.Open(context.Background(), store.OpenOptions{
		Path: fx.dbPath, Workflow: testWF, CflowVersion: "0.0.0-dev", Now: fx.now,
	})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := wfStore.RegisterProject(context.Background(), model.ProjectID(testProj), repo.dir, "repo"); err != nil {
		t.Fatalf("register project: %v", err)
	}
	if _, err := wfStore.Transact(context.Background(), 0, func(state model.State) (model.Decision, error) {
		return model.Decision{
			Mutations: []model.Mutation{model.WorkflowMutation{
				ID: testWF, Project: model.ProjectID(testProj),
				Stage: model.StageExecution, Runtime: model.RuntimeRunning,
				TargetBranch: "main", BaseCommit: base,
				IntegrationBranch: "cflow/" + testWF + "/integration", IntegrationHead: base,
			}},
		}, nil
	}); err != nil {
		t.Fatalf("seed workflow: %v", err)
	}
	if err := wfStore.Close(); err != nil {
		t.Fatalf("close seed store: %v", err)
	}

	fx.engine, err = recovery.NewRecoveryEngine(recovery.RecoveryEngineOptions{
		Supervisor: sup, GitFlow: flow,
		Home: home, ProjectKey: key,
		EvidenceDir: filepath.Join(home, "evidence"),
		OpenView: func(ctx context.Context, wf model.WorkflowID) (store.StoreView, error) {
			st, err := store.Open(ctx, store.OpenOptions{
				Path: fx.dbPath, Workflow: wf, ReadOnly: true, CflowVersion: "0.0.0-dev", Now: fx.now,
			})
			if err != nil {
				return store.StoreView{}, err
			}
			defer st.Close()
			return st.View(ctx, store.StoreQuery{})
		},
		OpenArtifacts: func(ctx context.Context, wf model.WorkflowID) (*artifact.Store, error) {
			return artifact.New(filepath.Join(home, "projects", key, "workflows", string(wf), "artifacts"), security.Registry{})
		},
	})
	if err != nil {
		t.Fatalf("new recovery engine: %v", err)
	}
	return fx
}

func newRepo(t *testing.T) *gitRunner {
	t.Helper()
	dir := t.TempDir()
	r := gitRunner{t: t, dir: dir}
	r.git("init", "-q", "-b", "main")
	r.git("config", "user.name", "Test User")
	r.git("config", "user.email", "test@example.com")
	r.write("seed.txt", "seed\n")
	r.git("add", "-A")
	r.git("commit", "-q", "-m", "base")
	return &r
}

// seedIntent commits one Effect Intent into the pending ledger.
func (fx *recoveryFixture) seedIntent(intent model.EffectIntent) {
	fx.t.Helper()
	fx.pending = append(fx.pending, intent)
	st, err := store.Open(context.Background(), store.OpenOptions{
		Path: fx.dbPath, Workflow: testWF, CflowVersion: "0.0.0-dev", Now: fx.now,
	})
	if err != nil {
		fx.t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	view, err := st.View(context.Background(), store.StoreQuery{})
	if err != nil {
		fx.t.Fatalf("view: %v", err)
	}
	if _, err := st.Transact(context.Background(), view.AggregateVersion, func(state model.State) (model.Decision, error) {
		return model.Decision{Effect: intent}, nil
	}); err != nil {
		fx.t.Fatalf("seed intent %T: %v", intent, err)
	}
}

// mergeTask performs one real --no-ff Integration merge of the Task
// Branch (the external fact the Recovery classifies) and counts it.
func (fx *recoveryFixture) mergeTask() string {
	fx.t.Helper()
	flow, err := gitflow.NewGitFlow(fx.sup, fx.repo.dir)
	if err != nil {
		fx.t.Fatalf("new gitflow: %v", err)
	}
	res, err := flow.Execute(context.Background(), gitflow.MergeIntegration{
		Path: fx.integration, Branch: fx.taskBranch,
	})
	if err != nil {
		fx.t.Fatalf("external merge: %v", err)
	}
	fx.mergeCount++
	mr, ok := res.(gitflow.MergeResult)
	if !ok {
		fx.t.Fatalf("external merge result = %T, want MergeResult", res)
	}
	return mr.Head
}

// RequireMergeCount asserts the exact number of performed merges: a
// completed merge is never repeated (expected-value compare-and-swap).
func (fx *recoveryFixture) RequireMergeCount(n int) {
	fx.t.Helper()
	if fx.mergeCount != n {
		fx.t.Fatalf("merge count = %d, want %d (a completed merge must never be repeated)", fx.mergeCount, n)
	}
}

// replaceIntegrationHistory points the Integration Worktree at a foreign
// orphan Commit (no parent, so not a descendant of the Task Base): a
// replaced integration history the recovery cannot uniquely explain.
func (fx *recoveryFixture) replaceIntegrationHistory() {
	fx.t.Helper()
	fx.repo.git("checkout", "-q", "--orphan", "orphan-tmp")
	fx.repo.git("rm", "-rfq", ".")
	if err := os.WriteFile(filepath.Join(fx.repo.dir, "foreign.txt"), []byte("foreign\n"), 0o755); err != nil {
		fx.t.Fatalf("write foreign: %v", err)
	}
	fx.repo.git("add", "-A")
	fx.repo.git("commit", "-q", "-m", "foreign history")
	orphan := fx.repo.head()
	fx.repo.git("-C", fx.integration, "checkout", "-q", "--detach", orphan)
	fx.repo.git("checkout", "-q", "main")
	fx.repo.git("branch", "-D", "orphan-tmp")
}

// dirtyIntegration writes one untracked file into the Integration
// Worktree.
func (fx *recoveryFixture) dirtyIntegration() {
	fx.t.Helper()
	if err := os.WriteFile(filepath.Join(fx.integration, "stray.txt"), []byte("stray\n"), 0o600); err != nil {
		fx.t.Fatalf("dirty integration: %v", err)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func mustReconcile(t *testing.T, fx *recoveryFixture) recovery.ReconciliationOutcome {
	t.Helper()
	out, err := fx.engine.Reconcile(context.Background(), recovery.Scope{Workflow: testWF})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return out
}

func requireDisposition(t *testing.T, out recovery.ReconciliationOutcome, want recovery.Disposition) {
	t.Helper()
	for _, d := range out.Dispositions {
		if d.Disposition == want {
			return
		}
	}
	t.Fatalf("no disposition %s in %+v", want, out.Dispositions)
}

func requireNoDisposition(t *testing.T, out recovery.ReconciliationOutcome, want recovery.Disposition) {
	t.Helper()
	for _, d := range out.Dispositions {
		if d.Disposition == want {
			t.Fatalf("unexpected disposition %s for %T: %s", want, d.Intent, d.Reason)
		}
	}
}

func dispositionOf(t *testing.T, out recovery.ReconciliationOutcome, intent model.EffectIntent) recovery.IntentDisposition {
	t.Helper()
	for _, d := range out.Dispositions {
		if fmt.Sprintf("%T", d.Intent) == fmt.Sprintf("%T", intent) {
			return d
		}
	}
	t.Fatalf("no disposition for %T in %+v", intent, out.Dispositions)
	return recovery.IntentDisposition{}
}

// mergeIntent is the standard seeded IntegrationMergeIntent.
func (fx *recoveryFixture) mergeIntent() model.EffectIntent {
	return model.IntegrationMergeIntent{
		Node: mergeNode, BaseHead: fx.baseHead,
		TaskBranch: fx.taskBranch, VerifiedCommit: fx.taskHead,
	}
}

// ---------------------------------------------------------------------------
// Intent reconciliation (design 17.2)
// ---------------------------------------------------------------------------

// TestRecoveryDoesNotRepeatCompletedMerge (brief Step 1, verbatim): a
// crash after the external merge but before the Result Commit leaves the
// IntegrationMergeIntent unfinished in the ledger; the external facts
// (Integration HEAD advanced, Task history contained, worktree clean)
// uniquely prove the intended result: ALREADY_COMPLETED, and the merge is
// never repeated.
func TestRecoveryDoesNotRepeatCompletedMerge(t *testing.T) {
	fx := crashAfterExternalMergeBeforeResultCommit(t)
	out := mustReconcile(t, fx)
	requireDisposition(t, out, recovery.AlreadyCompleted)
	fx.RequireMergeCount(1)
}

func crashAfterExternalMergeBeforeResultCommit(t *testing.T) *recoveryFixture {
	fx := newRecoveryFixture(t)
	fx.seedIntent(fx.mergeIntent())
	fx.mergeTask() // the external merge happened; the Result never committed
	return fx
}

// TestRecoverySafeToRetryForAbsentMerge: the intent is unfinished and no
// merge exists; the expected facts still match — SAFE_TO_RETRY, and
// nothing was merged.
func TestRecoverySafeToRetryForAbsentMerge(t *testing.T) {
	fx := newRecoveryFixture(t)
	fx.seedIntent(fx.mergeIntent())
	out := mustReconcile(t, fx)
	requireDisposition(t, out, recovery.SafeToRetry)
	fx.RequireMergeCount(0)
}

// TestRecoveryBlockedDriftOnForeignMerge: the Integration HEAD advanced
// through a merge that does NOT contain the verified Task Commit: the
// facts changed and cannot be safely reused — BLOCKED_DRIFT.
func TestRecoveryBlockedDriftOnForeignMerge(t *testing.T) {
	fx := newRecoveryFixture(t)
	fx.seedIntent(fx.mergeIntent())
	// A foreign Task Branch (never verified) is merged instead.
	fx.repo.git("branch", "cflow/"+testWF+"/task-other", fx.baseHead)
	fx.repo.git("checkout", "-q", "cflow/"+testWF+"/task-other")
	fx.repo.write("other.txt", "other\n")
	fx.repo.git("add", "-A")
	fx.repo.git("commit", "-q", "-m", "foreign task")
	foreign := fx.repo.head()
	fx.repo.git("checkout", "-q", "main")
	flow, err := gitflow.NewGitFlow(fx.sup, fx.repo.dir)
	if err != nil {
		t.Fatalf("new gitflow: %v", err)
	}
	if _, err := flow.Execute(context.Background(), gitflow.MergeIntegration{
		Path: fx.integration, Branch: "cflow/" + testWF + "/task-other",
	}); err != nil {
		t.Fatalf("foreign merge: %v", err)
	}
	fx.mergeCount++
	_ = foreign
	out := mustReconcile(t, fx)
	requireDisposition(t, out, recovery.BlockedDrift)
}

// TestRecoveryFatalInvariantOnReplacedHistory: the Integration HEAD moved
// to a Commit that is not a descendant of the recorded Base and the
// worktree is dirty: the facts are contradictory beyond safe repair —
// FATAL_INVARIANT.
func TestRecoveryFatalInvariantOnReplacedHistory(t *testing.T) {
	fx := newRecoveryFixture(t)
	fx.seedIntent(fx.mergeIntent())
	fx.replaceIntegrationHistory()
	fx.dirtyIntegration()
	out := mustReconcile(t, fx)
	requireDisposition(t, out, recovery.FatalInvariant)
}

// TestRecoveryDirtyAfterCompletedMergeIsBlockedDrift: the merge
// completed but the worktree carries Git-visible changes afterwards:
// facts changed — BLOCKED_DRIFT (never re-merged).
func TestRecoveryDirtyAfterCompletedMergeIsBlockedDrift(t *testing.T) {
	fx := crashAfterExternalMergeBeforeResultCommit(t)
	fx.dirtyIntegration()
	out := mustReconcile(t, fx)
	requireDisposition(t, out, recovery.BlockedDrift)
	fx.RequireMergeCount(1)
}

// TestRecoveryTaskWorktreeDispositions: an absent Task Worktree is
// SAFE_TO_RETRY; an existing one at the expected Base is ALREADY_COMPLETED
// (expected-absent / expected-value compare-and-swap).
func TestRecoveryTaskWorktreeDispositions(t *testing.T) {
	fx := newRecoveryFixture(t)
	fx.seedIntent(model.TaskWorktreeCreateIntent{Node: taskNode, Branch: fx.taskBranch, BaseHead: fx.baseHead})
	out := mustReconcile(t, fx)
	requireDisposition(t, out, recovery.AlreadyCompleted)

	other := newRecoveryFixture(t)
	other.seedIntent(model.TaskWorktreeCreateIntent{Node: "task-absent", Branch: other.taskBranch, BaseHead: other.baseHead})
	out = mustReconcile(t, other)
	requireDisposition(t, out, recovery.SafeToRetry)
}

// TestRecoveryAuditRefDispositions: an absent audit Ref is SAFE_TO_RETRY,
// a Ref at the expected value ALREADY_COMPLETED, and a Ref with a
// different value BLOCKED_DRIFT (the Ref moved: evidence changed).
func TestRecoveryAuditRefDispositions(t *testing.T) {
	auditRef := "refs/cflow/" + testWF + "/tasks/" + taskNode + "/attempts/1"

	absent := newRecoveryFixture(t)
	absent.seedIntent(model.GitAuditRefCreateIntent{Ref: auditRef, Head: absent.taskHead})
	out := mustReconcile(t, absent)
	requireDisposition(t, out, recovery.SafeToRetry)

	fixed := newRecoveryFixture(t)
	flow, err := gitflow.NewGitFlow(fixed.sup, fixed.repo.dir)
	if err != nil {
		t.Fatalf("new gitflow: %v", err)
	}
	if _, err := flow.Execute(context.Background(), gitflow.CreateAuditRef{Ref: auditRef, Head: fixed.taskHead}); err != nil {
		t.Fatalf("create audit ref: %v", err)
	}
	fixed.seedIntent(model.GitAuditRefCreateIntent{Ref: auditRef, Head: fixed.taskHead})
	out = mustReconcile(t, fixed)
	requireDisposition(t, out, recovery.AlreadyCompleted)

	moved := newRecoveryFixture(t)
	flow, err = gitflow.NewGitFlow(moved.sup, moved.repo.dir)
	if err != nil {
		t.Fatalf("new gitflow: %v", err)
	}
	if _, err := flow.Execute(context.Background(), gitflow.CreateAuditRef{Ref: auditRef, Head: moved.taskHead}); err != nil {
		t.Fatalf("create audit ref: %v", err)
	}
	// The Ref was moved to a different Commit afterwards.
	moved.repo.git("update-ref", auditRef, moved.baseHead)
	moved.seedIntent(model.GitAuditRefCreateIntent{Ref: auditRef, Head: moved.taskHead})
	out = mustReconcile(t, moved)
	requireDisposition(t, out, recovery.BlockedDrift)
}

// TestRecoveryVerificationManifestDispositions: a persisted Verification
// Manifest with the matching Catalog identity and range is
// ALREADY_COMPLETED; without the manifest the intent is SAFE_TO_RETRY.
func TestRecoveryVerificationManifestDispositions(t *testing.T) {
	intent := func(fx *recoveryFixture) model.EffectIntent {
		return model.VerificationRunIntent{
			Node: mergeNode, Catalog: model.CatalogRef{Revision: 1, Hash: strings.Repeat("a", 64)},
			CommitRange: fx.baseHead + ".." + fx.taskHead,
		}
	}
	absent := newRecoveryFixture(t)
	absent.seedIntent(intent(absent))
	out := mustReconcile(t, absent)
	requireDisposition(t, out, recovery.SafeToRetry)

	present := newRecoveryFixture(t)
	dir := filepath.Join(present.home, "evidence", "verification", testWF)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir evidence: %v", err)
	}
	manifest := fmt.Sprintf(`{"Node":"merge-s01","CatalogRef":{"Revision":1,"Hash":"%s"},"CommitRange":"%s..%s","Passed":true}`,
		strings.Repeat("a", 64), present.baseHead, present.taskHead)
	if err := os.WriteFile(filepath.Join(dir, mergeNode+".json"), []byte(manifest), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	present.seedIntent(intent(present))
	out = mustReconcile(t, present)
	requireDisposition(t, out, recovery.AlreadyCompleted)
}

// TestRecoveryArtifactDispositions: an ArtifactWriteIntent whose exact
// Revision exists in the Artifact Store is ALREADY_COMPLETED; an absent
// Revision is SAFE_TO_RETRY; a Revision whose file was deleted afterwards
// (an orphan: the aggregate wrote it, the file is gone) is BLOCKED_DRIFT.
func TestRecoveryArtifactDispositions(t *testing.T) {
	writeArtifact := func(fx *recoveryFixture, typ model.ArtifactType, rev int) model.ArtifactRef {
		fx.t.Helper()
		root := filepath.Join(fx.home, "projects", fx.projectKey, "workflows", testWF, "artifacts")
		if err := os.MkdirAll(root, 0o700); err != nil {
			fx.t.Fatalf("mkdir artifacts root: %v", err)
		}
		store, err := artifact.New(root, security.Registry{})
		if err != nil {
			fx.t.Fatalf("new artifact store: %v", err)
		}
		ref, err := store.Put(context.Background(), artifact.PutRequest{
			WorkflowID: testWF, Type: typ, Revision: rev, SchemaVersion: "1.0.0",
			CreatedAt: "2026-01-01T00:00:00Z",
			Producer:  artifact.ProducerRef{Purpose: "test"},
			Body:      []byte(fmt.Sprintf("body-%d", rev)),
		})
		if err != nil {
			fx.t.Fatalf("put artifact: %v", err)
		}
		return ref
	}

	written := newRecoveryFixture(t)
	ref := writeArtifact(written, model.ArtifactReport, 1)
	written.seedIntent(model.ArtifactWriteIntent{
		Ref:      model.ArtifactRef{Workflow: testWF, Type: model.ArtifactReport, Revision: 1, Hash: ref.Hash},
		Producer: model.PurposePlanning,
	})
	out := mustReconcile(t, written)
	requireDisposition(t, out, recovery.AlreadyCompleted)

	absent := newRecoveryFixture(t)
	absent.seedIntent(model.ArtifactWriteIntent{
		Ref:      model.ArtifactRef{Workflow: testWF, Type: model.ArtifactReport, Revision: 9, Hash: strings.Repeat("b", 64)},
		Producer: model.PurposePlanning,
	})
	out = mustReconcile(t, absent)
	requireDisposition(t, out, recovery.SafeToRetry)

	orphan := newRecoveryFixture(t)
	ref2 := writeArtifact(orphan, model.ArtifactReport, 1)
	// The file disappears after the write (the artifact is orphaned).
	path := filepath.Join(orphan.home, "projects", orphan.projectKey, "workflows", testWF, "artifacts",
		string(testWF), "report", "1", ref2.Hash)
	if err := os.Remove(path); err != nil {
		orphan.t.Fatalf("remove artifact file: %v", err)
	}
	orphan.seedIntent(model.ArtifactWriteIntent{
		Ref:      model.ArtifactRef{Workflow: testWF, Type: model.ArtifactReport, Revision: 1, Hash: ref2.Hash},
		Producer: model.PurposePlanning,
	})
	out = mustReconcile(t, orphan)
	requireDisposition(t, out, recovery.BlockedDrift)
}

// TestRecoveryProviderSessionDispositions: a ProviderStartIntent whose
// Session settled terminal is ALREADY_COMPLETED; a still-open Session is
// SAFE_TO_RETRY.
func TestRecoveryProviderSessionDispositions(t *testing.T) {
	seedSession := func(fx *recoveryFixture, id model.SessionID, status model.SessionStatus) {
		fx.t.Helper()
		st, err := store.Open(context.Background(), store.OpenOptions{
			Path: fx.dbPath, Workflow: testWF, CflowVersion: "0.0.0-dev", Now: fx.now,
		})
		if err != nil {
			fx.t.Fatalf("open store: %v", err)
		}
		defer st.Close()
		view, err := st.View(context.Background(), store.StoreQuery{})
		if err != nil {
			fx.t.Fatalf("view: %v", err)
		}
		var mutations []model.Mutation
		mutations = append(mutations, model.SessionAppendMutation{Session: model.Session{
			ID: id, Purpose: model.PurposePlanning, Status: status,
		}, Provider: "fake"})
		if status.IsTerminal() {
			mutations = append(mutations, model.SessionEndMutation{ID: id, Status: status})
		}
		if _, err := st.Transact(context.Background(), view.AggregateVersion, func(state model.State) (model.Decision, error) {
			return model.Decision{Mutations: mutations}, nil
		}); err != nil {
			fx.t.Fatalf("seed session: %v", err)
		}
	}

	done := newRecoveryFixture(t)
	seedSession(done, "session-1", model.SessionCompleted)
	done.seedIntent(model.ProviderStartIntent{Session: "session-1", Purpose: model.PurposePlanning, Route: "fake"})
	out := mustReconcile(t, done)
	requireDisposition(t, out, recovery.AlreadyCompleted)

	open := newRecoveryFixture(t)
	seedSession(open, "session-2", model.SessionStarting)
	open.seedIntent(model.ProviderStartIntent{Session: "session-2", Purpose: model.PurposePlanning, Route: "fake"})
	out = mustReconcile(t, open)
	requireDisposition(t, out, recovery.SafeToRetry)
}

// TestRecoveryEmptyLedgerReconcilesCleanly: an empty ledger produces no
// dispositions and no faults — the hook never blocks normal operation.
func TestRecoveryEmptyLedgerReconcilesCleanly(t *testing.T) {
	fx := newRecoveryFixture(t)
	out := mustReconcile(t, fx)
	if len(out.Dispositions) != 0 || len(out.Faults) != 0 {
		t.Fatalf("empty ledger produced dispositions %+v faults %+v", out.Dispositions, out.Faults)
	}
}
