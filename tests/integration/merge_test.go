// The serial Integration Merge and its conflict rollback end to end
// (Task 13, design 15.5, PRD 已确认：Merge Conflict 处理): a text conflict
// returns a typed MergeConflictResult (never an error), the managed
// Integration Worktree is restored to the recorded pre-merge HEAD by the
// RollbackMerge operation, and a clean merge produces a --no-ff Merge
// Commit whose parents are the pre-merge HEAD and the Task Branch HEAD.
package integration

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cflow.local/cflow/internal/gitflow"
	"cflow.local/cflow/internal/model"
)

// integrationMergeFixture owns a repository with an Integration Worktree
// and two Task Branches forked from the same Base Commit.
type integrationMergeFixture struct {
	t           *testing.T
	repo        *Repo
	flow        *gitflow.GitFlow
	base        string
	integration string
}

func newIntegrationMergeFixture(t *testing.T) *integrationMergeFixture {
	t.Helper()
	repo := newCommittedRepo(t)
	flow, err := gitflow.NewGitFlow(repo.sup, repo.Root)
	if err != nil {
		t.Fatalf("new gitflow: %v", err)
	}
	base := strings.TrimSpace(string(repo.git("rev-parse", "HEAD")))
	integration := filepath.Join(repo.Tmp, "cflow-worktrees", "integration")
	if err := os.MkdirAll(filepath.Dir(integration), 0o700); err != nil {
		t.Fatalf("mkdir worktrees: %v", err)
	}
	if _, err := flow.Execute(context.Background(), gitflow.CreateIntegration{
		Branch: "cflow/wf-1/integration", BaseCommit: base, Path: integration,
	}); err != nil {
		t.Fatalf("create integration: %v", err)
	}
	return &integrationMergeFixture{t: t, repo: repo, flow: flow, base: base, integration: integration}
}

// taskBranch creates one Task Branch/Worktree, writes the given content
// to the shared file, commits, and returns the branch name.
func (fx *integrationMergeFixture) taskBranch(name, content string) string {
	fx.t.Helper()
	path := filepath.Join(fx.repo.Tmp, "cflow-worktrees", name)
	branch := "cflow/wf-1/" + name
	if _, err := fx.flow.Execute(context.Background(), gitflow.CreateTask{
		Branch: branch, BaseHead: fx.base, Path: path,
	}); err != nil {
		fx.t.Fatalf("create task: %v", err)
	}
	if err := os.WriteFile(filepath.Join(path, "calc.ts"), []byte(content), 0o600); err != nil {
		fx.t.Fatalf("write calc: %v", err)
	}
	fx.repo.git("-C", path, "add", "-A")
	fx.repo.git("-C", path, "commit", "-q", "-m", name)
	return branch
}

// TestMergeConflictReturnsTypedResultAndRollsBack: two Task Branches
// editing the same lines conflict; the merge returns the typed
// MergeConflictResult and RollbackMerge restores the managed Integration
// Worktree to the recorded pre-merge HEAD (verified fail-closed).
func TestMergeConflictReturnsTypedResultAndRollsBack(t *testing.T) {
	fx := newIntegrationMergeFixture(t)
	b1 := fx.taskBranch("task-a", "export const v = 1;\n")
	b2 := fx.taskBranch("task-b", "export const v = 2;\n")

	// The first Task merges cleanly.
	res, err := fx.flow.Execute(context.Background(), gitflow.MergeIntegration{
		Path: fx.integration, Branch: b1,
	})
	if err != nil {
		t.Fatalf("first merge: %v", err)
	}
	mr, ok := res.(gitflow.MergeResult)
	if !ok {
		t.Fatalf("first merge result = %T, want MergeResult", res)
	}
	cf, err := fx.flow.Observe(context.Background(), gitflow.CommitInspect{Ref: mr.Head})
	if err != nil {
		t.Fatalf("merge commit facts: %v", err)
	}
	commitFacts := cf.(gitflow.CommitFacts)
	if len(commitFacts.Parents) != 2 {
		t.Fatalf("merge commit parents = %v, want the --no-ff pair (pre-merge HEAD, task branch)", commitFacts.Parents)
	}

	// The second Task edits the same lines: a text conflict.
	res, err = fx.flow.Execute(context.Background(), gitflow.MergeIntegration{
		Path: fx.integration, Branch: b2,
	})
	if err != nil {
		t.Fatalf("conflicting merge must return a typed result, got error: %v", err)
	}
	conflict, ok := res.(gitflow.MergeConflictResult)
	if !ok {
		t.Fatalf("conflicting merge result = %T, want MergeConflictResult", res)
	}
	if conflict.PreMergeHead != mr.Head {
		t.Fatalf("pre-merge head = %s, want the recorded %s", conflict.PreMergeHead, mr.Head)
	}

	// The rollback restores the managed Integration Worktree to the
	// recorded pre-merge HEAD.
	rb, err := fx.flow.Execute(context.Background(), gitflow.RollbackMerge{
		Path: fx.integration, ExpectedHead: mr.Head,
	})
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	rr, ok := rb.(gitflow.RollbackResult)
	if !ok || rr.Head != mr.Head {
		t.Fatalf("rollback result = %+v, want the recorded pre-merge head %s", rb, mr.Head)
	}
	status, err := fx.flow.Observe(context.Background(), gitflow.GitStatus{Dir: fx.integration, ExpectedHead: mr.Head, UntrackedAll: true})
	if err != nil {
		t.Fatalf("post-rollback status: %v", err)
	}
	st := status.(gitflow.StatusFacts)
	if !st.Clean() {
		t.Fatalf("post-rollback worktree is not clean: %+v", st)
	}
	// The conflicted file content is the merged Task's version (the
	// rollback restores the recorded head, never a half-merged state).
	data, err := os.ReadFile(filepath.Join(fx.integration, "calc.ts"))
	if err != nil {
		t.Fatalf("read calc: %v", err)
	}
	if string(data) != "export const v = 1;\n" {
		t.Fatalf("post-rollback content = %q, want the pre-merge content", data)
	}
	if strings.Contains(string(data), "<<<<<<<") {
		t.Fatalf("rollback left conflict markers behind")
	}
}

// TestIntegrationMergeSerialNoFF: a clean merge produces a separate
// --no-ff Merge Commit that preserves the Task's append-only history.
func TestIntegrationMergeSerialNoFF(t *testing.T) {
	fx := newIntegrationMergeFixture(t)
	b1 := fx.taskBranch("task-a", "export const v = 1;\n")
	res, err := fx.flow.Execute(context.Background(), gitflow.MergeIntegration{
		Path: fx.integration, Branch: b1,
	})
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	mr, ok := res.(gitflow.MergeResult)
	if !ok {
		t.Fatalf("merge result = %T, want MergeResult", res)
	}
	if mr.Head == fx.base {
		t.Fatalf("the merge did not advance the integration head")
	}
	// The Task Branch HEAD is an ancestor of the Integration HEAD (the
	// Task history is contained, append-only preserved).
	taskHead := strings.TrimSpace(string(fx.repo.git("rev-parse", "refs/heads/"+b1)))
	contained, err := fx.flow.Observe(context.Background(), gitflow.HistoryRange{From: mr.Head, To: taskHead})
	if err != nil {
		t.Fatalf("ancestry: %v", err)
	}
	if rf := contained.(gitflow.RangeFacts); len(rf.Commits) != 0 {
		t.Fatalf("task branch head is not contained in the integration history: %v", rf.Commits)
	}
	if code, ok := model.CodeOf(err); ok && code != "" {
		t.Fatalf("unexpected fault: %v", err)
	}
}
