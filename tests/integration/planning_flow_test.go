// Package integration: the planning lifecycle end to end (Task 10). The
// fixture drives real temporary Git repositories through the Process
// Supervisor, a real temporary CFLOW_HOME with the SQLite State Store and
// the Artifact Store, and the deterministic Fake Adapter. Every planning
// command runs through the Application seam exactly as the CLI routes it.
package integration

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"cflow.local/cflow/internal/agent"
	"cflow.local/cflow/internal/agent/fake"
	"cflow.local/cflow/internal/app"
	"cflow.local/cflow/internal/cli"
	"cflow.local/cflow/internal/gitflow"
	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/observe"
	"cflow.local/cflow/internal/process"
	"cflow.local/cflow/internal/security"
)

// ---------------------------------------------------------------------------
// fixture
// ---------------------------------------------------------------------------

// planningFixture owns one real temporary repository and CFLOW_HOME. The
// Application is rebuilt per phase with exactly the Fake fixture scripts
// that phase needs, sharing the deterministic Clock and ID source so the
// workflow and Session identities chain deterministically.
type planningFixture struct {
	t    *testing.T
	sup  process.Supervisor
	repo *Repo
	home string
	ids  model.IDSource
	now  func() time.Time

	// per-role counters give every fixture script a unique Provider
	// Session ID: the Runtime ledger rejects a reused id, and the fake
	// fixtures declare their ids up front.
	discussSeq int
	planSeq    int
	checkSeq   int

	// testdata is the absolute testdata root, captured before any test
	// chdir moves the process working directory.
	testdata string
}

func newPlanningFixture(t *testing.T) *planningFixture {
	t.Helper()
	repo := newCommittedRepo(t)
	ids := model.SequentialIDSource()
	now := func() time.Time { return time.Unix(1700000000, 0).UTC() }
	td, err := filepath.Abs(filepath.Join("..", "testdata"))
	if err != nil {
		t.Fatalf("resolve testdata: %v", err)
	}
	return &planningFixture{
		t:        t,
		sup:      process.NewSupervisor(process.NewOSAdapter()),
		repo:     repo,
		home:     filepath.Join(repo.Tmp, "home"),
		ids:      ids,
		now:      now,
		testdata: td,
	}
}

// app builds a fresh Application over the fixture repository with the
// given inline Fake fixture scripts (one script per phase; the Fake's
// single-script fallback binds it to any purpose).
func (fx *planningFixture) app(scripts ...string) *app.Application {
	fx.t.Helper()
	reg, err := agent.LoadProviderRegistry()
	if err != nil {
		fx.t.Fatalf("provider registry: %v", err)
	}
	prompts, err := agent.LoadPromptRegistry()
	if err != nil {
		fx.t.Fatalf("prompt registry: %v", err)
	}
	ad := fake.New(reg)
	for _, s := range scripts {
		if err := ad.LoadScript([]byte(s)); err != nil {
			fx.t.Fatalf("load fake script: %v", err)
		}
	}
	flow, err := gitflow.NewGitFlow(fx.sup, fx.repo.Root)
	if err != nil {
		fx.t.Fatalf("new gitflow: %v", err)
	}
	a, err := app.New(app.Options{
		Home:         fx.home,
		Project:      app.ProjectFor(fx.repo.Root),
		CflowVersion: "0.0.0-dev",
		Now:          fx.now,
		IDs:          fx.ids,
		Supervisor:   fx.sup,
		GitFlow:      flow,
		Prompts:      prompts,
		Agent: agent.RuntimeOptions{
			Registry:    reg,
			Redaction:   security.Registry{},
			Adapters:    map[string]agent.Adapter{"fake": ad},
			EvidenceDir: filepath.Join(fx.home, "evidence"),
		},
	})
	if err != nil {
		fx.t.Fatalf("new application: %v", err)
	}
	return a
}

func (fx *planningFixture) CreateWorkflow(name string) model.WorkflowID {
	fx.t.Helper()
	out, err := fx.app().Execute(context.Background(),
		app.CreateWorkflowCommand{Name: name, Provider: "fake", ConfirmDirty: false})
	if err != nil {
		fx.t.Fatalf("create workflow: %v", err)
	}
	return out.Workflow
}

func (fx *planningFixture) Discuss(wf model.WorkflowID, text string) app.Outcome {
	fx.t.Helper()
	fx.discussSeq++
	out, err := fx.app(discussionScript(fmt.Sprintf("d%d", fx.discussSeq), text)).Execute(context.Background(),
		app.DiscussRequirementCommand{Workflow: wf, Text: text, Provider: "fake"})
	if err != nil {
		fx.t.Fatalf("discuss: %v", err)
	}
	return out
}

func (fx *planningFixture) GeneratePlan(wf model.WorkflowID) app.Outcome {
	fx.t.Helper()
	fx.planSeq++
	out, err := fx.app(planScript(fmt.Sprintf("p%d", fx.planSeq), readTestdata(fx.t, "plans/valid.md"))).Execute(context.Background(),
		app.GeneratePlanCommand{Workflow: wf, Provider: "fake"})
	if err != nil {
		fx.t.Fatalf("generate plan: %v", err)
	}
	return out
}

func (fx *planningFixture) CheckPlan(wf model.WorkflowID) app.Outcome {
	fx.t.Helper()
	return fx.CheckPlanWith(wf, "pass")
}

func (fx *planningFixture) CheckPlanWith(wf model.WorkflowID, decision string) app.Outcome {
	fx.t.Helper()
	fx.checkSeq++
	out, err := fx.app(checkScript(fmt.Sprintf("c%d", fx.checkSeq), decision)).Execute(context.Background(),
		app.CheckPlanCommand{Workflow: wf, Provider: "fake"})
	if err != nil {
		fx.t.Fatalf("check plan: %v", err)
	}
	return out
}

func (fx *planningFixture) GeneratePlanWith(wf model.WorkflowID, markdown string) app.Outcome {
	fx.t.Helper()
	fx.planSeq++
	out, err := fx.app(planScript(fmt.Sprintf("p%d", fx.planSeq), markdown)).Execute(context.Background(),
		app.GeneratePlanCommand{Workflow: wf, Provider: "fake"})
	if err != nil {
		fx.t.Fatalf("generate plan: %v", err)
	}
	return out
}

func (fx *planningFixture) ApprovePlan(wf model.WorkflowID, revision int, hash string) error {
	fx.t.Helper()
	_, err := fx.app().Execute(context.Background(),
		app.ApprovePlanCommand{Workflow: wf, Revision: revision, Hash: hash})
	return err
}

// planningStatus is the fixture's own status projection with the verbatim
// field names of the brief's acceptance test.
type planningStatus struct {
	PlanStatus    model.PlanStatus
	RuntimeStatus model.RuntimeStatus
	PlanApproved  bool
	PlanRevision  int
	PlanHash      string
	Stage         model.WorkflowStage
	Approvals     int
}

func (fx *planningFixture) Status(wf model.WorkflowID) planningStatus {
	fx.t.Helper()
	view, err := fx.app().Query(context.Background(), app.StatusQuery{Workflow: wf})
	if err != nil {
		fx.t.Fatalf("status: %v", err)
	}
	sv := view.(app.StatusView)
	return planningStatus{
		PlanStatus:    sv.PlanStatus,
		RuntimeStatus: sv.Runtime,
		PlanApproved:  sv.PlanApproved,
		PlanRevision:  sv.PlanRevision,
		PlanHash:      sv.PlanHash,
		Stage:         sv.Stage,
	}
}

func (fx *planningFixture) Inspect(wf model.WorkflowID) app.InspectView {
	fx.t.Helper()
	view, err := fx.app().Query(context.Background(), app.InspectQuery{Workflow: wf})
	if err != nil {
		fx.t.Fatalf("inspect: %v", err)
	}
	iv := view.(app.InspectView)
	fx.t.Logf("inspect: %d approvals, %d sessions, plan %+v", len(iv.Approvals), len(iv.Sessions), iv.Plan)
	return iv
}

// ---------------------------------------------------------------------------
// fake fixture scripts (inline: the demo's deterministic provider output)
// ---------------------------------------------------------------------------

func discussionScript(sessionID, text string) string {
	return fmt.Sprintf(`{"fixture":"fake-run","script_version":1,"provider":"fake","dialect":"cflow.dialect.fake.v1","purpose":"planning","session_id":%s,"exit_code":0,"resume":"ok"}
{"type":"session_started","session_id":%s,"at_ms":0}
{"type":"assistant_message","session_id":%s,"text":"Understood: %s","at_ms":10}
{"type":"session_finished","session_id":%s,"result":{"accepted":true},"at_ms":20}`,
		strconv.Quote(sessionID), strconv.Quote(sessionID), strconv.Quote(sessionID), text, strconv.Quote(sessionID))
}

func planScript(sessionID, markdown string) string {
	return fmt.Sprintf(`{"fixture":"fake-run","script_version":1,"provider":"fake","dialect":"cflow.dialect.fake.v1","purpose":"planning","session_id":%s,"exit_code":0,"resume":"ok"}
{"type":"session_started","session_id":%s,"at_ms":0}
{"type":"assistant_message","session_id":%s,"text":"Drafting the plan.","at_ms":10}
{"type":"session_finished","session_id":%s,"result":{"plan_markdown":%s},"at_ms":20}`,
		strconv.Quote(sessionID), strconv.Quote(sessionID), strconv.Quote(sessionID), strconv.Quote(sessionID), strconv.Quote(markdown))
}

func checkResultJSON(decision string) string {
	switch decision {
	case "pass":
		return `{"decision":"pass","summary":"The plan is executable.","blockingGaps":[],"nonBlockingSuggestions":["none"],"confidence":0.91}`
	case "needs_revision":
		return `{"decision":"needs_revision","summary":"The plan needs revision.","blockingGaps":["acceptance criteria are missing"],"nonBlockingSuggestions":[],"confidence":0.6}`
	case "needs_discussion":
		return `{"decision":"needs_discussion","summary":"The requirement needs clarification.","blockingGaps":["scope of mutual exclusion"],"nonBlockingSuggestions":[],"confidence":0.5}`
	case "reject":
		return `{"decision":"reject","summary":"The plan cannot be executed.","blockingGaps":["the approach contradicts the constraints"],"nonBlockingSuggestions":[],"confidence":0.2}`
	}
	return `{"decision":"pass","summary":"","blockingGaps":[],"nonBlockingSuggestions":[],"confidence":0.9}`
}

func checkScript(sessionID, decision string) string {
	return fmt.Sprintf(`{"fixture":"fake-run","script_version":1,"provider":"fake","dialect":"cflow.dialect.fake.v1","purpose":"plan-check","session_id":%s,"exit_code":0,"resume":"ok"}
{"type":"session_started","session_id":%s,"at_ms":0}
{"type":"assistant_message","session_id":%s,"text":"Reviewing the plan.","at_ms":10}
{"type":"session_finished","session_id":%s,"result":%s,"at_ms":20}`,
		strconv.Quote(sessionID), strconv.Quote(sessionID), strconv.Quote(sessionID), strconv.Quote(sessionID), checkResultJSON(decision))
}

func readTestdata(t *testing.T, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "testdata", filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read testdata %s: %v", rel, err)
	}
	return string(data)
}

// readFixtureTestdata reads testdata through the fixture's absolute root
// (safe after a test chdir).
func (fx *planningFixture) readTestdata(rel string) string {
	fx.t.Helper()
	data, err := os.ReadFile(filepath.Join(fx.testdata, filepath.FromSlash(rel)))
	if err != nil {
		fx.t.Fatalf("read testdata %s: %v", rel, err)
	}
	return string(data)
}

// ---------------------------------------------------------------------------
// brief Step 1 (verbatim): the planning lifecycle acceptance test
// ---------------------------------------------------------------------------

func TestPlanCheckPausesWithoutApproving(t *testing.T) {
	fx := newPlanningFixture(t)
	wf := fx.CreateWorkflow("add divide")
	fx.Discuss(wf, "division by zero must error")
	plan := fx.GeneratePlan(wf)
	check := fx.CheckPlan(wf)
	if check.SessionID == plan.SessionID {
		t.Fatal("checker reused planner session")
	}
	view := fx.Status(wf)
	if view.PlanStatus != model.PlanChecked || view.RuntimeStatus != model.RuntimePaused {
		t.Fatalf("unexpected status %#v", view)
	}
	if view.PlanApproved {
		t.Fatal("checker pass became user approval")
	}
}

// TestPlanCheckNeedsRevisionThenRevisionRequiresReApproval: a
// needs_revision Check leaves the Plan in DRAFT, returns the Workflow to
// PLAN_GENERATION, and the revised Plan Revision must be checked and
// approved again before Spec generation.
func TestPlanCheckNeedsRevisionThenRevisionRequiresReApproval(t *testing.T) {
	fx := newPlanningFixture(t)
	wf := fx.CreateWorkflow("add divide")
	fx.Discuss(wf, "division by zero must error")
	fx.GeneratePlan(wf)

	out, err := fx.app(checkScript("c1", "needs_revision")).Execute(context.Background(),
		app.CheckPlanCommand{Workflow: wf, Provider: "fake"})
	if err != nil {
		t.Fatalf("check needs_revision: %v", err)
	}
	fx.checkSeq++
	view := fx.Status(wf)
	if view.PlanStatus != model.PlanDraft {
		t.Fatalf("after needs_revision plan status = %s, want DRAFT", view.PlanStatus)
	}
	if view.Stage != model.StagePlanGeneration {
		t.Fatalf("after needs_revision stage = %s, want PLAN_GENERATION", view.Stage)
	}
	if view.RuntimeStatus != model.RuntimeRunning {
		t.Fatalf("after needs_revision runtime = %s, want RUNNING", view.RuntimeStatus)
	}
	if out.SessionID == "" {
		t.Fatal("check outcome carried no session id")
	}

	// The revised Plan is a new immutable Revision: revision 2.
	fx.GeneratePlan(wf)
	view = fx.Status(wf)
	if view.PlanRevision != 2 {
		t.Fatalf("revised plan revision = %d, want 2", view.PlanRevision)
	}

	// The revised revision must pass an independent Check again.
	fx.CheckPlan(wf)
	view = fx.Status(wf)
	if view.PlanStatus != model.PlanChecked || view.RuntimeStatus != model.RuntimePaused {
		t.Fatalf("after revised check: %#v", view)
	}
	if err := fx.ApprovePlan(wf, view.PlanRevision, view.PlanHash); err != nil {
		t.Fatalf("approve revised plan: %v", err)
	}
	view = fx.Status(wf)
	if !view.PlanApproved || view.Stage != model.StageSpecGeneration {
		t.Fatalf("revised plan not approved: %#v", view)
	}
}

// TestPlanRevisionInvalidatesPriorApproval: a new Plan Revision after a
// committed Approval makes the prior Approval inapplicable: the Plan
// returns to DRAFT, the Workflow leaves SPEC_GENERATION, and re-approving
// the old Revision/Hash is APPROVAL_INPUT_CHANGED with no mutation.
func TestPlanRevisionInvalidatesPriorApproval(t *testing.T) {
	fx := newPlanningFixture(t)
	wf := fx.CreateWorkflow("add divide")
	fx.Discuss(wf, "division by zero must error")
	fx.GeneratePlan(wf)
	fx.CheckPlan(wf)
	view := fx.Status(wf)
	if err := fx.ApprovePlan(wf, view.PlanRevision, view.PlanHash); err != nil {
		t.Fatalf("approve plan: %v", err)
	}
	view = fx.Status(wf)
	if !view.PlanApproved || view.Stage != model.StageSpecGeneration {
		t.Fatalf("plan not approved: %#v", view)
	}
	oldRevision, oldHash := view.PlanRevision, view.PlanHash

	// The user adjusts the plan after Approval: a new Plan Revision.
	fx.GeneratePlan(wf)
	view = fx.Status(wf)
	if view.PlanRevision != oldRevision+1 || view.PlanStatus != model.PlanDraft {
		t.Fatalf("revised plan state: %#v", view)
	}
	if view.PlanApproved {
		t.Fatal("prior approval survived a new plan revision")
	}

	// The old Approval is append-only but no longer binds: re-approving
	// the old Revision/Hash is APPROVAL_INPUT_CHANGED with no mutation.
	if err := fx.ApprovePlan(wf, oldRevision, oldHash); err == nil {
		t.Fatal("approving the stale revision/hash succeeded")
	} else if code, ok := model.CodeOf(err); !ok || code != model.CodeApprovalInputChanged {
		t.Fatalf("stale approval error = %v, want APPROVAL_INPUT_CHANGED", err)
	}
	iv := fx.Inspect(wf)
	if len(iv.Approvals) != 1 {
		t.Fatalf("approvals after stale attempt = %d, want 1 (append-only)", len(iv.Approvals))
	}

	// The revised revision must be checked and approved afresh.
	fx.CheckPlan(wf)
	view = fx.Status(wf)
	if err := fx.ApprovePlan(wf, view.PlanRevision, view.PlanHash); err != nil {
		t.Fatalf("approve revised plan: %v", err)
	}
	view = fx.Status(wf)
	if !view.PlanApproved || view.Stage != model.StageSpecGeneration {
		t.Fatalf("revised plan not approved: %#v", view)
	}
}

// TestDirtyUserWorkspaceIsIsolatedAndRequiresConfirmation: creation on a
// dirty user workspace demands explicit confirmation, the dirty content
// never enters the Planning Snapshot, and the initial dirty facts are
// recorded in workflow.yaml (PRD 已确认：用户当前工作区隔离).
func TestDirtyUserWorkspaceIsIsolatedAndRequiresConfirmation(t *testing.T) {
	fx := newPlanningFixture(t)
	writeFile(t, fx.repo.Path("user-wip.txt"), "uncommitted secret")

	// Without confirmation the create refuses.
	a := fx.app()
	if _, err := a.Execute(context.Background(),
		app.CreateWorkflowCommand{Name: "dirty", Provider: "fake", ConfirmDirty: false}); err == nil {
		t.Fatal("create on a dirty workspace without confirmation succeeded")
	}

	// With confirmation the create records and isolates.
	out, err := a.Execute(context.Background(),
		app.CreateWorkflowCommand{Name: "dirty", Provider: "fake", ConfirmDirty: true})
	if err != nil {
		t.Fatalf("create with confirmation: %v", err)
	}
	if pathExists(filepath.Join(fx.home, "worktrees", app.ProjectFor(fx.repo.Root).Key,
		string(out.Workflow), "planning", "user-wip.txt")) {
		t.Fatal("dirty user file leaked into the planning snapshot")
	}
	requireFileContent(t, fx.repo.Path("user-wip.txt"), "uncommitted secret")

	manifest, err := os.ReadFile(filepath.Join(fx.home, "projects", app.ProjectFor(fx.repo.Root).Key,
		"workflows", string(out.Workflow), "workflow.yaml"))
	if err != nil {
		t.Fatalf("read workflow.yaml: %v", err)
	}
	text := string(manifest)
	if !strings.Contains(text, "initial_worktree_dirty: true") {
		t.Fatalf("workflow.yaml missing initial_worktree_dirty: true:\n%s", text)
	}
	if !strings.Contains(text, "initial_dirty_fingerprint: sha256:") {
		t.Fatalf("workflow.yaml missing initial_dirty_fingerprint:\n%s", text)
	}
}

// TestDetachedHeadCannotCreateButExistingWorkflowRemainsViewable: a
// Detached HEAD blocks new Workflow creation but existing Workflows stay
// viewable (PRD 启动与项目识别).
func TestDetachedHeadCannotCreateButExistingWorkflowRemainsViewable(t *testing.T) {
	fx := newPlanningFixture(t)
	wf := fx.CreateWorkflow("add divide")

	fx.repo.git("checkout", "-q", "--detach")
	facts := mustObserve(t, fx.repo, gitflow.ProjectDiscovery{})
	if !facts.Detached {
		t.Fatal("fixture did not detach")
	}

	a := fx.app()
	if _, err := a.Execute(context.Background(),
		app.CreateWorkflowCommand{Name: "second", Provider: "fake", ConfirmDirty: false}); err == nil {
		t.Fatal("create on a detached HEAD succeeded")
	}
	view, err := a.Query(context.Background(), app.StatusQuery{Workflow: wf})
	if err != nil {
		t.Fatalf("existing workflow not viewable: %v", err)
	}
	sv := view.(app.StatusView)
	if sv.Workflow != wf || sv.Stage != model.StageRequirementDiscussion {
		t.Fatalf("existing workflow status = %#v", sv)
	}
}

// TestProviderOutputMissingRequiredPlanSection: a Plan output missing one
// of the PRD's required sections is rejected: no Plan Revision is
// recorded, the Session settles, and a schema finding is persisted.
func TestProviderOutputMissingRequiredPlanSection(t *testing.T) {
	fx := newPlanningFixture(t)
	wf := fx.CreateWorkflow("add divide")
	fx.Discuss(wf, "division by zero must error")

	fx.planSeq++
	out, err := fx.app(planScript("p1", readTestdata(fx.t, "plans/invalid-missing-section.md"))).Execute(context.Background(),
		app.GeneratePlanCommand{Workflow: wf, Provider: "fake"})
	if err != nil {
		t.Fatalf("generate plan with invalid output: %v", err)
	}
	found := false
	for _, f := range out.Findings {
		if f.Code == model.CodeSchemaInvalid {
			found = true
		}
	}
	if !found {
		t.Fatalf("invalid plan output produced no schema finding: %+v", out.Findings)
	}
	view := fx.Status(wf)
	if view.PlanRevision != 0 || view.PlanStatus != "" {
		t.Fatalf("invalid plan output was recorded: %#v", view)
	}
	if view.Stage != model.StagePlanGeneration {
		t.Fatalf("stage after invalid output = %s, want PLAN_GENERATION", view.Stage)
	}

	// The user retries and the corrected output succeeds.
	fx.GeneratePlan(wf)
	view = fx.Status(wf)
	if view.PlanRevision != 1 || view.PlanStatus != model.PlanDraft {
		t.Fatalf("retry did not record the plan: %#v", view)
	}
}

// TestFailedCheckRestoresDraftAndAllowsRecheck: a crashed Checker run
// settles the Session with the failure and restores the Plan to DRAFT —
// the Plan cannot stay CHECKING without a judgment — so the independent
// Check can be retried and pass.
func TestFailedCheckRestoresDraftAndAllowsRecheck(t *testing.T) {
	fx := newPlanningFixture(t)
	wf := fx.CreateWorkflow("add divide")
	fx.Discuss(wf, "division by zero must error")
	fx.GeneratePlan(wf)

	fx.checkSeq++
	failed := fmt.Sprintf(`{"fixture":"fake-run","script_version":1,"provider":"fake","dialect":"cflow.dialect.fake.v1","purpose":"plan-check","session_id":"c%d","exit_code":0,"resume":"ok"}
{"type":"session_started","session_id":"c%d","at_ms":0}
{"type":"session_failed","session_id":"c%d","code":"AGENT_PROCESS_CRASHED","message":"checker crashed","at_ms":10}`,
		fx.checkSeq, fx.checkSeq, fx.checkSeq)
	out, err := fx.app(failed).Execute(context.Background(),
		app.CheckPlanCommand{Workflow: wf, Provider: "fake"})
	if err != nil {
		t.Fatalf("check with crashed checker: %v", err)
	}
	if out.SessionID == "" {
		t.Fatal("failed check outcome carried no session id")
	}
	view := fx.Status(wf)
	if view.PlanStatus != model.PlanDraft {
		t.Fatalf("after crashed check plan status = %s, want DRAFT", view.PlanStatus)
	}
	if view.Stage != model.StagePlanCheck {
		t.Fatalf("after crashed check stage = %s, want PLAN_CHECK", view.Stage)
	}

	// An independent re-check succeeds and passes.
	fx.CheckPlan(wf)
	view = fx.Status(wf)
	if view.PlanStatus != model.PlanChecked || view.RuntimeStatus != model.RuntimePaused {
		t.Fatalf("after re-check: %#v", view)
	}
}

// TestCLIPlanningLifecycle drives the line-oriented CLI over scripted
// stdin: create, discuss, generate, check, approve, and the planning
// views (brief Step 5: no full-screen TUI).
func TestCLIPlanningLifecycle(t *testing.T) {
	fx := newPlanningFixture(t)
	t.Chdir(fx.repo.Root)
	t.Setenv("CFLOW_HOME", fx.home)

	current := []string{}
	deps := cli.Dependencies{
		Build: observe.BuildInfo{Version: "0.0.0-test"},
		OpenApplication: func(ctx context.Context) (*app.Application, error) {
			return fx.app(current...), nil
		},
	}
	root := cli.NewRoot(deps)
	run := func(stdin string, args ...string) (string, error) {
		root.SetArgs(args)
		root.SetIn(strings.NewReader(stdin))
		var buf bytes.Buffer
		root.SetOut(&buf)
		root.SetErr(&buf)
		err := root.Execute()
		return buf.String(), err
	}

	out, err := run("", "workflow-create", "add divide")
	if err != nil {
		t.Fatalf("cli create: %v\n%s", err, out)
	}
	if !strings.Contains(out, "workflow workflow-1") {
		t.Fatalf("cli create output:\n%s", out)
	}

	current = []string{discussionScript("d1", "division by zero must error")}
	out, err = run("division by zero must error\n/done\n", "discuss")
	if err != nil {
		t.Fatalf("cli discuss: %v\n%s", err, out)
	}
	if !strings.Contains(out, "session: session-") {
		t.Fatalf("cli discuss output:\n%s", out)
	}

	current = []string{planScript("p1", fx.readTestdata("plans/valid.md"))}
	out, err = run("", "plan-generate")
	if err != nil {
		t.Fatalf("cli plan-generate: %v\n%s", err, out)
	}

	current = []string{checkScript("c1", "pass")}
	out, err = run("", "plan-check")
	if err != nil {
		t.Fatalf("cli plan-check: %v\n%s", err, out)
	}

	out, err = run("y\n", "plan-approve")
	if err != nil {
		t.Fatalf("cli plan-approve: %v\n%s", err, out)
	}
	if !strings.Contains(out, "plan approved") {
		t.Fatalf("cli plan-approve output:\n%s", out)
	}

	out, err = run("", "plan-show")
	if err != nil {
		t.Fatalf("cli plan-show: %v\n%s", err, out)
	}
	if !strings.Contains(out, "APPROVED") || !strings.Contains(out, "revision 1") {
		t.Fatalf("cli plan-show output:\n%s", out)
	}

	out, err = run("", "status")
	if err != nil {
		t.Fatalf("cli status: %v\n%s", err, out)
	}
	if !strings.Contains(out, "plan: APPROVED") || !strings.Contains(out, "stage: SPEC_GENERATION") {
		t.Fatalf("cli status output:\n%s", out)
	}

	// The user-driven adjustment loop: revise the approved plan through
	// the CLI; the new revision supersedes the old.
	current = []string{planScript("p2", fx.readTestdata("plans/valid.md"))}
	out, err = run("", "plan-generate")
	if err != nil {
		t.Fatalf("cli plan-generate (revise): %v\n%s", err, out)
	}
	out, err = run("", "plan-show")
	if err != nil {
		t.Fatalf("cli plan-show (revise): %v\n%s", err, out)
	}
	if !strings.Contains(out, "revision 2") || !strings.Contains(out, "DRAFT") {
		t.Fatalf("cli plan-show after revise output:\n%s", out)
	}

	// The full aggregate is inspectable through the CLI.
	out, err = run("", "inspect")
	if err != nil {
		t.Fatalf("cli inspect: %v\n%s", err, out)
	}
	if !strings.Contains(out, "approvals: 1") || !strings.Contains(out, "sessions: 4") {
		t.Fatalf("cli inspect output:\n%s", out)
	}
}

// TestDiscussionSessionIDPersists is the PRD scenario "讨论 | Session ID
// 被持久化": the turn's CFlow Session and Provider Session IDs are
// persisted in SQLite and survive a fresh Application.
func TestDiscussionSessionIDPersists(t *testing.T) {
	fx := newPlanningFixture(t)
	wf := fx.CreateWorkflow("add divide")
	out := fx.Discuss(wf, "division by zero must error")
	if out.SessionID == "" {
		t.Fatal("discussion outcome carried no session id")
	}

	iv := fx.Inspect(wf)
	if len(iv.Sessions) != 1 {
		t.Fatalf("sessions = %d, want 1", len(iv.Sessions))
	}
	s := iv.Sessions[0]
	if s.ID != out.SessionID {
		t.Fatalf("persisted session %s != outcome session %s", s.ID, out.SessionID)
	}
	if s.Purpose != model.PurposePlanning || s.Provider != "fake" {
		t.Fatalf("persisted session = %+v", s)
	}
	if s.ProviderSessionID == "" {
		t.Fatal("provider session id was not persisted")
	}
	if s.Status != model.SessionCompleted {
		t.Fatalf("session status = %s, want COMPLETED", s.Status)
	}
}
