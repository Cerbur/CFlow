package integration

// The safe cleanup end to end (Task 20, PRD 已确认：Cleanup 仅删除安全干净的
// 衍生目录, design 17.4): the real pipeline drives one workflow through
// planning, execution, serial merges, Final Verify, and exact-evidence
// completion; the cleanup dry run produces the immutable Manifest over
// the managed Worktrees; the explicit execution — the second confirmation
// binding the exact Manifest ID and hash — removes exactly those Worktrees
// through the non-force typed operation and preserves every Branch, Commit,
// the Planning Snapshot, and the COMPLETED Workflow.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cflow.local/cflow/internal/app"
	"cflow.local/cflow/internal/model"
)

// ---------------------------------------------------------------------------
// fixture
// ---------------------------------------------------------------------------

type cleanupIntegrationFixture struct {
	t  *testing.T
	af *applyIntegrationFixture
	wf model.WorkflowID
}

func newCleanupIntegrationFixture(t *testing.T) *cleanupIntegrationFixture {
	return &cleanupIntegrationFixture{t: t, af: newApplyIntegrationFixture(t)}
}

func (cf *cleanupIntegrationFixture) driveToCompletion() model.WorkflowID {
	cf.wf = cf.af.driveToCompletion()
	return cf.wf
}

func (cf *cleanupIntegrationFixture) taskNode() model.NodeID {
	iv := cf.af.fx.Inspect(cf.wf)
	for _, n := range iv.Nodes {
		if n.Kind == model.NodeAgentTask {
			return n.ID
		}
	}
	cf.t.Fatalf("no task node in the completed workflow")
	return ""
}

func (cf *cleanupIntegrationFixture) taskWorktreePath() string {
	return filepath.Join(cf.af.fx.home, "projects",
		app.ProjectFor(cf.af.fx.repo.Root).Key, string(cf.wf), "tmp", "tasks", string(cf.taskNode()))
}

func (cf *cleanupIntegrationFixture) integrationWorktreePath() string {
	// Layout Version 2: the aggregated Workspace IS the delivery mainline
	// (design 8.5, TUI task 7); the legacy layout reads the
	// worktrees/<key>/<wf>/integration path.
	return filepath.Join(cf.af.fx.home, "projects",
		app.ProjectFor(cf.af.fx.repo.Root).Key, string(cf.wf), "workspace")
}

func (cf *cleanupIntegrationFixture) planningWorktreePath() string {
	return filepath.Join(cf.af.fx.home, "worktrees",
		app.ProjectFor(cf.af.fx.repo.Root).Key, string(cf.wf), "planning")
}

// workspacePath returns the aggregated Workspace root of the fixture's
// workflow (Layout Version 2 planning mainline, design §8).
func (cf *cleanupIntegrationFixture) workspacePath() string {
	return filepath.Join(cf.af.fx.home, "projects",
		app.ProjectFor(cf.af.fx.repo.Root).Key, string(cf.wf), "workspace")
}

func (cf *cleanupIntegrationFixture) manifestOf(a *app.Application) *model.CleanupAttempt {
	out, err := a.Execute(context.Background(), app.DryRunCommand{Workflow: cf.wf})
	if err != nil {
		cf.t.Fatalf("cleanup dry run: %v", err)
	}
	if out.Cleanup == nil {
		cf.t.Fatalf("dry run produced no cleanup manifest")
	}
	return out.Cleanup
}

// ---------------------------------------------------------------------------
// tests
// ---------------------------------------------------------------------------

// TestCleanupDryRunThenExactExecuteRemovesManagedWorktrees: the full
// pipeline completes; the dry run creates the immutable Manifest and
// deletes nothing; the exact-confirmation execution removes exactly the
// managed Integration and Task Worktrees while preserving the Planning
// Snapshot, every Branch and Commit, and the COMPLETED Workflow.
func TestCleanupDryRunThenExactExecuteRemovesManagedWorktrees(t *testing.T) {
	cf := newCleanupIntegrationFixture(t)
	cf.driveToCompletion()
	a := cf.af.fx.app()

	// The dry run produces the Manifest and removes nothing.
	manifest := cf.manifestOf(a)
	if manifest.Status != model.CleanupStatusAwaitingConfirmation {
		t.Fatalf("dry run = %s, want AWAITING_CONFIRMATION", manifest.Status)
	}
	if manifest.Manifest.Hash == "" {
		t.Fatalf("dry run produced no manifest hash")
	}
	if _, err := os.Stat(cf.taskWorktreePath()); err != nil {
		t.Fatalf("dry run removed the task worktree: %v", err)
	}
	if _, err := os.Stat(cf.integrationWorktreePath()); err != nil {
		t.Fatalf("dry run removed the integration worktree: %v", err)
	}

	// The exact-confirmation execution removes the managed Worktrees.
	out, err := a.Execute(context.Background(), app.ExecuteCleanupCommand{
		Workflow: cf.wf,
		Manifest: model.ArtifactRef{Workflow: cf.wf, Type: model.ArtifactCleanupManifest,
			Revision: 1, Hash: manifest.Manifest.Hash},
	})
	if err != nil {
		t.Fatalf("execute cleanup: %v", err)
	}
	if out.Cleanup == nil || out.Cleanup.Status != model.CleanupStatusSucceeded {
		t.Fatalf("cleanup = %+v, want SUCCEEDED", out.Cleanup)
	}
	for _, it := range out.Cleanup.Items {
		if it.Status != model.CleanupItemCompleted {
			t.Fatalf("cleanup item %s = %s, want COMPLETED", it.CanonicalPath, it.Status)
		}
	}
	if _, err := os.Stat(cf.taskWorktreePath()); err == nil {
		t.Fatalf("task worktree still present")
	}
	// The Workspace (the aggregated delivery mainline, design 8.5) is
	// never a Cleanup target.
	if _, err := os.Stat(cf.integrationWorktreePath()); err != nil {
		t.Fatalf("workspace was removed by cleanup: %v", err)
	}
	if _, err := os.Stat(cf.workspacePath()); err != nil {
		t.Fatalf("workspace was removed: %v", err)
	}
	// Branches and commits survive.
	taskBranch := "cflow/" + string(cf.wf) + "/task-" + string(cf.taskNode())
	if out := strings.TrimSpace(string(cf.af.fx.repo.git("rev-parse", "--verify", "--quiet", "refs/heads/"+taskBranch))); out == "" {
		t.Fatalf("task branch %s was deleted", taskBranch)
	}
	if out := strings.TrimSpace(string(cf.af.fx.repo.git("rev-parse", "--verify", "--quiet", "refs/heads/cflow/"+string(cf.wf)+"/workspace"))); out == "" {
		t.Fatalf("workspace branch was deleted")
	}
	iv := cf.af.fx.Inspect(cf.wf)
	if iv.Status.Stage != model.StageCompleted || iv.Status.Runtime != model.RuntimeSucceeded {
		t.Fatalf("workflow = %s/%s after cleanup, want COMPLETED/SUCCEEDED",
			iv.Status.Stage, iv.Status.Runtime)
	}
}

// TestCleanupIntegrationRejectsIgnoredContent: an ignored file in the Task
// Worktree is a fact mismatch for the strictly-safer cleanup
// re-observation; the execution refuses and the Worktree stays.
func TestCleanupIntegrationRejectsIgnoredContent(t *testing.T) {
	cf := newCleanupIntegrationFixture(t)
	cf.driveToCompletion()
	wt := cf.taskWorktreePath()
	cf.af.fx.repo.gitAt(wt, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(wt, ".gitignore"), []byte("*.bin\n"), 0o600); err != nil {
		t.Fatalf("write .gitignore: %v", err)
	}
	cf.af.fx.repo.gitAt(wt, "add", ".gitignore")
	cf.af.fx.repo.gitAt(wt, "commit", "-q", "-m", "ignore binaries")
	if err := os.WriteFile(filepath.Join(wt, "cache.bin"), []byte("cache"), 0o600); err != nil {
		t.Fatalf("write ignored file: %v", err)
	}
	a := cf.af.fx.app()
	manifest := cf.manifestOf(a)
	_, err := a.Execute(context.Background(), app.ExecuteCleanupCommand{
		Workflow: cf.wf,
		Manifest: model.ArtifactRef{Workflow: cf.wf, Type: model.ArtifactCleanupManifest,
			Revision: 1, Hash: manifest.Manifest.Hash},
	})
	if err == nil {
		t.Fatalf("ignored content must refuse the cleanup execution")
	}
	if code, ok := model.CodeOf(err); !ok || code != model.CodeCleanupFactsChanged {
		t.Fatalf("cleanup fault = %v, want %s", err, model.CodeCleanupFactsChanged)
	}
	if _, serr := os.Stat(wt); serr != nil {
		t.Fatalf("the refused cleanup removed the task worktree")
	}
	if out := strings.TrimSpace(string(cf.af.fx.repo.git("rev-parse", "--verify", "--quiet",
		"refs/heads/cflow/"+string(cf.wf)+"/task-"+string(cf.taskNode())))); out == "" {
		t.Fatalf("the refused cleanup deleted the task branch")
	}
}
