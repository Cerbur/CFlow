package gitflow

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/process"
	"cflow.local/cflow/internal/security"
)

// worktree lifecycle primitives (design 15.2). Every creation is
// compare-and-swap guarded: the expected repository state (base commit
// exists, branch absent, destination path absent and outside every
// existing worktree) is verified before `git worktree add` runs, and the
// resulting registry entry is verified afterwards. The user's target
// branch is never touched, no local or global Git config is ever written,
// and an existing path or ref is never reused.

// createPlanningSnapshot creates the Planning Snapshot Worktree: a
// detached worktree fixed at the recorded Base Commit. No ref is created
// (PRD Worktree 策略: only the Integration Ref is withheld until
// Execution Approval).
func (g *GitFlow) createPlanningSnapshot(ctx context.Context, op CreatePlanningSnapshot) (GitResult, error) {
	if err := validateHead(op.BaseCommit); err != nil {
		return nil, err
	}
	path, err := g.validateWorktreePath(ctx, op.Path)
	if err != nil {
		return nil, err
	}
	if err := g.requireCommit(ctx, op.BaseCommit); err != nil {
		return nil, err
	}
	env := childEnv()
	if _, _, exit, err := g.run(ctx, g.dir, env, defaultGitTimeout, "worktree", "add", "--detach", path, op.BaseCommit); err != nil {
		return nil, err
	} else if exit.Fact != process.FactProcessExit || exit.Code != 0 {
		return nil, model.NewFault(model.CodeStateInvariantViolation,
			"gitflow: planning snapshot could not be created")
	}
	entry, err := g.verifiedEntry(ctx, path)
	if err != nil {
		return nil, err
	}
	if entry.Detached != true || entry.Head != op.BaseCommit {
		return nil, model.NewFault(model.CodeStateInvariantViolation,
			"gitflow: planning snapshot does not match the recorded base")
	}
	return PlanningSnapshotResult{Worktree: path, Head: entry.Head}, nil
}

// createIntegration creates the Integration Branch/Worktree from the
// recorded Base Commit (design 15.2: only after Execution Approval).
func (g *GitFlow) createIntegration(ctx context.Context, op CreateIntegration) (GitResult, error) {
	if err := validateBranchName(op.Branch); err != nil {
		return nil, err
	}
	if err := validateHead(op.BaseCommit); err != nil {
		return nil, err
	}
	path, err := g.validateWorktreePath(ctx, op.Path)
	if err != nil {
		return nil, err
	}
	if err := g.requireCommit(ctx, op.BaseCommit); err != nil {
		return nil, err
	}
	ref := "refs/heads/" + op.Branch
	if exists, err := g.refExists(ctx, ref); err != nil {
		return nil, err
	} else if exists {
		return nil, model.NewFault(model.CodeStateInvariantViolation,
			"gitflow: worktree branch already exists")
	}
	env := childEnv()
	if _, _, exit, err := g.run(ctx, g.dir, env, defaultGitTimeout, "worktree", "add", "-b", op.Branch, path, op.BaseCommit); err != nil {
		return nil, err
	} else if exit.Fact != process.FactProcessExit || exit.Code != 0 {
		return nil, model.NewFault(model.CodeStateInvariantViolation,
			"gitflow: integration worktree could not be created")
	}
	entry, err := g.verifiedEntry(ctx, path)
	if err != nil {
		return nil, err
	}
	if entry.Detached || entry.Branch != op.Branch || entry.Head != op.BaseCommit {
		return nil, model.NewFault(model.CodeStateInvariantViolation,
			"gitflow: integration worktree does not match the expected state")
	}
	return IntegrationWorktreeResult{Worktree: path, Branch: op.Branch, Head: entry.Head}, nil
}

// createTask creates one isolated Task Branch/Worktree from the verified
// Integration HEAD recorded when the Task became Ready (design 15.2). An
// unknown BaseHead is a fail-closed expected-HEAD mismatch: the Task
// never silently rebases onto a different baseline.
func (g *GitFlow) createTask(ctx context.Context, op CreateTask) (GitResult, error) {
	if err := validateBranchName(op.Branch); err != nil {
		return nil, err
	}
	if err := validateHead(op.BaseHead); err != nil {
		return nil, err
	}
	path, err := g.validateWorktreePath(ctx, op.Path)
	if err != nil {
		return nil, err
	}
	if err := g.requireCommit(ctx, op.BaseHead); err != nil {
		return nil, err
	}
	ref := "refs/heads/" + op.Branch
	if exists, err := g.refExists(ctx, ref); err != nil {
		return nil, err
	} else if exists {
		return nil, model.NewFault(model.CodeStateInvariantViolation,
			"gitflow: worktree branch already exists")
	}
	env := childEnv()
	if _, _, exit, err := g.run(ctx, g.dir, env, defaultGitTimeout, "worktree", "add", "-b", op.Branch, path, op.BaseHead); err != nil {
		return nil, err
	} else if exit.Fact != process.FactProcessExit || exit.Code != 0 {
		return nil, model.NewFault(model.CodeStateInvariantViolation,
			"gitflow: task worktree could not be created")
	}
	entry, err := g.verifiedEntry(ctx, path)
	if err != nil {
		return nil, err
	}
	if entry.Detached || entry.Branch != op.Branch || entry.Head != op.BaseHead {
		return nil, model.NewFault(model.CodeStateInvariantViolation,
			"gitflow: task worktree does not match the expected state")
	}
	return TaskWorktreeResult{Worktree: path, Branch: op.Branch, Head: entry.Head}, nil
}

// createApply creates the isolated Apply Branch/Worktree from the
// recorded Target HEAD (design 15.2: user Apply → isolated Apply
// Branch/Worktree). The same expected-state compare-and-swap guards the
// creation as every other Worktree primitive: base commit exists, branch
// absent, destination path absent and outside every existing worktree,
// and the resulting registry entry verified after creation. The user's
// target branch is never touched.
func (g *GitFlow) createApply(ctx context.Context, op CreateApply) (GitResult, error) {
	if err := validateBranchName(op.Branch); err != nil {
		return nil, err
	}
	if err := validateHead(op.BaseHead); err != nil {
		return nil, err
	}
	path, err := g.validateWorktreePath(ctx, op.Path)
	if err != nil {
		return nil, err
	}
	if err := g.requireCommit(ctx, op.BaseHead); err != nil {
		return nil, err
	}
	ref := "refs/heads/" + op.Branch
	if exists, err := g.refExists(ctx, ref); err != nil {
		return nil, err
	} else if exists {
		return nil, model.NewFault(model.CodeStateInvariantViolation,
			"gitflow: apply branch already exists")
	}
	env := childEnv()
	if _, _, exit, err := g.run(ctx, g.dir, env, defaultGitTimeout, "worktree", "add", "-b", op.Branch, path, op.BaseHead); err != nil {
		return nil, err
	} else if exit.Fact != process.FactProcessExit || exit.Code != 0 {
		return nil, model.NewFault(model.CodeStateInvariantViolation,
			"gitflow: apply worktree could not be created")
	}
	entry, err := g.verifiedEntry(ctx, path)
	if err != nil {
		return nil, err
	}
	if entry.Detached || entry.Branch != op.Branch || entry.Head != op.BaseHead {
		return nil, model.NewFault(model.CodeStateInvariantViolation,
			"gitflow: apply worktree does not match the expected state")
	}
	return ApplyWorktreeResult{Worktree: path, Branch: op.Branch, Head: entry.Head}, nil
}

// createAuditRef creates one append-only audit ref with expected-absent
// semantics (PRD Recovery: Expected-Absent `git update-ref`). An existing
// ref is never overwritten or moved.
func (g *GitFlow) createAuditRef(ctx context.Context, op CreateAuditRef) (GitResult, error) {
	if err := validateAuditRef(op.Ref); err != nil {
		return nil, err
	}
	if err := validateHead(op.Head); err != nil {
		return nil, err
	}
	env := childEnv()
	if _, errOut, exit, err := g.run(ctx, g.dir, env, defaultGitTimeout, "update-ref", op.Ref, op.Head, ""); err != nil {
		return nil, err
	} else if exit.Fact != process.FactProcessExit || exit.Code != 0 {
		return nil, model.NewFault(model.CodeStateInvariantViolation,
			"gitflow: audit ref already exists: "+string(errOut))
	}
	facts, err := g.refLookup(ctx, RefLookup{Ref: op.Ref})
	if err != nil {
		return nil, err
	}
	if !facts.Exists || facts.Value != op.Head {
		return nil, model.NewFault(model.CodeStateInvariantViolation,
			"gitflow: audit ref does not match its expected head")
	}
	return AuditRefResult{Ref: op.Ref, Head: op.Head}, nil
}

// requireCommit verifies that head resolves to a commit object in the
// repository. A missing expected base is an invariant failure: the caller
// recorded it earlier and reconciliation is impossible (design 15.2
// Recovery must never silently pick another baseline).
func (g *GitFlow) requireCommit(ctx context.Context, head string) error {
	_, _, exit, err := g.run(ctx, g.dir, childEnv(), defaultGitTimeout, "rev-parse", "--verify", head+"^{commit}")
	if err != nil {
		return err
	}
	if exit.Fact != process.FactProcessExit || exit.Code != 0 {
		return model.NewFault(model.CodeStateInvariantViolation,
			"gitflow: expected base head is not a commit")
	}
	return nil
}

// refExists reports whether a full refname exists in the repository.
func (g *GitFlow) refExists(ctx context.Context, ref string) (bool, error) {
	facts, err := g.refLookup(ctx, RefLookup{Ref: ref})
	if err != nil {
		return false, err
	}
	return facts.Exists, nil
}

// verifiedEntry returns the registry entry for path, failing closed when
// the worktree does not exist.
func (g *GitFlow) verifiedEntry(ctx context.Context, path string) (WorktreeEntry, error) {
	wf, err := g.worktreeList(ctx)
	if err != nil {
		return WorktreeEntry{}, err
	}
	for _, e := range wf.Entries {
		if e.Path == path {
			return e, nil
		}
	}
	return WorktreeEntry{}, model.NewFault(model.CodeStateInvariantViolation,
		"gitflow: created worktree is missing from the registry")
}

// validateWorktreePath validates a destination worktree path: absolute
// and canonical, with a security-guard-verified parent (Task 3: canonical
// identity, owner, owner-only mode, no group/other-writable ancestors),
// a leaf that does not exist, and a location outside every existing
// worktree including the user's own working tree.
func (g *GitFlow) validateWorktreePath(ctx context.Context, path string) (string, error) {
	if path == "" {
		return "", model.InvalidInputFault("gitflow: worktree path is empty")
	}
	if !filepath.IsAbs(path) {
		return "", model.InvalidInputFault("gitflow: worktree path must be absolute")
	}
	clean := filepath.Clean(path)
	parent := filepath.Dir(clean)
	if _, err := security.CheckPath(security.PathRequest{Path: parent, Kind: security.KindDir}); err != nil {
		return "", err
	}
	if _, err := os.Lstat(clean); err == nil {
		return "", model.InvalidInputFault("gitflow: worktree path already exists")
	}
	wf, err := g.worktreeList(ctx)
	if err != nil {
		return "", err
	}
	for _, e := range wf.Entries {
		if clean == e.Path || strings.HasPrefix(clean, e.Path+string(filepath.Separator)) {
			return "", model.InvalidInputFault("gitflow: worktree path is inside an existing worktree")
		}
	}
	return clean, nil
}

// ---------------------------------------------------------------------------
// Worktree removal and the safe-clean gate (design 17.4)
// ---------------------------------------------------------------------------

// removeWorktree removes one managed Worktree through the exact,
// non-force `git worktree remove <path>` form. The path must be absolute
// and canonical and must be a registered Worktree; a dirty, locked, or
// occupied Worktree is refused with CLEANUP_TARGET_DIRTY and is never
// force-removed (`git worktree prune` is never run and `--force` never
// appears). The caller records the expected registry entry before the
// removal and re-observes the registry afterwards, so a crash between the
// removal and the Result settles from the actual state.
func (g *GitFlow) removeWorktree(ctx context.Context, op RemoveWorktree) (GitResult, error) {
	if op.Path == "" {
		return nil, model.InvalidInputFault("gitflow: worktree removal requires an exact path")
	}
	clean := filepath.Clean(op.Path)
	if !filepath.IsAbs(clean) {
		return nil, model.InvalidInputFault("gitflow: worktree removal path must be absolute")
	}
	// The path must be a registered Worktree (the compare-and-swap the
	// caller recorded): a foreign path is never removed.
	if _, err := g.verifiedEntry(ctx, clean); err != nil {
		return nil, err
	}
	env := childEnv()
	_, errOut, exit, err := g.run(ctx, g.dir, env, defaultGitTimeout, "worktree", "remove", clean)
	if err != nil {
		return nil, err
	}
	if exit.Fact != process.FactProcessExit || exit.Code != 0 {
		return nil, model.NewFault(model.CodeCleanupTargetDirty,
			"gitflow: worktree removal refused (dirty, locked, or occupied): "+string(errOut))
	}
	return WorktreeRemovedResult{Path: clean}, nil
}

// worktreeInProgress observes the state markers of one managed Worktree's
// gitdir: an unfinished merge/rebase/cherry-pick/revert/bisect or an
// administrative lock file. The safe-clean gate refuses a target carrying
// either (git worktree remove would refuse the Worktree anyway; the gate
// refuses before any deletion is attempted, design 17.4). The exact
// Worktree must be registered; its gitdir is resolved with
// `git rev-parse --git-dir` inside the Worktree (the porcelain registry
// does not carry the gitdir).
func (g *GitFlow) worktreeInProgress(ctx context.Context, q WorktreeInProgress) (GitFacts, error) {
	if q.Path == "" {
		return nil, model.InvalidInputFault("gitflow: a worktree path is required")
	}
	clean := filepath.Clean(q.Path)
	if !filepath.IsAbs(clean) {
		return nil, model.InvalidInputFault("gitflow: worktree path must be absolute")
	}
	wf, err := g.worktreeList(ctx)
	if err != nil {
		return nil, err
	}
	registered := false
	for _, e := range wf.Entries {
		if e.Path == clean {
			registered = true
			break
		}
	}
	if !registered {
		return nil, model.NewFault(model.CodeCleanupFactsChanged,
			"gitflow: the worktree is not registered")
	}
	out, _, exit, err := g.run(ctx, clean, childEnv(), defaultGitTimeout, "rev-parse", "--git-dir")
	if err != nil {
		return nil, err
	}
	if exit.Fact != process.FactProcessExit || exit.Code != 0 {
		return nil, model.InvalidInputFault("gitflow: the worktree gitdir cannot be resolved")
	}
	gitDir := strings.TrimSpace(string(out))
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(clean, gitDir)
	}
	inProgress, locked, reason := worktreeStateMarkers(gitDir)
	return WorktreeInProgressFacts{InProgress: inProgress, Locked: locked, Reason: reason}, nil
}

// worktreeStateMarkers scans one worktree gitdir for the state files an
// in-progress Git operation or a lock leaves behind. inProgress markers
// are the merge/rebase/cherry-pick/revert/bisect state files; locked is
// any administrative *.lock file (index.lock, HEAD.lock, ...). Reason
// names the first marker found, for diagnostics.
func worktreeStateMarkers(gitDir string) (inProgress, locked bool, reason string) {
	markers := []string{
		"MERGE_HEAD", "MERGE_MSG", "MERGE_MODE", "AUTO_MERGE",
		"CHERRY_PICK_HEAD", "REVERT_HEAD",
		"BISECT_LOG", "BISECT_START",
		"rebase-merge", "rebase-apply", "sequencer",
	}
	for _, m := range markers {
		if _, err := os.Lstat(filepath.Join(gitDir, m)); err == nil {
			return true, false, m
		}
	}
	entries, err := os.ReadDir(gitDir)
	if err != nil {
		return false, false, ""
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".lock") {
			return false, true, e.Name()
		}
	}
	return false, false, ""
}
