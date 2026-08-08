package app

// The safe cleanup protocol (Task 20, PRD 已确认：Cleanup 仅删除安全干净的
// 衍生目录, design 17.4): the destructive-boundary tests. The default
// command produces ONLY the immutable Dry Run Manifest and deletes
// nothing; the explicit execution revalidates every item's facts against
// the exact confirmed Manifest and removes each target only through the
// exact non-force operation — never escalating to a force, never deleting
// a dirty or drifted target, never touching a Branch/ref/Commit/SQLite
// state/evidence. Partial results stop subsequent items and Block the
// Attempt; Recovery reconciles by exact path + Git Worktree Registry +
// Intent/Result.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/store"
)

// ---------------------------------------------------------------------------
// fixture
// ---------------------------------------------------------------------------

// cleanupFixture drives one completed workflow (with its managed Task and
// Integration Worktrees) through the safe cleanup protocol over the
// recording gitTrace, so the tests observe every git argv and can inject
// crashes at the exact boundaries.
type cleanupFixture struct {
	t  *testing.T
	af *applyFixture
	wf model.WorkflowID
	a  *Application // the most recent cleanup-phase Application
}

func newCleanupFixture(t *testing.T) *cleanupFixture {
	af := completedWorkflowForApply(t)
	return &cleanupFixture{t: t, af: af, wf: af.wf}
}

// cleanupApp builds a fresh Application over the recording gitTrace.
func (cf *cleanupFixture) cleanupApp() *Application {
	cf.a = cf.af.applyApp()
	return cf.a
}

func (cf *cleanupFixture) scratchItems(paths []string) []model.CleanupItem {
	items := make([]model.CleanupItem, 0, len(paths))
	for _, p := range paths {
		items = append(items, model.CleanupItem{Kind: model.CleanupScratch, CanonicalPath: p})
	}
	return items
}

// cleanupManifest is the Dry Run outcome the fixture returns: the attempt
// identity and the exact Manifest hash the confirmation binds.
type cleanupManifest struct {
	ID   model.CleanupAttemptID
	Hash string
	att  model.CleanupAttempt
}

// PlanCleanup runs the dry run (auto-collected managed Worktrees plus the
// explicit exact scratch paths) and returns the immutable Manifest.
func (cf *cleanupFixture) PlanCleanup(scratch ...string) cleanupManifest {
	cf.t.Helper()
	out, err := cf.cleanupApp().Execute(context.Background(),
		DryRunCommand{Workflow: cf.wf, Items: cf.scratchItems(scratch)})
	if err != nil {
		cf.t.Fatalf("plan cleanup: %v", err)
	}
	if out.Cleanup == nil {
		cf.t.Fatalf("dry run produced no cleanup manifest")
	}
	return cleanupManifest{ID: out.Cleanup.ID, Hash: out.Cleanup.Manifest.Hash, att: *out.Cleanup}
}

// planErr runs the dry run and returns its error.
func (cf *cleanupFixture) planErr(scratch ...string) error {
	_, err := cf.cleanupApp().Execute(context.Background(),
		DryRunCommand{Workflow: cf.wf, Items: cf.scratchItems(scratch)})
	return err
}

// executeOutcome runs the explicit execution of the confirmed Manifest and
// returns the Outcome (a partial-result Block carries the outcome, not an
// error; a decision-level fault returns the error).
func (cf *cleanupFixture) executeOutcome(manifestID model.CleanupAttemptID, manifestHash string) (Outcome, error) {
	cf.t.Helper()
	return cf.cleanupApp().Execute(context.Background(), ExecuteCleanupCommand{
		Workflow: cf.wf,
		Manifest: model.ArtifactRef{Workflow: cf.wf, Type: model.ArtifactCleanupManifest,
			Revision: 1, Hash: manifestHash},
	})
}

// ExecuteCleanup runs the explicit execution and returns only its error.
func (cf *cleanupFixture) ExecuteCleanup(manifestID model.CleanupAttemptID, manifestHash string) error {
	_, err := cf.executeOutcome(manifestID, manifestHash)
	return err
}

func (cf *cleanupFixture) inspect() InspectView {
	return aInspect(cf.t, cf.cleanupApp(), cf.wf)
}

func (cf *cleanupFixture) latestCleanup() *model.CleanupAttempt {
	iv := cf.inspect()
	if len(iv.CleanupAttempts) == 0 {
		cf.t.Fatalf("no cleanup attempt recorded")
	}
	return &iv.CleanupAttempts[len(iv.CleanupAttempts)-1]
}

// gitIn runs a git command inside dir (fixture identity env) and fails the
// test on error.
func (cf *cleanupFixture) gitIn(dir string, args ...string) string {
	cf.t.Helper()
	cmd := execGit(dir, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		cf.t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return string(out)
}

func (cf *cleanupFixture) taskNode() model.NodeID {
	cf.t.Helper()
	iv := cf.inspect()
	for _, n := range iv.Nodes {
		if n.Kind == model.NodeAgentTask {
			return n.ID
		}
	}
	cf.t.Fatalf("no task node in the completed workflow")
	return ""
}

func (cf *cleanupFixture) taskWorktreePath() string {
	// Layout Version 2: the aggregated temporary root <root>/tmp/tasks/
	// <node> (design 8.5, TUI task 7); the legacy layout reads the
	// worktrees/<key>/<wf>/tasks/<node> path.
	return filepath.Join(cf.af.fx.home, "projects", ProjectFor(cf.af.fx.root).Key, string(cf.wf), "tmp", "tasks", string(cf.taskNode()))
}

func (cf *cleanupFixture) integrationWorktreePath() string {
	// Layout Version 2: the aggregated Workspace IS the delivery mainline
	// (design 8.5, TUI task 7); the legacy layout reads the
	// worktrees/<key>/<wf>/integration path.
	return filepath.Join(cf.af.fx.home, "projects", ProjectFor(cf.af.fx.root).Key, string(cf.wf), "workspace")
}

func (cf *cleanupFixture) planningWorktreePath() string {
	return filepath.Join(cf.af.fx.home, "worktrees", ProjectFor(cf.af.fx.root).Key, string(cf.wf), "planning")
}

// workspacePath is the aggregated Workspace root of the fixture's
// workflow (Layout Version 2, design §8).
func (cf *cleanupFixture) workspacePath() string {
	return filepath.Join(cf.af.fx.home, "projects", ProjectFor(cf.af.fx.root).Key, string(cf.wf), "workspace")
}

func (cf *cleanupFixture) taskBranch() string {
	return "cflow/" + string(cf.wf) + "/task-" + string(cf.taskNode())
}

func (cf *cleanupFixture) integrationBranch() string {
	// Layout Version 2: the Workspace Branch is the delivery mainline
	// (design 8.5, TUI task 7); the legacy layout reads the Integration
	// Branch.
	return "cflow/" + string(cf.wf) + "/workspace"
}

// ---------------------------------------------------------------------------
// worktree mutation helpers
// ---------------------------------------------------------------------------

// WriteIgnoredTaskFile commits a `.gitignore` into the Task Worktree and
// writes a file that git ignores (the PRD Commit/Clean gate form: ignored
// content never counts toward the ordinary Dirty Fingerprint, so the
// strict cleanup re-observation surfaces it as a fact mismatch).
func (cf *cleanupFixture) WriteIgnoredTaskFile(rel string) {
	cf.t.Helper()
	wt := cf.taskWorktreePath()
	if err := os.WriteFile(filepath.Join(wt, ".gitignore"), []byte("*.bin\n"), 0o600); err != nil {
		cf.t.Fatalf("write .gitignore: %v", err)
	}
	cf.gitIn(wt, "add", ".gitignore")
	cf.gitIn(wt, "commit", "-q", "-m", "ignore binaries")
	if err := os.WriteFile(filepath.Join(wt, filepath.FromSlash(rel)), []byte("cache content\n"), 0o600); err != nil {
		cf.t.Fatalf("write ignored file: %v", err)
	}
}

// WriteTaskFile writes a regular (non-ignored) file into the Task
// Worktree.
func (cf *cleanupFixture) WriteTaskFile(rel, content string) {
	cf.t.Helper()
	wt := cf.taskWorktreePath()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(wt, filepath.FromSlash(rel))), 0o700); err != nil {
		cf.t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wt, filepath.FromSlash(rel)), []byte(content), 0o600); err != nil {
		cf.t.Fatalf("write %s: %v", rel, err)
	}
}

// StageTaskFile writes and stages a file in the Task Worktree.
func (cf *cleanupFixture) StageTaskFile(rel, content string) {
	cf.t.Helper()
	cf.WriteTaskFile(rel, content)
	cf.gitIn(cf.taskWorktreePath(), "add", rel)
}

// DirtyTaskWorktree unstages a tracked file in the Task Worktree.
func (cf *cleanupFixture) DirtyTaskWorktree(rel, content string) {
	cf.t.Helper()
	cf.WriteTaskFile(rel, content)
}

// AdvanceTaskHead commits a change on the Task Branch (a HEAD drift the
// execution must reject).
func (cf *cleanupFixture) AdvanceTaskHead() {
	cf.t.Helper()
	wt := cf.taskWorktreePath()
	cf.gitIn(wt, "commit", "-q", "--allow-empty", "-m", "late task head advance")
}

// StartInProgressMerge writes the MERGE_HEAD state marker into the Task
// Worktree's gitdir (an in-progress merge the safe-clean gate refuses).
func (cf *cleanupFixture) StartInProgressMerge() {
	cf.t.Helper()
	wt := cf.taskWorktreePath()
	head := strings.TrimSpace(cf.gitIn(wt, "rev-parse", "HEAD"))
	gitDir := strings.TrimSpace(cf.gitIn(wt, "rev-parse", "--git-dir"))
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(wt, gitDir)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "MERGE_HEAD"), []byte(head+"\n"), 0o600); err != nil {
		cf.t.Fatalf("write MERGE_HEAD: %v", err)
	}
}

// seedRunningProcess appends a RUNNING managed process record to the
// workflow aggregate (bound to an existing Session) so the decision sees
// a live process.
func (cf *cleanupFixture) seedRunningProcess() {
	cf.t.Helper()
	a := cf.cleanupApp()
	st, err := a.ensureWriteStore(context.Background(), cf.wf)
	if err != nil {
		cf.t.Fatalf("open store: %v", err)
	}
	view, err := st.View(context.Background(), store.StoreQuery{})
	if err != nil {
		cf.t.Fatalf("store view: %v", err)
	}
	session := ""
	for _, s := range view.State.Sessions {
		session = string(s.ID)
		break
	}
	if session == "" {
		cf.t.Fatalf("the completed workflow has no session to bind the seeded process")
	}
	_, err = st.Transact(context.Background(), view.AggregateVersion, func(state model.State) (model.Decision, error) {
		return model.Decision{
			Mutations: []model.Mutation{model.ProcessAppendMutation{Process: model.ProcessRecord{
				ID: "proc-cleanup-1", Session: model.SessionID(session), Status: model.ProcessStatusRunning,
				StartedAt: state.Now,
			}}},
		}, nil
	})
	if err != nil {
		cf.t.Fatalf("seed running process: %v", err)
	}
}

// ---------------------------------------------------------------------------
// assertions
// ---------------------------------------------------------------------------

func (cf *cleanupFixture) RequireTaskWorktreePresent() {
	cf.t.Helper()
	cf.RequireWorktreePresent(cf.taskWorktreePath())
}

func (cf *cleanupFixture) RequireWorktreePresent(path string) {
	cf.t.Helper()
	if _, err := os.Stat(path); err != nil {
		cf.t.Fatalf("worktree %s was removed: %v", path, err)
	}
	if out := cf.gitIn(cf.af.fx.root, "worktree", "list", "--porcelain"); !strings.Contains(out, "worktree "+path) {
		cf.t.Fatalf("worktree %s is missing from the registry", path)
	}
}

func (cf *cleanupFixture) RequireWorktreeRemoved(path string) {
	cf.t.Helper()
	if _, err := os.Stat(path); err == nil {
		cf.t.Fatalf("worktree %s still present", path)
	}
	if out := cf.gitIn(cf.af.fx.root, "worktree", "list", "--porcelain"); strings.Contains(out, "worktree "+path) {
		cf.t.Fatalf("worktree %s still registered", path)
	}
}

func (cf *cleanupFixture) RequireTaskBranchPresent() {
	cf.t.Helper()
	cf.RequireBranchPresent(cf.taskBranch())
}

func (cf *cleanupFixture) RequireBranchPresent(branch string) {
	cf.t.Helper()
	ref := "refs/heads/" + branch
	if out := strings.TrimSpace(cf.gitIn(cf.af.fx.root, "rev-parse", "--verify", "--quiet", ref)); out == "" {
		cf.t.Fatalf("branch %s was deleted", branch)
	}
}

func (cf *cleanupFixture) RequireCommitPresent(head string) {
	cf.t.Helper()
	cf.gitIn(cf.af.fx.root, "cat-file", "-e", head+"^{commit}")
}

func (cf *cleanupFixture) RequireWorkflowCompleted() {
	cf.t.Helper()
	iv := cf.inspect()
	if iv.Status.Stage != model.StageCompleted || iv.Status.Runtime != model.RuntimeSucceeded {
		cf.t.Fatalf("workflow = %s/%s, want COMPLETED/SUCCEEDED", iv.Status.Stage, iv.Status.Runtime)
	}
	if len(iv.Sessions) == 0 {
		cf.t.Fatalf("cleanup removed the session records")
	}
}

func (cf *cleanupFixture) requireItem(item *model.CleanupItem, status model.CleanupItemStatus, code model.Code) {
	cf.t.Helper()
	if item == nil {
		cf.t.Fatalf("cleanup item is missing")
	}
	if item.Status != status {
		cf.t.Fatalf("cleanup item %s = %s, want %s", item.CanonicalPath, item.Status, status)
	}
	if status == model.CleanupItemFailed && item.FailureCode != code {
		cf.t.Fatalf("cleanup item failure = %s, want %s", item.FailureCode, code)
	}
}

// ---------------------------------------------------------------------------
// the mandated verbatim test (brief Step 1)
// ---------------------------------------------------------------------------

// TestCleanupRejectsIgnoredContentAndPreservesBranch: an ignored file in
// the Task Worktree (which the ordinary Task gate counts as clean) is a
// fact mismatch for the strictly-safer cleanup re-observation; the
// execution refuses with CLEANUP_FACT_MISMATCH and the Worktree and its
// Branch stay present.
func TestCleanupRejectsIgnoredContentAndPreservesBranch(t *testing.T) {
	fx := newCleanupFixture(t)
	fx.WriteIgnoredTaskFile("cache.bin")
	manifest := fx.PlanCleanup()
	err := fx.ExecuteCleanup(manifest.ID, manifest.Hash)
	assertFaultCode(t, err, model.CodeCleanupFactsChanged)
	fx.RequireTaskWorktreePresent()
	fx.RequireTaskBranchPresent()
	fx.RequireWorkflowCompleted()
}

// ---------------------------------------------------------------------------
// the case list
// ---------------------------------------------------------------------------

// TestCleanupDryRunDeletesNothing: the default command only produces the
// immutable Manifest (awaiting the explicit confirmation) and removes no
// target and no branch.
func TestCleanupDryRunDeletesNothing(t *testing.T) {
	fx := newCleanupFixture(t)
	manifest := fx.PlanCleanup()
	if manifest.att.Status != model.CleanupStatusAwaitingConfirmation {
		t.Fatalf("dry run attempt = %s, want AWAITING_CONFIRMATION", manifest.att.Status)
	}
	if manifest.Hash == "" {
		t.Fatalf("dry run produced no manifest hash")
	}
	fx.RequireTaskWorktreePresent()
	fx.RequireWorktreePresent(fx.integrationWorktreePath())
	fx.RequireTaskBranchPresent()
	fx.RequireBranchPresent(fx.integrationBranch())
	fx.RequireWorkflowCompleted()
}

// TestCleanupRejectsDirtyStagedContent: staged content present at the dry
// run is recorded dirty and the execution refuses with
// CLEANUP_TARGET_DIRTY; the Worktree and Branch stay present.
func TestCleanupRejectsDirtyStagedContent(t *testing.T) {
	fx := newCleanupFixture(t)
	fx.StageTaskFile("staged.txt", "staged content\n")
	manifest := fx.PlanCleanup()
	err := fx.ExecuteCleanup(manifest.ID, manifest.Hash)
	assertFaultCode(t, err, model.CodeCleanupTargetDirty)
	fx.RequireTaskWorktreePresent()
	fx.RequireTaskBranchPresent()
	fx.RequireWorkflowCompleted()
}

// TestCleanupRejectsDirtyUnstagedAndUntrackedContent: unstaged and
// untracked content present at the dry run refuse with CLEANUP_TARGET_DIRTY.
func TestCleanupRejectsDirtyUnstagedAndUntrackedContent(t *testing.T) {
	fx := newCleanupFixture(t)
	fx.DirtyTaskWorktree("src/divide/divide.go", "package divide\n\n// dirty\n")
	fx.WriteTaskFile("untracked.txt", "untracked content\n")
	manifest := fx.PlanCleanup()
	err := fx.ExecuteCleanup(manifest.ID, manifest.Hash)
	assertFaultCode(t, err, model.CodeCleanupTargetDirty)
	fx.RequireTaskWorktreePresent()
	fx.RequireTaskBranchPresent()
	fx.RequireWorkflowCompleted()
}

// TestCleanupRejectsInProgressGitOperation: an in-progress merge state
// marker in the Worktree gitdir makes the target not safe-clean; the
// execution refuses with CLEANUP_TARGET_DIRTY (git would refuse the
// removal; CFlow refuses before attempting it).
func TestCleanupRejectsInProgressGitOperation(t *testing.T) {
	fx := newCleanupFixture(t)
	fx.StartInProgressMerge()
	manifest := fx.PlanCleanup()
	err := fx.ExecuteCleanup(manifest.ID, manifest.Hash)
	assertFaultCode(t, err, model.CodeCleanupTargetDirty)
	fx.RequireTaskWorktreePresent()
	fx.RequireTaskBranchPresent()
	fx.RequireWorkflowCompleted()
}

// TestCleanupRejectsActiveProcess: a RUNNING managed process blocks the
// dry run with CLEANUP_ACTIVE_PROCESS.
func TestCleanupRejectsActiveProcess(t *testing.T) {
	fx := newCleanupFixture(t)
	fx.seedRunningProcess()
	err := fx.planErr()
	assertFaultCode(t, err, model.CodeCleanupActiveProcess)
	fx.RequireTaskWorktreePresent()
	fx.RequireTaskBranchPresent()
}

// TestCleanupRejectsHeadDrift: a HEAD advanced after the dry run is a
// fact mismatch the execution rejects with CLEANUP_FACT_MISMATCH.
func TestCleanupRejectsHeadDrift(t *testing.T) {
	fx := newCleanupFixture(t)
	manifest := fx.PlanCleanup()
	fx.AdvanceTaskHead()
	err := fx.ExecuteCleanup(manifest.ID, manifest.Hash)
	assertFaultCode(t, err, model.CodeCleanupFactsChanged)
	fx.RequireTaskWorktreePresent()
	fx.RequireTaskBranchPresent()
	fx.RequireWorkflowCompleted()
}

// TestCleanupRejectsFingerprintDrift: content added after the dry run is
// a fact mismatch the execution rejects with CLEANUP_FACT_MISMATCH.
func TestCleanupRejectsFingerprintDrift(t *testing.T) {
	fx := newCleanupFixture(t)
	manifest := fx.PlanCleanup()
	fx.WriteTaskFile("late.txt", "late untracked content\n")
	err := fx.ExecuteCleanup(manifest.ID, manifest.Hash)
	assertFaultCode(t, err, model.CodeCleanupFactsChanged)
	fx.RequireTaskWorktreePresent()
	fx.RequireTaskBranchPresent()
	fx.RequireWorkflowCompleted()
}

// TestCleanupRejectsManifestHashMismatch: the explicit confirmation must
// bind the exact Manifest hash; a changed hash is CLEANUP_FACT_MISMATCH
// and deletes nothing.
func TestCleanupRejectsManifestHashMismatch(t *testing.T) {
	fx := newCleanupFixture(t)
	manifest := fx.PlanCleanup()
	err := fx.ExecuteCleanup(manifest.ID, "0000000000000000000000000000000000000000000000000000000000000000")
	assertFaultCode(t, err, model.CodeCleanupFactsChanged)
	fx.RequireTaskWorktreePresent()
	fx.RequireTaskBranchPresent()
	fx.RequireWorkflowCompleted()
}

// TestCleanupRejectsBroadScratchPaths: the exact scratch guard refuses
// the Workspace root, the CFLOW_HOME root, the filesystem root, `~`, and
// a broad ancestor of the Workspace root, before any Manifest is produced.
func TestCleanupRejectsBroadScratchPaths(t *testing.T) {
	fx := newCleanupFixture(t)
	for _, bad := range []string{
		fx.af.fx.root,               // the Workspace root
		fx.af.fx.home,               // the CFLOW_HOME root
		"/",                         // the filesystem root
		"~",                         // the home token
		filepath.Dir(fx.af.fx.root), // a broad ancestor of the Workspace root
	} {
		if err := fx.planErr(bad); err == nil {
			t.Fatalf("scratch path %q must be rejected", bad)
		}
	}
	fx.RequireTaskWorktreePresent()
	fx.RequireTaskBranchPresent()
}

// TestCleanupRejectsSymlinkEscapeScratch: a scratch target that resolves
// through a symlink is never removed.
func TestCleanupRejectsSymlinkEscapeScratch(t *testing.T) {
	fx := newCleanupFixture(t)
	link := filepath.Join(fx.af.fx.home, "escape-link")
	if err := os.Symlink(fx.af.fx.root, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if err := fx.planErr(link); err == nil {
		t.Fatalf("a symlink scratch target must be rejected")
	}
	fx.RequireTaskWorktreePresent()
	fx.RequireTaskBranchPresent()
}

// TestCleanupRejectsWrongOwnerScratch: a target not owned by the effective
// user fails the owner gate (/etc/passwd is root-owned on every POSIX
// system; when running as root the guard still fails closed).
func TestCleanupRejectsWrongOwnerScratch(t *testing.T) {
	fx := newCleanupFixture(t)
	if _, err := os.Lstat("/etc/passwd"); err != nil {
		t.Skipf("no /etc/passwd: %v", err)
	}
	if err := fx.planErr("/etc/passwd"); err == nil {
		t.Fatalf("a non-owned scratch target must be rejected")
	}
	fx.RequireTaskWorktreePresent()
	fx.RequireTaskBranchPresent()
}

// TestCleanupPartialResultStopsAtFailedItem: the Task Worktree removal
// command is injected to fail, so the Task item FAILS and the Attempt
// Blocks with partial results explicit; the scratch item is never
// requested and every unremoved target stays preserved. The aggregated
// Workspace (the delivery mainline, design 8.5) is never a Cleanup
// target.
func TestCleanupPartialResultStopsAtFailedItem(t *testing.T) {
	fx := newCleanupFixture(t)
	scratch := filepath.Join(fx.af.fx.home, "scratch", "run-1", "tmp")
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		t.Fatalf("mkdir scratch: %v", err)
	}
	if err := os.WriteFile(filepath.Join(scratch, "payload.txt"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write scratch payload: %v", err)
	}
	manifest := fx.PlanCleanup(scratch)
	// Fail the Task Worktree removal command itself: the Task item (0)
	// fails, the scratch item (1) is never requested.
	fx.af.trace.armFailCall(func(args []string) bool {
		return len(args) >= 3 && args[0] == "worktree" && args[1] == "remove" && args[2] == fx.taskWorktreePath()
	})
	out, err := fx.executeOutcome(manifest.ID, manifest.Hash)
	if err != nil {
		t.Fatalf("execute cleanup: %v", err)
	}
	if out.Cleanup == nil || out.Cleanup.Status != model.CleanupStatusBlocked {
		t.Fatalf("cleanup = %+v, want a BLOCKED attempt with partial results", out.Cleanup)
	}
	att := out.Cleanup
	for i := range att.Items {
		switch att.Items[i].CanonicalPath {
		case fx.taskWorktreePath():
			fx.requireItem(&att.Items[i], model.CleanupItemFailed, model.CodeCleanupItemFailed)
		case scratch:
			if att.Items[i].Status != model.CleanupItemPending {
				t.Fatalf("scratch item = %s, want PENDING (never requested beyond the failure)", att.Items[i].Status)
			}
		}
	}
	fx.RequireTaskWorktreePresent()
	fx.RequireTaskBranchPresent()
	fx.RequireWorktreePresent(fx.workspacePath())
	if _, err := os.Stat(scratch); err != nil {
		t.Fatalf("scratch %s was touched by a blocked cleanup", scratch)
	}
	fx.RequireWorkflowCompleted()
}

// TestCleanupCrashAfterRemovalRecovers: the Task Worktree removal
// succeeds but the post-removal verification is injected to fail, leaving
// the item REQUESTED with the Worktree already gone; the retry reconciles
// (the exact target is absent) and completes the remaining items without
// ever re-removing or pretending the removed target present. The
// aggregated Workspace (the delivery mainline) is never a Cleanup target.
func TestCleanupCrashAfterRemovalRecovers(t *testing.T) {
	fx := newCleanupFixture(t)
	manifest := fx.PlanCleanup()
	fx.af.trace.armFailAfter(func(args []string) bool {
		return len(args) >= 3 && args[0] == "worktree" && args[1] == "remove" && args[2] == fx.taskWorktreePath()
	})
	if err := fx.ExecuteCleanup(manifest.ID, manifest.Hash); err == nil {
		t.Fatalf("the injected post-removal crash must fail the execution")
	}
	fx.RequireWorktreeRemoved(fx.taskWorktreePath())
	iv := fx.inspect()
	att := iv.CleanupAttempts[len(iv.CleanupAttempts)-1]
	if att.Status != model.CleanupStatusRunning {
		t.Fatalf("cleanup = %s after the crash, want RUNNING (unsettled)", att.Status)
	}
	if att.Items[0].Status != model.CleanupItemRequested {
		t.Fatalf("item 0 = %s after the crash, want REQUESTED", att.Items[0].Status)
	}

	fx.af.trace.disarm()
	out, err := fx.executeOutcome(manifest.ID, manifest.Hash)
	if err != nil {
		t.Fatalf("cleanup recovery: %v", err)
	}
	if out.Cleanup == nil || out.Cleanup.Status != model.CleanupStatusSucceeded {
		t.Fatalf("cleanup after recovery = %+v, want SUCCEEDED", out.Cleanup)
	}
	fx.RequireWorktreeRemoved(fx.taskWorktreePath())
	fx.RequireTaskBranchPresent()
	fx.RequireWorktreeRemoved(fx.workspacePath())
	fx.RequireBranchPresent(fx.integrationBranch())
	fx.RequireWorkflowCompleted()
}

// TestCleanupRemovesCleanWorktreesAndPreservesEvidence: a fully clean
// terminal workflow removes exactly the managed Worktrees and preserves
// every Branch, Commit, the Planning Snapshot, the aggregate, and its
// Sessions/evidence; the Workflow stays COMPLETED and the manifest stays
// immutable.
func TestCleanupRemovesCleanWorktreesAndPreservesEvidence(t *testing.T) {
	fx := newCleanupFixture(t)
	taskHead := strings.TrimSpace(fx.gitIn(fx.taskWorktreePath(), "rev-parse", "HEAD"))
	manifest := fx.PlanCleanup()
	out, err := fx.executeOutcome(manifest.ID, manifest.Hash)
	if err != nil {
		t.Fatalf("execute cleanup: %v", err)
	}
	if out.Cleanup == nil || out.Cleanup.Status != model.CleanupStatusSucceeded {
		t.Fatalf("cleanup = %+v, want SUCCEEDED", out.Cleanup)
	}
	fx.RequireWorktreeRemoved(fx.taskWorktreePath())
	// The Workspace is a managed code directory of the explicit Cleanup
	// (design §8.5, TUI task 15): it is removed.
	fx.RequireWorktreeRemoved(fx.workspacePath())
	// Branches, Commits, audit data, and the Workflow aggregate stay.
	fx.RequireBranchPresent(fx.integrationBranch())
	fx.RequireTaskBranchPresent()
	fx.RequireCommitPresent(taskHead)
	fx.RequireCommitPresent(strings.TrimSpace(fx.gitIn(fx.af.fx.root, "rev-parse", "refs/heads/"+fx.integrationBranch())))
	fx.RequireWorkflowCompleted()
	// No invocation anywhere in the cleanup path ever used a force flag.
	for _, spec := range fx.af.trace.specs {
		for _, arg := range spec.Args {
			if arg == "-f" || arg == "--force" {
				t.Fatalf("cleanup issued the force flag: %v", spec.Args)
			}
		}
	}
	if !fx.af.trace.everyGit(func(args []string) bool {
		if len(args) < 2 || args[0] != "worktree" || args[1] != "remove" {
			return true
		}
		return len(args) == 3 // worktree remove <path>: no --force, no --keep
	}) {
		t.Fatalf("a worktree remove invocation carried an unexpected argument")
	}
}

// TestCleanupRemovesExactScratchPath: the exact scratch target is removed
// and a sentinel outside it is untouched.
func TestCleanupRemovesExactScratchPath(t *testing.T) {
	fx := newCleanupFixture(t)
	scratch := filepath.Join(fx.af.fx.home, "scratch", "run-1", "tmp")
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		t.Fatalf("mkdir scratch: %v", err)
	}
	if err := os.WriteFile(filepath.Join(scratch, "payload.txt"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write scratch payload: %v", err)
	}
	sentinel := filepath.Join(fx.af.fx.home, "scratch", "run-1", "keep.txt")
	if err := os.WriteFile(sentinel, []byte("keep"), 0o600); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}
	manifest := fx.PlanCleanup(scratch)
	out, err := fx.executeOutcome(manifest.ID, manifest.Hash)
	if err != nil {
		t.Fatalf("execute cleanup: %v", err)
	}
	if out.Cleanup == nil || out.Cleanup.Status != model.CleanupStatusSucceeded {
		t.Fatalf("cleanup = %+v, want SUCCEEDED", out.Cleanup)
	}
	if _, err := os.Stat(scratch); err == nil {
		t.Fatalf("scratch %s still present", scratch)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("sentinel %s was removed by a broader-than-exact deletion", sentinel)
	}
	fx.RequireWorktreeRemoved(fx.taskWorktreePath())
	fx.RequireTaskBranchPresent()
	fx.RequireWorktreeRemoved(fx.workspacePath())
	fx.RequireWorkflowCompleted()
}

// TestCleanupRequiresTerminalWorkflow: a non-terminal workflow is refused
// with CLEANUP_WORKFLOW_NOT_TERMINAL and never produces a Manifest.
func TestCleanupRequiresTerminalWorkflow(t *testing.T) {
	fx := newExecutionFixture(t)
	wf, err := fx.create("cleanup-demo", false)
	if err != nil {
		t.Fatal(err)
	}
	_, err = fx.app().Execute(context.Background(), DryRunCommand{Workflow: wf})
	assertFaultCode(t, err, model.CodeCleanupWorkflowNotTerminal)
}
