package gitflow_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cflow.local/cflow/internal/gitflow"
	"cflow.local/cflow/internal/model"
)

func worktreeAt(t *testing.T, repo *Repo, path string) *gitflow.WorktreeEntry {
	t.Helper()
	facts, err := repo.flow().Observe(context.Background(), gitflow.WorktreeList{})
	if err != nil {
		t.Fatalf("worktree list: %v", err)
	}
	wf := facts.(gitflow.WorktreeFacts)
	for i := range wf.Entries {
		if wf.Entries[i].Path == path {
			return &wf.Entries[i]
		}
	}
	t.Fatalf("no worktree entry for %s in %+v", path, wf.Entries)
	return nil
}

func TestCreatePlanningSnapshot(t *testing.T) {
	repo := newCommittedRepo(t)
	head := strings.TrimSpace(string(repo.git("rev-parse", "HEAD")))
	facts := mustObserve(t, repo, gitflow.ProjectDiscovery{})
	if facts.Head != head {
		t.Fatalf("observed head %q, want %q", facts.Head, head)
	}

	res := mustExecute(t, repo, gitflow.CreatePlanningSnapshot{BaseCommit: facts.Head})
	snap, ok := res.(gitflow.PlanningSnapshotResult)
	if !ok {
		t.Fatalf("result type %T, want PlanningSnapshotResult", res)
	}
	if snap.Worktree != repo.WtPath("planning") {
		t.Fatalf("worktree path = %q, want %q", snap.Worktree, repo.WtPath("planning"))
	}
	if snap.Head != head {
		t.Fatalf("snapshot head = %q, want %q", snap.Head, head)
	}
	if !pathExists(filepath.Join(snap.Worktree, "init.txt")) {
		t.Fatal("snapshot worktree missing committed file")
	}
	// The snapshot is detached: it must not move the target branch.
	entry := worktreeAt(t, repo, snap.Worktree)
	if !entry.Detached {
		t.Fatal("planning snapshot must be detached")
	}
	if entry.Head != head {
		t.Fatalf("registry head = %q, want %q", entry.Head, head)
	}
	mainHead := strings.TrimSpace(string(repo.git("rev-parse", "refs/heads/main")))
	if mainHead != head {
		t.Fatalf("target branch moved to %q, want %q", mainHead, head)
	}
}

func TestPlanningSnapshotFixedAtRecordedBase(t *testing.T) {
	repo := newCommittedRepo(t)
	base := strings.TrimSpace(string(repo.git("rev-parse", "HEAD")))
	// The user's HEAD advances after observation.
	repo.write("more.txt", "more")
	repo.git("add", "more.txt")
	repo.git("commit", "-q", "-m", "more")
	latest := strings.TrimSpace(string(repo.git("rev-parse", "HEAD")))

	res := mustExecute(t, repo, gitflow.CreatePlanningSnapshot{BaseCommit: base})
	snap := res.(gitflow.PlanningSnapshotResult)
	if snap.Head != base {
		t.Fatalf("snapshot head = %q, want recorded base %q (not latest %q)", snap.Head, base, latest)
	}
	if pathExists(filepath.Join(snap.Worktree, "more.txt")) {
		t.Fatal("snapshot leaked post-base content")
	}
}

func TestPlanningSnapshotUnknownBase(t *testing.T) {
	repo := newCommittedRepo(t)
	_, err := repo.flow().Execute(context.Background(), gitflow.CreatePlanningSnapshot{
		BaseCommit: strings.Repeat("0", 40),
		Path:       repo.WtPath("planning"),
	})
	if code := faultCode(t, err); code != model.CodeStateInvariantViolation {
		t.Fatalf("unknown base code = %s, want STATE_INVARIANT_VIOLATION", code)
	}
	if pathExists(repo.WtPath("planning")) {
		t.Fatal("failed snapshot left a worktree behind")
	}
}

func TestCreateIntegrationWorktree(t *testing.T) {
	repo := newCommittedRepo(t)
	base := strings.TrimSpace(string(repo.git("rev-parse", "HEAD")))

	res := mustExecute(t, repo, gitflow.CreateIntegration{
		Branch:     "cflow/wf-20260802-001/integration",
		BaseCommit: base,
	})
	integ, ok := res.(gitflow.IntegrationWorktreeResult)
	if !ok {
		t.Fatalf("result type %T, want IntegrationWorktreeResult", res)
	}
	if integ.Branch != "cflow/wf-20260802-001/integration" || integ.Head != base {
		t.Fatalf("integration result = %+v", integ)
	}
	entry := worktreeAt(t, repo, integ.Worktree)
	if entry.Detached || entry.Branch != "cflow/wf-20260802-001/integration" {
		t.Fatalf("integration registry entry = %+v", entry)
	}
	if entry.Head != base {
		t.Fatalf("integration head = %q, want %q", entry.Head, base)
	}
	// The branch really exists.
	facts, err := repo.flow().Observe(context.Background(), gitflow.RefLookup{Ref: "refs/heads/cflow/wf-20260802-001/integration"})
	if err != nil {
		t.Fatal(err)
	}
	if !facts.(gitflow.RefFacts).Exists {
		t.Fatal("integration ref missing")
	}
}

func TestCreateIntegrationRefCollision(t *testing.T) {
	repo := newCommittedRepo(t)
	base := strings.TrimSpace(string(repo.git("rev-parse", "HEAD")))
	// Pre-create the branch the workflow would own.
	repo.git("branch", "cflow/wf-test/integration")

	_, err := repo.flow().Execute(context.Background(), gitflow.CreateIntegration{
		Branch:     "cflow/wf-test/integration",
		BaseCommit: base,
		Path:       repo.WtPath("integration"),
	})
	if code := faultCode(t, err); code != model.CodeStateInvariantViolation {
		t.Fatalf("ref collision code = %s, want STATE_INVARIANT_VIOLATION", code)
	}
	if pathExists(repo.WtPath("integration")) {
		t.Fatal("colliding create left a worktree behind")
	}
}

func TestCreateIntegrationPathCollision(t *testing.T) {
	repo := newCommittedRepo(t)
	base := strings.TrimSpace(string(repo.git("rev-parse", "HEAD")))
	if err := os.Mkdir(repo.WtPath("integration"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := repo.flow().Execute(context.Background(), gitflow.CreateIntegration{
		Branch:     "cflow/wf-test/integration",
		BaseCommit: base,
		Path:       repo.WtPath("integration"),
	})
	if code := faultCode(t, err); code != model.CodeInvalidInput {
		t.Fatalf("path collision code = %s, want INVALID_INPUT", code)
	}
}

func TestCreateIntegrationRejectsWorktreeInsideWorkingTree(t *testing.T) {
	repo := newCommittedRepo(t)
	base := strings.TrimSpace(string(repo.git("rev-parse", "HEAD")))
	_, err := repo.flow().Execute(context.Background(), gitflow.CreateIntegration{
		Branch:     "cflow/wf-test/integration",
		BaseCommit: base,
		Path:       filepath.Join(repo.Root, "nested-worktree"),
	})
	if code := faultCode(t, err); code != model.CodeInvalidInput {
		t.Fatalf("in-tree worktree code = %s, want INVALID_INPUT", code)
	}
	if pathExists(filepath.Join(repo.Root, "nested-worktree")) {
		t.Fatal("in-tree worktree was created")
	}
}

func TestCreateTaskFromIntegrationHead(t *testing.T) {
	repo := newCommittedRepo(t)
	base := strings.TrimSpace(string(repo.git("rev-parse", "HEAD")))
	integ := mustExecute(t, repo, gitflow.CreateIntegration{
		Branch:     "cflow/wf-test/integration",
		BaseCommit: base,
	}).(gitflow.IntegrationWorktreeResult)

	task := mustExecute(t, repo, gitflow.CreateTask{
		Branch:   "cflow/wf-test/S01",
		BaseHead: integ.Head,
	}).(gitflow.TaskWorktreeResult)

	if task.Branch != "cflow/wf-test/S01" || task.Head != integ.Head {
		t.Fatalf("task result = %+v", task)
	}
	entry := worktreeAt(t, repo, task.Worktree)
	if entry.Branch != "cflow/wf-test/S01" || entry.Head != integ.Head || entry.Detached {
		t.Fatalf("task registry entry = %+v", entry)
	}
	// The task worktree is isolated from the user's tree.
	repo.write("user-wip.txt", "uncommitted")
	if pathExists(filepath.Join(task.Worktree, "user-wip.txt")) {
		t.Fatal("task worktree leaked user's dirty file")
	}
}

func TestCreateTaskExpectedHeadMismatch(t *testing.T) {
	repo := newCommittedRepo(t)
	// The caller expects the task base to be a verified Integration HEAD;
	// an unknown commit is a fail-closed mismatch, not a silent rebase.
	_, err := repo.flow().Execute(context.Background(), gitflow.CreateTask{
		Branch:   "cflow/wf-test/S01",
		BaseHead: strings.Repeat("1", 40),
		Path:     repo.WtPath("task"),
	})
	if code := faultCode(t, err); code != model.CodeStateInvariantViolation {
		t.Fatalf("expected-head mismatch code = %s, want STATE_INVARIANT_VIOLATION", code)
	}
	if pathExists(repo.WtPath("task")) {
		t.Fatal("mismatched task left a worktree behind")
	}
}

func TestCreateTaskTaskBranchCollisionAcrossWorktrees(t *testing.T) {
	repo := newCommittedRepo(t)
	base := strings.TrimSpace(string(repo.git("rev-parse", "HEAD")))
	mustExecute(t, repo, gitflow.CreateIntegration{
		Branch:     "cflow/wf-test/integration",
		BaseCommit: base,
	})
	mustExecute(t, repo, gitflow.CreateTask{
		Branch:   "cflow/wf-test/S01",
		BaseHead: base,
	})
	// A second task claiming the same branch must fail closed.
	_, err := repo.flow().Execute(context.Background(), gitflow.CreateTask{
		Branch:   "cflow/wf-test/S01",
		BaseHead: base,
		Path:     repo.WtPath("task2"),
	})
	if code := faultCode(t, err); code != model.CodeStateInvariantViolation {
		t.Fatalf("task branch collision code = %s, want STATE_INVARIANT_VIOLATION", code)
	}
	if pathExists(repo.WtPath("task2")) {
		t.Fatal("colliding task left a worktree behind")
	}
}

func TestWorktreeListRegistry(t *testing.T) {
	repo := newCommittedRepo(t)
	base := strings.TrimSpace(string(repo.git("rev-parse", "HEAD")))
	mustExecute(t, repo, gitflow.CreatePlanningSnapshot{BaseCommit: base})
	mustExecute(t, repo, gitflow.CreateIntegration{Branch: "cflow/wf-test/integration", BaseCommit: base})
	mustExecute(t, repo, gitflow.CreateTask{Branch: "cflow/wf-test/S01", BaseHead: base})

	facts, err := repo.flow().Observe(context.Background(), gitflow.WorktreeList{})
	if err != nil {
		t.Fatal(err)
	}
	entries := facts.(gitflow.WorktreeFacts).Entries
	if len(entries) != 4 {
		t.Fatalf("registry has %d entries, want 4: %+v", len(entries), entries)
	}
	seen := map[string]bool{}
	for _, e := range entries {
		seen[e.Path] = true
		if !e.Detached && e.Branch == "" {
			t.Fatalf("attached entry without branch: %+v", e)
		}
		if e.Head == "" {
			t.Fatalf("entry without head: %+v", e)
		}
	}
	for _, want := range []string{repo.Root, repo.WtPath("planning"), repo.WtPath("integration"), repo.WtPath("task")} {
		if !seen[want] {
			t.Fatalf("registry missing %s: %+v", want, entries)
		}
	}
}

func TestCreateAuditRef(t *testing.T) {
	repo := newCommittedRepo(t)
	head := strings.TrimSpace(string(repo.git("rev-parse", "HEAD")))
	auditRef := "refs/audit/cflow/wf-test/S01/1"

	res := mustExecute(t, repo, gitflow.CreateAuditRef{Ref: auditRef, Head: head})
	if _, ok := res.(gitflow.AuditRefResult); !ok {
		t.Fatalf("result type %T, want AuditRefResult", res)
	}
	facts, err := repo.flow().Observe(context.Background(), gitflow.RefLookup{Ref: auditRef})
	if err != nil {
		t.Fatal(err)
	}
	rf := facts.(gitflow.RefFacts)
	if !rf.Exists || rf.Value != head {
		t.Fatalf("audit ref facts = %+v", rf)
	}
	// Append-only: a second create with the same expected-absent
	// semantics must fail and never overwrite.
	_, err = repo.flow().Execute(context.Background(), gitflow.CreateAuditRef{Ref: auditRef, Head: head})
	if code := faultCode(t, err); code != model.CodeStateInvariantViolation {
		t.Fatalf("duplicate audit ref code = %s, want STATE_INVARIANT_VIOLATION", code)
	}
}

func TestCreateAuditRefRejectsNonRefNamespace(t *testing.T) {
	repo := newCommittedRepo(t)
	head := strings.TrimSpace(string(repo.git("rev-parse", "HEAD")))
	for _, evil := range []string{"audit/plain", "-c", "HEAD", "refs/../escape", "refs/" + strings.Repeat("a", 300)} {
		_, err := repo.flow().Execute(context.Background(), gitflow.CreateAuditRef{Ref: evil, Head: head})
		if code := faultCode(t, err); code != model.CodeInvalidInput {
			t.Fatalf("evil audit ref %q code = %s, want INVALID_INPUT", evil, code)
		}
	}
}

func TestWorktreeOpsDoNotMutateTargetBranchOrConfig(t *testing.T) {
	repo := newCommittedRepo(t)
	base := strings.TrimSpace(string(repo.git("rev-parse", "HEAD")))
	configBefore := strings.TrimSpace(string(repo.git("config", "--local", "--list")))
	mustExecute(t, repo, gitflow.CreatePlanningSnapshot{BaseCommit: base})
	mustExecute(t, repo, gitflow.CreateIntegration{Branch: "cflow/wf-test/integration", BaseCommit: base})
	mustExecute(t, repo, gitflow.CreateTask{Branch: "cflow/wf-test/S01", BaseHead: base})

	if head := strings.TrimSpace(string(repo.git("rev-parse", "refs/heads/main"))); head != base {
		t.Fatalf("target branch moved to %q, want %q", head, base)
	}
	configAfter := strings.TrimSpace(string(repo.git("config", "--local", "--list")))
	if configBefore != configAfter {
		t.Fatalf("local config changed:\nbefore: %s\nafter: %s", configBefore, configAfter)
	}
}
