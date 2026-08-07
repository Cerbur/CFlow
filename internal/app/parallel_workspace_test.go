package app

// Parallel Task merge into the single Workspace (TUI task 7, design 8.5):
// parallel sibling Tasks share the verified Workspace Head as their Base,
// and their serial --no-ff merges advance the SAME Workspace Branch. The
// aggregated Workspace is the only long-lived delivery mainline; no
// Integration Worktree exists on the aggregated layout.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cflow.local/cflow/internal/model"
)

// parallelImplementationScript is the Fake coding Session output that
// writes and commits one distinct file per Task Worktree (the parallel
// sibling shape): each Task's commit lands in its own Worktree only.
func parallelImplementationScript() string {
	return `{"fixture":"fake-run","script_version":1,"provider":"fake","dialect":"cflow.dialect.fake.v1","purpose":"implementation","session_id":"i1","exit_code":0,"resume":"ok","tasks":{"task-s01":{"writes":[{"path":"src/divide/divide.go","content":"package divide\n\n// Divide returns a/b.\nfunc Divide(a, b int) (int, error) {\n\treturn a / b, nil\n}\n"}],"commit":"implement divide s01"},"task-s02":{"writes":[{"path":"src/multiply/multiply.go","content":"package multiply\n\n// Multiply returns a*b.\nfunc Multiply(a, b int) int {\n\treturn a * b\n}\n"}],"commit":"implement multiply s02"}}}
{"type":"session_started","session_id":"i1","at_ms":0}
{"type":"assistant_message","session_id":"i1","text":"Implemented both tasks.","at_ms":10}
{"type":"session_finished","session_id":"i1","result":{"summary":"implemented"},"at_ms":20}`
}

// parallelSpecScript is the Spec Generation Session output carrying TWO
// sibling Specs (S01, S02): the compiled Workflow then has two agent-task
// Nodes, two verify Nodes, and two merge Nodes.
func parallelSpecScript() string {
	return `{"fixture":"fake-run","script_version":1,"provider":"fake","dialect":"cflow.dialect.fake.v1","purpose":"spec-generation","session_id":"s1","exit_code":0,"resume":"ok"}
{"type":"session_started","session_id":"s1","at_ms":0}
{"type":"assistant_message","session_id":"s1","text":"Splitting the plan.","at_ms":10}
{"type":"session_finished","session_id":"s1","result":{"specs":[{"id":"s01","goal":"implement divide","depends_on":[],"write_scope":["src/divide/**"],"read_scope":[],"locks":[],"acceptance":{"verification_command_ids":["verify"]},"route":{"provider":"fake","model":"default","budget":10},"timeout_seconds":1800,"max_retry":0},{"id":"s02","goal":"implement multiply","depends_on":[],"write_scope":["src/multiply/**"],"read_scope":[],"locks":[],"acceptance":{"verification_command_ids":["verify"]},"route":{"provider":"fake","model":"default","budget":10},"timeout_seconds":1800,"max_retry":0}],"proposed_commands":[]},"at_ms":20}`
}

// parallelPatch is the Workflow Optimization patch adding the observation
// checkpoint to the merge-s02 node (mirroring checkpointPatch).
const parallelPatch = `{"schema":"cflow-workflow-patch-1","operations":[{"op":"add_checkpoint","node_id":"merge-s02"}]}`

// startParallelTasks drives the workflow through the execution gate with
// the two-spec plan and dispatches the two sibling Tasks until both coding
// Sessions committed.
func startParallelTasks(t *testing.T, fx *planningFixture) (model.WorkflowID, *Application) {
	t.Helper()
	// Two parallel siblings require a concurrency cap of 2 (design 8.5:
	// parallel temporary Task Worktrees).
	if err := os.MkdirAll(fx.home, 0o700); err != nil {
		t.Fatalf("mkdir home: %v", err)
	}
	if err := os.WriteFile(filepath.Join(fx.home, "config.yaml"), []byte("concurrency: 2\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	wf := fx.planningApproved()
	if _, err := fx.app(parallelSpecScript()).Execute(context.Background(),
		GenerateSpecsCommand{Workflow: wf, Provider: "fake"}); err != nil {
		t.Fatalf("spec generation: %v", err)
	}
	if _, err := fx.app(patchOutputScript("w1", parallelPatch)).Execute(context.Background(),
		CompileWorkflowCommand{Workflow: wf, Provider: "fake"}); err != nil {
		t.Fatalf("workflow compilation: %v", err)
	}
	if _, err := fx.app().Execute(context.Background(), ExecutionDryRunCommand{Workflow: wf}); err != nil {
		t.Fatalf("execution dry run: %v", err)
	}
	qv, err := fx.app().Query(context.Background(), ExecutionPreviewQuery{Workflow: wf})
	if err != nil {
		t.Fatalf("execution preview: %v", err)
	}
	approveExecution(t, fx, wf, qv.(ExecutionPreviewView))
	a := fx.app(parallelImplementationScript(), reviewPassScript(), finalReviewPassScript())
	for i := 0; i < 8; i++ {
		if _, err := a.Execute(context.Background(), DispatchCommand{Workflow: wf}); err != nil {
			t.Fatalf("dispatch pass %d: %v", i, err)
		}
	}
	return wf, a
}

// finishAndReview drives one Task through the commit gate, the
// deterministic verification, and the independent review.
func finishAndReview(t *testing.T, a *Application, wf model.WorkflowID, node string) {
	t.Helper()
	taskID := model.NodeID("task-" + strings.ToLower(node))
	for i := 0; i < 12; i++ {
		if _, err := a.Execute(context.Background(), DispatchCommand{Workflow: wf}); err != nil {
			t.Fatalf("finish/review %s pass %d: %v", node, i, err)
		}
		iv := aInspect(t, a, wf)
		for _, n := range iv.Nodes {
			if n.ID == taskID && n.Kind == model.NodeAgentTask && n.Status == model.NodeSucceeded {
				return
			}
		}
	}
	t.Fatalf("task %s did not finish within the pass budget", node)
}

// driveUntilComplete runs dispatch passes until the Workflow records
// COMPLETED (every merge serial, the Final Verify chain, and the exact
// evidence completion).
func driveUntilComplete(t *testing.T, a *Application, wf model.WorkflowID) {
	t.Helper()
	for i := 0; i < 24; i++ {
		if _, err := a.Execute(context.Background(), DispatchCommand{Workflow: wf}); err != nil {
			t.Fatalf("dispatch pass %d: %v", i, err)
		}
		iv := aInspect(t, a, wf)
		if iv.Status.Stage == model.StageCompleted {
			return
		}
	}
	t.Fatalf("workflow did not complete within the pass budget")
}

// requireWorkspaceContains asserts the Workspace Branch contains every
// sibling Task's implementation Commit (the serial merges all landed in
// the single Workspace).
func requireWorkspaceContains(t *testing.T, fx *planningFixture, wf model.WorkflowID, taskIDs ...string) {
	t.Helper()
	// The task branch of each sibling: cflow/<wf>/task-<node-id> (the node
	// id is the compiled "task-<spec>" form, so the branch is
	// cflow/<wf>/task-task-<spec>).
	for _, id := range taskIDs {
		branch := "cflow/" + string(wf) + "/task-task-" + strings.ToLower(id)
		commit := strings.TrimSpace(fx.git("rev-parse", "refs/heads/"+branch))
		if commit == "" {
			t.Fatalf("task branch %s has no commit", branch)
		}
		if out := strings.TrimSpace(fx.git("merge-base", "--is-ancestor", commit, "refs/heads/cflow/"+string(wf)+"/workspace")); out != "" {
			t.Fatalf("task %s commit %s is not contained in the workspace", id, commit)
		}
		if out := strings.TrimSpace(fx.git("rev-list", "cflow/"+string(wf)+"/workspace.."+commit)); out != "" {
			t.Fatalf("task %s commit %s is not an ancestor of the workspace head", id, commit)
		}
	}
}

// requireNoIntegrationWorktree asserts the aggregated layout created no
// Integration Worktree/Branch (the Workspace is the single mainline).
func requireNoIntegrationWorktree(t *testing.T, fx *planningFixture, wf model.WorkflowID) {
	t.Helper()
	integrationPath := filepath.Join(fx.home, "worktrees", ProjectFor(fx.root).Key, string(wf), "integration")
	if pathExists(integrationPath) {
		t.Fatalf("integration worktree %s exists on the aggregated layout", integrationPath)
	}
	cmd := execGit(fx.root, "rev-parse", "--verify", "--quiet", "refs/heads/cflow/"+string(wf)+"/integration")
	if out, err := cmd.Output(); err == nil && strings.TrimSpace(string(out)) != "" {
		t.Fatalf("integration branch exists on the aggregated layout: %s", out)
	}
}

// TestParallelTasksMergeSeriallyIntoWorkspace is the TUI task 7 failure
// test: two parallel sibling Tasks merge serially into the single
// Workspace (design 8.5), the Workspace Branch contains both Tasks'
// verified Commits, and no Integration Worktree ever exists.
func TestParallelTasksMergeSeriallyIntoWorkspace(t *testing.T) {
	fx := newExecutionFixture(t)
	wf, a := startParallelTasks(t, fx)
	finishAndReview(t, a, wf, "S01")
	finishAndReview(t, a, wf, "S02")
	driveUntilComplete(t, a, wf)
	requireWorkspaceContains(t, fx, wf, "S01", "S02")
	requireNoIntegrationWorktree(t, fx, wf)

	iv := aInspect(t, a, wf)
	if iv.Status.Stage != model.StageCompleted || iv.Status.Runtime != model.RuntimeSucceeded {
		t.Fatalf("workflow = %s/%s, want COMPLETED/SUCCEEDED", iv.Status.Stage, iv.Status.Runtime)
	}
	// The Workspace Head advanced past the base through the serial merges.
	workspaceHead := strings.TrimSpace(fx.git("rev-parse", "refs/heads/cflow/"+string(wf)+"/workspace"))
	baseHead := strings.TrimSpace(fx.git("rev-parse", "refs/heads/main"))
	if workspaceHead == baseHead {
		t.Fatal("the workspace head did not advance past the base")
	}
	// The user's Target Branch never moved.
	if out := strings.TrimSpace(fx.git("rev-parse", "refs/heads/main")); out != baseHead {
		t.Fatalf("the target branch moved: %s -> %s", baseHead, out)
	}
}
