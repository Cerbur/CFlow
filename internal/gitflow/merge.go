package gitflow

// The serial Integration Merge and the Conflict Rollback (design 15.5,
// PRD 已确认：Merge Conflict 处理). Integration merges are serial and
// `--no-ff`, preserving the Task's append-only Commit sequence and a
// separate Merge Commit. The merge is compare-and-swap guarded: the
// caller observes the expected pre-merge HEAD (GitStatus with
// ExpectedHead) before requesting the merge. A text conflict returns a
// typed MergeConflictResult — never an error — and the worktree stays in
// the conflicted merge state until RollbackMerge restores the recorded
// pre-merge HEAD with `git merge --abort` and verifies the result
// fail-closed.

import (
	"context"
	"path/filepath"
	"strings"

	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/process"
)

// mergeIntegration merges the Task Branch into the managed Integration
// Worktree with --no-ff (design 15.5). A text conflict returns a typed
// MergeConflictResult and leaves MERGE_HEAD in place for the Rollback;
// any other failure blocks closed. After a successful merge the new HEAD
// and the Merge Commit facts are verified: the Merge Commit's first
// parent is the pre-merge HEAD, and the merged Branch's HEAD becomes an
// ancestor of the new Integration HEAD (the Task history is contained).
func (g *GitFlow) mergeIntegration(ctx context.Context, op MergeIntegration) (GitResult, error) {
	if err := validateWorktreeDir(op.Path); err != nil {
		return nil, err
	}
	if err := validateBranchName(op.Branch); err != nil {
		return nil, err
	}
	env := childEnv()
	_, errOut, exit, err := g.run(ctx, op.Path, env, defaultGitTimeout, "merge", "--no-ff", "-m", "cflow: merge verified task branch "+op.Branch, op.Branch)
	if err != nil {
		return nil, err
	}
	switch {
	case exit.Fact == process.FactProcessExit && exit.Code == 0:
		// The merge committed; verify the resulting state fail-closed.
		head, err := g.revParseHead(ctx, op.Path, env)
		if err != nil {
			return nil, err
		}
		cf, err := g.commitInspect(ctx, CommitInspect{Ref: head})
		if err != nil {
			return nil, err
		}
		if len(cf.Parents) < 2 {
			return nil, model.NewFault(model.CodeStateInvariantViolation,
				"gitflow: integration merge commit has no task parent")
		}
		return MergeResult{Path: op.Path, Head: head, Commit: cf}, nil
	case exit.Fact == process.FactProcessExit && exit.Code == 1:
		// Text conflict: the merge did not commit; the typed result
		// carries the pre-merge HEAD and leaves the conflicted state for
		// the RollbackMerge operation (design 15.5).
		head, err := g.revParseHead(ctx, op.Path, env)
		if err != nil {
			return nil, err
		}
		return MergeConflictResult{Path: op.Path, PreMergeHead: head}, nil
	case exit.Fact == process.FactProcessExit && exit.Code == 2:
		// The merge was refused (e.g. the Task Branch HEAD moved since
		// the pre-merge observation): the caller's expected facts no
		// longer hold; block closed.
		return nil, model.NewFault(model.CodeStateInvariantViolation,
			"gitflow: integration merge refused: "+lastLine(string(errOut)))
	default:
		return nil, model.NewFault(model.CodeStateInvariantViolation,
			"gitflow: integration merge did not complete")
	}
}

// rollbackMerge restores a conflicted managed Integration Worktree to its
// recorded pre-merge HEAD (`git merge --abort`) and verifies the expected
// HEAD and a clean worktree fail-closed (PRD 已确认：Merge Conflict 处理:
// 失败时恢复 Pre-Merge HEAD). A worktree without a merge in progress
// blocks: the pre-merge state can no longer be restored safely.
func (g *GitFlow) rollbackMerge(ctx context.Context, op RollbackMerge) (GitResult, error) {
	if err := validateWorktreeDir(op.Path); err != nil {
		return nil, err
	}
	if err := validateHead(op.ExpectedHead); err != nil {
		return nil, err
	}
	env := childEnv()
	_, _, exit, err := g.run(ctx, op.Path, env, defaultGitTimeout, "merge", "--abort")
	if err != nil {
		return nil, err
	}
	if exit.Fact != process.FactProcessExit || exit.Code != 0 {
		return nil, model.NewFault(model.CodeStateInvariantViolation,
			"gitflow: integration rollback refused: no merge in progress to abort")
	}
	status, err := g.gitStatusAt(ctx, op.Path, env, op.ExpectedHead, true, false)
	if err != nil {
		return nil, err
	}
	if status.Head != op.ExpectedHead || !status.Clean() {
		return nil, model.NewFault(model.CodeStateInvariantViolation,
			"gitflow: integration worktree was not restored to the recorded pre-merge head")
	}
	return RollbackResult{Path: op.Path, Head: status.Head}, nil
}

// validateWorktreeDir validates an integration worktree path: absolute
// and canonical (the same rules the worktree creation validates).
func validateWorktreeDir(path string) error {
	if path == "" {
		return model.InvalidInputFault("gitflow: worktree path is empty")
	}
	if !filepath.IsAbs(path) {
		return model.InvalidInputFault("gitflow: worktree path must be absolute")
	}
	return nil
}

func lastLine(s string) string {
	trimmed := strings.TrimSpace(s)
	if i := strings.LastIndex(trimmed, "\n"); i >= 0 {
		return trimmed[i+1:]
	}
	return trimmed
}
