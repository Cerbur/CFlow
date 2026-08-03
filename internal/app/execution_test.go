package app

// Execution lifecycle Application tests (Task 11): Verification Catalog
// discovery from the fixed Base Commit, Spec generation, Workflow
// compilation, the Execution Dry Run gate with the Commit Preflight, the
// exact Execution Approval, and the Integration Worktree creation that
// only the Approval may request (PRD Worktree 策略). Real repositories,
// real SQLite, deterministic Fake Adapter (design 22.1).

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"cflow.local/cflow/internal/artifact"
	"cflow.local/cflow/internal/compile"
	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/platform"
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
	return driveToExecutionGateWithPatch(t, fx, wf, checkpointPatch)
}

// driveToExecutionGateWithPatch is driveToExecutionGate with an explicit
// Patch IR for the Workflow Optimization Session.
func driveToExecutionGateWithPatch(t *testing.T, fx *planningFixture, wf model.WorkflowID, patchJSON string) ExecutionPreviewView {
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

	out, err = fx.app(patchOutputScript("w1", patchJSON)).Execute(context.Background(),
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

// drivePlanningToApproval runs the planning lifecycle through Plan
// Approval and returns the workflow id.
func drivePlanningToApproval(t *testing.T, fx *planningFixture) model.WorkflowID {
	t.Helper()
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
	return wf
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

// specOutputScriptWithProposals is the Spec Generation Session output
// with an explicit proposed_commands list.
func specOutputScriptWithProposals(sessionID, specJSON, proposalsJSON string) string {
	return fmt.Sprintf(`{"fixture":"fake-run","script_version":1,"provider":"fake","dialect":"cflow.dialect.fake.v1","purpose":"spec-generation","session_id":%s,"exit_code":0,"resume":"ok"}
{"type":"session_started","session_id":%s,"at_ms":0}
{"type":"assistant_message","session_id":%s,"text":"Splitting the plan.","at_ms":10}
{"type":"session_finished","session_id":%s,"result":{"specs":[%s],"proposed_commands":[%s]},"at_ms":20}`,
		strconv.Quote(sessionID), strconv.Quote(sessionID), strconv.Quote(sessionID),
		strconv.Quote(sessionID), specJSON, proposalsJSON)
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
	wf := drivePlanningToApproval(t, fx)
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

// TestAppliedPatchOpsPersistedAndDisplayed: applied scheduling
// operations (route pins, budget tightenings) are persisted as
// non-blocking Compile Findings and displayed at the Execution Approval
// gate, so the user approves exactly the patched execution instead of
// the raw Spec budgets (review fix #1).
func TestAppliedPatchOpsPersistedAndDisplayed(t *testing.T) {
	fx := newExecutionFixture(t)
	wf := drivePlanningToApproval(t, fx)
	const appliedOpsPatch = `{"schema":"cflow-workflow-patch-1","operations":[{"op":"pin_route","node_id":"task-s01","provider":"fake"},{"op":"tighten_budget","node_id":"task-s01","budget":5}]}`
	pv := driveToExecutionGateWithPatch(t, fx, wf, appliedOpsPatch)

	var applied []model.Finding
	for _, f := range pv.Findings {
		if f.Code == model.CodeWorkflowPatchApplied {
			applied = append(applied, f)
		}
	}
	if len(applied) != 2 {
		t.Fatalf("applied patch findings = %+v, want 2", applied)
	}
	texts := applied[0].Text + "\n" + applied[1].Text
	if !strings.Contains(texts, "pin_route task-s01 -> fake") ||
		!strings.Contains(texts, "tighten_budget task-s01 -> 5") {
		t.Fatalf("applied patch findings = %+v", applied)
	}
	if len(pv.Budgets) != 1 || pv.Budgets[0].Budget != 10 {
		t.Fatalf("preview budgets = %+v (the spec budget; the tightening is displayed as a finding)", pv.Budgets)
	}
	// The exact preview still approves: the applied operations are part
	// of what the user sees at the gate.
	approveExecution(t, fx, wf, pv)
	view := fx.status(wf)
	if view.Stage != model.StageExecution {
		t.Fatalf("after approval with applied patch: %#v", view)
	}
}

// TestProposedCommandsPromotedToCatalogRevision: the Spec Agent's
// proposed commands are validated with the Catalog policy and written as
// a successor immutable Catalog Revision; the Spec may reference them
// and the Compiler builds their Verify nodes (review fix #2).
func TestProposedCommandsPromotedToCatalogRevision(t *testing.T) {
	fx := newExecutionFixture(t)
	// A second wrapper fixed at the Base Commit the Agent proposes.
	if err := os.WriteFile(filepath.Join(fx.root, "scripts", "lint.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write lint.sh: %v", err)
	}
	git := func(args ...string) {
		if out, err := execGit(fx.root, args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("add", "scripts/lint.sh")
	git("commit", "-q", "-m", "add lint wrapper")
	wf := drivePlanningToApproval(t, fx)

	const lintProposal = `{"command_id":"lint","executable":"scripts/lint.sh","args":[],"cwd":".","purpose":"task_verify","timeout_seconds":600,"expected_exit_codes":[0],"max_output_bytes":10485760,"env":["PATH","TMPDIR"]}`
	const divideSpecWithLint = `{"id":"s01","goal":"implement divide","depends_on":[],"write_scope":["src/divide/**"],"read_scope":[],"locks":[],"acceptance":{"verification_command_ids":["verify","lint"]},"route":{"provider":"fake","model":"default","budget":10},"timeout_seconds":1800,"max_retry":2}`
	if _, err := fx.app(specOutputScriptWithProposals("s1", divideSpecWithLint, lintProposal)).Execute(context.Background(),
		GenerateSpecsCommand{Workflow: wf, Provider: "fake"}); err != nil {
		t.Fatalf("spec generation: %v", err)
	}

	// The active Catalog revision is the successor and contains the
	// promoted proposal with its Base-fixed identity.
	a := fx.app()
	store, err := a.artifactStore(wf)
	if err != nil {
		t.Fatalf("artifact store: %v", err)
	}
	ref, err := store.Resolve(context.Background(), artifact.ResolveRequest{WorkflowID: wf, Type: model.ArtifactCatalog})
	if err != nil {
		t.Fatalf("resolve catalog: %v", err)
	}
	if ref.Revision != 2 {
		t.Fatalf("catalog revision = %d, want 2 (successor with the promoted proposal)", ref.Revision)
	}
	catalogBody, err := store.Get(context.Background(), ref)
	if err != nil {
		t.Fatalf("read catalog: %v", err)
	}
	// The Artifact Store canonically serializes structured bodies to
	// JSON (design 10.2); the successor body carries the promoted entry.
	if !strings.Contains(string(catalogBody), `"command_id":"lint"`) ||
		!strings.Contains(string(catalogBody), "agent-proposal:scripts/lint.sh@sha256:") {
		t.Fatalf("successor catalog body = %s", catalogBody)
	}

	// Two Verify nodes run in parallel, so the user's concurrency
	// configuration must allow it (the Compiler enforces the cap).
	if err := os.WriteFile(filepath.Join(fx.home, "config.yaml"), []byte("concurrency: 2\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	// The Compiler builds one Verify node per acceptance command and the
	// preview displays the promoted identity.
	if _, err := fx.app(patchOutputScript("w1", checkpointPatch)).Execute(context.Background(),
		CompileWorkflowCommand{Workflow: wf, Provider: "fake"}); err != nil {
		t.Fatalf("workflow compilation: %v", err)
	}
	workflowRef, err := store.Resolve(context.Background(), artifact.ResolveRequest{WorkflowID: wf, Type: model.ArtifactWorkflow})
	if err != nil {
		t.Fatalf("resolve workflow: %v", err)
	}
	workflowBody, err := store.Get(context.Background(), workflowRef)
	if err != nil {
		t.Fatalf("read workflow: %v", err)
	}
	wfIR, err := compile.ParseWorkflow(workflowBody)
	if err != nil {
		t.Fatalf("parse workflow: %v", err)
	}
	verifyNodes := 0
	for _, n := range wfIR.Nodes {
		if n.Type == "verify" {
			verifyNodes++
		}
	}
	if verifyNodes != 2 {
		t.Fatalf("verify nodes = %d, want 2 (verify + lint)", verifyNodes)
	}

	if _, err := fx.app().Execute(context.Background(), ExecutionDryRunCommand{Workflow: wf}); err != nil {
		t.Fatalf("execution dry run: %v", err)
	}
	qv, err := fx.app().Query(context.Background(), ExecutionPreviewQuery{Workflow: wf})
	if err != nil {
		t.Fatalf("execution preview: %v", err)
	}
	pv := qv.(ExecutionPreviewView)
	found := false
	for _, c := range pv.CommandIdentities {
		if c.CommandID == "lint" && c.Executable == "scripts/lint.sh" && len(c.SHA256) == 64 {
			found = true
		}
	}
	if !found {
		t.Fatalf("promoted command identity missing: %+v", pv.CommandIdentities)
	}
}

// TestRejectedProposalNotPromoted: a Proposal whose wrapper is not fixed
// at the Base Commit fails the policy and never enters the Catalog; the
// successor revision is not written and the Compiler rejects the
// dangling reference (review fix #2).
func TestRejectedProposalNotPromoted(t *testing.T) {
	fx := newExecutionFixture(t)
	wf := drivePlanningToApproval(t, fx)

	const ghostProposal = `{"command_id":"ghost","executable":"scripts/ghost.sh","args":[],"cwd":".","purpose":"task_verify","timeout_seconds":600,"expected_exit_codes":[0],"max_output_bytes":10485760,"env":["PATH","TMPDIR"]}`
	const divideSpecWithGhost = `{"id":"s01","goal":"implement divide","depends_on":[],"write_scope":["src/divide/**"],"read_scope":[],"locks":[],"acceptance":{"verification_command_ids":["verify","ghost"]},"route":{"provider":"fake","model":"default","budget":10},"timeout_seconds":1800,"max_retry":2}`
	if _, err := fx.app(specOutputScriptWithProposals("s1", divideSpecWithGhost, ghostProposal)).Execute(context.Background(),
		GenerateSpecsCommand{Workflow: wf, Provider: "fake"}); err != nil {
		t.Fatalf("spec generation: %v", err)
	}

	// The rejected proposal never produced a successor revision.
	a := fx.app()
	store, err := a.artifactStore(wf)
	if err != nil {
		t.Fatalf("artifact store: %v", err)
	}
	ref, err := store.Resolve(context.Background(), artifact.ResolveRequest{WorkflowID: wf, Type: model.ArtifactCatalog})
	if err != nil {
		t.Fatalf("resolve catalog: %v", err)
	}
	if ref.Revision != 1 {
		t.Fatalf("catalog revision = %d, want 1 (rejected proposal must not be promoted)", ref.Revision)
	}

	// The Compiler rejects the dangling reference.
	_, err = fx.app(patchOutputScript("w1", checkpointPatch)).Execute(context.Background(),
		CompileWorkflowCommand{Workflow: wf, Provider: "fake"})
	if err == nil {
		t.Fatal("compile with a dangling proposal reference succeeded")
	} else if code, ok := model.CodeOf(err); !ok || code != model.CodeSchemaInvalid {
		t.Fatalf("compile error = %v, want SCHEMA_INVALID for the unknown command id", err)
	}
}

// ---------------------------------------------------------------------------
// Task 12: serialized dispatch, Task Worktrees, Fake coding execution
// ---------------------------------------------------------------------------

// implementationScript is the deterministic Fake coding Session output:
// the script declares the files the Coding Agent writes into its working
// directory (the Task Worktree) when the Session finishes. Coding output
// never sets state: only the committed Attempt and the observed Git facts
// matter.
func implementationScript(sessionID string) string {
	return fmt.Sprintf(`{"fixture":"fake-run","script_version":1,"provider":"fake","dialect":"cflow.dialect.fake.v1","purpose":"implementation","session_id":%s,"exit_code":0,"resume":"ok","writes":[{"path":"src/divide/divide.go","content":"package divide\n\n// Divide returns a/b.\nfunc Divide(a, b int) (int, error) {\n\treturn 0, nil\n}\n"}]}
{"type":"session_started","session_id":%s,"at_ms":0}
{"type":"assistant_message","session_id":%s,"text":"Implemented Divide.","at_ms":10}
{"type":"session_finished","session_id":%s,"result":{"summary":"implemented"},"at_ms":20}`,
		strconv.Quote(sessionID), strconv.Quote(sessionID), strconv.Quote(sessionID), strconv.Quote(sessionID))
}

// planningApproved drives the planning lifecycle through the Plan
// Approval and returns the workflow identity (a test may adjust the
// fixture configuration between planning and the execution gate).
func (fx *planningFixture) planningApproved() model.WorkflowID {
	fx.t.Helper()
	return drivePlanningToApproval(fx.t, fx)
}

// executionGate drives the execution lifecycle (Specs, Compile, Dry Run,
// Execution Approval) for one approved workflow and returns a fresh
// Application with the dispatch probe installed.
func (fx *planningFixture) executionGate(wf model.WorkflowID, scripts ...string) *Application {
	fx.t.Helper()
	pv := driveToExecutionGate(fx.t, fx, wf)
	approveExecution(fx.t, fx, wf, pv)
	a := fx.app(scripts...)
	fx.probe = &callProbe{}
	a.probe = fx.probe
	fx.wf = wf
	return a
}

// executionReady drives the real pipeline through the Execution Approval
// and returns a fresh Application with the dispatch probe installed plus
// the workflow identity.
func (fx *planningFixture) executionReady(scripts ...string) (*Application, model.WorkflowID) {
	fx.t.Helper()
	wf := fx.planningApproved()
	return fx.executionGate(wf, scripts...), wf
}

// dispatchPlanFor installs and dispatches one plan through the real
// dispatch machinery (design 12), holding the mutation lock batch exactly
// as Execute does.
func (fx *planningFixture) dispatchPlanFor(a *Application, wf model.WorkflowID, plan *dispatchPlan) error {
	fx.t.Helper()
	ctx := context.Background()
	st, err := a.ensureWriteStore(ctx, wf)
	if err != nil {
		return err
	}
	holds, err := a.acquireMutationLocks(ctx, wf)
	if err != nil {
		return err
	}
	defer releaseHolds(holds)
	_, err = a.dispatchPass(ctx, st, wf, plan, false)
	return err
}

// RunReadyTasks drives the pipeline to Execution Approval and then runs
// one dispatch pass over the fixture plan (node "S01", the brief's
// fixture identity), recording the protocol probe.
func (fx *planningFixture) RunReadyTasks() {
	fx.t.Helper()
	a, wf := fx.executionReady(implementationScript("i1"))
	if err := fx.dispatchPlanFor(a, wf, fixturePlan()); err != nil {
		fx.t.Fatalf("dispatch: %v", err)
	}
}

// RequireOrder asserts the probe recorded first strictly before second.
func (fx *planningFixture) RequireOrder(first, second string) {
	fx.t.Helper()
	if fx.probe == nil {
		fx.t.Fatal("no probe recorded: RunReadyTasks was not called")
	}
	steps := fx.probe.Calls()
	fi, si := -1, -1
	for i, s := range steps {
		if s == first && fi < 0 {
			fi = i
		}
		if s == second && si < 0 {
			si = i
		}
	}
	if fi < 0 {
		fx.t.Fatalf("probe never recorded %q in %v", first, steps)
	}
	if si < 0 {
		fx.t.Fatalf("probe never recorded %q in %v", second, steps)
	}
	if fi >= si {
		fx.t.Fatalf("order violated: %q at %d must precede %q at %d in %v", first, fi, second, si, steps)
	}
}

// fixturePlan is the default execution fixture plan: one independent Task
// node "S01" (the brief's fixture identity) on the fake route.
func fixturePlan() *dispatchPlan {
	return &dispatchPlan{nodes: []dispatchNode{{
		id: "S01", kind: model.NodeAgentTask, specID: "S01",
		retry: 2, timeout: 1800, route: "fake",
		writeScope: []string{"src/divide/**"},
	}}}
}

// TestAttemptCommitsBeforeProviderStart (brief Step 1, verbatim): the
// RUNNING Attempt row commits before the Coding Session starts, so an
// in-memory queued goroutine is never an in-flight Attempt and no start
// can cross a committed Dispatch Gate closure.
func TestAttemptCommitsBeforeProviderStart(t *testing.T) {
	fx := newExecutionFixture(t)
	fx.RunReadyTasks()
	fx.RequireOrder("attempt:S01:commit", "provider:S01:start")
}

// TestDispatchDefersSharedLockConflict: two Tasks sharing a resource lock
// are statically incompatible (PRD 并行安全判断: resource_locks must be
// disjoint); only the first dispatches in the pass and the second waits
// for a later pass.
func TestDispatchDefersSharedLockConflict(t *testing.T) {
	fx := newExecutionFixture(t)
	a, wf := fx.executionReady(implementationScript("i1"))
	plan := &dispatchPlan{nodes: []dispatchNode{
		{id: "S01", kind: model.NodeAgentTask, specID: "S01", retry: 0, timeout: 1800,
			route: "fake", locks: []string{"db-shard-1"}},
		{id: "S02", kind: model.NodeAgentTask, specID: "S02", retry: 0, timeout: 1800,
			route: "fake", locks: []string{"db-shard-1"}},
	}}
	if err := fx.dispatchPlanFor(a, wf, plan); err != nil {
		fx.t.Fatalf("dispatch: %v", err)
	}
	iv := fx.inspect(wf)
	statusByID := map[model.NodeID]model.NodeStatus{}
	runningAttempts := 0
	for _, n := range iv.Nodes {
		statusByID[n.ID] = n.Status
	}
	for _, at := range iv.Attempts {
		if at.Status == model.AttemptRunning {
			runningAttempts++
		}
	}
	if statusByID["S01"] != model.NodeRunning {
		t.Fatalf("S01 status = %s, want RUNNING (locked task dispatches first)", statusByID["S01"])
	}
	if statusByID["S02"] != model.NodePending {
		t.Fatalf("S02 status = %s, want PENDING (locked conflict defers the second task)", statusByID["S02"])
	}
	if runningAttempts != 1 {
		t.Fatalf("running attempts = %d, want 1", runningAttempts)
	}
}

// TestDispatchHonorsConcurrencyCap: the user's configured concurrency
// bound caps one pass; the excess eligible Task waits (PRD 并发上限).
func TestDispatchHonorsConcurrencyCap(t *testing.T) {
	fx := newExecutionFixture(t)
	wf := fx.planningApproved()
	// The concurrency bound is the user's configuration (PRD 并发上限); it
	// must be in place before the Compiler validates the skeleton.
	if err := os.MkdirAll(fx.home, 0o700); err != nil {
		fx.t.Fatalf("mkdir home: %v", err)
	}
	if err := os.WriteFile(filepath.Join(fx.home, "config.yaml"), []byte("concurrency: 1\n"), 0o600); err != nil {
		fx.t.Fatalf("write config: %v", err)
	}
	a := fx.executionGate(wf, implementationScript("i1"))
	plan := &dispatchPlan{nodes: []dispatchNode{
		{id: "S01", kind: model.NodeAgentTask, specID: "S01", retry: 0, timeout: 1800,
			route: "fake", writeScope: []string{"src/a/**"}},
		{id: "S02", kind: model.NodeAgentTask, specID: "S02", retry: 0, timeout: 1800,
			route: "fake", writeScope: []string{"src/b/**"}},
	}}
	if err := fx.dispatchPlanFor(a, wf, plan); err != nil {
		fx.t.Fatalf("dispatch: %v", err)
	}
	iv := fx.inspect(wf)
	statusByID := map[model.NodeID]model.NodeStatus{}
	for _, n := range iv.Nodes {
		statusByID[n.ID] = n.Status
	}
	if statusByID["S01"] != model.NodeRunning || statusByID["S02"] != model.NodePending {
		t.Fatalf("statuses = %v, want S01 RUNNING and S02 PENDING under the cap of 1", statusByID)
	}
}

// TestDispatchAfterPauseAllocatesNothing: a committed gate closure (the
// Pause command) stops all further allocation; the pure Scheduler reads
// the closed gate from the persisted aggregate and no queued goroutine
// counts as running.
func TestDispatchAfterPauseAllocatesNothing(t *testing.T) {
	fx := newExecutionFixture(t)
	a, wf := fx.executionReady(implementationScript("i1"))
	if _, err := a.Execute(context.Background(), PauseWorkflowCommand{Workflow: wf}); err != nil {
		fx.t.Fatalf("pause: %v", err)
	}
	if err := fx.dispatchPlanFor(a, wf, fixturePlan()); err != nil {
		fx.t.Fatalf("dispatch: %v", err)
	}
	iv := fx.inspect(wf)
	if len(iv.Attempts) != 0 {
		t.Fatalf("attempts = %+v; no attempt may start across a committed closure", iv.Attempts)
	}
	if len(iv.Nodes) != 1 || iv.Nodes[0].Status != model.NodePending {
		t.Fatalf("nodes = %+v, want the single node still PENDING", iv.Nodes)
	}
}

// TestDispatchOneProjectWriter: the Project Writer lock serializes
// mutating Runtimes; a dispatch on the same Project is refused while
// another writer holds it.
func TestDispatchOneProjectWriter(t *testing.T) {
	fx := newExecutionFixture(t)
	a, wf := fx.executionReady(implementationScript("i1"))
	// Another mutating Runtime (a different goroutine, so the LockSet's
	// per-goroutine lock order never trips) holds the mutation lock batch
	// in the fixed order (design 18.1): the dispatch's Project Writer
	// acquisition is refused as project-busy.
	held := make(chan struct{})
	release := make(chan struct{})
	go func() {
		ls, err := a.lockSet()
		if err != nil {
			close(held)
			return
		}
		holds := make([]*platform.Hold, 0, 3)
		for _, take := range []func(context.Context) (*platform.Hold, error){
			ls.SchemaShared,
			func(ctx context.Context) (*platform.Hold, error) { return ls.ProjectWriter(ctx, a.project.Key) },
			func(ctx context.Context) (*platform.Hold, error) {
				return ls.WorkflowOwner(ctx, a.project.Key, string(wf))
			},
		} {
			h, err := take(context.Background())
			if err != nil {
				releaseHolds(holds)
				close(held)
				return
			}
			holds = append(holds, h)
		}
		close(held)
		<-release
		releaseHolds(holds)
	}()
	<-held
	_, err := a.Execute(context.Background(), DispatchCommand{Workflow: wf})
	close(release)
	if err == nil {
		t.Fatal("dispatch succeeded while the project writer lock was held")
	} else if code, ok := model.CodeOf(err); !ok || code != model.CodeDatabaseMigrationFailed {
		t.Fatalf("dispatch error = %v, want the project-busy fault", err)
	}
}

// TestDifferentProjectsDispatchConcurrently: two Projects' workflows
// dispatch in parallel goroutines without contention (the lock namespaces
// and store files are per-Project).
func TestDifferentProjectsDispatchConcurrently(t *testing.T) {
	fx1 := newExecutionFixture(t)
	fx2 := newExecutionFixture(t)
	a1, wf1 := fx1.executionReady(implementationScript("i1"))
	a2, wf2 := fx2.executionReady(implementationScript("i1"))
	type job struct {
		a  *Application
		fx *planningFixture
		wf model.WorkflowID
	}
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, j := range []job{{a1, fx1, wf1}, {a2, fx2, wf2}} {
		wg.Add(1)
		go func(j job) {
			defer wg.Done()
			errs <- j.fx.dispatchPlanFor(j.a, j.wf, fixturePlan())
		}(j)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			fx1.t.Fatalf("concurrent dispatch: %v", err)
		}
	}
	for _, j := range []job{{a1, fx1, wf1}, {a2, fx2, wf2}} {
		iv := j.fx.inspect(j.wf)
		running := 0
		for _, at := range iv.Attempts {
			if at.Status == model.AttemptRunning {
				running++
			}
		}
		if running != 1 {
			t.Fatalf("workflow %s running attempts = %d, want 1", j.wf, running)
		}
	}
}

// TestCodingOccursOnlyInTaskWorktree: the Fake coding Session's writes
// land only in the Task Worktree; the user workspace, the Planning
// Snapshot, and the Integration Worktree are untouched (PRD Worktree 策略).
func TestCodingOccursOnlyInTaskWorktree(t *testing.T) {
	fx := newExecutionFixture(t)
	fx.RunReadyTasks()
	base := filepath.Join(fx.home, "worktrees", ProjectFor(fx.root).Key, string(fx.wf))
	coded := filepath.Join("src", "divide", "divide.go")
	if !pathExists(filepath.Join(base, "tasks", "S01", coded)) {
		t.Fatalf("the coded file must land in the Task Worktree %s", filepath.Join(base, "tasks", "S01", coded))
	}
	for _, root := range []string{
		fx.root,
		filepath.Join(base, "planning"),
		filepath.Join(base, "integration"),
	} {
		if pathExists(filepath.Join(root, coded)) {
			t.Fatalf("the coded file leaked into %s", root)
		}
	}
}

// TestTaskBaseIsVerifiedIntegrationHeadAtReadiness: the Task Base Commit
// recorded at readiness is the Integration HEAD, and the Task Branch and
// Worktree are created from it (PRD Worktree 策略).
func TestTaskBaseIsVerifiedIntegrationHeadAtReadiness(t *testing.T) {
	fx := newExecutionFixture(t)
	fx.RunReadyTasks()
	// The Task Base must equal the Integration Worktree's actual HEAD (the
	// verified Integration HEAD at readiness, PRD Worktree 策略).
	integrationPath := filepath.Join(fx.home, "worktrees", ProjectFor(fx.root).Key, string(fx.wf), "integration")
	out, err := execGit(integrationPath, "rev-parse", "HEAD").CombinedOutput()
	if err != nil {
		t.Fatalf("resolve integration head: %v", err)
	}
	head := strings.TrimSpace(string(out))
	if head == "" {
		t.Fatal("no integration head recorded")
	}
	db, err := sql.Open("sqlite", filepath.Join(fx.home, "cflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var base, branch, worktree string
	if err := db.QueryRow(
		`SELECT task_base_commit, branch_name, worktree_path FROM tasks WHERE id = 'S01'`).
		Scan(&base, &branch, &worktree); err != nil {
		t.Fatalf("read task row: %v", err)
	}
	if base != head {
		t.Fatalf("task base = %s, want the integration head %s", base, head)
	}
	if branch != "cflow/"+string(fx.wf)+"/task-S01" {
		t.Fatalf("task branch = %q", branch)
	}
	if !pathExists(worktree) {
		t.Fatalf("task worktree %s was not created", worktree)
	}
	out, err = execGit(fx.root, "rev-parse", "--verify", branch).CombinedOutput()
	if err != nil {
		t.Fatalf("resolve task branch: %v\n%s", err, out)
	}
	if strings.TrimSpace(string(out)) != base {
		t.Fatalf("task branch head = %s, want the recorded base %s", strings.TrimSpace(string(out)), base)
	}
}
