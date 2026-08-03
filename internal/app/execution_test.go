package app

// Execution lifecycle Application tests (Task 11): Verification Catalog
// discovery from the fixed Base Commit, Spec generation, Workflow
// compilation, the Execution Dry Run gate with the Commit Preflight, the
// exact Execution Approval, and the Integration Worktree creation that
// only the Approval may request (PRD Worktree 策略). Real repositories,
// real SQLite, deterministic Fake Adapter (design 22.1).

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"cflow.local/cflow/internal/artifact"
	"cflow.local/cflow/internal/model"
)

// ---------------------------------------------------------------------------
// fixture
// ---------------------------------------------------------------------------

// newExecutionFixture builds the planning fixture over a repository that
// carries the fixed verification wrappers at the Base Commit and a
// configured Git identity (the Commit Preflight resolves it).
func newExecutionFixture(t *testing.T) *planningFixture {
	t.Helper()
	fx := newPlanningFixture(t)
	git := func(args ...string) {
		if out, err := execGit(fx.root, args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("config", "user.name", "Test User")
	git("config", "user.email", "test@example.com")
	if err := os.MkdirAll(filepath.Join(fx.root, "scripts"), 0o755); err != nil {
		t.Fatalf("mkdir scripts: %v", err)
	}
	if err := os.WriteFile(filepath.Join(fx.root, "scripts", "verify.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write verify.sh: %v", err)
	}
	if err := os.WriteFile(filepath.Join(fx.root, "scripts", "final-verify.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write final-verify.sh: %v", err)
	}
	git("add", "scripts")
	git("commit", "-q", "-m", "add verification wrappers")
	return fx
}

// specOutputScript is the deterministic Spec Generation Session output:
// one Spec referencing the discovered "verify" command.
func specOutputScript(sessionID, specJSON string) string {
	return fmt.Sprintf(`{"fixture":"fake-run","script_version":1,"provider":"fake","dialect":"cflow.dialect.fake.v1","purpose":"spec-generation","session_id":%s,"exit_code":0,"resume":"ok"}
{"type":"session_started","session_id":%s,"at_ms":0}
{"type":"assistant_message","session_id":%s,"text":"Splitting the plan.","at_ms":10}
{"type":"session_finished","session_id":%s,"result":{"specs":[%s],"proposed_commands":[]},"at_ms":20}`,
		strconv.Quote(sessionID), strconv.Quote(sessionID), strconv.Quote(sessionID),
		strconv.Quote(sessionID), specJSON)
}

func patchOutputScript(sessionID, patchJSON string) string {
	return fmt.Sprintf(`{"fixture":"fake-run","script_version":1,"provider":"fake","dialect":"cflow.dialect.fake.v1","purpose":"workflow-optimization","session_id":%s,"exit_code":0,"resume":"ok"}
{"type":"session_started","session_id":%s,"at_ms":0}
{"type":"assistant_message","session_id":%s,"text":"Proposing a scheduling patch.","at_ms":10}
{"type":"session_finished","session_id":%s,"result":%s,"at_ms":20}`,
		strconv.Quote(sessionID), strconv.Quote(sessionID), strconv.Quote(sessionID),
		strconv.Quote(sessionID), patchJSON)
}

const divideSpec = `{"id":"s01","goal":"implement divide","depends_on":[],"write_scope":["src/divide/**"],"read_scope":[],"locks":[],"acceptance":{"verification_command_ids":["verify"]},"route":{"provider":"fake","model":"default","budget":10},"timeout_seconds":1800,"max_retry":2}`

const checkpointPatch = `{"schema":"cflow-workflow-patch-1","operations":[{"op":"add_checkpoint","node_id":"merge-s01"}]}`

// approveCheckedPlan drives the planning lifecycle through Plan Approval.
func approveCheckedPlan(t *testing.T, fx *planningFixture, wf model.WorkflowID) {
	t.Helper()
	pv := fx.planView(wf)
	if pv.PlanStatus != model.PlanChecked {
		t.Fatalf("plan = %+v, want CHECKED", pv)
	}
	if _, err := fx.app().Execute(context.Background(),
		ApprovePlanCommand{Workflow: wf, Revision: pv.Revision, Hash: pv.Hash}); err != nil {
		t.Fatalf("approve plan: %v", err)
	}
}

// driveToExecutionGate runs the execution lifecycle through the paused
// Execution Approval gate and returns the preview view.
func driveToExecutionGate(t *testing.T, fx *planningFixture, wf model.WorkflowID) ExecutionPreviewView {
	t.Helper()
	out, err := fx.app(specOutputScript("s1", divideSpec)).Execute(context.Background(),
		GenerateSpecsCommand{Workflow: wf, Provider: "fake"})
	if err != nil {
		t.Fatalf("spec generation: %v", err)
	}
	if out.SessionID == "" {
		t.Fatal("spec generation outcome carried no session id")
	}
	view := fx.status(wf)
	if view.Stage != model.StageWorkflowGeneration || view.Runtime != model.RuntimeRunning {
		t.Fatalf("after spec generation: %#v", view)
	}

	out, err = fx.app(patchOutputScript("w1", checkpointPatch)).Execute(context.Background(),
		CompileWorkflowCommand{Workflow: wf, Provider: "fake"})
	if err != nil {
		t.Fatalf("workflow compilation: %v", err)
	}
	if out.SessionID == "" {
		t.Fatal("compilation outcome carried no session id")
	}
	view = fx.status(wf)
	if view.Stage != model.StageWorkflowGeneration || view.Runtime != model.RuntimeRunning {
		t.Fatalf("after compilation: %#v", view)
	}

	if _, err := fx.app().Execute(context.Background(), ExecutionDryRunCommand{Workflow: wf}); err != nil {
		t.Fatalf("execution dry run: %v", err)
	}
	view = fx.status(wf)
	if view.Stage != model.StageWorkflowGeneration || view.Runtime != model.RuntimePaused {
		t.Fatalf("after execution dry run: %#v", view)
	}

	qv, err := fx.app().Query(context.Background(), ExecutionPreviewQuery{Workflow: wf})
	if err != nil {
		t.Fatalf("execution preview: %v", err)
	}
	return qv.(ExecutionPreviewView)
}

// status is the fixture's status projection.
func (fx *planningFixture) status(wf model.WorkflowID) StatusView {
	fx.t.Helper()
	view, err := fx.app().Query(context.Background(), StatusQuery{Workflow: wf})
	if err != nil {
		fx.t.Fatalf("status: %v", err)
	}
	return view.(StatusView)
}

// inspect is the fixture's full aggregate projection.
func (fx *planningFixture) inspect(wf model.WorkflowID) InspectView {
	fx.t.Helper()
	view, err := fx.app().Query(context.Background(), InspectQuery{Workflow: wf})
	if err != nil {
		fx.t.Fatalf("inspect: %v", err)
	}
	return view.(InspectView)
}

// approveExecution approves the exact preview hashes.
func approveExecution(t *testing.T, fx *planningFixture, wf model.WorkflowID, pv ExecutionPreviewView) {
	t.Helper()
	if _, err := fx.app().Execute(context.Background(), ApproveExecutionCommand{
		Workflow: wf,
		PlanHash: pv.PlanHash, SpecHashes: pv.SpecHashes, CatalogHash: pv.CatalogHash,
		WorkflowHash: pv.WorkflowHash, RoutingHash: pv.RoutingHash, BudgetHash: pv.BudgetHash,
		CommitPolicyHash: pv.CommitPolicyHash,
	}); err != nil {
		t.Fatalf("execution approval: %v", err)
	}
}

// ---------------------------------------------------------------------------
// the full lifecycle: Plan Approval -> Specs -> Compile -> Dry Run ->
// Execution Approval -> Integration Worktree
// ---------------------------------------------------------------------------

func TestExecutionLifecyclePlanToIntegrationWorktree(t *testing.T) {
	fx := newExecutionFixture(t)
	wf, err := fx.create("add divide", false)
	if err != nil {
		t.Fatal(err)
	}
	fx.discussSeq++
	if _, err := fx.app(discussionScript("d1", "division by zero must error")).Execute(context.Background(),
		DiscussRequirementCommand{Workflow: wf, Text: "division by zero must error", Provider: "fake"}); err != nil {
		t.Fatal(err)
	}
	fx.planSeq++
	if _, err := fx.app(planScript("p1", validPlan())).Execute(context.Background(),
		GeneratePlanCommand{Workflow: wf, Provider: "fake"}); err != nil {
		t.Fatal(err)
	}
	fx.checkSeq++
	if _, err := fx.app(checkScript("c1", "pass")).Execute(context.Background(),
		CheckPlanCommand{Workflow: wf, Provider: "fake"}); err != nil {
		t.Fatal(err)
	}
	approveCheckedPlan(t, fx, wf)

	pv := driveToExecutionGate(t, fx, wf)

	// The Dry Run binds every execution input: plan, spec, catalog, and
	// the compiled workflow revisions and hashes, plus the Commit
	// Preflight fingerprint.
	if pv.Plan == nil || pv.Plan.Revision != 1 || pv.Plan.Hash == "" {
		t.Fatalf("preview plan = %+v", pv.Plan)
	}
	if pv.Spec == nil || pv.Spec.Revision != 1 || pv.Spec.Hash == "" {
		t.Fatalf("preview spec = %+v", pv.Spec)
	}
	if pv.Catalog == nil || pv.Catalog.Revision != 1 || pv.Catalog.Hash == "" {
		t.Fatalf("preview catalog = %+v", pv.Catalog)
	}
	if pv.WorkflowArtifact == nil || pv.WorkflowArtifact.Revision != 1 || pv.WorkflowArtifact.Hash == "" {
		t.Fatalf("preview workflow = %+v", pv.WorkflowArtifact)
	}
	if pv.Preflight == nil || pv.Preflight.Revision != 1 || pv.Preflight.Fingerprint == "" ||
		pv.Preflight.EvidenceHash == "" {
		t.Fatalf("preview preflight = %+v", pv.Preflight)
	}
	if pv.CommitPolicyHash == "" || pv.CommitPolicyHash != pv.Preflight.EvidenceHash {
		t.Fatalf("commit policy hash = %q, preflight = %+v", pv.CommitPolicyHash, pv.Preflight)
	}
	// Routes, budgets, the trust boundary, the Worktree plan, and the
	// command identities are all displayed.
	if len(pv.Routes) != 1 || pv.Routes[0].Provider != "fake" {
		t.Fatalf("routes = %+v", pv.Routes)
	}
	if len(pv.Budgets) != 1 || pv.Budgets[0].MaxRetry != 2 {
		t.Fatalf("budgets = %+v", pv.Budgets)
	}
	if pv.TotalAgentRuns != 3 || pv.TotalRetries != 2 {
		t.Fatalf("totals = runs %d retries %d", pv.TotalAgentRuns, pv.TotalRetries)
	}
	if len(pv.ParallelGroups) == 0 {
		t.Fatal("no parallel groups in the preview")
	}
	if len(pv.CommandIdentities) == 0 {
		t.Fatal("no command identities in the preview")
	}
	found := false
	for _, c := range pv.CommandIdentities {
		if c.CommandID == "verify" && c.Executable == "scripts/verify.sh" && len(c.SHA256) == 64 {
			found = true
		}
	}
	if !found {
		t.Fatalf("command identities = %+v", pv.CommandIdentities)
	}
	if !strings.Contains(pv.TrustBoundary, "no sandbox guarantee") {
		t.Fatalf("trust boundary = %q", pv.TrustBoundary)
	}
	if len(pv.WorktreePlan) == 0 {
		t.Fatal("no worktree plan in the preview")
	}
	// The compiled workflow carries the integration resource lock on its
	// merge node.
	if len(pv.Locks) != 1 || pv.Locks[0].Lock != "integration:"+string(wf) {
		t.Fatalf("locks = %+v", pv.Locks)
	}

	// The exact Approval advances to EXECUTION and alone requests the
	// Integration Worktree creation.
	approveExecution(t, fx, wf, pv)
	view := fx.status(wf)
	if view.Stage != model.StageExecution || view.Runtime != model.RuntimeRunning {
		t.Fatalf("after execution approval: %#v", view)
	}
	iv := fx.inspect(wf)
	if len(iv.Approvals) != 2 {
		t.Fatalf("approvals = %d, want 2 (plan + execution)", len(iv.Approvals))
	}
	execution := iv.Approvals[1]
	if execution.Kind != model.ApprovalExecution || execution.Fingerprint != pv.Preflight.Fingerprint {
		t.Fatalf("execution approval = %+v", execution)
	}
	if len(execution.Refs) != 4 {
		t.Fatalf("execution approval refs = %+v, want all four artifacts", execution.Refs)
	}

	// The Integration Worktree exists at the recorded Base Commit, and
	// the Integration Branch exists in the repository.
	integrationPath := filepath.Join(fx.home, "worktrees", ProjectFor(fx.root).Key, string(wf), "integration")
	if !pathExists(integrationPath) {
		t.Fatalf("integration worktree %s was not created", integrationPath)
	}
	if out, err := execGit(fx.root, "branch", "--list", "cflow/"+string(wf)+"/integration").CombinedOutput(); err != nil {
		t.Fatalf("list integration branch: %v", err)
	} else if !strings.Contains(string(out), "cflow/"+string(wf)+"/integration") {
		t.Fatalf("integration branch missing: %s", out)
	}
}

// TestExecutionApprovalRejectsChangedInputs: any reference change since
// the displayed preview is APPROVAL_INPUT_CHANGED with no mutation, and
// the workflow stays paused for a regenerated preview.
func TestExecutionApprovalRejectsChangedInputs(t *testing.T) {
	fx := newExecutionFixture(t)
	wf, err := fx.create("add divide", false)
	if err != nil {
		t.Fatal(err)
	}
	fx.discussSeq++
	if _, err := fx.app(discussionScript("d1", "division by zero must error")).Execute(context.Background(),
		DiscussRequirementCommand{Workflow: wf, Text: "division by zero must error", Provider: "fake"}); err != nil {
		t.Fatal(err)
	}
	fx.planSeq++
	if _, err := fx.app(planScript("p1", validPlan())).Execute(context.Background(),
		GeneratePlanCommand{Workflow: wf, Provider: "fake"}); err != nil {
		t.Fatal(err)
	}
	fx.checkSeq++
	if _, err := fx.app(checkScript("c1", "pass")).Execute(context.Background(),
		CheckPlanCommand{Workflow: wf, Provider: "fake"}); err != nil {
		t.Fatal(err)
	}
	approveCheckedPlan(t, fx, wf)
	pv := driveToExecutionGate(t, fx, wf)

	// A stale workflow hash is APPROVAL_INPUT_CHANGED with no mutation.
	_, err = fx.app().Execute(context.Background(), ApproveExecutionCommand{
		Workflow: wf,
		PlanHash: pv.PlanHash, SpecHashes: pv.SpecHashes, CatalogHash: pv.CatalogHash,
		WorkflowHash: "stale-hash", RoutingHash: pv.RoutingHash, BudgetHash: pv.BudgetHash,
		CommitPolicyHash: pv.CommitPolicyHash,
	})
	if err == nil {
		t.Fatal("stale workflow hash approval succeeded")
	} else if code, ok := model.CodeOf(err); !ok || code != model.CodeApprovalInputChanged {
		t.Fatalf("stale approval error = %v, want APPROVAL_INPUT_CHANGED", err)
	}
	iv := fx.inspect(wf)
	if len(iv.Approvals) != 1 {
		t.Fatalf("approvals = %d, want 1 (append-only)", len(iv.Approvals))
	}
	view := fx.status(wf)
	if view.Stage != model.StageWorkflowGeneration || view.Runtime != model.RuntimePaused {
		t.Fatalf("workflow moved after the stale approval: %#v", view)
	}
	if pathExists(filepath.Join(fx.home, "worktrees", ProjectFor(fx.root).Key, string(wf), "integration")) {
		t.Fatal("integration worktree was created without a matching approval")
	}
}

// TestExecutionApprovalRequiresPausedGate: after resume the workflow is
// RUNNING again and the approval is refused until a fresh Dry Run
// re-pauses the gate and revalidates every reference (PRD: resume must
// return to the same gate and revalidate hashes; it cannot cross it).
func TestExecutionApprovalRequiresPausedGate(t *testing.T) {
	fx := newExecutionFixture(t)
	wf, err := fx.create("add divide", false)
	if err != nil {
		t.Fatal(err)
	}
	fx.discussSeq++
	if _, err := fx.app(discussionScript("d1", "division by zero must error")).Execute(context.Background(),
		DiscussRequirementCommand{Workflow: wf, Text: "division by zero must error", Provider: "fake"}); err != nil {
		t.Fatal(err)
	}
	fx.planSeq++
	if _, err := fx.app(planScript("p1", validPlan())).Execute(context.Background(),
		GeneratePlanCommand{Workflow: wf, Provider: "fake"}); err != nil {
		t.Fatal(err)
	}
	fx.checkSeq++
	if _, err := fx.app(checkScript("c1", "pass")).Execute(context.Background(),
		CheckPlanCommand{Workflow: wf, Provider: "fake"}); err != nil {
		t.Fatal(err)
	}
	approveCheckedPlan(t, fx, wf)
	pv := driveToExecutionGate(t, fx, wf)

	if _, err := fx.app().Execute(context.Background(), ResumeWorkflowCommand{Workflow: wf}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	_, err = fx.app().Execute(context.Background(), ApproveExecutionCommand{
		Workflow: wf,
		PlanHash: pv.PlanHash, SpecHashes: pv.SpecHashes, CatalogHash: pv.CatalogHash,
		WorkflowHash: pv.WorkflowHash, RoutingHash: pv.RoutingHash, BudgetHash: pv.BudgetHash,
		CommitPolicyHash: pv.CommitPolicyHash,
	})
	if err == nil {
		t.Fatal("approval from the resumed RUNNING state succeeded")
	} else if code, ok := model.CodeOf(err); !ok || code != model.CodeInvalidInput {
		t.Fatalf("approval from RUNNING error = %v, want INVALID_INPUT", err)
	}

	// The fresh Dry Run re-validates and re-pauses the gate; the exact
	// new preview approves.
	if _, err := fx.app().Execute(context.Background(), ExecutionDryRunCommand{Workflow: wf}); err != nil {
		t.Fatalf("re-dry-run: %v", err)
	}
	qv, err := fx.app().Query(context.Background(), ExecutionPreviewQuery{Workflow: wf})
	if err != nil {
		t.Fatalf("regenerated preview: %v", err)
	}
	approveExecution(t, fx, wf, qv.(ExecutionPreviewView))
	view := fx.status(wf)
	if view.Stage != model.StageExecution || view.Runtime != model.RuntimeRunning {
		t.Fatalf("after regenerated approval: %#v", view)
	}
}

// TestCompiledWorkflowHashStableAcrossRuns: the compiled Workflow
// Artifact hash is deterministic across independent fresh flows (the
// golden hashes remain stable across runs, brief Step 6).
func TestCompiledWorkflowHashStableAcrossRuns(t *testing.T) {
	first := compiledWorkflowHash(t)
	second := compiledWorkflowHash(t)
	if first == "" || first != second {
		t.Fatalf("compiled workflow hash changed across runs: %s vs %s", first, second)
	}
}

func compiledWorkflowHash(t *testing.T) string {
	t.Helper()
	fx := newExecutionFixture(t)
	wf, err := fx.create("add divide", false)
	if err != nil {
		t.Fatal(err)
	}
	fx.discussSeq++
	if _, err := fx.app(discussionScript("d1", "division by zero must error")).Execute(context.Background(),
		DiscussRequirementCommand{Workflow: wf, Text: "division by zero must error", Provider: "fake"}); err != nil {
		t.Fatal(err)
	}
	fx.planSeq++
	if _, err := fx.app(planScript("p1", validPlan())).Execute(context.Background(),
		GeneratePlanCommand{Workflow: wf, Provider: "fake"}); err != nil {
		t.Fatal(err)
	}
	fx.checkSeq++
	if _, err := fx.app(checkScript("c1", "pass")).Execute(context.Background(),
		CheckPlanCommand{Workflow: wf, Provider: "fake"}); err != nil {
		t.Fatal(err)
	}
	approveCheckedPlan(t, fx, wf)
	if _, err := fx.app(specOutputScript("s1", divideSpec)).Execute(context.Background(),
		GenerateSpecsCommand{Workflow: wf, Provider: "fake"}); err != nil {
		t.Fatalf("spec generation: %v", err)
	}
	if _, err := fx.app(patchOutputScript("w1", checkpointPatch)).Execute(context.Background(),
		CompileWorkflowCommand{Workflow: wf, Provider: "fake"}); err != nil {
		t.Fatalf("workflow compilation: %v", err)
	}
	a := fx.app()
	store, err := a.artifactStore(wf)
	if err != nil {
		t.Fatalf("artifact store: %v", err)
	}
	ref, err := store.Resolve(context.Background(), artifact.ResolveRequest{WorkflowID: wf, Type: model.ArtifactWorkflow})
	if err != nil {
		t.Fatalf("resolve workflow artifact: %v", err)
	}
	return ref.Hash
}
