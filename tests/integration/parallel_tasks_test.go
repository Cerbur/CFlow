// Package integration: the parallel Task dispatch end to end (Task 12).
// One workflow carries four calculator Specs; after the Execution
// Approval the dispatch pass starts the three independent Tasks in
// parallel (overlapping in virtual time) while the dependent Task waits
// for its dependency Merge Nodes. Every Task codes only inside its own
// Task Worktree created from the verified Integration HEAD (PRD Worktree
// 策略, 并行安全判断). The fixture drives the real pipeline exactly as
// the CLI routes it: no app internals are touched.
package integration

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"cflow.local/cflow/internal/app"
	"cflow.local/cflow/internal/model"
)

// calculatorSpecs is the deterministic multi-Spec output of the Spec
// Generation Session: three independent Tasks and one Task that depends on
// the first two Merge Nodes, with disjoint write scopes.
const calculatorSpecs = `{"id":"s01","goal":"implement add","depends_on":[],"write_scope":["src/calc/add/**"],"read_scope":[],"locks":[],"acceptance":{"verification_command_ids":["verify"]},"route":{"provider":"fake","model":"default","budget":10},"timeout_seconds":1800,"max_retry":2}
{"id":"s02","goal":"implement subtract","depends_on":[],"write_scope":["src/calc/sub/**"],"read_scope":[],"locks":[],"acceptance":{"verification_command_ids":["verify"]},"route":{"provider":"fake","model":"default","budget":10},"timeout_seconds":1800,"max_retry":2}
{"id":"s03","goal":"implement multiply","depends_on":[],"write_scope":["src/calc/mul/**"],"read_scope":[],"locks":[],"acceptance":{"verification_command_ids":["verify"]},"route":{"provider":"fake","model":"default","budget":10},"timeout_seconds":1800,"max_retry":2}
{"id":"s04","goal":"implement divide","depends_on":["s01","s02"],"write_scope":["src/calc/div/**"],"read_scope":[],"locks":[],"acceptance":{"verification_command_ids":["verify"]},"route":{"provider":"fake","model":"default","budget":10},"timeout_seconds":1800,"max_retry":2}`

// calculatorSpecsScript wraps the four Specs in the Session output. The
// frames are one JSON object per line, so the spec set joins onto the
// session_finished line.
func calculatorSpecsScript(sessionID string) string {
	return fmt.Sprintf(`{"fixture":"fake-run","script_version":1,"provider":"fake","dialect":"cflow.dialect.fake.v1","purpose":"spec-generation","session_id":%s,"exit_code":0,"resume":"ok"}
{"type":"session_started","session_id":%s,"at_ms":0}
{"type":"assistant_message","session_id":%s,"text":"Splitting the plan.","at_ms":10}
{"type":"session_finished","session_id":%s,"result":{"specs":[%s],"proposed_commands":[]},"at_ms":20}`,
		strconv.Quote(sessionID), strconv.Quote(sessionID), strconv.Quote(sessionID), strconv.Quote(sessionID),
		strings.ReplaceAll(calculatorSpecs, "\n", ","))
}

// calculatorPatchScript is the Workflow Optimization Session output: one
// added Checkpoint on the first Merge.
func calculatorPatchScript(sessionID string) string {
	return fmt.Sprintf(`{"fixture":"fake-run","script_version":1,"provider":"fake","dialect":"cflow.dialect.fake.v1","purpose":"workflow-optimization","session_id":%s,"exit_code":0,"resume":"ok"}
{"type":"session_started","session_id":%s,"at_ms":0}
{"type":"assistant_message","session_id":%s,"text":"Proposing a scheduling patch.","at_ms":10}
{"type":"session_finished","session_id":%s,"result":{"schema":"cflow-workflow-patch-1","operations":[{"op":"add_checkpoint","node_id":"merge-s01"}]},"at_ms":20}`,
		strconv.Quote(sessionID), strconv.Quote(sessionID), strconv.Quote(sessionID), strconv.Quote(sessionID))
}

// implementationScript is the deterministic Fake coding Session output:
// one shared script per purpose, writing the coded file into whichever
// Task Worktree the run's working directory is.
func implementationScript(sessionID string) string {
	return fmt.Sprintf(`{"fixture":"fake-run","script_version":1,"provider":"fake","dialect":"cflow.dialect.fake.v1","purpose":"implementation","session_id":%s,"exit_code":0,"resume":"ok","writes":[{"path":"src/calc/divide.go","content":"package calc\n\n// Divide returns a/b.\nfunc Divide(a, b int) (int, error) {\n\treturn 0, nil\n}\n"}]}
{"type":"session_started","session_id":%s,"at_ms":0}
{"type":"assistant_message","session_id":%s,"text":"Implemented the calculator task.","at_ms":10}
{"type":"session_finished","session_id":%s,"result":{"summary":"implemented"},"at_ms":20}`,
		strconv.Quote(sessionID), strconv.Quote(sessionID), strconv.Quote(sessionID), strconv.Quote(sessionID))
}

// parallelFixture is the execution fixture: the planning fixture with the
// verification wrappers fixed at the Base Commit and a configured Git
// identity (the Commit Preflight resolves it).
type parallelFixture struct {
	*planningFixture
}

func newParallelFixture(t *testing.T) *parallelFixture {
	t.Helper()
	fx := &parallelFixture{planningFixture: newPlanningFixture(t)}
	fx.repo.git("config", "user.name", "Test User")
	fx.repo.git("config", "user.email", "test@example.com")
	if err := os.MkdirAll(filepath.Join(fx.repo.Root, "scripts"), 0o755); err != nil {
		t.Fatalf("mkdir scripts: %v", err)
	}
	if err := os.WriteFile(filepath.Join(fx.repo.Root, "scripts", "verify.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write verify.sh: %v", err)
	}
	if err := os.WriteFile(filepath.Join(fx.repo.Root, "scripts", "final-verify.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write final-verify.sh: %v", err)
	}
	fx.repo.git("add", "scripts")
	fx.repo.git("commit", "-q", "-m", "add verification wrappers")
	return fx
}

// driveToExecutionApproval runs the full planning and execution lifecycle
// through the Execution Approval with the calculator Spec set and returns
// the preview plus the workflow identity.
func (fx *parallelFixture) driveToExecutionApproval(t *testing.T) (model.WorkflowID, app.ExecutionPreviewView) {
	t.Helper()
	wf := fx.CreateWorkflow("calculator")
	fx.Discuss(wf, "a calculator with four operations; divide must error on zero")
	plan := fx.GeneratePlan(wf)
	if plan.SessionID == "" {
		t.Fatal("plan generation carried no session id")
	}
	check := fx.CheckPlan(wf)
	if check.SessionID == plan.SessionID {
		t.Fatal("checker reused the planner session")
	}
	pv := fx.planView(wf)
	if err := fx.ApprovePlan(wf, pv.Revision, pv.Hash); err != nil {
		t.Fatalf("approve plan: %v", err)
	}

	// Four independent-to-dispatch Specs require a concurrency of at least
	// three; the cap must be configured before the Compiler validates the
	// skeleton (PRD 并发上限).
	cfg := filepath.Join(fx.home, "config.yaml")
	if err := os.MkdirAll(fx.home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, []byte("concurrency: 4\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	specOut, err := fx.app(calculatorSpecsScript("s1")).Execute(context.Background(),
		app.GenerateSpecsCommand{Workflow: wf, Provider: "fake"})
	if err != nil {
		t.Fatalf("spec generation: %v", err)
	}
	if specOut.SessionID == "" {
		t.Fatal("spec generation carried no session id")
	}
	if _, err := fx.app(calculatorPatchScript("w1")).Execute(context.Background(),
		app.CompileWorkflowCommand{Workflow: wf, Provider: "fake"}); err != nil {
		t.Fatalf("workflow compilation: %v", err)
	}
	if _, err := fx.app().Execute(context.Background(),
		app.ExecutionDryRunCommand{Workflow: wf}); err != nil {
		t.Fatalf("execution dry run: %v", err)
	}
	qview, err := fx.app().Query(context.Background(), app.ExecutionPreviewQuery{Workflow: wf})
	if err != nil {
		t.Fatalf("execution preview: %v", err)
	}
	preview := qview.(app.ExecutionPreviewView)
	if len(preview.SpecHashes) != 1 {
		t.Fatalf("preview spec hashes = %v, want the one spec-set artifact", preview.SpecHashes)
	}
	if _, err := fx.app().Execute(context.Background(), app.ApproveExecutionCommand{
		Workflow:         wf,
		PlanHash:         preview.PlanHash,
		SpecHashes:       preview.SpecHashes,
		CatalogHash:      preview.CatalogHash,
		WorkflowHash:     preview.WorkflowHash,
		RoutingHash:      preview.RoutingHash,
		BudgetHash:       preview.BudgetHash,
		CommitPolicyHash: preview.CommitPolicyHash,
	}); err != nil {
		t.Fatalf("execution approval: %v", err)
	}
	return wf, preview
}

// planView projects the active Plan revision and hash for the approval.
func (fx *parallelFixture) planView(wf model.WorkflowID) app.PlanView {
	fx.t.Helper()
	view, err := fx.app().Query(context.Background(), app.PlanQuery{Workflow: wf})
	if err != nil {
		fx.t.Fatalf("plan query: %v", err)
	}
	return view.(app.PlanView)
}

// TestParallelTasksDispatchOverlapWhileDependentWaits (brief Step 5): the
// three independent calculator Tasks overlap in virtual time while S04
// waits on the Merge Nodes of S01 and S02.
func TestParallelTasksDispatchOverlapWhileDependentWaits(t *testing.T) {
	fx := newParallelFixture(t)
	wf, _ := fx.driveToExecutionApproval(t)

	out, err := fx.app(implementationScript("i1")).Execute(context.Background(),
		app.DispatchCommand{Workflow: wf})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if out.Workflow != wf {
		t.Fatalf("dispatch outcome workflow = %s, want %s", out.Workflow, wf)
	}

	iv := fx.Inspect(wf)
	statusByID := map[model.NodeID]model.NodeStatus{}
	for _, n := range iv.Nodes {
		statusByID[n.ID] = n.Status
	}
	// The three independent Tasks are RUNNING; the dependent Task waits on
	// the Merge Nodes of S01 and S02.
	for _, id := range []string{"task-s01", "task-s02", "task-s03"} {
		if statusByID[model.NodeID(id)] != model.NodeRunning {
			t.Fatalf("node %s status = %s, want RUNNING", id, statusByID[model.NodeID(id)])
		}
	}
	if statusByID["task-s04"] != model.NodePending {
		t.Fatalf("node task-s04 status = %s, want PENDING (waits on merge-s01 and merge-s02)", statusByID["task-s04"])
	}
	// Verify and Merge Nodes of the running Tasks are not yet ready.
	for _, id := range []string{"verify-s01", "merge-s01", "final-verify"} {
		if statusByID[model.NodeID(id)] == model.NodeRunning || statusByID[model.NodeID(id)] == model.NodeSucceeded {
			t.Fatalf("node %s must not be running before its dependencies settle", id)
		}
	}

	// Virtual-time overlap: the three RUNNING Attempts share the fixed
	// fixture clock instant and are all in flight simultaneously.
	running := 0
	var startedAt time.Time
	for _, at := range iv.Attempts {
		if at.Status != model.AttemptRunning {
			continue
		}
		running++
		if !strings.HasPrefix(string(at.Key.Node), "task-s") {
			t.Fatalf("running attempt %s is not a task attempt", at.Key)
		}
		if startedAt.IsZero() {
			startedAt = at.StartedAt
		}
		if !at.StartedAt.Equal(startedAt) {
			t.Fatalf("attempt %s started at %v, want the shared instant %v (virtual-time overlap)", at.Key, at.StartedAt, startedAt)
		}
	}
	if running != 3 {
		t.Fatalf("running attempts = %d, want 3", running)
	}

	// Each Task coded only inside its own Task Worktree created from the
	// recorded Task Base (the Integration HEAD); the user workspace, the
	// Planning Snapshot, and the Integration Worktree carry no coded file.
	base := filepath.Join(fx.home, "worktrees", app.ProjectFor(fx.repo.Root).Key, string(wf))
	coded := filepath.Join("src", "calc", "divide.go")
	for _, id := range []string{"task-s01", "task-s02", "task-s03"} {
		wt := filepath.Join(base, "tasks", id)
		if !pathExists(filepath.Join(wt, coded)) {
			t.Fatalf("the coded file must land in the Task Worktree %s", wt)
		}
		if out := fx.repo.git("branch", "--list", "cflow/"+string(wf)+"/task-"+id); !strings.Contains(string(out), "cflow/"+string(wf)+"/task-"+id) {
			t.Fatalf("task branch missing for %s: %s", id, out)
		}
	}
	for _, root := range []string{
		fx.repo.Root,
		filepath.Join(base, "planning"),
		filepath.Join(base, "integration"),
	} {
		if pathExists(filepath.Join(root, coded)) {
			t.Fatalf("the coded file leaked into %s", root)
		}
	}
}

// TestDispatchRequiresExecutionApproval: dispatch before the Execution
// Approval cannot start anything (the workflow is not at EXECUTION and
// the graph is not installed).
func TestDispatchRequiresExecutionApproval(t *testing.T) {
	fx := newParallelFixture(t)
	wf := fx.CreateWorkflow("calculator")
	fx.Discuss(wf, "a calculator")
	_, err := fx.app(implementationScript("i1")).Execute(context.Background(),
		app.DispatchCommand{Workflow: wf})
	if err == nil {
		t.Fatal("dispatch before the execution approval succeeded")
	}
	iv := fx.Inspect(wf)
	if len(iv.Attempts) != 0 {
		t.Fatalf("attempts = %+v, want none before the execution approval", iv.Attempts)
	}
}
