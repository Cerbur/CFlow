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
	message := op.Message
	if message == "" {
		message = "cflow: merge verified task branch " + op.Branch
	}
	env := childEnv()
	out, errOut, exit, err := g.run(ctx, op.Path, env, defaultGitTimeout, "merge", "--no-ff", "-m", message, op.Branch)
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

// completeMerge finishes a conflicted Apply staging merge after the ONE
// restricted Merge Resolution Attempt (design 15.5): only the exact
// conflict files are staged — any change outside them fails closed — and
// the Merge Commit is created with the recorded parents (`git commit`
// completes the merge while MERGE_HEAD is set, exactly what `git merge
// --continue` runs). The Merge Commit must carry at least two parents and
// the Apply Worktree must be clean afterwards.
func (g *GitFlow) completeMerge(ctx context.Context, op CompleteMerge) (GitResult, error) {
	if err := validateWorktreeDir(op.Path); err != nil {
		return nil, err
	}
	if op.Message == "" {
		return nil, model.InvalidInputFault("gitflow: merge continuation message is required")
	}
	env := childEnv()
	// A merge must actually be in progress.
	if _, _, exit, err := g.run(ctx, op.Path, env, defaultGitTimeout, "rev-parse", "-q", "--verify", "MERGE_HEAD"); err != nil {
		return nil, err
	} else if exit.Fact != process.FactProcessExit || exit.Code != 0 {
		return nil, model.NewFault(model.CodeStateInvariantViolation,
			"gitflow: no merge in progress to continue")
	}
	// The resolution write scope is exactly the conflict file set.
	if len(op.ConflictFiles) == 0 {
		return nil, model.InvalidInputFault("gitflow: merge continuation requires the conflict file set")
	}
	for _, rel := range op.ConflictFiles {
		clean := filepath.Clean(filepath.FromSlash(rel))
		if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return nil, model.InvalidInputFault("gitflow: conflict file escapes the worktree")
		}
	}
	stage := append([]string{"add", "--"}, op.ConflictFiles...)
	if _, _, exit, err := g.run(ctx, op.Path, env, defaultGitTimeout, stage...); err != nil {
		return nil, err
	} else if exit.Fact != process.FactProcessExit || exit.Code != 0 {
		return nil, model.NewFault(model.CodeStateInvariantViolation,
			"gitflow: conflict files cannot be staged")
	}
	if _, _, exit, err := g.run(ctx, op.Path, env, defaultGitTimeout, "commit", "-m", op.Message); err != nil {
		return nil, err
	} else if exit.Fact != process.FactProcessExit || exit.Code != 0 {
		return nil, model.NewFault(model.CodeStateInvariantViolation,
			"gitflow: the apply merge could not be completed")
	}
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
			"gitflow: the apply merge commit has no integration parent")
	}
	status, err := g.gitStatusAt(ctx, op.Path, env, head, true, false)
	if err != nil {
		return nil, err
	}
	if !status.Clean() {
		return nil, model.NewFault(model.CodeStateInvariantViolation,
			"gitflow: the apply worktree is not clean after the merge continuation")
	}
	return MergeResult{Path: op.Path, Head: head, Commit: cf}, nil
}

// updateRef performs the final compare-and-swap fast-forward of the
// Target Branch (design 15.5, PRD 已确认：显式受保护 Apply steps 5-6): the
// expected old value is re-observed immediately before the update, the
// staging head must be a descendant of it (a fast-forward), and the ref
// is updated only through the three-argument expected-value form of `git
// update-ref`. There is no force-update argv anywhere in this path. The
// reported outcome is the observed actual ref after the swap.
func (g *GitFlow) updateRef(ctx context.Context, op UpdateRef) (GitResult, error) {
	if err := validateAuditRef(op.Ref); err != nil {
		return nil, err
	}
	if err := validateHead(op.New); err != nil {
		return nil, err
	}
	if err := validateHead(op.Expected); err != nil {
		return nil, err
	}
	env := childEnv()
	facts, err := g.refLookup(ctx, RefLookup{Ref: op.Ref, Expected: op.Expected})
	if err != nil {
		return nil, err
	}
	if !facts.Matches {
		return nil, model.NewFault(model.CodeTargetHeadChanged,
			"gitflow: the target ref no longer matches the recorded head")
	}
	if !g.isDescendantOf(ctx, g.dir, env, op.New, op.Expected) {
		return nil, model.NewFault(model.CodeTargetHeadChanged,
			"gitflow: the staging head is not a fast-forward of the recorded target head")
	}
	if _, _, exit, err := g.run(ctx, g.dir, env, defaultGitTimeout, "update-ref", op.Ref, op.New, op.Expected); err != nil {
		return nil, err
	} else if exit.Fact != process.FactProcessExit || exit.Code != 0 {
		return nil, model.NewFault(model.CodeTargetHeadChanged,
			"gitflow: the target ref moved during the compare-and-swap")
	}
	observed, err := g.refLookup(ctx, RefLookup{Ref: op.Ref})
	if err != nil {
		return nil, err
	}
	if !observed.Exists || observed.Value != op.New {
		return nil, model.NewFault(model.CodeTargetHeadChanged,
			"gitflow: the observed target ref does not match the delivered head")
	}
	return UpdateRefResult{Ref: op.Ref, Observed: observed.Value}, nil
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
