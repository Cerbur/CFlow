// Package integration exercises the GitFlow module end to end against real
// temporary repositories created through the Process Supervisor (design
// 22.1). Every repository runs with system and global Git configs disabled
// and its own identity, so the developer's real Git configuration can
// never leak into test facts.
package integration

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cflow.local/cflow/internal/gitflow"
	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/process"
)

// Repo is one real temporary Git repository fixture. All Git commands run
// argv-only through the Process Supervisor.
type Repo struct {
	t    *testing.T
	sup  process.Supervisor
	Git  string
	Tmp  string
	Root string
	WTs  string
}

func newRepo(t *testing.T) *Repo {
	t.Helper()
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("git not found: %v", err)
	}
	canon, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("canonicalize temp dir: %v", err)
	}
	r := &Repo{t: t, sup: process.NewSupervisor(process.NewOSAdapter()), Git: git, Tmp: canon}
	r.Root = filepath.Join(canon, "repo")
	r.WTs = filepath.Join(canon, "cflow-worktrees")
	if err := os.Mkdir(r.Root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(r.WTs, 0o700); err != nil {
		t.Fatal(err)
	}
	r.gitAt(r.Root, "init", "-b", "main", "-q", r.Root)
	r.gitAt(r.Root, "config", "user.name", "Test User")
	r.gitAt(r.Root, "config", "user.email", "test@example.com")
	return r
}

func newCommittedRepo(t *testing.T) *Repo {
	t.Helper()
	r := newRepo(t)
	writeFile(t, r.Path("init.txt"), "init")
	r.gitAt(r.Root, "add", "init.txt")
	r.gitAt(r.Root, "commit", "-q", "-m", "init")
	return r
}

func (r *Repo) Path(rel string) string {
	return filepath.Join(r.Root, filepath.FromSlash(rel))
}

func (r *Repo) WtPath(name string) string {
	return filepath.Join(r.WTs, name)
}

func (r *Repo) flow() *gitflow.GitFlow {
	f, err := gitflow.NewGitFlow(r.sup, r.Root)
	if err != nil {
		r.t.Fatalf("new gitflow: %v", err)
	}
	return f
}

// git runs one supervised git command in the repository root.
func (r *Repo) git(args ...string) []byte {
	return r.gitAt(r.Root, args...)
}

func (r *Repo) gitAt(dir string, args ...string) []byte {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, exit, err := runGit(ctx, r.sup, r.Git, dir, args...)
	if err != nil {
		r.t.Fatalf("git %v: %v", args, err)
	}
	if exit.Fact != process.FactProcessExit || exit.Code != 0 {
		r.t.Fatalf("git %v exited %+v: %s", args, exit, out)
	}
	return out
}

func runGit(ctx context.Context, sup process.Supervisor, git, dir string, args ...string) ([]byte, process.Exit, error) {
	h, events, err := sup.Start(ctx, process.ProcessSpec{
		Executable: git,
		Args:       args,
		Dir:        dir,
		Env: map[string]string{
			"PATH":                os.Getenv("PATH"),
			"GIT_CONFIG_NOSYSTEM": "1",
			"GIT_CONFIG_GLOBAL":   "/dev/null",
			"GIT_TERMINAL_PROMPT": "0",
		},
		Timeout: 30 * time.Second,
	})
	if err != nil {
		return nil, process.Exit{}, err
	}
	var out []byte
	for ev := range events {
		if ev.Kind == process.EventFrameOut {
			out = append(out, ev.Frame...)
			out = append(out, '\n')
		}
	}
	exit, err := sup.Wait(ctx, h)
	return out, exit, err
}

// mustObserve observes q and returns the concrete ProjectFacts.
func mustObserve(t *testing.T, repo *Repo, q gitflow.GitQuery) gitflow.ProjectFacts {
	t.Helper()
	facts, err := repo.flow().Observe(context.Background(), q)
	if err != nil {
		t.Fatalf("observe %T: %v", q, err)
	}
	pf, ok := facts.(gitflow.ProjectFacts)
	if !ok {
		t.Fatalf("observe %T returned %T, want ProjectFacts", q, facts)
	}
	return pf
}

// mustExecute runs one Git operation, filling fixture defaults for empty
// worktree paths, branch names, and preflight revisions.
func mustExecute(t *testing.T, repo *Repo, op gitflow.GitOperation) gitflow.GitResult {
	t.Helper()
	switch o := op.(type) {
	case gitflow.CreatePlanningSnapshot:
		if o.Path == "" {
			o.Path = repo.WtPath("planning")
		}
		op = o
	case gitflow.CreateIntegration:
		if o.Path == "" {
			o.Path = repo.WtPath("integration")
		}
		if o.Branch == "" {
			o.Branch = "cflow/wf-test/integration"
		}
		op = o
	case gitflow.CreateTask:
		if o.Path == "" {
			o.Path = repo.WtPath("task")
		}
		if o.Branch == "" {
			o.Branch = "cflow/wf-test/task"
		}
		op = o
	case gitflow.CommitPreflight:
		if o.Revision == "" {
			o.Revision = "test-1"
		}
		op = o
	}
	res, err := repo.flow().Execute(context.Background(), op)
	if err != nil {
		t.Fatalf("execute %T: %v", op, err)
	}
	return res
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func requireFileContent(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(got) != want {
		t.Fatalf("%s content = %q, want %q", path, got, want)
	}
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func faultCode(t *testing.T, err error) model.Code {
	t.Helper()
	if f, ok := err.(*model.Fault); ok {
		return f.Code
	}
	t.Fatalf("error is %T %v, want model.Fault", err, err)
	return ""
}

// TestDirtyUserWorkspaceDoesNotEnterPlanningSnapshot is the brief's
// verbatim acceptance test: the user's dirty files never leak into the
// fixed Planning Snapshot and remain byte-identical in the user's tree.
func TestDirtyUserWorkspaceDoesNotEnterPlanningSnapshot(t *testing.T) {
	repo := newCommittedRepo(t)
	writeFile(t, repo.Path("user-wip.txt"), "uncommitted")
	facts := mustObserve(t, repo, gitflow.ProjectDiscovery{})
	snap := mustExecute(t, repo, gitflow.CreatePlanningSnapshot{BaseCommit: facts.Head})
	if pathExists(filepath.Join(snap.(gitflow.PlanningSnapshotResult).Worktree, "user-wip.txt")) {
		t.Fatal("dirty user file leaked into fixed snapshot")
	}
	requireFileContent(t, repo.Path("user-wip.txt"), "uncommitted")
}

// TestWorkflowWorktreeLifecycle walks the design 15.2 lifecycle: discovery
// records Target Branch and Base Commit; the Planning Snapshot is created
// first; Integration is created from the Base Commit; a Task starts from
// the verified Integration HEAD. The user's dirty working tree stays
// byte-identical and the Target Branch never moves.
func TestWorkflowWorktreeLifecycle(t *testing.T) {
	repo := newCommittedRepo(t)
	base := strings.TrimSpace(string(repo.git("rev-parse", "HEAD")))

	// User keeps a dirty working tree while the Workflow runs.
	writeFile(t, repo.Path("user-wip.txt"), "uncommitted-v1")

	facts := mustObserve(t, repo, gitflow.ProjectDiscovery{})
	if facts.Branch != "main" || facts.Head != base || facts.Detached || facts.Unborn {
		t.Fatalf("discovery facts = %+v", facts)
	}

	// Workflow creation: Planning Snapshot only.
	snap := mustExecute(t, repo, gitflow.CreatePlanningSnapshot{BaseCommit: facts.Head}).(gitflow.PlanningSnapshotResult)
	if pathExists(filepath.Join(snap.Worktree, "user-wip.txt")) {
		t.Fatal("dirty user file leaked into planning snapshot")
	}

	// Execution Approval committed: Integration Branch/Worktree from Base.
	integ := mustExecute(t, repo, gitflow.CreateIntegration{
		Branch:     "cflow/wf-20260802-001/integration",
		BaseCommit: facts.Head,
	}).(gitflow.IntegrationWorktreeResult)
	if integ.Head != base {
		t.Fatalf("integration head = %q, want base %q", integ.Head, base)
	}

	// Task becomes Ready: Task Branch/Worktree from verified Integration HEAD.
	task := mustExecute(t, repo, gitflow.CreateTask{
		Branch:   "cflow/wf-20260802-001/S01",
		BaseHead: integ.Head,
	}).(gitflow.TaskWorktreeResult)
	if task.Head != integ.Head {
		t.Fatalf("task head = %q, want integration head %q", task.Head, integ.Head)
	}

	// The task worktree starts clean at the base.
	status, err := repo.flow().Observe(context.Background(), gitflow.GitStatus{Dir: task.Worktree})
	if err != nil {
		t.Fatalf("task status: %v", err)
	}
	sf := status.(gitflow.StatusFacts)
	if sf.Head != base || sf.Dirty.StagedCount+sf.Dirty.UnstagedCount+sf.Dirty.UntrackedCount != 0 {
		t.Fatalf("task worktree not clean at base: %+v", sf)
	}

	// The user's dirty files remain byte-identical and the target branch
	// never moved (design 15.2: Workflow completed -> Target unchanged).
	requireFileContent(t, repo.Path("user-wip.txt"), "uncommitted-v1")
	if head := strings.TrimSpace(string(repo.git("rev-parse", "refs/heads/main"))); head != base {
		t.Fatalf("target branch moved to %q, want %q", head, base)
	}

	// The worktree registry reflects all four worktrees.
	wf, err := repo.flow().Observe(context.Background(), gitflow.WorktreeList{})
	if err != nil {
		t.Fatal(err)
	}
	entries := wf.(gitflow.WorktreeFacts).Entries
	if len(entries) != 4 {
		t.Fatalf("registry has %d entries, want 4: %+v", len(entries), entries)
	}
}

// TestCommitPreflightGateEndToEnd runs the preflight, makes a commit with
// the approved policy, and verifies the actual Commit evidence against the
// Preflight (design 15.4).
func TestCommitPreflightGateEndToEnd(t *testing.T) {
	repo := newCommittedRepo(t)
	ev := mustExecute(t, repo, gitflow.CommitPreflight{Revision: "wf-1"}).(gitflow.PreflightEvidence)
	if ev.PolicyFingerprint == "" {
		t.Fatal("preflight produced no policy fingerprint")
	}

	// The Application creates the managed Task Worktree before the
	// coding session starts.
	base := strings.TrimSpace(string(repo.git("rev-parse", "HEAD")))
	mustExecute(t, repo, gitflow.CreateIntegration{Branch: "cflow/wf-test/integration", BaseCommit: base})
	task := mustExecute(t, repo, gitflow.CreateTask{Branch: "cflow/wf-test/S01", BaseHead: base}).(gitflow.TaskWorktreeResult)

	// A coding agent commits inside its managed Task worktree.
	repo.gitAt(task.Worktree, "commit", "-q", "--allow-empty", "-m", "implementation")
	head := strings.TrimSpace(string(repo.gitAt(task.Worktree, "rev-parse", "HEAD")))

	res, err := repo.flow().Execute(context.Background(), gitflow.VerifyCommit{
		Ref:               head,
		ExpectedAuthor:    ev.Author,
		ExpectedCommitter: ev.Committer,
		ExpectedSigning:   ev.Signing,
	})
	if err != nil {
		t.Fatalf("verify against preflight: %v", err)
	}
	if _, ok := res.(gitflow.VerifyCommitResult); !ok {
		t.Fatalf("verify result type %T, want VerifyCommitResult", res)
	}
}

// TestPreflightBlocksBeforeAnyCommit: a missing identity blocks before
// any commit-capable process can start and mutates nothing.
func TestPreflightBlocksBeforeAnyCommit(t *testing.T) {
	repo := newCommittedRepo(t)
	t.Setenv("GIT_AUTHOR_NAME", "")
	t.Setenv("GIT_AUTHOR_EMAIL", "")
	headBefore := strings.TrimSpace(string(repo.git("rev-parse", "HEAD")))
	configBefore := strings.TrimSpace(string(repo.git("config", "--local", "--list")))

	_, err := repo.flow().Execute(context.Background(), gitflow.CommitPreflight{Revision: "blocked"})
	if code := faultCode(t, err); code != model.CodeGitIdentityNotConfigured {
		t.Fatalf("code = %s, want GIT_IDENTITY_NOT_CONFIGURED", code)
	}
	if head := strings.TrimSpace(string(repo.git("rev-parse", "HEAD"))); head != headBefore {
		t.Fatalf("target HEAD moved: %q -> %q", headBefore, head)
	}
	if cfg := strings.TrimSpace(string(repo.git("config", "--local", "--list"))); cfg != configBefore {
		t.Fatalf("local config changed by blocked preflight:\nbefore: %s\nafter: %s", configBefore, cfg)
	}
}
