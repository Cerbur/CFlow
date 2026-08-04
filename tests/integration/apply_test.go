package integration

// The protected Apply end to end (Task 19, PRD 已确认：显式受保护 Apply,
// design 15.5): the real pipeline drives one workflow through planning,
// execution, serial --no-ff merges, Final Verify, and exact-evidence
// completion; the explicit PrepareApply stages the Integration output in
// an isolated Apply Worktree (never the user's workspace), revalidates
// the Catalog and the independent Apply Verification Session, and the
// explicit ExecuteApply delivers through the compare-and-swap
// fast-forward — or refuses with TARGET_HEAD_DRIFTED when the Target
// Branch advanced after the staging verification, leaving the Target
// exactly at the late advance and the Workflow COMPLETED.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cflow.local/cflow/internal/app"
	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/observe"
)

// ---------------------------------------------------------------------------
// execution drive scripts (the integration fixture carries the planning
// scripts; the execution + apply scripts live here)
// ---------------------------------------------------------------------------

const integrationSpec = `{"id":"s01","goal":"implement divide","depends_on":[],"write_scope":["src/divide/divide.go"],"read_scope":[],"locks":[],"acceptance":{"verification_command_ids":["verify"]},"route":{"provider":"fake","model":"default","budget":10},"timeout_seconds":1800,"max_retry":2}`

const integrationPatch = `{"schema":"cflow-workflow-patch-1","operations":[{"op":"add_checkpoint","node_id":"merge-s01"}]}`

func integrationSpecScript(sessionID string) string {
	return fmt.Sprintf(`{"fixture":"fake-run","script_version":1,"provider":"fake","dialect":"cflow.dialect.fake.v1","purpose":"spec-generation","session_id":%q,"exit_code":0,"resume":"ok"}
{"type":"session_started","session_id":%q,"at_ms":0}
{"type":"assistant_message","session_id":%q,"text":"Splitting the plan.","at_ms":10}
{"type":"session_finished","session_id":%q,"result":{"specs":[%s],"proposed_commands":[]},"at_ms":20}`,
		sessionID, sessionID, sessionID, sessionID, integrationSpec)
}

func integrationPatchScript(sessionID string) string {
	return fmt.Sprintf(`{"fixture":"fake-run","script_version":1,"provider":"fake","dialect":"cflow.dialect.fake.v1","purpose":"workflow-optimization","session_id":%q,"exit_code":0,"resume":"ok"}
{"type":"session_started","session_id":%q,"at_ms":0}
{"type":"assistant_message","session_id":%q,"text":"Proposing a scheduling patch.","at_ms":10}
{"type":"session_finished","session_id":%q,"result":%s,"at_ms":20}`,
		sessionID, sessionID, sessionID, sessionID, integrationPatch)
}

const divideSource = "package divide\n\n// Divide returns a/b.\nfunc Divide(a, b int) (int, error) {\n\treturn a / b, nil\n}\n"

func integrationImplementationScript() string {
	return fmt.Sprintf(`{"fixture":"fake-run","script_version":1,"provider":"fake","dialect":"cflow.dialect.fake.v1","purpose":"implementation","session_id":"i1","exit_code":0,"resume":"ok","tasks":{"task-s01":{"writes":[{"path":"src/divide/divide.go","content":%q}],"commit":"implement divide"}}}
{"type":"session_started","session_id":"i1","at_ms":0}
{"type":"assistant_message","session_id":"i1","text":"Implemented Divide.","at_ms":10}
{"type":"session_finished","session_id":"i1","result":{"summary":"implemented"},"at_ms":20}`, divideSource)
}

func integrationReviewScript(sessionID, purpose string) string {
	return fmt.Sprintf(`{"fixture":"fake-run","script_version":1,"provider":"fake","dialect":"cflow.dialect.fake.v1","purpose":%q,"session_id":%q,"exit_code":0,"resume":"ok"}
{"type":"session_started","session_id":%q,"at_ms":0}
{"type":"assistant_message","session_id":%q,"text":"Reviewed the result.","at_ms":10}
{"type":"session_finished","session_id":%q,"result":{"decision":"PASS","report":"PASS\n\nFindings:\n- none\n"},"at_ms":20}`,
		purpose, sessionID, sessionID, sessionID, sessionID)
}

func integrationApplyVerifyScript() string {
	return integrationReviewScript("av1", "apply-verification")
}

// ---------------------------------------------------------------------------
// fixture
// ---------------------------------------------------------------------------

// applyIntegrationFixture wraps the integration planning fixture with the
// repository wrappers committed at the Base Commit and the completed
// workflow identity.
type applyIntegrationFixture struct {
	t  *testing.T
	fx *planningFixture
	wf model.WorkflowID
}

func newApplyIntegrationFixture(t *testing.T) *applyIntegrationFixture {
	t.Helper()
	fx := newPlanningFixture(t)
	write := func(rel, content string) {
		t.Helper()
		path := fx.repo.Path(filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	write("scripts/verify.sh", "#!/bin/sh\nexit 0\n")
	write("scripts/final-verify.sh", "#!/bin/sh\nexit 0\n")
	write("scripts/apply-verify.sh", "#!/bin/sh\nexit 0\n")
	fx.repo.git("add", "scripts")
	fx.repo.git("commit", "-q", "-m", "add verification wrappers")
	return &applyIntegrationFixture{t: t, fx: fx}
}

// driveToCompletion runs the real pipeline through the exact-evidence
// completion and returns the workflow identity.
func (af *applyIntegrationFixture) driveToCompletion() model.WorkflowID {
	t := af.t
	fx := af.fx
	wf := fx.CreateWorkflow("apply-demo")
	fx.Discuss(wf, "Implement division with an error on zero divisor.")
	fx.GeneratePlan(wf)
	fx.CheckPlan(wf)
	st := fx.Status(wf)
	if err := fx.ApprovePlan(wf, st.PlanRevision, st.PlanHash); err != nil {
		t.Fatalf("approve plan: %v", err)
	}
	if _, err := fx.app(integrationSpecScript("s1")).Execute(context.Background(),
		app.GenerateSpecsCommand{Workflow: wf, Provider: "fake"}); err != nil {
		t.Fatalf("generate specs: %v", err)
	}
	if _, err := fx.app(integrationPatchScript("w1")).Execute(context.Background(),
		app.CompileWorkflowCommand{Workflow: wf, Provider: "fake"}); err != nil {
		t.Fatalf("compile workflow: %v", err)
	}
	if _, err := fx.app().Execute(context.Background(),
		app.ExecutionDryRunCommand{Workflow: wf}); err != nil {
		t.Fatalf("execution dry run: %v", err)
	}
	qview, err := fx.app().Query(context.Background(), app.ExecutionPreviewQuery{Workflow: wf})
	if err != nil {
		t.Fatalf("execution preview: %v", err)
	}
	pv := qview.(app.ExecutionPreviewView)
	if _, err := fx.app().Execute(context.Background(), app.ApproveExecutionCommand{
		Workflow:         wf,
		PlanHash:         pv.PlanHash,
		SpecHashes:       pv.SpecHashes,
		CatalogHash:      pv.CatalogHash,
		WorkflowHash:     pv.WorkflowHash,
		RoutingHash:      pv.RoutingHash,
		BudgetHash:       pv.BudgetHash,
		CommitPolicyHash: pv.CommitPolicyHash,
	}); err != nil {
		t.Fatalf("execution approval: %v", err)
	}
	a := fx.app(integrationImplementationScript(),
		integrationReviewScript("r1", "review"),
		integrationReviewScript("fr1", "final-verification"))
	for i := 0; i < 24; i++ {
		if _, err := a.Execute(context.Background(), app.DispatchCommand{Workflow: wf}); err != nil {
			t.Fatalf("dispatch pass %d: %v", i, err)
		}
		iv := fx.Inspect(wf)
		if iv.Status.Stage == model.StageCompleted {
			return wf
		}
	}
	t.Fatalf("workflow did not complete within the dispatch budget")
	return wf
}

func (af *applyIntegrationFixture) latestApply() *model.ApplyAttempt {
	iv := af.fx.Inspect(af.wf)
	if len(iv.ApplyAttempts) == 0 {
		return nil
	}
	return &iv.ApplyAttempts[len(iv.ApplyAttempts)-1]
}

func (af *applyIntegrationFixture) targetHead() string {
	return strings.TrimSpace(string(af.fx.repo.git("rev-parse", "refs/heads/main")))
}

// ---------------------------------------------------------------------------
// tests
// ---------------------------------------------------------------------------

// TestApplyDeliversCompletedWorkflowToTarget: the full pipeline completes,
// the explicit PrepareApply stages the Integration output in the isolated
// Apply Worktree and revalidates it (deterministic apply-verify command +
// the independent Apply Verification Session), and the explicit
// ExecuteApply fast-forwards the Target Branch to the verified staging
// head. The Workflow stays COMPLETED and the Final Report shows the Apply
// outcome.
func TestApplyDeliversCompletedWorkflowToTarget(t *testing.T) {
	af := newApplyIntegrationFixture(t)
	wf := af.driveToCompletion()

	a := af.fx.app(integrationApplyVerifyScript())
	out, err := a.Execute(context.Background(), app.PrepareApplyCommand{Workflow: wf})
	if err != nil {
		t.Fatalf("prepare apply: %v", err)
	}
	if out.Apply == nil || out.Apply.Status != model.ApplyAwaitingConfirmation {
		t.Fatalf("apply after staging = %+v, want AWAITING_CONFIRMATION", out.Apply)
	}
	att := out.Apply
	staging := strings.TrimSpace(string(af.fx.repo.git("rev-parse",
		fmt.Sprintf("refs/heads/cflow/%s/apply-%d", wf, att.Number))))

	if _, err := a.Execute(context.Background(), app.ExecuteApplyCommand{Workflow: wf}); err != nil {
		t.Fatalf("execute apply: %v", err)
	}
	if got := af.targetHead(); got != staging {
		t.Fatalf("target = %s, want the verified staging head %s", got, staging)
	}
	iv := af.fx.Inspect(wf)
	if iv.Status.Stage != model.StageCompleted || iv.Status.Runtime != model.RuntimeSucceeded {
		t.Fatalf("workflow = %s/%s after the apply, want COMPLETED/SUCCEEDED",
			iv.Status.Stage, iv.Status.Runtime)
	}
	// The delivered tree carries the Integration output.
	if strings.TrimSpace(string(af.fx.repo.git("cat-file", "-e", staging+":src/divide/divide.go"))) != "" {
		t.Fatalf("the delivered target misses the integration output")
	}
	// The Final Report renders the Apply outcome (Task 18 seam: no longer
	// NOT_RUN once an Apply attempt exists).
	view, err := a.Query(context.Background(), app.ReportQuery{Workflow: wf, Build: observe.BuildInfo{Version: "0.0.0-dev"}})
	if err != nil {
		t.Fatalf("report query: %v", err)
	}
	if rv := view.(app.ReportView); rv.Report.Apply.Status != model.ApplySucceeded.String() {
		t.Fatalf("report apply status = %s, want %s", rv.Report.Apply.Status, model.ApplySucceeded)
	}
}

// TestApplyTargetCASLateAdvanceBlocksDelivery: the Target Branch advances
// after the staging verification passed; the explicit delivery refuses
// with TARGET_HEAD_DRIFTED and the Target stays exactly at the late
// advance while the Workflow remains COMPLETED (design 15.5, PRD Target
// Branch Drift).
func TestApplyTargetCASLateAdvanceBlocksDelivery(t *testing.T) {
	af := newApplyIntegrationFixture(t)
	wf := af.driveToCompletion()

	a := af.fx.app(integrationApplyVerifyScript())
	out, err := a.Execute(context.Background(), app.PrepareApplyCommand{Workflow: wf})
	if err != nil {
		t.Fatalf("prepare apply: %v", err)
	}
	if out.Apply == nil || out.Apply.Status != model.ApplyAwaitingConfirmation {
		t.Fatalf("apply after staging = %+v, want AWAITING_CONFIRMATION", out.Apply)
	}
	af.fx.repo.git("commit", "-q", "--allow-empty", "-m", "late user advance")
	late := strings.TrimSpace(string(af.fx.repo.git("rev-parse", "HEAD")))

	_, err = a.Execute(context.Background(), app.ExecuteApplyCommand{Workflow: wf})
	if err == nil {
		t.Fatalf("the late advance must block the delivery")
	}
	code, ok := model.CodeOf(err)
	if !ok || code != model.CodeTargetHeadChanged {
		t.Fatalf("delivery fault = %v, want %s", err, model.CodeTargetHeadChanged)
	}
	if got := af.targetHead(); got != late {
		t.Fatalf("target = %s, want the late advance %s", got, late)
	}
	iv := af.fx.Inspect(wf)
	if iv.Status.Stage != model.StageCompleted || iv.Status.Runtime != model.RuntimeSucceeded {
		t.Fatalf("workflow = %s/%s, want COMPLETED/SUCCEEDED", iv.Status.Stage, iv.Status.Runtime)
	}
}
