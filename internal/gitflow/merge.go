package gitflow

// The serial Integration Merge and the Conflict Rollback (design 15.5,
// PRD 已确认：Merge Conflict 处理). Integration merges are serial and
// `--no-ff`, preserving the Task's append-only Commit sequence and a
// separate Merge Commit. The merge is compare-and-swap guarded: the
// caller observes the expected pre-merge HEAD (GitStatus with
// ExpectedHead) before requesting the merge.
//
// A text conflict (git's deterministic "CONFLICT" marker) returns a typed
// MergeConflictResult — never an error — and the worktree stays in the
// conflicted merge state until RollbackMerge restores the recorded
// pre-merge HEAD. A merge that COMMITTED but failed its post-merge checks
// (Merge Commit identity drift, dirty post-merge Worktree) is restored by
// the same RollbackMerge operation through the guarded reset: only the
// managed Integration Worktree is moved back to the recorded pre-merge
// HEAD, and only when the current HEAD is a descendant of it (a foreign
// or replaced history is never destroyed; the failed Merge Commit stays
// as captured evidence).

import (
	"context"
	"path/filepath"
	"strings"

	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/process"
)

// mergeIntegration merges the Task Branch into the managed Integration
// Worktree with --no-ff (design 15.5). A text conflict (exit 1 with the
// deterministic "CONFLICT" marker) returns a typed MergeConflictResult
// and leaves MERGE_HEAD in place for the Rollback; any other exit-1
// failure (a refused merge that did not commit) blocks closed — it must
// never be mistaken for a conflict, which would send it down the
// unrollable conflict path. After a successful merge the new HEAD and
// the Merge Commit facts are verified: the Merge Commit has at least two
// parents (the pre-merge HEAD and the merged Task Branch HEAD, so the
// Task history is contained).
func (g *GitFlow) mergeIntegration(ctx context.Context, op MergeIntegration) (GitResult, error) {
	if err := validateWorktreeDir(op.Path); err != nil {
		return nil, err
	}
	if err := validateBranchName(op.Branch); err != nil {
		return nil, err
	}
	env := childEnv()
	out, errOut, exit, err := g.run(ctx, op.Path, env, defaultGitTimeout, "merge", "--no-ff", "-m", "cflow: merge verified task branch "+op.Branch, op.Branch)
	if err != nil {
		return nil, err
	}
	// git writes the conflict marker to stdout, the refusal reasons to
	// stderr; both streams are checked.
	conflictMarker := strings.Contains(string(out), "CONFLICT") || strings.Contains(string(errOut), "CONFLICT")
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
	case exit.Fact == process.FactProcessExit && exit.Code == 1 && conflictMarker:
		// Text conflict: the merge did not commit; the typed result
		// carries the pre-merge HEAD and leaves the conflicted state for
		// the RollbackMerge operation (design 15.5).
		head, err := g.revParseHead(ctx, op.Path, env)
		if err != nil {
			return nil, err
		}
		return MergeConflictResult{Path: op.Path, PreMergeHead: head}, nil
	case exit.Fact == process.FactProcessExit && exit.Code == 1:
		// A non-conflict merge failure (the merge was refused and did not
		// commit): the caller's expected facts no longer hold; block
		// closed. The Rollback then observes the unchanged pre-merge HEAD
		// and completes as a no-op.
		return nil, model.NewFault(model.CodeStateInvariantViolation,
			"gitflow: integration merge refused: "+lastLine(string(errOut)))
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

// rollbackMerge restores the managed Integration Worktree to the recorded
// pre-merge HEAD (PRD 已确认：Merge Conflict 处理: 失败时恢复 Pre-Merge
// HEAD) and verifies the result fail-closed. Three states are restored:
//
//  1. the Worktree already sits at the recorded HEAD and is clean — a
//     merge that was refused before changing anything: the rollback is a
//     verified no-op;
//  2. a conflicted merge in progress (HEAD unchanged, unmerged entries) —
//     `git merge --abort` restores the pre-merge state;
//  3. a merge that COMMITTED (HEAD advanced past the recorded head by a
//     post-merge-check failure such as Merge Commit identity drift) —
//     the guarded reset `git reset --hard <recorded head>` moves ONLY the
//     managed Integration Worktree back. The reset is refuse-to-run when
//     the current HEAD is not a descendant of the recorded head: a
//     foreign or replaced history is never destroyed.
//
// Only the managed Integration Worktree is ever touched; the user's
// working tree, the Task Branches, and the recorded evidence are not.
func (g *GitFlow) rollbackMerge(ctx context.Context, op RollbackMerge) (GitResult, error) {
	if err := validateWorktreeDir(op.Path); err != nil {
		return nil, err
	}
	if err := validateHead(op.ExpectedHead); err != nil {
		return nil, err
	}
	env := childEnv()
	current, err := g.revParseHead(ctx, op.Path, env)
	if err != nil {
		return nil, err
	}
	switch {
	case current == op.ExpectedHead:
		// The HEAD already is the recorded pre-merge head: either a
		// refused merge that changed nothing (case 1), or a conflicted
		// merge whose HEAD never moved (case 2).
		status, err := g.gitStatusAt(ctx, op.Path, env, op.ExpectedHead, true, false)
		if err != nil {
			return nil, err
		}
		if status.Clean() {
			// Nothing to restore: the pre-merge state is already in place.
			return RollbackResult{Path: op.Path, Head: status.Head}, nil
		}
		// Unmerged entries: abort the in-progress merge.
		if _, _, exit, err := g.run(ctx, op.Path, env, defaultGitTimeout, "merge", "--abort"); err != nil {
			return nil, err
		} else if exit.Fact != process.FactProcessExit || exit.Code != 0 {
			return nil, model.NewFault(model.CodeStateInvariantViolation,
				"gitflow: integration rollback refused: no merge in progress to abort")
		}
	default:
		// The HEAD moved: a committed merge that failed its post-merge
		// checks. The current HEAD must be a descendant of the recorded
		// pre-merge head — a replaced or foreign history is never
		// destroyed by the rollback.
		if !g.isDescendantOf(ctx, op.Path, env, current, op.ExpectedHead) {
			return nil, model.NewFault(model.CodeStateInvariantViolation,
				"gitflow: integration rollback refused: the current head is not a descendant of the recorded pre-merge head")
		}
		if _, _, exit, err := g.run(ctx, op.Path, env, defaultGitTimeout, "reset", "--hard", op.ExpectedHead); err != nil {
			return nil, err
		} else if exit.Fact != process.FactProcessExit || exit.Code != 0 {
			return nil, model.NewFault(model.CodeStateInvariantViolation,
				"gitflow: integration rollback reset failed")
		}
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

// isDescendantOf reports whether from's history contains to (to is an
// ancestor of from: rev-list from..to is empty).
func (g *GitFlow) isDescendantOf(ctx context.Context, dir string, env map[string]string, from, to string) bool {
	out, _, exit, err := g.run(ctx, dir, env, defaultGitTimeout, "rev-list", from+".."+to)
	if err != nil {
		return false
	}
	if exit.Fact != process.FactProcessExit || exit.Code != 0 {
		return false
	}
	return len(strings.TrimSpace(string(out))) == 0
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
