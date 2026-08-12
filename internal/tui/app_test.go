package tui

// Root TUI Model tests (TUI tasks 9, 10, 14): the Model loads the real
// read-only Workspace projection through the shared Application, page
// navigation reaches every lifecycle page, user actions map to the exact
// typed Application Commands, first-Enter previews followed by second-Enter
// execution, and the controlled-stop protocol executes the real Pause and
// Force Stop.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"cflow.local/cflow/internal/app"
	"cflow.local/cflow/internal/foreground"
	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/process"
)

func TestWorkspaceQueryRetriesWithoutStaleSelection(t *testing.T) {
	ctrl := &staleWorkspaceQueryController{}
	m := newModel(Dependencies{})
	m.ctrl = ctrl
	msg := m.queryProjectionMsg(PageWorkspace, app.ProjectWorkspaceQuery{Selected: "wf-removed"})
	projection, ok := msg.(projectionMsg)
	if !ok || projection.err != nil {
		t.Fatalf("workspace query fallback = %#v", msg)
	}
	if len(ctrl.queries) != 2 {
		t.Fatalf("workspace queries = %d, want stale selection plus fallback", len(ctrl.queries))
	}
	if got := ctrl.queries[1].(app.ProjectWorkspaceQuery).Selected; got != "" {
		t.Fatalf("fallback selected = %q, want empty", got)
	}
	if _, ok := projection.view.(app.WorkspaceView); !ok {
		t.Fatalf("fallback view = %T, want app.WorkspaceView", projection.view)
	}
}

func TestWorkflowMenuRendersInsideThePersistentWorkbench(t *testing.T) {
	m := newModel(Dependencies{})
	m.ready = true
	m.width = 150
	m.height = 37
	m.page = PageWorkflowMenu
	m.selected = "wf-1"
	m.workspace = longWorkspaceViewModel()
	m.workspace.Selected = WorkflowItem{ID: "wf-1", Name: "calculator", Stage: model.StageRequirementDiscussion, Runtime: model.RuntimePaused}
	m.workflowMenu = app.WorkflowMenuView{
		Workflow: "wf-1",
		Name:     "calculator",
		Stage:    model.StageRequirementDiscussion,
		Runtime:  model.RuntimePaused,
		Entries: []app.WorkflowMenuEntry{
			{ID: "resume", Group: app.MenuGroupContinue, Kind: app.MenuEntryAction, Label: "Resume", Action: app.MenuActionResume},
			{ID: "stage", Group: app.MenuGroupView, Kind: app.MenuEntryReadonly, Label: "Current Stage", Route: app.MenuRouteCurrentStage},
		},
		DefaultIndex: 0,
	}
	m.workflowMenuModel = MapWorkflowMenu(m.workflowMenu)

	got := render(m)
	for _, want := range []string{"WORKFLOWS", "WORKFLOW MENU", "INSPECTOR", "CONTINUE", "RESUME"} {
		if !strings.Contains(got, want) {
			t.Fatalf("nested Workflow Menu render misses %q:\n%s", want, got)
		}
	}
	if strings.HasPrefix(strings.TrimSpace(got), "WORKFLOW MENU") {
		t.Fatalf("Workflow Menu replaced the persistent workbench instead of occupying its center:\n%s", got)
	}
	assertWorkspaceBounds(t, got, m.width, m.height)
}

func TestNestedWorkspaceContentUsesTheResponsiveShellAtSmallSizes(t *testing.T) {
	m := newModel(Dependencies{})
	m.ready = true
	m.width = 100
	m.height = 6
	m.page = PageWorkflowMenu
	m.selected = "wf-1"
	m.workflowMenu = app.WorkflowMenuView{
		Workflow: "wf-1",
		Name:     "calculator",
		Stage:    model.StageRequirementDiscussion,
		Runtime:  model.RuntimePaused,
		Entries: []app.WorkflowMenuEntry{{
			ID: "stage", Group: app.MenuGroupView, Kind: app.MenuEntryReadonly,
			Label: "Current Stage", Route: app.MenuRouteCurrentStage,
		}},
	}
	m.workflowMenuModel = MapWorkflowMenu(m.workflowMenu)

	got := render(m)
	assertWorkspaceBounds(t, got, m.width, m.height)
	if strings.ContainsAny(got, "╭╮├┤╰╯") {
		t.Fatalf("small nested workspace rendered a partial panel:\n%s", got)
	}
	if !strings.Contains(visibleTerminalText(got), "WORKFLOW MENU") {
		t.Fatalf("small nested workspace lost its center content:\n%s", got)
	}
}

func TestHomeEnterChangesOnlyTheCenterWorkspaceContent(t *testing.T) {
	ctrl := &workflowMenuController{view: app.WorkflowMenuView{
		Workflow: "wf-1", Name: "calculator", Stage: model.StageRequirementDiscussion, Runtime: model.RuntimePaused,
		Entries: []app.WorkflowMenuEntry{{ID: "stage", Group: app.MenuGroupView, Kind: app.MenuEntryReadonly, Label: "Current Stage", Route: app.MenuRouteCurrentStage}},
	}}
	m := newModel(Dependencies{})
	m.ctrl = ctrl
	m.ready = true
	m.width = 150
	m.height = 37
	m.selected = "wf-1"
	m.workspace = longWorkspaceViewModel()
	m.workspace.Rows = []WorkflowRow{{Kind: WorkflowRowExisting, ID: "wf-1", Name: "calculator"}}

	updated, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = updated.(Model)
	if cmd == nil {
		t.Fatal("Home Enter did not query the Workflow Menu")
	}
	m = step(t, m, projectionMsg{page: PageWorkflowMenu, workflow: "wf-1", view: ctrl.view})
	frame := visibleTerminalText(render(m))
	for _, want := range []string{"WORKFLOWS", "WORKFLOW MENU", "INSPECTOR", "CURRENT STAGE"} {
		if !strings.Contains(frame, want) {
			t.Fatalf("Home Enter center transition misses %q:\n%s", want, frame)
		}
	}
	if m.page != PageWorkflowMenu || m.navigation.Current().Layer != LayerWorkflowMenu {
		t.Fatalf("Home Enter route = page %v frame %+v", m.page, m.navigation.Current())
	}
}

type staleWorkspaceQueryController struct {
	queries []app.Query
}

func (c *staleWorkspaceQueryController) Execute(context.Context, app.Command) (app.Outcome, error) {
	return app.Outcome{}, nil
}

func (c *staleWorkspaceQueryController) Query(_ context.Context, q app.Query) (app.View, error) {
	c.queries = append(c.queries, q)
	workspaceQ, ok := q.(app.ProjectWorkspaceQuery)
	if !ok {
		return nil, model.InvalidInputFault("unexpected query")
	}
	if workspaceQ.Selected != "" {
		return nil, model.InvalidInputFault("no such workflow: " + string(workspaceQ.Selected))
	}
	return app.WorkspaceView{Workflows: []app.WorkflowSummary{{ID: "wf-1"}}}, nil
}

func (*staleWorkspaceQueryController) DriveOnce(context.Context, model.WorkflowID) (app.DriveOutcome, error) {
	return app.DriveOutcome{}, nil
}

func (*staleWorkspaceQueryController) EscalateStop() {}

func TestApplyProjectionNormalizesStaleWorkspaceSelection(t *testing.T) {
	m := newModel(Dependencies{})
	m.selected = "wf-removed"
	m, _ = m.applyProjection(projectionMsg{
		page: PageWorkspace,
		view: app.WorkspaceView{
			Selected:     "wf-removed",
			Workflows:    []app.WorkflowSummary{{ID: "wf-1", Runtime: model.RuntimePaused}},
			Lifecycle:    &app.WorkflowLifecycleView{Status: app.StatusView{Workflow: "wf-1", Runtime: model.RuntimePaused}},
			LegalActions: []app.LegalAction{{Label: "Resume", Kind: model.ResumeWorkflow}},
		},
	})
	if m.selected != "wf-1" || m.workspace.Selected.ID != "wf-1" {
		t.Fatalf("root selection = %q, workspace selection = %q; want wf-1", m.selected, m.workspace.Selected.ID)
	}
}

func TestCommandStatusWaitsForProjectionAcknowledgement(t *testing.T) {
	m := newModel(Dependencies{})
	m.page = PageExecutionApproval
	m.selected = "wf-1"
	m.commandState = &commandState{inFlight: true, generation: 1, pending: 1, ackPage: PageExecutionApproval, workflow: "wf-1"}

	m, _ = m.applyCommand(commandDoneMsg{
		cmd:        app.GenerateSpecsCommand{Workflow: "wf-1"},
		generation: 1,
	})
	if m.status == "specs generated" || m.pendingProjectionStatus != "specs generated" {
		t.Fatalf("command completion claimed final status before projection: status=%q pending=%q", m.status, m.pendingProjectionStatus)
	}

	m, _ = m.applyProjection(projectionMsg{
		page:              PageExecutionApproval,
		workflow:          "wf-1",
		generation:        1,
		commandGeneration: 1,
		view:              app.ExecutionPreviewView{Workflow: "wf-1"},
	})
	if m.status != "specs generated" || m.pendingProjectionStatus != "" {
		t.Fatalf("projection acknowledgement did not publish final status: status=%q pending=%q", m.status, m.pendingProjectionStatus)
	}
}

func TestNavigationProjectionDoesNotAcknowledgeCommand(t *testing.T) {
	m := newModel(Dependencies{})
	m.page = PageWorkspace
	m.selected = "wf-1"
	m.commandState = &commandState{inFlight: true, generation: 3, pending: 3, ackPage: PageWorkspace, workflow: "wf-2"}
	m.pendingProjectionStatus = "command complete"

	// This is an ordinary navigation query issued while the mutation is in
	// flight. It shares the command generation but is not the command's
	// acknowledgement query.
	m, _ = m.applyProjection(projectionMsg{
		page:              PageWorkspace,
		workflow:          "wf-1",
		generation:        11,
		commandGeneration: 0,
		view: app.WorkspaceView{
			Selected:  "wf-1",
			Workflows: []app.WorkflowSummary{{ID: "wf-1"}},
		},
	})
	if !m.commandState.inFlight {
		t.Fatal("ordinary navigation projection acknowledged the command")
	}
	if m.status == "command complete" {
		t.Fatal("ordinary navigation projection published the command status")
	}
}

func TestCommandAcknowledgementRequiresBoundWorkflow(t *testing.T) {
	m := newModel(Dependencies{})
	m.page = PageWorkspace
	m.selected = "wf-2"
	m.commandState = &commandState{
		inFlight: true, generation: 4, pending: 4, ackPage: PageWorkspace, workflow: "wf-1",
	}
	m.pendingProjectionStatus = "command complete"

	m, _ = m.applyProjection(projectionMsg{
		page:              PageWorkspace,
		workflow:          "wf-2",
		generation:        14,
		commandGeneration: 4,
		view:              app.WorkspaceView{Selected: "wf-2", Workflows: []app.WorkflowSummary{{ID: "wf-2"}}},
	})
	if !m.commandState.inFlight {
		t.Fatal("different-workflow projection acknowledged the command")
	}
	if m.status == "command complete" {
		t.Fatal("different-workflow projection published the command status")
	}
}

func TestCommandAcknowledgementRetainsOriginPageAcrossNavigation(t *testing.T) {
	m := newModel(Dependencies{})
	m.page = PageExecutionApproval
	m.selected = "wf-1"
	_ = m.executeCmd(app.GenerateSpecsCommand{Workflow: "wf-1"})
	m.page = PagePlanApproval
	_ = m.reloadCmd()
	if got, want := m.commandState.ackPage, PageExecutionApproval; got != want {
		t.Fatalf("ack page changed after navigation: got %v, want %v", got, want)
	}
}

func TestProjectionSequenceRejectsOutOfOrderQuery(t *testing.T) {
	m := newModel(Dependencies{})
	m.page = PagePlanApproval
	m.selected = "wf-1"

	m, _ = m.applyProjection(projectionMsg{
		page:       PagePlanApproval,
		workflow:   "wf-1",
		generation: 12,
		view:       app.PlanView{Workflow: "wf-1", Revision: 2, Hash: "new"},
	})
	m, _ = m.applyProjection(projectionMsg{
		page:       PagePlanApproval,
		workflow:   "wf-1",
		generation: 11,
		view:       app.PlanView{Workflow: "wf-1", Revision: 1, Hash: "old"},
	})
	if m.plan.Revision != 2 || m.plan.Hash != "new" {
		t.Fatalf("older projection replaced newer state: %+v", m.plan)
	}
}

func TestProjectionErrorDoesNotAcknowledgeCommand(t *testing.T) {
	m := newModel(Dependencies{})
	m.selected = "wf-1"
	m.commandState = &commandState{inFlight: true, generation: 4, pending: 4, ackPage: PageWorkspace}
	m.pendingProjectionStatus = "command complete"

	m, _ = m.applyProjection(projectionMsg{
		page:       PageWorkspace,
		workflow:   "wf-1",
		generation: 4,
		err:        model.InvalidInputFault("workspace projection unavailable"),
	})
	if !m.commandState.inFlight {
		t.Fatal("projection error incorrectly acknowledged the command")
	}
	if m.pendingProjectionStatus != "command complete" {
		t.Fatalf("projection error discarded pending status: %q", m.pendingProjectionStatus)
	}
}

func TestMalformedProjectionDoesNotAcknowledgeCommand(t *testing.T) {
	m := newModel(Dependencies{})
	m.page = PageWorkspace
	m.selected = "wf-1"
	m.commandState = &commandState{inFlight: true, generation: 10, pending: 10, ackPage: PageWorkspace}
	m.pendingProjectionStatus = "command complete"

	m, _ = m.applyProjection(projectionMsg{
		page:              PageWorkspace,
		workflow:          "wf-1",
		generation:        10,
		commandGeneration: 10,
	})
	if !m.commandState.inFlight {
		t.Fatal("malformed projection acknowledged the command")
	}
	if m.pendingProjectionStatus != "command complete" {
		t.Fatalf("pending status = %q, want command complete", m.pendingProjectionStatus)
	}
}

func TestResetSelectionStateClearsWorkflowBoundPages(t *testing.T) {
	m := newModel(Dependencies{})
	m.terminal.Report = "old report"
	m.terminal.ApplyPreview = "old apply"
	m.terminal.CleanupPreview = "old cleanup"
	m.execution = NewExecutionModel("wf-old")
	m.execution.Log = []string{"old event"}
	m.resetSelectionState()
	if m.terminal.Report != "" || m.terminal.ApplyPreview != "" || m.terminal.CleanupPreview != "" {
		t.Fatalf("terminal state survived selection reset: %+v", m.terminal)
	}
	if m.execution.Workflow != "" || len(m.execution.Log) != 0 {
		t.Fatalf("execution state survived selection reset: %+v", m.execution)
	}
}

func TestRunnerStopUsesBoundWorkflow(t *testing.T) {
	rec := &recordingController{ctrl: &migrationController{}}
	m := newModel(Dependencies{})
	m.ctrl = rec
	m.running = true
	m.runnerWorkflow = "wf-running"
	m.selected = "wf-selected"
	m.runCancel = func() {}
	m.page = PageExecution

	updated, cmd := m.Update(tea.KeyPressMsg{Code: KeyCtrlCRune, Mod: tea.ModCtrl})
	if cmd == nil {
		t.Fatal("controlled stop produced no pause command")
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, nested := range batch {
			if nested != nil {
				msg = nested()
				break
			}
		}
	}
	if _, ok := msg.(commandDoneMsg); !ok {
		t.Fatalf("pause command result = %T, want commandDoneMsg (updated=%+v)", msg, updated)
	}
	if len(rec.executed) != 1 {
		t.Fatalf("executed commands = %v, want one pause", rec.executed)
	}
	pause, ok := rec.executed[0].(app.PauseWorkflowCommand)
	if !ok || pause.Workflow != "wf-running" {
		t.Fatalf("pause command = %#v, want wf-running", rec.executed[0])
	}
}

func TestStaleReportDoesNotOverwriteSelectedWorkflow(t *testing.T) {
	m := newModel(Dependencies{})
	m.selected = "wf-new"
	m.terminal.Report = "new report"
	m, _ = func() (Model, tea.Cmd) {
		updated, cmd := m.Update(reportLoadedMsg{workflow: "wf-old", markdown: "old report"})
		return updated.(Model), cmd
	}()
	if m.terminal.Report != "new report" {
		t.Fatalf("stale report overwrote selected workflow: %q", m.terminal.Report)
	}
}

func TestUnexpectedExecutionProjectionErrorDoesNotAcknowledgeCommand(t *testing.T) {
	m := newModel(Dependencies{})
	m.page = PageExecutionApproval
	m.selected = "wf-1"
	m.commandState = &commandState{inFlight: true, generation: 6, pending: 6, ackPage: PageExecutionApproval}
	m.pendingProjectionStatus = "workflow compiled"

	m, _ = m.applyProjection(projectionMsg{
		page:       PageExecutionApproval,
		workflow:   "wf-1",
		generation: 6,
		err:        model.InvalidInputFault("execution inputs are incomplete; generate specs and compile the workflow first"),
	})
	if !m.commandState.inFlight {
		t.Fatal("unexpected execution projection error acknowledged the command")
	}
	if m.pendingProjectionStatus != "workflow compiled" {
		t.Fatalf("pending status = %q, want workflow compiled", m.pendingProjectionStatus)
	}
}

func TestExpectedEmptyExecutionProjectionAcknowledgesCommand(t *testing.T) {
	m := newModel(Dependencies{})
	m.page = PageExecutionApproval
	m.selected = "wf-1"
	m.workspace.Lifecycle = &LifecycleItem{ID: "wf-1", Stage: model.StageSpecGeneration}
	m.commandState = &commandState{inFlight: true, generation: 5, pending: 5, ackPage: PageExecutionApproval, workflow: "wf-1"}
	m.pendingProjectionStatus = "specs generated"

	m, _ = m.applyProjection(projectionMsg{
		page:              PageExecutionApproval,
		workflow:          "wf-1",
		generation:        5,
		commandGeneration: 5,
		err:               model.InvalidInputFault("execution inputs are incomplete; no preview is available"),
	})
	if m.commandState.inFlight {
		t.Fatal("expected-empty execution projection did not acknowledge the command")
	}
	if m.status != "specs generated" || m.pendingProjectionStatus != "" {
		t.Fatalf("status = %q, pending = %q; want acknowledged status", m.status, m.pendingProjectionStatus)
	}
	if m.preview.Workflow != "wf-1" {
		t.Fatalf("preview workflow = %q, want wf-1", m.preview.Workflow)
	}
	if m.preview.PlanHash != "" || len(m.preview.SpecHashes) != 0 {
		t.Fatalf("expected empty preview, got %+v", m.preview)
	}
}

func TestExpectedEmptyExecutionProjectionDoesNotAcknowledgeCompile(t *testing.T) {
	m := newModel(Dependencies{})
	m.page = PageExecutionApproval
	m.selected = "wf-1"
	m.commandState = &commandState{inFlight: true, generation: 8, pending: 8, ackPage: PageExecutionApproval, workflow: "wf-1"}
	m.pendingProjectionStatus = "workflow compiled"

	m, _ = m.applyProjection(projectionMsg{
		page:              PageExecutionApproval,
		workflow:          "wf-1",
		generation:        8,
		commandGeneration: 8,
		err:               model.InvalidInputFault("execution inputs are incomplete; no preview is available"),
	})
	if !m.commandState.inFlight {
		t.Fatal("compile projection's missing preview was incorrectly treated as expected-empty")
	}
	if m.pendingProjectionStatus != "workflow compiled" {
		t.Fatalf("pending status = %q, want workflow compiled", m.pendingProjectionStatus)
	}
}

func TestExpectedEmptyExecutionProjectionRequiresSpecStageWhenIdle(t *testing.T) {
	m := newModel(Dependencies{})
	m.page = PageExecutionApproval
	m.selected = "wf-1"
	m.workspace.Lifecycle = &LifecycleItem{ID: "wf-1", Stage: model.StageWorkflowGeneration}

	m, _ = m.applyProjection(projectionMsg{
		page:       PageExecutionApproval,
		workflow:   "wf-1",
		generation: 2,
		err:        model.InvalidInputFault("execution inputs are incomplete; no preview is available"),
	})
	if m.preview.Workflow != "" {
		t.Fatalf("late empty preview was normalized outside spec stage: %+v", m.preview)
	}
}

func TestGenerateSpecsAcknowledgesBoundExpectedEmptyProjection(t *testing.T) {
	m := newModel(Dependencies{})
	m.ctrl = &expectedEmptyPreviewController{}
	m.page = PageExecutionApproval
	m.selected = "wf-1"

	command := m.executeCmd(app.GenerateSpecsCommand{Workflow: "wf-1"})
	done, ok := command().(commandDoneMsg)
	if !ok {
		t.Fatal("GenerateSpecs command did not return commandDoneMsg")
	}
	m, reload := m.applyCommand(done)
	m = runCmds(t, m, reload)
	if m.commandState.inFlight {
		t.Fatal("expected-empty projection left the GenerateSpecs command in flight")
	}
	if m.status != "specs generated" {
		t.Fatalf("status = %q, want specs generated", m.status)
	}
}

func TestExpectedEmptySpecsStatusDoesNotNormalizeNavigationProjection(t *testing.T) {
	m := newModel(Dependencies{})
	m.page = PageExecutionApproval
	m.selected = "wf-1"
	m.commandState = &commandState{
		inFlight: true,
		pending:  7,
		ackPage:  PageDiscussion,
		workflow: "wf-1",
	}
	m.pendingProjectionStatus = "specs generated"

	m, _ = m.applyProjection(projectionMsg{
		page:       PageExecutionApproval,
		workflow:   "wf-1",
		generation: 8,
		err:        model.InvalidInputFault("execution inputs are incomplete; no preview is available"),
	})
	if m.preview.Workflow != "" {
		t.Fatalf("navigation projection borrowed pending Specs status: %+v", m.preview)
	}
	if m.status == "specs generated" {
		t.Fatalf("navigation projection published pending Specs status: %q", m.status)
	}
}

func TestCommandAcknowledgementSurvivesSelectionNavigation(t *testing.T) {
	m := newModel(Dependencies{})
	m.selected = "wf-2"
	m.commandState = &commandState{inFlight: true, generation: 3, pending: 3, ackPage: PageWorkspace, workflow: "wf-1"}
	m.pendingProjectionStatus = "command complete"
	m.workspace.Selected.ID = "wf-2"

	m, _ = m.applyProjection(projectionMsg{
		page:              PageWorkspace,
		workflow:          "wf-1",
		generation:        3,
		commandGeneration: 3,
		view: app.WorkspaceView{
			Selected:  "wf-1",
			Workflows: []app.WorkflowSummary{{ID: "wf-1"}},
		},
	})
	if m.commandState.inFlight {
		t.Fatal("stale command projection left the command gate in flight")
	}
	if m.selected != "wf-2" || m.workspace.Selected.ID != "wf-2" {
		t.Fatalf("stale command projection overwrote selection: selected=%q workspace=%q", m.selected, m.workspace.Selected.ID)
	}
	if m.status == "command complete" {
		t.Fatalf("stale command projection published old-workflow status: %q", m.status)
	}
}

func TestWorkspaceProjectionAcknowledgesWorkspaceBackedPageCommand(t *testing.T) {
	m := newModel(Dependencies{})
	m.page = PageExecution
	m.selected = "wf-1"
	m.commandState = &commandState{inFlight: true, generation: 7, pending: 7, workflow: "wf-1"}
	m.reloadCmd()
	if got, want := m.commandState.ackPage, PageWorkspace; got != want {
		t.Fatalf("ack page = %v, want %v for workspace-backed execution page", got, want)
	}

	m, _ = m.applyProjection(projectionMsg{
		page:              PageWorkspace,
		workflow:          "wf-1",
		generation:        7,
		commandGeneration: 7,
		view: app.WorkspaceView{
			Selected:  "wf-1",
			Workflows: []app.WorkflowSummary{{ID: "wf-1", Runtime: model.RuntimePaused}},
		},
	})
	if m.commandState.inFlight {
		t.Fatal("workspace projection did not acknowledge the command")
	}
}

func TestApplyProjectionStaleSelectionClearsBoundState(t *testing.T) {
	m := newModel(Dependencies{})
	m.selected = "wf-removed"
	m.provider = "claude"
	m.discussion.Provider = "claude"
	m.plan = app.PlanView{Workflow: "wf-removed", Revision: 2}
	m.preview = app.ExecutionPreviewView{Workflow: "wf-removed", WorkflowHash: "old"}
	m.pendingDecision = "adopt workspace"

	m, _ = m.applyProjection(projectionMsg{
		page: PageWorkspace,
		view: app.WorkspaceView{
			Selected:  "wf-1",
			Workflows: []app.WorkflowSummary{{ID: "wf-1", Runtime: model.RuntimePaused}},
			Lifecycle: &app.WorkflowLifecycleView{Status: app.StatusView{Workflow: "wf-1", Runtime: model.RuntimePaused}},
		},
	})
	if m.selected != "wf-1" || m.provider == "claude" || m.discussion.Provider != "" ||
		m.plan.Workflow != "" || m.preview.Workflow != "" || m.pendingDecision != "" {
		t.Fatalf("stale selection state survived recovery: selected=%q provider=%q discussion=%q plan=%+v preview=%+v decision=%q",
			m.selected, m.provider, m.discussion.Provider, m.plan, m.preview, m.pendingDecision)
	}
}

func TestWorkspaceStatusStaysInsideViewport(t *testing.T) {
	m := newModel(Dependencies{})
	m.ready = true
	m.page = PageWorkspace
	m.width = 120
	m.height = 30
	m.status = "workflow created"

	out := render(m)
	if !strings.Contains(out, "status: workflow created") {
		t.Fatalf("workspace status is not rendered:\n%s", out)
	}
	if got := lipgloss.Height(out); got > m.height {
		t.Fatalf("workspace render has %d content rows for a %d-row viewport", got, m.height)
	}
	if strings.Count(out, "status:") != 1 {
		t.Fatalf("workspace status/footer should be a single row, got %q", out)
	}
}

func TestNonWorkspaceStatusPreservesDiagnosticText(t *testing.T) {
	m := newModel(Dependencies{})
	m.ready = true
	m.page = PagePauseExit
	m.width = 80
	m.height = 24
	m.status = "first diagnostic line\nsecond diagnostic line"

	out := render(m)
	if !strings.Contains(out, "status: first diagnostic line second diagnostic line") {
		t.Fatalf("non-Workspace status was not rendered in the persistent footer: %q", out)
	}
}

func TestWorkspaceStatusIsSingleLineAndBounded(t *testing.T) {
	for _, tc := range []struct {
		name   string
		width  int
		height int
		status string
	}{
		{name: "narrow wrapped status", width: 20, height: 2, status: "第一行\n第二行 with a very long suffix"},
		{name: "single row viewport", width: 12, height: 1, status: "状态 with a very long message"},
		{name: "two row viewport", width: 16, height: 2, status: "two\nrows with a long suffix"},
		{name: "three row viewport", width: 18, height: 3, status: "three\nrows with a long suffix"},
		{name: "ansi and cjk", width: 24, height: 4, status: "\033[31m错误\033[0m: 发生了一个很长的问题"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newModel(Dependencies{})
			m.ready = true
			m.page = PageWorkspace
			m.width = tc.width
			m.height = tc.height
			m.status = tc.status
			out := render(m)
			if got := lipgloss.Height(out); got > tc.height {
				t.Fatalf("render has %d rows for %d-row viewport: %q", got, tc.height, out)
			}
			rows := strings.Split(out, "\n")
			for i, row := range rows {
				if got := lipgloss.Width(row); got > tc.width {
					t.Fatalf("row %d width %d > %d: %q", i, got, tc.width, row)
				}
			}
			if tc.height == 1 {
				if !strings.Contains(out, "/") {
					t.Fatalf("minimal Workspace footer disappeared: %q", out)
				}
			} else if !strings.Contains(out, "status:") {
				t.Fatalf("status disappeared: %q", out)
			}
			if strings.Count(out, "status:") > 1 {
				t.Fatalf("status was rendered more than once: %q", out)
			}
		})
	}
}

func TestWorkspaceMinimalViewportKeepsFooterVisible(t *testing.T) {
	for _, tc := range []struct {
		width  int
		height int
	}{
		{width: 20, height: 1},
		{width: 20, height: 2},
	} {
		t.Run(fmt.Sprintf("%dx%d", tc.width, tc.height), func(t *testing.T) {
			m := newModel(Dependencies{})
			m.ready = true
			m.page = PageWorkspace
			m.width = tc.width
			m.height = tc.height
			m.status = "status that must not hide the footer"
			out := render(m)
			if got := lipgloss.Height(out); got > tc.height {
				t.Fatalf("render height %d > %d: %q", got, tc.height, out)
			}
			if !strings.Contains(out, "/") {
				t.Fatalf("minimal Workspace footer is missing: %q", out)
			}
		})
	}
}

func TestWorkspaceRootCompactFooterUsesHomeNavigationHints(t *testing.T) {
	for _, tc := range []struct {
		width  int
		height int
	}{
		{width: 60, height: 18},
		{width: 80, height: 24},
	} {
		t.Run(fmt.Sprintf("%dx%d", tc.width, tc.height), func(t *testing.T) {
			m := newModel(Dependencies{})
			m.ready = true
			m.page = PageWorkspace
			m.width = tc.width
			m.height = tc.height
			m.workspace = longWorkspaceViewModel()
			m.workspace.Actions = []Action{ActionResume, ActionPause, ActionCancel, ActionMigrate}
			m.status = "a long provider/status message that is optional footer detail"
			out := render(m)
			if got := lipgloss.Height(out); got > tc.height {
				t.Fatalf("render height %d > %d: %q", got, tc.height, out)
			}
			for i, line := range strings.Split(out, "\n") {
				if got := lipgloss.Width(line); got > tc.width {
					t.Fatalf("row %d width %d > %d: %q", i, got, tc.width, line)
				}
			}
			for _, want := range []string{"/ command", "status:"} {
				if !strings.Contains(out, want) {
					t.Fatalf("compact root render misses %q: %q", want, out)
				}
			}
			for _, forbidden := range []string{"r resume", "p pause", "x cancel", "m migrate", "n create", "←→ lifecycle"} {
				if strings.Contains(out, forbidden) {
					t.Fatalf("compact root render retains legacy Home hint %q: %q", forbidden, out)
				}
			}
		})
	}
}

// recordingController wraps the shared Application and records every
// typed Command the TUI issues and every EscalateStop call.
type recordingController struct {
	ctrl      controller
	executed  []app.Command
	escalated int
}

type migrationController struct{ executed []app.Command }

type expectedEmptyPreviewController struct{}

func (*expectedEmptyPreviewController) Execute(context.Context, app.Command) (app.Outcome, error) {
	return app.Outcome{Workflow: "wf-1"}, nil
}
func (*expectedEmptyPreviewController) Query(_ context.Context, q app.Query) (app.View, error) {
	switch q := q.(type) {
	case app.ProjectWorkspaceQuery:
		return app.WorkspaceView{
			Selected:  q.Selected,
			Workflows: []app.WorkflowSummary{{ID: "wf-1", Runtime: model.RuntimeRunning, Stage: model.StageSpecGeneration}},
			Lifecycle: &app.WorkflowLifecycleView{Status: app.StatusView{Workflow: "wf-1", Stage: model.StageSpecGeneration}},
		}, nil
	case app.ExecutionPreviewQuery:
		return nil, model.InvalidInputFault("execution inputs are incomplete; no preview is available")
	default:
		return nil, model.InvalidInputFault("unexpected query")
	}
}
func (*expectedEmptyPreviewController) DriveOnce(context.Context, model.WorkflowID) (app.DriveOutcome, error) {
	return app.DriveOutcome{}, nil
}
func (*expectedEmptyPreviewController) EscalateStop() {}

func (m *migrationController) Execute(_ context.Context, cmd app.Command) (app.Outcome, error) {
	m.executed = append(m.executed, cmd)
	return app.Outcome{Workflow: "wf-legacy"}, nil
}
func (m *migrationController) Query(_ context.Context, q app.Query) (app.View, error) {
	switch q.(type) {
	case app.ProjectWorkspaceQuery:
		return app.WorkspaceView{
			Selected: "wf-legacy", Workflows: []app.WorkflowSummary{{ID: "wf-legacy", Runtime: model.RuntimePaused}},
			Lifecycle:    &app.WorkflowLifecycleView{Status: app.StatusView{Workflow: "wf-legacy", LayoutVersion: 1}},
			LegalActions: []app.LegalAction{{Label: "Migrate layout", Hint: "layout-migration"}},
		}, nil
	case app.LayoutMigrationPreviewQuery:
		return app.MigrationPreviewView{Workflow: "wf-legacy", From: 1, To: 2,
			ManifestHash: "manifest-1", Moves: []model.PathMove{{Kind: model.MoveKindArtifact, Source: "/old", Destination: "/new"}}}, nil
	}
	return nil, model.InvalidInputFault("unexpected query")
}
func (*migrationController) DriveOnce(context.Context, model.WorkflowID) (app.DriveOutcome, error) {
	return app.DriveOutcome{}, nil
}
func (*migrationController) EscalateStop() {}

// TestModelMigrationEntryPointsRequirePreviewThenExecute drives the TUI's
// explicit surface: first Enter opens the selected Preview, second Enter
// executes the typed Prepare/Execute command, and y/Y/n/N do not confirm.
func TestModelMigrationEntryPointsRequirePreviewThenExecute(t *testing.T) {
	ctrl := &migrationController{}
	m := newModel(Dependencies{})
	m.ctrl = ctrl
	m = load(t, m)
	m.selected = "wf-legacy"
	m.page = PageWorkflowMenu
	m.workflowMenu = app.WorkflowMenuView{Workflow: "wf-legacy"}
	m.workflowMenuModel = WorkflowMenuModel{Workflow: "wf-legacy", Loaded: true}
	var cmd tea.Cmd
	m, cmd = m.routeWorkflowMenuItem(MenuItem{Action: app.MenuActionMigrate, Label: "Migration"})
	m = runCmds(t, m, cmd)
	if !strings.Contains(render(m), "manifest-1") {
		t.Fatalf("migration preview page not opened:\n%s", render(m))
	}
	m = press(t, m, 'p', 0)
	m = press(t, m, tea.KeyEnter, 0)
	if len(ctrl.executed) != 0 {
		t.Fatalf("first Enter executed Prepare: %v", ctrl.executed)
	}
	m = press(t, m, 'p', 0)
	m = press(t, m, tea.KeyEnter, 0)
	if len(ctrl.executed) != 1 {
		t.Fatalf("second Enter did not prepare: %v", ctrl.executed)
	}
	if _, ok := ctrl.executed[0].(app.PrepareLayoutMigrationCommand); !ok {
		t.Fatalf("Prepare command type = %T", ctrl.executed[0])
	}
	m = press(t, m, 'e', 0)
	m = press(t, m, tea.KeyEnter, 0)
	if len(ctrl.executed) != 1 {
		t.Fatalf("first Enter executed Execute: %v", ctrl.executed)
	}
	m = press(t, m, 'e', 0)
	m = press(t, m, tea.KeyEnter, 0)
	if len(ctrl.executed) != 2 {
		t.Fatalf("second Enter did not execute migration: %v", ctrl.executed)
	}
	if _, ok := ctrl.executed[1].(app.ExecuteLayoutMigrationCommand); !ok {
		t.Fatalf("Execute command type = %T", ctrl.executed[1])
	}
}

// preparedMigrationController returns a complete PREPARED migration
// preview so the render test can assert the full evidence.
type preparedMigrationController struct{}

func (preparedMigrationController) Execute(_ context.Context, cmd app.Command) (app.Outcome, error) {
	return app.Outcome{Workflow: "wf-legacy"}, nil
}
func (preparedMigrationController) Query(_ context.Context, q app.Query) (app.View, error) {
	switch q.(type) {
	case app.ProjectWorkspaceQuery:
		return app.WorkspaceView{
			Selected: "wf-legacy", Workflows: []app.WorkflowSummary{{ID: "wf-legacy", Runtime: model.RuntimePaused}},
			Lifecycle:    &app.WorkflowLifecycleView{Status: app.StatusView{Workflow: "wf-legacy", LayoutVersion: 1}},
			LegalActions: []app.LegalAction{{Label: "Migrate layout", Hint: "layout-migration"}},
		}, nil
	case app.LayoutMigrationPreviewQuery:
		return app.MigrationPreviewView{
			Workflow: "wf-legacy", From: 1, To: 2, Status: "PREPARED",
			MigrationID: "migration-wf-legacy-abc123", ManifestHash: "manifest-1",
			ManifestPath: "/cflow/projects/p/wf-legacy/state/layout-migrations/migration-wf-legacy-abc123.json",
			BackupPath:   "/cflow/projects/p/wf-legacy/state/layout-migrations/migration-wf-legacy-abc123.db.backup",
			BackupHash:   "backup-1", BackupSize: 4096,
			SourceSnapshotHash:      "snapshot-hash-1",
			ExpectedWorkspacePath:   "/cflow/projects/p/wf-legacy/workspace",
			ExpectedWorkspaceBranch: "cflow/wf-legacy/integration",
			ExpectedWorkspaceHead:   "1111111111111111111111111111111111111111",
			Moves: []model.PathMove{
				{Kind: model.MoveKindWorktree, Source: "/cflow/worktrees/p/wf-legacy/integration",
					Destination: "/cflow/projects/p/wf-legacy/workspace",
					Branch:      "cflow/wf-legacy/integration", Head: "1111111111111111111111111111111111111111"},
				{Kind: model.MoveKindArtifact, Source: "/cflow/projects/p/wf-legacy/workflows/wf-legacy/artifacts",
					Destination: "/cflow/projects/p/wf-legacy/artifacts", Digest: "digest-1"},
			},
		}, nil
	}
	return nil, model.InvalidInputFault("unexpected query")
}
func (preparedMigrationController) DriveOnce(context.Context, model.WorkflowID) (app.DriveOutcome, error) {
	return app.DriveOutcome{}, nil
}
func (preparedMigrationController) EscalateStop() {}

// TestModelMigrationRenderShowsCompleteEvidence proves the TUI migration
// page renders the full prepared evidence (finding 6): migration row/
// status, manifest and backup identity, database impact, and per-move
// branch/head/digest.
func TestModelMigrationRenderShowsCompleteEvidence(t *testing.T) {
	ctrl := &preparedMigrationController{}
	m := newModel(Dependencies{})
	m.ctrl = ctrl
	m = load(t, m)
	m.selected = "wf-legacy"
	m.page = PageWorkflowMenu
	m.workflowMenu = app.WorkflowMenuView{Workflow: "wf-legacy"}
	m.workflowMenuModel = WorkflowMenuModel{Workflow: "wf-legacy", Loaded: true}
	var cmd tea.Cmd
	m, cmd = m.routeWorkflowMenuItem(MenuItem{Action: app.MenuActionMigrate, Label: "Migration"})
	m = runCmds(t, m, cmd)
	out := render(m)
	for _, want := range []string{
		"status: PREPARED",
		"migration id: migration-wf-legacy-abc123",
		"manifest path:",
		"backup:",
		"source snapshot: snapshot-hash-1",
		"database impact: workspace=",
		"branch=cflow/wf-legacy/integration",
		"digest=digest-1",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("migration render missing %q:\n%s", want, out)
		}
	}
}

// blockedController returns a BLOCKED workspace whose LegalActions the
// test controls: resumeLegal decides whether the Runtime permits Resume.
type blockedController struct {
	executed    []app.Command
	resumeLegal bool
}

func (c *blockedController) Execute(_ context.Context, cmd app.Command) (app.Outcome, error) {
	c.executed = append(c.executed, cmd)
	return app.Outcome{Workflow: "wf-1"}, nil
}
func (c *blockedController) Query(_ context.Context, q app.Query) (app.View, error) {
	if _, ok := q.(app.ProjectWorkspaceQuery); !ok {
		return nil, model.InvalidInputFault("unexpected query")
	}
	actions := []app.LegalAction{{Label: "Inspect", Hint: "blocked"}}
	if c.resumeLegal {
		actions = append(actions, app.LegalAction{Label: "Resume", Kind: model.ResumeWorkflow})
	}
	return app.WorkspaceView{
		Selected:  "wf-1",
		Workflows: []app.WorkflowSummary{{ID: "wf-1", Runtime: model.RuntimeBlocked}},
		Lifecycle: &app.WorkflowLifecycleView{
			Status:  app.StatusView{Workflow: "wf-1", Stage: model.StageExecution, Runtime: model.RuntimeBlocked},
			Blocked: true,
		},
		LegalActions: actions,
		Health:       app.HealthView{GitAvailable: true, Providers: []app.ProviderHealth{{Name: "fake", Compatible: true}}},
	}, nil
}
func (*blockedController) DriveOnce(context.Context, model.WorkflowID) (app.DriveOutcome, error) {
	return app.DriveOutcome{}, nil
}
func (*blockedController) EscalateStop() {}

// TestModelBlockedPageIssuesNoResumeWithoutLegalAction: the Blocked page
// issues a Resume command ONLY when the Runtime LegalActions include it.
// A blocked workflow whose LegalActions contain NO Resume renders no
// resume key/hint and pressing r issues no Resume command.
func TestModelBlockedPageIssuesNoResumeWithoutLegalAction(t *testing.T) {
	ctrl := &blockedController{}
	m := newModel(Dependencies{})
	m.ctrl = ctrl
	m = load(t, m)
	m.page = PageBlocked
	if got := render(m); strings.Contains(got, "r resume") {
		t.Fatalf("blocked page hard-codes the resume hint:\n%s", got)
	}
	m = press(t, m, 'r', 0)
	if len(ctrl.executed) != 0 {
		t.Fatalf("blocked page without a resume legal action executed %v", ctrl.executed)
	}
}

// TestModelBlockedPageKeepsResumeWhenLegal: when the Runtime LegalActions
// DO contain Resume the Blocked page renders the hint and pressing r
// issues the typed ResumeWorkflowCommand.
func TestModelBlockedPageKeepsResumeWhenLegal(t *testing.T) {
	ctrl := &blockedController{resumeLegal: true}
	m := newModel(Dependencies{})
	m.ctrl = ctrl
	m = load(t, m)
	m.page = PageBlocked
	if got := render(m); !strings.Contains(got, "r resume") {
		t.Fatalf("blocked page lost the runtime resume hint:\n%s", got)
	}
	m = press(t, m, 'r', 0)
	if len(ctrl.executed) != 1 {
		t.Fatalf("blocked page with a resume legal action executed %v", ctrl.executed)
	}
	if _, ok := ctrl.executed[0].(app.ResumeWorkflowCommand); !ok {
		t.Fatalf("blocked resume command type = %T", ctrl.executed[0])
	}
}

// workspaceActionsController returns one workspace whose LegalActions the
// test controls (a PAUSED workflow for the resume tests).
type workspaceActionsController struct {
	executed    []app.Command
	resumeLegal bool
}

func (c *workspaceActionsController) Execute(_ context.Context, cmd app.Command) (app.Outcome, error) {
	c.executed = append(c.executed, cmd)
	return app.Outcome{Workflow: "wf-1"}, nil
}
func (c *workspaceActionsController) Query(_ context.Context, q app.Query) (app.View, error) {
	if _, ok := q.(app.ProjectWorkspaceQuery); !ok {
		return nil, model.InvalidInputFault("unexpected query")
	}
	var actions []app.LegalAction
	if c.resumeLegal {
		actions = append(actions, app.LegalAction{Label: "Resume", Kind: model.ResumeWorkflow})
	}
	return app.WorkspaceView{
		Selected:  "wf-1",
		Workflows: []app.WorkflowSummary{{ID: "wf-1", Runtime: model.RuntimePaused}},
		Lifecycle: &app.WorkflowLifecycleView{
			Status: app.StatusView{Workflow: "wf-1", Stage: model.StageWorkflowGeneration, Runtime: model.RuntimePaused},
		},
		LegalActions: actions,
		Health:       app.HealthView{GitAvailable: true},
	}, nil
}
func (*workspaceActionsController) DriveOnce(context.Context, model.WorkflowID) (app.DriveOutcome, error) {
	return app.DriveOutcome{}, nil
}
func (*workspaceActionsController) EscalateStop() {}

// TestModelHomeResumeShortcutIsInert: Home never executes a mutation,
// including when Resume is a projected legal action. The Workflow Menu owns
// the action-preview route.
func TestModelHomeResumeShortcutIsInert(t *testing.T) {
	ctrl := &workspaceActionsController{}
	m := load(t, testModel(&recordingController{ctrl: ctrl}))
	m = press(t, m, 'r', 0)
	if len(ctrl.executed) != 0 {
		t.Fatalf("Home r executed %v", ctrl.executed)
	}
}

// executionController is the Execution page seam: the workspace projection
// and the DriveOnce result the test controls. resumeErr, when set, makes
// the Runtime reject the Resume command (the stale-projection case: the
// workspace still shows Resume against an already-RUNNING workflow).
type executionController struct {
	executed   []app.Command
	driveCalls int
	actions    []app.LegalAction
	runtime    model.RuntimeStatus
	resumeErr  error
}

func (c *executionController) Execute(_ context.Context, cmd app.Command) (app.Outcome, error) {
	c.executed = append(c.executed, cmd)
	if _, ok := cmd.(app.ResumeWorkflowCommand); ok && c.resumeErr != nil {
		return app.Outcome{}, c.resumeErr
	}
	return app.Outcome{Workflow: "wf-1"}, nil
}
func (c *executionController) Query(_ context.Context, q app.Query) (app.View, error) {
	if _, ok := q.(app.ProjectWorkspaceQuery); !ok {
		return nil, model.InvalidInputFault("unexpected query")
	}
	return app.WorkspaceView{
		Selected:  "wf-1",
		Workflows: []app.WorkflowSummary{{ID: "wf-1", Runtime: c.runtime}},
		Lifecycle: &app.WorkflowLifecycleView{
			Status: app.StatusView{Workflow: "wf-1", Stage: model.StageExecution, Runtime: c.runtime},
		},
		LegalActions: c.actions,
		Health:       app.HealthView{GitAvailable: true},
	}, nil
}
func (c *executionController) DriveOnce(_ context.Context, _ model.WorkflowID) (app.DriveOutcome, error) {
	c.driveCalls++
	return app.DriveOutcome{Kind: app.DriveTerminal, Reason: "terminal"}, nil
}
func (*executionController) EscalateStop() {}

// TestModelExecutionResumeDrivenByLegalActions: the Execution page issues
// the Resume command ONLY when the Runtime LegalActions include it; a
// workflow without the resume legal action starts the Foreground Runner
// directly and never sends ResumeWorkflowCommand.
func TestModelExecutionResumeDrivenByLegalActions(t *testing.T) {
	// A PAUSED workflow whose LegalActions include Resume: r resumes first.
	paused := &executionController{
		actions: []app.LegalAction{{Label: "Resume", Kind: model.ResumeWorkflow}},
		runtime: model.RuntimePaused,
	}
	m := load(t, testModel(&recordingController{ctrl: paused}))
	m.page = PageExecution
	m = press(t, m, 'r', 0)
	if len(paused.executed) != 1 {
		t.Fatalf("execution r with a resume legal action executed %v", paused.executed)
	}
	if _, ok := paused.executed[0].(app.ResumeWorkflowCommand); !ok {
		t.Fatalf("execution resume command type = %T", paused.executed[0])
	}

	// A RUNNING workflow whose LegalActions contain NO Resume: r starts the
	// runner directly and never issues ResumeWorkflowCommand.
	running := &executionController{
		actions: []app.LegalAction{{Label: "Pause", Kind: model.PauseWorkflow}},
		runtime: model.RuntimeRunning,
	}
	m2 := load(t, testModel(&recordingController{ctrl: running}))
	m2.page = PageExecution
	m2 = press(t, m2, 'r', 0)
	if len(running.executed) != 0 {
		t.Fatalf("execution r without a resume legal action executed %v", running.executed)
	}
	if running.driveCalls != 1 {
		t.Fatalf("the runner was not started (drive calls = %d)", running.driveCalls)
	}
}

// TestModelExecutionResumeRejectedStartsRunner covers the stale-projection
// window after an execution approval: the workflow is already RUNNING but
// the workspace projection still renders Resume as a legal action. Pressing
// r issues a ResumeWorkflowCommand the Kernel rejects; the rejected resume
// must clear the pending resume-then-run and fall back to starting the
// Foreground Runner directly (DriveOnce is a safe bounded step over the
// already-running workflow).
func TestModelExecutionResumeRejectedStartsRunner(t *testing.T) {
	ctrl := &executionController{
		actions:   []app.LegalAction{{Label: "Resume", Kind: model.ResumeWorkflow}},
		runtime:   model.RuntimeRunning,
		resumeErr: model.InvalidInputFault("resume rejected: workflow is already running"),
	}
	m := load(t, testModel(&recordingController{ctrl: ctrl}))
	m.page = PageExecution
	m = press(t, m, 'r', 0)
	if len(ctrl.executed) != 1 {
		t.Fatalf("execution r executed %v, want exactly the rejected resume", ctrl.executed)
	}
	if _, ok := ctrl.executed[0].(app.ResumeWorkflowCommand); !ok {
		t.Fatalf("execution resume command type = %T", ctrl.executed[0])
	}
	if ctrl.driveCalls != 1 {
		t.Fatalf("the runner was not started after the rejected resume (drive calls = %d)", ctrl.driveCalls)
	}
	if m.resumeThenRun {
		t.Fatal("resumeThenRun was not cleared after the rejected resume")
	}
}

// TestModelExecutionHintDrivenByLegalActions pins the reloaded-UI signal of
// the E2E: the Execution page hint is driven by the Runtime LegalActions.
// A PAUSED workflow (Resume legal) renders "r resume & run"; once the
// post-approval projection reloads the RUNNING workflow (no Resume legal)
// the hint drops the resume and renders "r start the runner".
func TestModelExecutionHintDrivenByLegalActions(t *testing.T) {
	paused := &executionController{
		actions: []app.LegalAction{{Label: "Resume", Kind: model.ResumeWorkflow}},
		runtime: model.RuntimePaused,
	}
	m := load(t, testModel(&recordingController{ctrl: paused}))
	m.page = PageExecution
	if got := render(m); !strings.Contains(got, "r resume & run") {
		t.Fatalf("paused execution hint lost the resume:\n%s", got)
	}

	running := &executionController{
		actions: []app.LegalAction{{Label: "Pause", Kind: model.PauseWorkflow}},
		runtime: model.RuntimeRunning,
	}
	m2 := load(t, testModel(&recordingController{ctrl: running}))
	m2.page = PageExecution
	got := render(m2)
	if !strings.Contains(got, "r start the runner") {
		t.Fatalf("running execution hint did not drop the resume:\n%s", got)
	}
	if strings.Contains(got, "resume & run") {
		t.Fatalf("running execution hint still offers the resume:\n%s", got)
	}
}

// createController is the Create page seam: it answers the workspace load
// and the DiscoveryQuery with the target Git facts the test controls, and
// records every CreateWorkflowCommand. discoveryErr, when set, makes the
// DiscoveryQuery fail so no target facts ever load.
type createController struct {
	executed     []app.Command
	queries      []app.Query
	dirty        bool
	discoveryErr error
}

func (c *createController) Execute(_ context.Context, cmd app.Command) (app.Outcome, error) {
	c.executed = append(c.executed, cmd)
	return app.Outcome{Workflow: "wf-new"}, nil
}
func (c *createController) Query(_ context.Context, q app.Query) (app.View, error) {
	c.queries = append(c.queries, q)
	switch q.(type) {
	case app.ProjectWorkspaceQuery:
		workspaceQ := q.(app.ProjectWorkspaceQuery)
		view := app.WorkspaceView{
			Project: app.ProjectView{Name: "repo", Root: "/repo"},
			Health:  app.HealthView{GitAvailable: true, Providers: []app.ProviderHealth{{Name: "fake", Compatible: true}}},
		}
		if workspaceQ.Selected == "wf-new" {
			view.Selected = "wf-new"
			view.Workflows = []app.WorkflowSummary{{ID: "wf-new", Name: "calculator", Runtime: model.RuntimePaused}}
		}
		return view, nil
	case app.WorkflowMenuQuery:
		return app.WorkflowMenuView{
			Workflow: "wf-new",
			Name:     "calculator",
			Entries: []app.WorkflowMenuEntry{
				{ID: "current-stage", Group: app.MenuGroupView, Kind: app.MenuEntryReadonly, Label: "Current Stage", Route: app.MenuRouteCurrentStage},
				{ID: "start-discussion", Group: app.MenuGroupContinue, Kind: app.MenuEntryAction, Label: "Start Native Discussion", Action: app.MenuActionStartDiscussion},
			},
			DefaultIndex: 1,
		}, nil
	case app.DiscoveryQuery:
		if c.discoveryErr != nil {
			return nil, c.discoveryErr
		}
		return app.DiscoveryView{
			Branch: "main", Head: "0123456789abcdef",
			Dirty: c.dirty, DirtyFingerprint: "sha256:deadbeef",
			StagedCount: 1, UnstagedCount: 0, UntrackedCount: 1,
		}, nil
	}
	return nil, model.InvalidInputFault("unexpected query")
}
func (*createController) DriveOnce(context.Context, model.WorkflowID) (app.DriveOutcome, error) {
	return app.DriveOutcome{}, nil
}
func (*createController) EscalateStop() {}

// createPage opens the Create page and types the workflow name.
func createPage(t *testing.T, m Model, name string) Model {
	t.Helper()
	m = press(t, m, tea.KeyEnter, 0)
	return typeText(t, m, name)
}

// typeText types one string through the Model as individual text key
// presses (the create name and handoff inputs use KeyPressMsg.Text).
func typeText(t *testing.T, m Model, s string) Model {
	t.Helper()
	for _, r := range s {
		m = step(t, m, tea.KeyPressMsg{Code: r, Text: string(r), Mod: 0})
	}
	return m
}

func TestCreateNameEnterOpensPreviewButDoesNotCreate(t *testing.T) {
	ctrl := &createController{}
	m := newModel(Dependencies{})
	m.ctrl = ctrl
	m.page = PageCreate
	m.createInput = "calculator"
	m.createDirty = &app.DiscoveryView{Branch: "main", Head: "abc123"}

	m = press(t, m, tea.KeyEnter, 0)
	if !m.createConfirm {
		t.Fatal("Enter did not open Create Preview")
	}
	if len(ctrl.executed) != 0 {
		t.Fatalf("name Enter executed commands: %v", ctrl.executed)
	}
}

func TestCreatePreviewEnterExecutes(t *testing.T) {
	ctrl := &createController{dirty: true}
	m := newModel(Dependencies{})
	m.ctrl = ctrl
	m.page = PageCreate
	m.createInput = "calculator"
	m.createDirty = &app.DiscoveryView{
		Branch:           "main",
		Head:             "abc123",
		Dirty:            true,
		DirtyFingerprint: "sha256:dirty",
		StagedCount:      1,
	}
	m.createConfirm = true
	m.ready, m.width, m.height = true, 120, 40

	if got := render(m); !strings.Contains(got, "DIRTY") || !strings.Contains(got, "sha256:dirty") {
		t.Fatalf("preview lost dirty target facts:\n%s", got)
	}
	m = press(t, m, tea.KeyEnter, 0)
	if len(ctrl.executed) != 1 {
		t.Fatalf("preview Enter did not create: %v", ctrl.executed)
	}
	cc, ok := ctrl.executed[0].(app.CreateWorkflowCommand)
	if !ok {
		t.Fatalf("create command type = %T", ctrl.executed[0])
	}
	if cc.Name != "calculator" || !cc.ConfirmDirty {
		t.Fatalf("create = %+v, want Name calculator and ConfirmDirty:true", cc)
	}
}

func TestCreatePreviewCleanDiscoveryUsesFalseConfirmDirty(t *testing.T) {
	ctrl := &createController{dirty: false}
	m := newModel(Dependencies{})
	m.ctrl = ctrl
	m = load(t, m)
	m = createPage(t, m, "calculator")

	m = press(t, m, tea.KeyEnter, 0)
	if !m.createConfirm {
		t.Fatal("name Enter did not open Create Preview")
	}
	m = press(t, m, tea.KeyEnter, 0)

	if len(ctrl.executed) != 1 {
		t.Fatalf("clean preview Enter executed %d commands: %v", len(ctrl.executed), ctrl.executed)
	}
	cc, ok := ctrl.executed[0].(app.CreateWorkflowCommand)
	if !ok {
		t.Fatalf("create command type = %T", ctrl.executed[0])
	}
	if cc.ConfirmDirty {
		t.Fatalf("clean DiscoveryView created with ConfirmDirty=true: %+v", cc)
	}
}

func TestCreatePreviewMissingDiscoveryFactsFailsClosed(t *testing.T) {
	ctrl := &createController{discoveryErr: model.InvalidInputFault("discovery failed")}
	m := newModel(Dependencies{})
	m.ctrl = ctrl
	m = load(t, m)
	m = createPage(t, m, "calculator")

	m = press(t, m, tea.KeyEnter, 0)
	if !m.createConfirm {
		t.Fatal("name Enter did not open Create Preview")
	}
	if m.createDirty != nil {
		t.Fatalf("unavailable Discovery facts unexpectedly loaded: %+v", m.createDirty)
	}
	m = press(t, m, tea.KeyEnter, 0)

	if len(ctrl.executed) != 0 {
		t.Fatalf("missing Discovery facts issued CreateWorkflowCommand: %v", ctrl.executed)
	}
	if !strings.Contains(m.status, "target facts unavailable") {
		t.Fatalf("missing Discovery facts status = %q", m.status)
	}
}

func TestCreatePreviewYNAQDoNotConfirm(t *testing.T) {
	ctrl := &createController{}
	m := newModel(Dependencies{})
	m.ctrl = ctrl
	m.page = PageCreate
	m.createInput = "calculator"
	m.createDirty = &app.DiscoveryView{Branch: "main", Head: "abc123"}
	m.createConfirm = true
	m.ready, m.width, m.height = true, 120, 40
	if got := render(m); strings.Contains(got, "y create") || strings.Contains(got, "[y/N]") || strings.Contains(got, "default no") {
		t.Fatalf("preview still advertises y/n confirmation:\n%s", got)
	}
	for _, key := range []rune{'y', 'Y', 'n', 'N', 'q'} {
		m = press(t, m, key, 0)
		if len(ctrl.executed) != 0 || !m.createConfirm {
			t.Fatalf("%q confirmed creation: executed=%v confirm=%v", key, ctrl.executed, m.createConfirm)
		}
	}
}

func TestCreatePreviewEscReturnsToEditing(t *testing.T) {
	ctrl := &createController{}
	m := newModel(Dependencies{})
	m.ctrl = ctrl
	m.page = PageCreate
	m.createInput = "calculator"
	m.createDirty = &app.DiscoveryView{Branch: "main", Head: "abc123"}
	m.createConfirm = true

	m = press(t, m, tea.KeyEsc, 0)
	if m.createConfirm || m.createInput != "calculator" {
		t.Fatalf("preview Esc did not restore editing: confirm=%v input=%q", m.createConfirm, m.createInput)
	}
}

func TestWorkflowMenuAfterCreateAcknowledgement(t *testing.T) {
	ctrl := &createController{}
	m := newModel(Dependencies{})
	m.ctrl = ctrl
	m.page = PageCreate
	m.navigation = NavigationStack{Frames: []NavigationFrame{{Layer: LayerHome, Page: PageWorkspace}, {Layer: LayerCreateWorkspace, Page: PageCreate}}}
	m.createInput = "calculator"
	m.createDirty = &app.DiscoveryView{Branch: "main", Head: "abc123"}
	m.createConfirm = true

	m = press(t, m, tea.KeyEnter, 0)
	if m.selected != "wf-new" || m.page != PageWorkflowMenu {
		t.Fatalf("create acknowledgement route = selected=%q page=%v", m.selected, m.page)
	}
	if got := m.navigation.Current(); got.Layer != LayerWorkflowMenu || got.Page != PageWorkflowMenu {
		t.Fatalf("create acknowledgement navigation = %+v", got)
	}
	if m.workflowMenuIndex != 1 || m.workflowMenuModel.Selected != 1 {
		t.Fatalf("menu did not use Application default index: index=%d model=%+v", m.workflowMenuIndex, m.workflowMenuModel)
	}
	workspaceQuery := false
	for _, q := range ctrl.queries {
		if workspace, ok := q.(app.ProjectWorkspaceQuery); ok && workspace.Selected == "wf-new" {
			workspaceQuery = true
		}
		if menu, ok := q.(app.WorkflowMenuQuery); ok && menu.Workflow == "wf-new" {
			if !workspaceQuery {
				t.Fatal("workflow menu query ran before bound workspace acknowledgement")
			}
			return
		}
	}
	t.Fatal("create acknowledgement did not query the bound Workflow Menu")
}

func (r *recordingController) Execute(ctx context.Context, cmd app.Command) (app.Outcome, error) {
	r.executed = append(r.executed, cmd)
	return r.ctrl.Execute(ctx, cmd)
}

func (r *recordingController) Query(ctx context.Context, q app.Query) (app.View, error) {
	return r.ctrl.Query(ctx, q)
}

func (r *recordingController) DriveOnce(ctx context.Context, wf model.WorkflowID) (app.DriveOutcome, error) {
	return r.ctrl.DriveOnce(ctx, wf)
}

func (r *recordingController) EscalateStop() {
	r.escalated++
	r.ctrl.EscalateStop()
}

// hasExecuted reports whether the controller executed a command of the
// given type.
func (r *recordingController) hasExecuted(anyCommand any) bool {
	for _, c := range r.executed {
		switch anyCommand.(type) {
		case app.ResumeWorkflowCommand:
			if _, ok := c.(app.ResumeWorkflowCommand); ok {
				return true
			}
		case app.PauseWorkflowCommand:
			if _, ok := c.(app.PauseWorkflowCommand); ok {
				return true
			}
		case app.CancelWorkflowCommand:
			if _, ok := c.(app.CancelWorkflowCommand); ok {
				return true
			}
		case app.ApprovePlanCommand:
			if _, ok := c.(app.ApprovePlanCommand); ok {
				return true
			}
		case app.ApproveExecutionCommand:
			if _, ok := c.(app.ApproveExecutionCommand); ok {
				return true
			}
		case app.GeneratePlanCommand:
			if _, ok := c.(app.GeneratePlanCommand); ok {
				return true
			}
		case app.CheckPlanCommand:
			if _, ok := c.(app.CheckPlanCommand); ok {
				return true
			}
		case app.GenerateSpecsCommand:
			if _, ok := c.(app.GenerateSpecsCommand); ok {
				return true
			}
		case app.CompileWorkflowCommand:
			if _, ok := c.(app.CompileWorkflowCommand); ok {
				return true
			}
		case app.ExecutionDryRunCommand:
			if _, ok := c.(app.ExecutionDryRunCommand); ok {
				return true
			}
		case app.AdoptWorkspaceCommand:
			if _, ok := c.(app.AdoptWorkspaceCommand); ok {
				return true
			}
		case app.PrepareApplyCommand:
			if _, ok := c.(app.PrepareApplyCommand); ok {
				return true
			}
		case app.ExecuteApplyCommand:
			if _, ok := c.(app.ExecuteApplyCommand); ok {
				return true
			}
		case app.DryRunCommand:
			if _, ok := c.(app.DryRunCommand); ok {
				return true
			}
		case app.ExecuteCleanupCommand:
			if _, ok := c.(app.ExecuteCleanupCommand); ok {
				return true
			}
		case app.FinishDiscussionCommand:
			if _, ok := c.(app.FinishDiscussionCommand); ok {
				return true
			}
		case app.CreateWorkflowCommand:
			if _, ok := c.(app.CreateWorkflowCommand); ok {
				return true
			}
		}
	}
	return false
}

// testModel builds the root Model over the recording controller.
func testModel(rec *recordingController) Model {
	m := newModel(Dependencies{})
	m.ctrl = rec
	return m
}

// step processes one message, then runs every returned command to its
// message and feeds the results back through the Model until nothing is
// pending.
func step(t *testing.T, m Model, msg tea.Msg) Model {
	t.Helper()
	upd, cmd := m.Update(msg)
	return runCmds(t, upd.(Model), cmd)
}

func runCmds(t *testing.T, m Model, cmd tea.Cmd) Model {
	t.Helper()
	if cmd == nil {
		return m
	}
	msg := cmd()
	switch batch := msg.(type) {
	case tea.BatchMsg:
		for _, c := range batch {
			m = runCmds(t, m, c)
		}
		return m
	default:
		return step(t, m, msg)
	}
}

// load drives the Init command (the read-only workspace load) on a
// sized terminal.
func load(t *testing.T, m Model) Model {
	t.Helper()
	m = step(t, m, tea.WindowSizeMsg{Width: 120, Height: 40})
	return runCmds(t, m, m.Init())
}

// press is one key press through the Model.
func press(t *testing.T, m Model, code rune, mod tea.KeyMod) Model {
	t.Helper()
	return step(t, m, tea.KeyPressMsg{Code: code, Mod: mod})
}

// TestModelLoadsRealWorkspaceView is the root-model failure test: the
// Model queries the shared Application and renders the real Workspace
// View; opening the TUI never resumes, dispatches, applies, or cleans
// up.
func TestModelLoadsRealWorkspaceView(t *testing.T) {
	fx := newTUIFixture(t)
	ref := &appRef{fx: fx}
	ctx := context.Background()
	a, err := ref.open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	wf, err := a.Execute(ctx, app.CreateWorkflowCommand{Name: "calculator", Provider: "fake", ConfirmDirty: true})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	rec := &recordingController{ctrl: a}
	m := load(t, testModel(rec))

	if m.workspace.Project.Name != "repo" {
		t.Fatalf("project = %+v", m.workspace.Project)
	}
	if len(m.workspace.Workflows) != 1 || m.workspace.Workflows[0].ID != wf.Workflow {
		t.Fatalf("workflows = %+v", m.workspace.Workflows)
	}
	got := render(m)
	for _, want := range []string{"project:", "workflows:", "workflow calculator (" + string(wf.Workflow) + ")", "health:"} {
		if !strings.Contains(got, want) {
			t.Fatalf("workspace render misses %q:\n%s", want, got)
		}
	}
	// The read-only load never started the lifecycle: no Resume/Pause/
	// Dispatch/Apply/Cleanup command ran and the workflow state is
	// unchanged.
	if rec.hasExecuted(app.ResumeWorkflowCommand{}) || rec.hasExecuted(app.PauseWorkflowCommand{}) ||
		rec.hasExecuted(app.PrepareApplyCommand{}) || rec.hasExecuted(app.DryRunCommand{}) {
		t.Fatalf("the workspace load executed mutation commands: %v", rec.executed)
	}
	view, err := a.Query(ctx, app.StatusQuery{Workflow: wf.Workflow})
	if err != nil {
		t.Fatal(err)
	}
	if st := view.(app.StatusView); st.Stage != model.StageRequirementDiscussion || st.Runtime != model.RuntimePending {
		t.Fatalf("workflow after the TUI load = %s/%s, want REQUIREMENT_DISCUSSION/PENDING", st.Stage, st.Runtime)
	}
}

// TestModelHomeLifecycleNavigationIsInert: Home keys never cross lifecycle
// stages; stage routes are entered from Workflow Menu.
func TestModelNavigationReachesLifecyclePages(t *testing.T) {
	fx := newTUIFixture(t)
	ref := &appRef{fx: fx}
	ctx := context.Background()
	a, err := ref.open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Execute(ctx, app.CreateWorkflowCommand{Name: "calculator", Provider: "fake", ConfirmDirty: true}); err != nil {
		t.Fatal(err)
	}
	rec := &recordingController{ctrl: a}
	m := load(t, testModel(rec))
	if m.page != PageWorkspace {
		t.Fatalf("initial page = %d", m.page)
	}
	for _, key := range []rune{tea.KeyTab, tea.KeyLeft, tea.KeyRight} {
		m = press(t, m, key, 0)
		if m.page != PageWorkspace || m.navigation.Current().Layer != LayerHome {
			t.Fatalf("Home key %q crossed lifecycle route: page=%v frame=%+v", key, m.page, m.navigation.Current())
		}
	}
	// Navigation never executed a mutation.
	for _, c := range rec.executed {
		switch c.(type) {
		case app.ResumeWorkflowCommand, app.PauseWorkflowCommand, app.CancelWorkflowCommand,
			app.ApprovePlanCommand, app.ApproveExecutionCommand:
			t.Fatalf("navigation executed %T", c)
		}
	}
}

// TestModelWorkspaceSelectionIsReadOnly: up/down only changes the
// selected workflow (pure UI state); no command is executed.
func TestModelWorkspaceSelectionIsReadOnly(t *testing.T) {
	fx := newTUIFixture(t)
	ref := &appRef{fx: fx}
	ctx := context.Background()
	a, err := ref.open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	first, err := a.Execute(ctx, app.CreateWorkflowCommand{Name: "one", Provider: "fake", ConfirmDirty: true})
	if err != nil {
		t.Fatal(err)
	}
	second, err := a.Execute(ctx, app.CreateWorkflowCommand{Name: "two", Provider: "fake", ConfirmDirty: true})
	if err != nil {
		t.Fatal(err)
	}
	rec := &recordingController{ctrl: a}
	m := load(t, testModel(rec))
	if m.selected != first.Workflow {
		t.Fatalf("initial selection = %s, want %s", m.selected, first.Workflow)
	}
	m = press(t, m, tea.KeyDown, 0)
	if m.selected != second.Workflow {
		t.Fatalf("selection after down = %s, want %s", m.selected, second.Workflow)
	}
	if len(rec.executed) != 0 {
		t.Fatalf("selection executed commands: %v", rec.executed)
	}
}

func TestModelHomeRowsSelectNewWithoutQueryOrMutation(t *testing.T) {
	ctrl := &homeRowsController{}
	m := newModel(Dependencies{})
	m.ctrl = ctrl
	m.selected = "wf-1"
	m.workspace = MapWorkspace(app.WorkspaceView{
		Selected:  "wf-1",
		Workflows: []app.WorkflowSummary{{ID: "wf-1", Name: "one", Runtime: model.RuntimePaused}},
		Lifecycle: &app.WorkflowLifecycleView{Status: app.StatusView{Workflow: "wf-1", Runtime: model.RuntimePaused}},
	})

	next, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	got := next.(Model)
	if cmd != nil {
		t.Fatal("selecting the UI-only New Workflow row queried the Application")
	}
	if got.selected != "" || got.workspace.Selected.ID != "" || got.workspace.Lifecycle != nil || len(got.workspace.Actions) != 0 {
		t.Fatalf("New Workflow selection retained runtime facts: selected=%q workspace=%+v", got.selected, got.workspace)
	}
	if ctrl.executes != 0 || len(ctrl.queries) != 0 {
		t.Fatalf("New Workflow selection touched Application: executes=%d queries=%v", ctrl.executes, ctrl.queries)
	}
}

func TestModelHomeRowsEnterRoutesByRowKind(t *testing.T) {
	ctrl := &homeRowsController{}
	m := newModel(Dependencies{})
	m.ctrl = ctrl
	m.workspace = MapWorkspace(app.WorkspaceView{
		Workflows: []app.WorkflowSummary{{ID: "wf-1", Name: "one", Runtime: model.RuntimePaused}},
	})

	next, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	got := next.(Model)
	if got.page != PageCreate || got.navigation.Current().Layer != LayerCreateWorkspace {
		t.Fatalf("New Workflow Enter = page %v frame %+v", got.page, got.navigation.Current())
	}
	if cmd == nil {
		t.Fatal("New Workflow Enter did not request the read-only discovery projection")
	}
	if ctrl.executes != 0 {
		t.Fatalf("New Workflow Enter executed %d mutations", ctrl.executes)
	}

	m = newModel(Dependencies{})
	m.ctrl = ctrl
	m.selected = "wf-1"
	m.workspace = MapWorkspace(app.WorkspaceView{
		Selected:  "wf-1",
		Workflows: []app.WorkflowSummary{{ID: "wf-1", Name: "one", Runtime: model.RuntimePaused}},
	})
	next, cmd = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	got = next.(Model)
	if cmd == nil || got.page != PageWorkflowMenu || got.navigation.Current().Layer != LayerWorkflowMenu || got.navigation.Current().Workflow != "wf-1" {
		t.Fatalf("existing Workflow Enter = page %v frame %+v cmd=%v", got.page, got.navigation.Current(), cmd != nil)
	}
	if ctrl.executes != 0 {
		t.Fatalf("existing Workflow Enter executed %d mutations", ctrl.executes)
	}
}

func TestModelHomeRowsReloadOnlyExistingWorkflowSelection(t *testing.T) {
	ctrl := &homeRowsController{}
	m := newModel(Dependencies{})
	m.ctrl = ctrl
	m.workspace = MapWorkspace(app.WorkspaceView{
		Workflows: []app.WorkflowSummary{{ID: "wf-1", Name: "one", Runtime: model.RuntimePaused}},
	})

	m = press(t, m, tea.KeyDown, 0)
	if m.selected != "wf-1" || m.workspace.Selected.ID != "wf-1" {
		t.Fatalf("existing row selection = selected %q workspace %+v", m.selected, m.workspace.Selected)
	}
	if len(ctrl.queries) != 1 {
		t.Fatalf("existing row selection queries = %v, want one", ctrl.queries)
	}
	q, ok := ctrl.queries[0].(app.ProjectWorkspaceQuery)
	if !ok || q.Selected != "wf-1" {
		t.Fatalf("existing row query = %#v", ctrl.queries[0])
	}
	if ctrl.executes != 0 {
		t.Fatalf("existing row selection executed %d mutations", ctrl.executes)
	}
}

func TestModelHomeNDoesNotOpenCreateWorkspace(t *testing.T) {
	m := newModel(Dependencies{})
	next, cmd := m.Update(tea.KeyPressMsg{Code: 'n'})
	got := next.(Model)
	if cmd != nil || got.page != PageWorkspace || got.navigation.Current().Layer != LayerHome {
		t.Fatalf("n changed Home route: page=%v frame=%+v cmd=%v", got.page, got.navigation.Current(), cmd != nil)
	}
}

func TestModelHomeLeftRightAreInert(t *testing.T) {
	ctrl := &homeRowsController{}
	m := newModel(Dependencies{})
	m.ctrl = ctrl
	m.selected = "wf-1"
	m.workspace = MapWorkspace(app.WorkspaceView{
		Selected:  "wf-1",
		Workflows: []app.WorkflowSummary{{ID: "wf-1", Name: "one", Runtime: model.RuntimePaused}},
	})

	for _, key := range []rune{tea.KeyLeft, tea.KeyRight} {
		next, cmd := m.Update(tea.KeyPressMsg{Code: key})
		got := next.(Model)
		if cmd != nil || got.page != PageWorkspace || got.navigation.Current().Page != PageWorkspace {
			t.Fatalf("Home key %q changed navigation: page=%v frame=%+v command=%v", key, got.page, got.navigation.Current(), cmd != nil)
		}
		if got.selected != m.selected {
			t.Fatalf("Home key %q changed selected workflow: got=%q want=%q", key, got.selected, m.selected)
		}
	}
	if len(ctrl.queries) != 0 || ctrl.executes != 0 {
		t.Fatalf("Home left/right touched Application: queries=%v executes=%d", ctrl.queries, ctrl.executes)
	}
}

func TestModelHomeTabDoesNotEnterLifecycleStages(t *testing.T) {
	ctrl := &homeRowsController{}
	m := newModel(Dependencies{})
	m.ctrl = ctrl
	m.selected = "wf-1"
	m.workspace = MapWorkspace(app.WorkspaceView{
		Selected:  "wf-1",
		Workflows: []app.WorkflowSummary{{ID: "wf-1", Name: "one", Runtime: model.RuntimePaused}},
	})

	next, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	got := next.(Model)
	if cmd != nil || got.page != PageWorkspace || got.navigation.Current().Layer != LayerHome {
		t.Fatalf("Home Tab changed lifecycle route: page=%v frame=%+v command=%v", got.page, got.navigation.Current(), cmd != nil)
	}
}

func TestModelHomeMutationShortcutsAreInert(t *testing.T) {
	ctrl := &homeRowsController{}
	m := newModel(Dependencies{})
	m.ctrl = ctrl
	m.selected = "wf-1"
	m.workspace = MapWorkspace(app.WorkspaceView{
		Selected:  "wf-1",
		Workflows: []app.WorkflowSummary{{ID: "wf-1", Name: "one", Runtime: model.RuntimeRunning}},
		Lifecycle: &app.WorkflowLifecycleView{Status: app.StatusView{Workflow: "wf-1", Runtime: model.RuntimeRunning}},
		LegalActions: []app.LegalAction{
			{Kind: model.PauseWorkflow},
			{Kind: model.ResumeWorkflow},
			{Kind: model.CancelWorkflow},
		},
	})

	for _, key := range []rune{'r', 'R', 'p', 'P', 'x', 'X', 'm', 'M'} {
		next, cmd := m.Update(tea.KeyPressMsg{Code: key, Text: string(key)})
		got := next.(Model)
		if cmd != nil || got.page != PageWorkspace || got.navigation.Current().Layer != LayerHome {
			t.Fatalf("Home shortcut %q changed route: page=%v frame=%+v cmd=%v", key, got.page, got.navigation.Current(), cmd != nil)
		}
		if got.selected != m.selected {
			t.Fatalf("Home shortcut %q changed selection: got=%q want=%q", key, got.selected, m.selected)
		}
	}
	if len(ctrl.queries) != 0 || ctrl.executes != 0 {
		t.Fatalf("Home mutation shortcuts touched Application: queries=%v executes=%d", ctrl.queries, ctrl.executes)
	}
}

func TestStageWorkspaceEscReturnsToActualParent(t *testing.T) {
	pages := []Page{PageDiscussion, PagePlanApproval, PageExecutionApproval, PageExecution, PageBlocked, PageTerminal, PageCancel, PageMigration}
	for _, page := range pages {
		t.Run(pageName(page), func(t *testing.T) {
			m := newModel(Dependencies{})
			m.page = page
			m.selected = "wf-1"
			m.navigation = NavigationStack{Frames: []NavigationFrame{
				{Layer: LayerHome, Page: PageWorkspace},
				{Layer: LayerWorkflowMenu, Page: PageWorkflowMenu, Workflow: "wf-1", Index: 2},
				{Layer: LayerStageWorkspace, Page: page, Workflow: "wf-1"},
			}}

			next, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
			got := next.(Model)
			if cmd != nil || got.page != PageWorkflowMenu || got.navigation.Current().Layer != LayerWorkflowMenu {
				t.Fatalf("Esc parent = page=%v frame=%+v command=%v", got.page, got.navigation.Current(), cmd != nil)
			}
		})
	}
}

func TestCancelRequiresPreviewThenEnterAndIgnoresYN(t *testing.T) {
	ctrl := &homeRowsController{}
	m := newModel(Dependencies{})
	m.ctrl = ctrl
	m.selected = "wf-1"
	m.page = PageCancel
	m.navigation = NavigationStack{Frames: []NavigationFrame{
		{Layer: LayerHome, Page: PageWorkspace},
		{Layer: LayerWorkflowMenu, Page: PageWorkflowMenu, Workflow: "wf-1"},
		{Layer: LayerStageWorkspace, Page: PageCancel, Workflow: "wf-1"},
	}}

	for _, key := range []rune{'y', 'Y', 'n', 'N'} {
		m = press(t, m, key, 0)
		if ctrl.executes != 0 || m.page != PageCancel {
			t.Fatalf("%q mutated or left cancel preview: page=%v executions=%d", key, m.page, ctrl.executes)
		}
	}
	m = press(t, m, tea.KeyEnter, 0)
	if ctrl.executes != 0 || m.page != PageCancel {
		t.Fatalf("first Enter did not stay in cancel preview: page=%v executions=%d", m.page, ctrl.executes)
	}
	m = press(t, m, tea.KeyEnter, 0)
	if ctrl.executes != 1 {
		t.Fatalf("second Enter did not execute cancel: executions=%d", ctrl.executes)
	}
}

func TestTerminalPreviewDoesNotCarryAcrossSections(t *testing.T) {
	ctrl := &homeRowsController{}
	m := newModel(Dependencies{})
	m.ctrl = ctrl
	m.selected = "wf-1"
	m.page = PageTerminal
	m.terminal.ApplyPreview = "apply preview"
	m.navigation = NavigationStack{Frames: []NavigationFrame{
		{Layer: LayerHome, Page: PageWorkspace},
		{Layer: LayerWorkflowMenu, Page: PageWorkflowMenu, Workflow: "wf-1"},
		{Layer: LayerStageWorkspace, Page: PageTerminal, Workflow: "wf-1"},
	}}

	m = press(t, m, tea.KeyEnter, 0)
	m = press(t, m, tea.KeyRight, 0)
	m = press(t, m, tea.KeyEnter, 0)
	if ctrl.executes != 0 {
		t.Fatalf("Apply executed without a current-section preview: %d", ctrl.executes)
	}
}

func TestModelCreateWorkspaceEscPopsNavigationHome(t *testing.T) {
	ctrl := &homeRowsController{}
	m := newModel(Dependencies{})
	m.ctrl = ctrl
	m.workspace = MapWorkspace(app.WorkspaceView{})

	m = press(t, m, tea.KeyEnter, 0)
	if m.page != PageCreate || m.navigation.Current().Layer != LayerCreateWorkspace {
		t.Fatalf("New Workflow Enter = page %v frame %+v", m.page, m.navigation.Current())
	}

	m = press(t, m, tea.KeyEsc, 0)
	if m.page != PageWorkspace || len(m.navigation.Frames) != 1 {
		t.Fatalf("Create Esc did not pop to Home: page=%v stack=%+v", m.page, m.navigation)
	}
	if frame := m.navigation.Current(); frame.Layer != LayerHome || frame.Page != PageWorkspace {
		t.Fatalf("Create Esc restored wrong Home frame: %+v", frame)
	}
}

func TestModelCreatePreviewEscReturnsToEditing(t *testing.T) {
	ctrl := &createController{dirty: false}
	m := newModel(Dependencies{})
	m.ctrl = ctrl
	m = load(t, m)
	m = createPage(t, m, "calculator")

	m = press(t, m, tea.KeyEnter, 0)
	if m.page != PageCreate || !m.createConfirm || m.createInput != "calculator" {
		t.Fatalf("Create name Enter = page %v confirm=%v input=%q", m.page, m.createConfirm, m.createInput)
	}

	m = press(t, m, tea.KeyEsc, 0)
	if m.page != PageCreate || m.navigation.Current().Layer != LayerCreateWorkspace {
		t.Fatalf("Create Preview Esc left Create: page=%v frame=%+v", m.page, m.navigation.Current())
	}
	if m.createConfirm || m.createInput != "calculator" {
		t.Fatalf("Create Preview Esc did not restore editing: confirm=%v input=%q", m.createConfirm, m.createInput)
	}
}

func TestModelHomeExistingRowHighlightUpdatesBeforeProjection(t *testing.T) {
	m := newModel(Dependencies{})
	m.selected = "wf-1"
	m.workspace = MapWorkspace(app.WorkspaceView{
		Selected: "wf-1",
		Workflows: []app.WorkflowSummary{
			{ID: "wf-1", Name: "one", Runtime: model.RuntimePaused},
			{ID: "wf-2", Name: "two", Runtime: model.RuntimeRunning},
		},
		Lifecycle: &app.WorkflowLifecycleView{Status: app.StatusView{Workflow: "wf-1", Runtime: model.RuntimePaused}},
	})
	previousFacts := m.workspace.Selected

	next, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	got := next.(Model)
	if cmd == nil {
		t.Fatal("existing row selection did not request the read-only projection")
	}
	if got.workspace.Selected.ID != previousFacts.ID || got.workspace.Lifecycle == nil || got.workspace.Lifecycle.ID != previousFacts.ID {
		t.Fatalf("selection invented or discarded projection facts: selected=%+v lifecycle=%+v", got.workspace.Selected, got.workspace.Lifecycle)
	}
	if got.workspace.SelectedRow != 2 {
		t.Fatalf("selected row = %d, want row 2 (New, wf-1, wf-2)", got.workspace.SelectedRow)
	}
	frame := visibleTerminalText(RenderWorkspace(got.workspace, 120, 45))
	if !strings.Contains(frame, "▸ two (wf-2)") || strings.Contains(frame, "▸ one (wf-1)") {
		t.Fatalf("Home row highlight did not update immediately:\n%s", frame)
	}
}

type homeRowsController struct {
	executes int
	queries  []app.Query
}

func (c *homeRowsController) Execute(context.Context, app.Command) (app.Outcome, error) {
	c.executes++
	return app.Outcome{}, nil
}

func (c *homeRowsController) Query(_ context.Context, q app.Query) (app.View, error) {
	c.queries = append(c.queries, q)
	switch q := q.(type) {
	case app.ProjectWorkspaceQuery:
		return app.WorkspaceView{
			Selected:  q.Selected,
			Workflows: []app.WorkflowSummary{{ID: "wf-1", Name: "one", Runtime: model.RuntimePaused}},
			Lifecycle: &app.WorkflowLifecycleView{Status: app.StatusView{Workflow: q.Selected, Runtime: model.RuntimePaused}},
		}, nil
	case app.DiscoveryQuery:
		return app.DiscoveryView{}, nil
	default:
		return nil, model.InvalidInputFault("unexpected query")
	}
}

func (*homeRowsController) DriveOnce(context.Context, model.WorkflowID) (app.DriveOutcome, error) {
	return app.DriveOutcome{}, nil
}

func (*homeRowsController) EscalateStop() {}

// TestModelActionsMapToTypedCommands: the workspace legal actions map to
// the exact typed Application Commands.
func TestModelActionsMapToTypedCommands(t *testing.T) {
	for _, tc := range []struct {
		name   string
		action app.MenuAction
		want   app.Command
	}{
		{name: "resume", action: app.MenuActionResume, want: app.ResumeWorkflowCommand{Workflow: "wf-1"}},
		{name: "pause", action: app.MenuActionPause, want: app.PauseWorkflowCommand{Workflow: "wf-1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recordingController{ctrl: &migrationController{}}
			m := newModel(Dependencies{})
			m.ctrl = rec
			m.selected = "wf-1"
			m.page = PageWorkflowMenu
			m.workflowMenu = app.WorkflowMenuView{
				Workflow: "wf-1",
				Entries: []app.WorkflowMenuEntry{{
					ID: tc.name, Kind: app.MenuEntryAction, Action: tc.action,
				}},
			}
			m.workflowMenuModel = MapWorkflowMenu(m.workflowMenu)
			m.navigation = NavigationStack{Frames: []NavigationFrame{
				{Layer: LayerHome, Page: PageWorkspace},
				{Layer: LayerWorkflowMenu, Page: PageWorkflowMenu, Workflow: "wf-1"},
			}}

			m = press(t, m, tea.KeyEnter, 0)
			if m.page != PageActionPreview || len(rec.executed) != 0 {
				t.Fatalf("menu Enter did not open action preview: page=%v executes=%v", m.page, rec.executed)
			}
			m = press(t, m, tea.KeyEnter, 0)
			if !m.actionPreviewed || len(rec.executed) != 0 {
				t.Fatalf("first preview Enter executed command: previewed=%v executes=%v", m.actionPreviewed, rec.executed)
			}
			m = press(t, m, tea.KeyEnter, 0)
			if len(rec.executed) != 1 || !reflect.DeepEqual(rec.executed[0], tc.want) {
				t.Fatalf("second preview Enter = %#v, want %#v", rec.executed, tc.want)
			}
		})
	}
}

// TestModelPlanApprovalMapsToTypedCommand: 'g' generates the plan, 'k'
// checks it, and the explicit confirmation issues ApprovePlanCommand
// with the exact revision and hash.
func TestModelPlanApprovalMapsToTypedCommand(t *testing.T) {
	fx := newTUIFixture(t)
	ref := &appRef{fx: fx, scripts: []string{
		planScript(fx.next("p")), checkPlanPassScript(fx.next("c")),
	}}
	ctx := context.Background()
	a, err := ref.open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	fx.createWithDiscussion(ctx, a)
	rec := &recordingController{ctrl: a}
	m := load(t, testModel(rec))
	// Navigate to the Plan Approval page.
	m.page = PagePlanApproval
	m.navigation = NavigationStack{Frames: []NavigationFrame{{Layer: LayerHome, Page: PageWorkspace}, {Layer: LayerWorkflowMenu, Page: PageWorkflowMenu, Workflow: m.selected}, {Layer: LayerStageWorkspace, Page: PagePlanApproval, Workflow: m.selected}}}
	m = runCmds(t, m, m.queryCmd(PagePlanApproval, app.PlanQuery{Workflow: m.selected}))
	if m.page != PagePlanApproval {
		t.Fatalf("page = %d, want plan approval", m.page)
	}
	// 'g' generates the plan.
	m = press(t, m, 'g', 0)
	if !rec.hasExecuted(app.GeneratePlanCommand{}) {
		t.Fatalf("g did not execute GeneratePlanCommand: %v", rec.executed)
	}
	if m.plan.Revision != 1 || m.plan.Hash == "" {
		t.Fatalf("plan after generate = %+v", m.plan)
	}
	// 'k' runs the independent check.
	m = press(t, m, 'k', 0)
	if !rec.hasExecuted(app.CheckPlanCommand{}) {
		t.Fatalf("k did not execute CheckPlanCommand: %v", rec.executed)
	}
	// First Enter opens the exact plan preview; second Enter executes approval.
	m = press(t, m, tea.KeyEnter, 0)
	if rec.hasExecuted(app.ApprovePlanCommand{}) {
		t.Fatal("first Enter executed plan approval")
	}
	// The second Enter executes approval for the exact plan.
	m = press(t, m, tea.KeyEnter, 0)
	if !rec.hasExecuted(app.ApprovePlanCommand{}) {
		t.Fatalf("second Enter did not approve: %v", rec.executed)
	}
	for _, c := range rec.executed {
		if ap, ok := c.(app.ApprovePlanCommand); ok {
			if ap.Revision != 1 || ap.Hash == "" || ap.Hash != m.plan.Hash {
				t.Fatalf("approve = %+v, plan = %+v", ap, m.plan)
			}
		}
	}
}

// TestModelPlanCheckQueuesUntilFreshProjection prevents a stale Plan Approval
// projection from issuing CheckPlan against old facts, while preserving the
// user's explicit check request until the GeneratePlan acknowledgement
// projection arrives.
func TestModelPlanCheckQueuesUntilFreshProjection(t *testing.T) {
	rec := &recordingController{ctrl: &migrationController{}}
	m := newModel(Dependencies{})
	m.ctrl = rec
	m.page = PagePlanApproval
	m.selected = "wf-1"
	m.plan = app.PlanView{
		Workflow: "wf-1",
		Stage:    model.StagePlanGeneration,
		Runtime:  model.RuntimeRunning,
	}

	m.commandState = &commandState{
		inFlight: true, generation: 1, pending: 1,
		ackPage: PagePlanApproval, workflow: "wf-1",
	}
	// The command has completed, but its asynchronous projection reload has
	// not been applied yet: this is the timing shown in the user report.
	m, _ = m.applyCommand(commandDoneMsg{
		cmd:        app.GeneratePlanCommand{Workflow: "wf-1"},
		generation: 1,
	})
	m = press(t, m, 'k', 0)

	for _, cmd := range rec.executed {
		if _, ok := cmd.(app.CheckPlanCommand); ok {
			t.Fatalf("stale projection issued CheckPlanCommand: %v", rec.executed)
		}
	}
	if !m.pendingPlanCheck {
		t.Fatal("stale check request was not queued")
	}

	m, cmd := m.applyProjection(projectionMsg{
		page:              PagePlanApproval,
		workflow:          "wf-1",
		generation:        2,
		commandGeneration: 1,
		view: app.PlanView{
			Workflow:   "wf-1",
			Stage:      model.StagePlanCheck,
			Runtime:    model.RuntimePaused,
			PlanStatus: model.PlanDraft,
			Revision:   1,
			Hash:       "fresh-plan-hash",
		},
	})
	if cmd == nil {
		t.Fatal("fresh plan projection did not resume the queued check")
	}
	_ = cmd()
	if !rec.hasExecuted(app.CheckPlanCommand{}) {
		t.Fatalf("queued check did not execute: %v", rec.executed)
	}
}

func TestPlanCheckOperationLogCorrelatesActionCommandAndProjection(t *testing.T) {
	var log bytes.Buffer
	rec := &recordingController{ctrl: &migrationController{}}
	m := newModel(Dependencies{OperationLog: &log})
	m.ctrl = rec
	m.page = PagePlanApproval
	m.selected = "wf-1"
	m.plan = app.PlanView{
		Workflow:   "wf-1",
		Stage:      model.StagePlanCheck,
		Runtime:    model.RuntimePaused,
		PlanStatus: model.PlanDraft,
		Revision:   1,
		Hash:       "plan-hash",
	}

	m, cmd := m.handlePlanApprovalKey(tea.KeyPressMsg{Code: 'k'})
	if cmd == nil {
		t.Fatal("check key did not return a command")
	}
	raw := cmd()
	msg, ok := raw.(commandDoneMsg)
	if !ok {
		t.Fatalf("command result = %T, want commandDoneMsg", raw)
	}
	m, _ = m.applyCommand(msg)
	operationID := m.commandState.operationID
	if operationID == "" {
		t.Fatal("command did not carry an operation id")
	}
	m, _ = m.applyProjection(projectionMsg{
		page:              PagePlanApproval,
		workflow:          "wf-1",
		generation:        2,
		commandGeneration: 1,
		operationID:       operationID,
		view: app.PlanView{
			Workflow:   "wf-1",
			Stage:      model.StagePlanCheck,
			Runtime:    model.RuntimePaused,
			PlanStatus: model.PlanChecked,
			Revision:   1,
			Hash:       "plan-hash",
		},
	})

	var entries []operationLogEntry
	for _, line := range strings.Split(strings.TrimSpace(log.String()), "\n") {
		if line == "" {
			continue
		}
		var entry operationLogEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("decode operation log: %v", err)
		}
		entries = append(entries, entry)
	}
	wantKinds := []string{"user_action", "command_started", "command_result", "projection_applied"}
	if len(entries) < len(wantKinds) {
		t.Fatalf("operation entries = %+v, want at least %v", entries, wantKinds)
	}
	for i, want := range wantKinds {
		if entries[i].Kind != want {
			t.Fatalf("entry %d kind = %q, want %q; entries = %+v", i, entries[i].Kind, want, entries)
		}
		if entries[i].OperationID != operationID {
			t.Fatalf("entry %d operation id = %q, want %q", i, entries[i].OperationID, operationID)
		}
	}
	if entries[0].Action != "plan_check" {
		t.Fatalf("user action = %q, want plan_check", entries[0].Action)
	}
	if entries[1].Command != "app.CheckPlanCommand" {
		t.Fatalf("command = %q, want app.CheckPlanCommand", entries[1].Command)
	}
	if entries[2].Result != "ok" || entries[3].Result != "accepted" {
		t.Fatalf("operation outcomes = %+v", entries[2:4])
	}
}

func TestProjectionOperationLogCapturesQueryLifecycle(t *testing.T) {
	var log bytes.Buffer
	m := newModel(Dependencies{OperationLog: &log})
	m.ctrl = &migrationController{}
	m.selected = "wf-legacy"
	m.commandState.operationID = "op-7"

	msg := m.queryProjectionMsgAt(
		PageWorkspace,
		app.ProjectWorkspaceQuery{Selected: "wf-legacy"},
		3,
		0,
	).(projectionMsg)
	m, _ = m.applyProjection(msg)

	var entries []operationLogEntry
	for _, line := range strings.Split(strings.TrimSpace(log.String()), "\n") {
		if line == "" {
			continue
		}
		var entry operationLogEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("decode operation log: %v", err)
		}
		entries = append(entries, entry)
	}
	wantKinds := []string{"query_started", "query_result", "projection_applied"}
	if len(entries) != len(wantKinds) {
		t.Fatalf("operation entries = %+v, want exactly %v", entries, wantKinds)
	}
	for i, want := range wantKinds {
		if entries[i].Kind != want {
			t.Fatalf("entry %d kind = %q, want %q; entries = %+v", i, entries[i].Kind, want, entries)
		}
		if entries[i].OperationID != "op-7" {
			t.Fatalf("entry %d operation id = %q, want op-7", i, entries[i].OperationID)
		}
	}
	if entries[0].Query != "app.ProjectWorkspaceQuery" ||
		entries[1].Result != "ok" ||
		entries[2].Result != "accepted" {
		t.Fatalf("query lifecycle entries = %+v", entries)
	}
}

// TestModelPlanApprovalWaitsForCheckedProjection preserves a two-Enter
// approval pressed after the CheckPlan command settled but before the refreshed
// PlanView arrived. The approval must bind the revision/hash from that
// fresh projection instead of dropping the user's action or using stale
// local data.
func TestModelPlanApprovalWaitsForCheckedProjection(t *testing.T) {
	rec := &recordingController{ctrl: &migrationController{}}
	m := newModel(Dependencies{})
	m.ctrl = rec
	m.page = PagePlanApproval
	m.selected = "wf-1"
	m.plan = app.PlanView{
		Workflow:   "wf-1",
		Stage:      model.StagePlanGeneration,
		Runtime:    model.RuntimePaused,
		PlanStatus: model.PlanDraft,
	}

	m, _ = m.applyCommand(commandDoneMsg{cmd: app.CheckPlanCommand{}})
	m = press(t, m, tea.KeyEnter, 0)
	m = press(t, m, tea.KeyEnter, 0)
	if rec.hasExecuted(app.ApprovePlanCommand{}) {
		t.Fatal("stale projection issued ApprovePlanCommand")
	}
	if !m.pendingPlanApproval {
		t.Fatal("explicit approval was not queued while projection was stale")
	}

	m, cmd := m.applyProjection(projectionMsg{
		page: PagePlanApproval,
		view: app.PlanView{
			Workflow:   "wf-1",
			Stage:      model.StagePlanCheck,
			Runtime:    model.RuntimePaused,
			PlanStatus: model.PlanChecked,
			Revision:   1,
			Hash:       "fresh-plan-hash",
		},
	})
	if cmd == nil {
		t.Fatal("fresh checked projection did not resume the queued approval")
	}
	if msg := cmd(); msg == nil {
		t.Fatal("queued approval command returned no completion message")
	}
	var approved app.ApprovePlanCommand
	for _, executed := range rec.executed {
		if got, ok := executed.(app.ApprovePlanCommand); ok {
			approved = got
		}
	}
	if approved.Revision != 1 || approved.Hash != "fresh-plan-hash" {
		t.Fatalf("approval = %+v, want fresh revision/hash", approved)
	}
}

func TestModelPlanProjectionIgnoresOutOfOrderOlderState(t *testing.T) {
	rec := &recordingController{ctrl: &migrationController{}}
	m := newModel(Dependencies{})
	m.ctrl = rec
	m.page = PagePlanApproval
	m.selected = "wf-1"

	m, _ = m.applyProjection(projectionMsg{
		page: PagePlanApproval,
		view: app.PlanView{
			Workflow:         "wf-1",
			AggregateVersion: 10,
			Stage:            model.StagePlanCheck,
			Runtime:          model.RuntimePaused,
			PlanStatus:       model.PlanChecked,
			Revision:         1,
			Hash:             "fresh-plan-hash",
		},
	})
	m, _ = m.applyProjection(projectionMsg{
		page: PagePlanApproval,
		view: app.PlanView{
			Workflow:         "wf-1",
			AggregateVersion: 9,
			Stage:            model.StagePlanCheck,
			Runtime:          model.RuntimePaused,
			PlanStatus:       model.PlanChecking,
			Revision:         1,
			Hash:             "fresh-plan-hash",
		},
	})
	if m.plan.PlanStatus != model.PlanChecked || m.plan.AggregateVersion != 10 {
		t.Fatalf("older projection replaced fresh state: %+v", m.plan)
	}

	m = press(t, m, tea.KeyEnter, 0)
	m = press(t, m, tea.KeyEnter, 0)
	var approved app.ApprovePlanCommand
	for _, executed := range rec.executed {
		if got, ok := executed.(app.ApprovePlanCommand); ok {
			approved = got
		}
	}
	if approved.Revision != 1 || approved.Hash != "fresh-plan-hash" {
		t.Fatalf("approval = %+v, want fresh revision/hash", approved)
	}
}

func TestModelPlanCheckTerminalProjectionSettlesStatus(t *testing.T) {
	tests := []struct {
		name   string
		view   app.PlanView
		status string
	}{
		{
			name: "needs discussion",
			view: app.PlanView{
				Stage:      model.StageRequirementDiscussion,
				Runtime:    model.RuntimeRunning,
				PlanStatus: model.PlanDraft,
			},
			status: "plan check needs discussion",
		},
		{
			name: "needs revision",
			view: app.PlanView{
				Stage:      model.StagePlanGeneration,
				Runtime:    model.RuntimeRunning,
				PlanStatus: model.PlanDraft,
			},
			status: "plan check needs revision",
		},
		{
			name: "rejected",
			view: app.PlanView{
				Stage:      model.StagePlanCheck,
				Runtime:    model.RuntimePaused,
				PlanStatus: model.PlanRejected,
			},
			status: "plan rejected",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newModel(Dependencies{})
			m.page = PagePlanApproval
			m.selected = "wf-1"
			m.plan = app.PlanView{
				Workflow:   "wf-1",
				Stage:      model.StagePlanCheck,
				PlanStatus: model.PlanDraft,
			}
			m.planCheckInFlight = true
			m.pendingPlanStatus = "plan checked"
			m.pendingPlanApproval = true
			m.status = "plan check finished; refreshing plan projection…"

			tt.view.Workflow = "wf-1"
			m, _ = m.applyProjection(projectionMsg{page: PagePlanApproval, view: tt.view})

			if m.status != tt.status {
				t.Fatalf("status = %q, want %q", m.status, tt.status)
			}
			if m.pendingPlanStatus != "" {
				t.Fatalf("pending plan status = %q, want empty", m.pendingPlanStatus)
			}
			if m.planCheckInFlight {
				t.Fatal("plan check remained in flight after terminal projection")
			}
			if m.pendingPlanApproval {
				t.Fatal("approval remained queued after non-checked terminal projection")
			}
		})
	}
}

// TestModelPlanGeneratedStatusFollowsProjection prevents the command
// callback from claiming success before the refreshed PlanView is visible.
func TestModelPlanGeneratedStatusFollowsProjection(t *testing.T) {
	m := newModel(Dependencies{})
	m.page = PagePlanApproval
	m.selected = "wf-1"
	m.plan = app.PlanView{
		Workflow: "wf-1",
		Stage:    model.StagePlanGeneration,
		Runtime:  model.RuntimeRunning,
	}

	m, _ = m.applyCommand(commandDoneMsg{cmd: app.GeneratePlanCommand{}})
	if strings.Contains(m.status, "plan generated") {
		t.Fatalf("command callback claimed generated before projection: %q", m.status)
	}

	m, _ = m.applyProjection(projectionMsg{
		page: PagePlanApproval,
		view: app.PlanView{
			Workflow:   "wf-1",
			Stage:      model.StagePlanGeneration,
			Runtime:    model.RuntimeRunning,
			Revision:   0,
			PlanStatus: model.PlanDraft,
		},
	})
	if strings.Contains(m.status, "plan generated") {
		t.Fatalf("stale projection claimed generated: %q", m.status)
	}

	m, _ = m.applyProjection(projectionMsg{
		page: PagePlanApproval,
		view: app.PlanView{
			Workflow:   "wf-1",
			Stage:      model.StagePlanCheck,
			Runtime:    model.RuntimePaused,
			Revision:   1,
			Hash:       "plan-hash",
			PlanStatus: model.PlanDraft,
		},
	})
	if m.status != "plan generated" {
		t.Fatalf("fresh projection status = %q, want plan generated", m.status)
	}
}

// TestModelExecutionApprovalMapsToTypedCommand: 's' generates the specs,
// 'w' compiles the workflow, 'd' runs the dry run, and the explicit
// confirmation issues ApproveExecutionCommand binding the exact preview
// hashes (including the frozen Change Set).
func TestModelExecutionApprovalMapsToTypedCommand(t *testing.T) {
	fx := newTUIFixture(t)
	ref := &appRef{fx: fx, scripts: []string{
		planScript(fx.next("p")), checkPlanPassScript(fx.next("c")),
		specScript(fx.next("s")), workflowScript(fx.next("w")),
	}}
	ctx := context.Background()
	a, err := ref.open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	wf := fx.createWithDiscussion(ctx, a)
	if err := fx.approvePlan(ctx, a, wf); err != nil {
		t.Fatal(err)
	}
	rec := &recordingController{ctrl: a}
	m := load(t, testModel(rec))
	// Use the already-built Home → Workflow Menu → Stage stack for the
	// execution-approval projection test.
	m.page = PageExecutionApproval
	m.navigation = NavigationStack{Frames: []NavigationFrame{{Layer: LayerHome, Page: PageWorkspace}, {Layer: LayerWorkflowMenu, Page: PageWorkflowMenu, Workflow: m.selected}, {Layer: LayerStageWorkspace, Page: PageExecutionApproval, Workflow: m.selected}}}
	m = runCmds(t, m, m.queryCmd(PageExecutionApproval, app.ExecutionPreviewQuery{Workflow: m.selected}))
	if m.page != PageExecutionApproval {
		t.Fatalf("page = %d, want execution approval", m.page)
	}
	m = press(t, m, 's', 0)
	if !rec.hasExecuted(app.GenerateSpecsCommand{}) {
		t.Fatalf("s did not generate specs: %v", rec.executed)
	}
	m = press(t, m, 'w', 0)
	if !rec.hasExecuted(app.CompileWorkflowCommand{}) {
		t.Fatalf("w did not compile: %v", rec.executed)
	}
	m = press(t, m, 'd', 0)
	if !rec.hasExecuted(app.ExecutionDryRunCommand{}) {
		t.Fatalf("d did not run the dry run: %v", rec.executed)
	}
	pv := m.preview
	if pv.PlanHash == "" || pv.ChangeSetHash == "" {
		t.Fatalf("preview = %+v, want the plan and change set hashes", pv)
	}
	// First Enter opens the exact execution preview; second Enter executes approval.
	m = press(t, m, tea.KeyEnter, 0)
	if rec.hasExecuted(app.ApproveExecutionCommand{}) {
		t.Fatal("first Enter executed execution approval")
	}
	m = press(t, m, tea.KeyEnter, 0)
	if !rec.hasExecuted(app.ApproveExecutionCommand{}) {
		t.Fatalf("second Enter did not approve the execution: %v", rec.executed)
	}
	for _, c := range rec.executed {
		if ap, ok := c.(app.ApproveExecutionCommand); ok {
			if ap.PlanHash != pv.PlanHash || ap.CatalogHash != pv.CatalogHash ||
				ap.WorkflowHash != pv.WorkflowHash || ap.ChangeSetHash != pv.ChangeSetHash {
				t.Fatalf("approve = %+v, preview = %+v", ap, pv)
			}
		}
	}
}

// TestModelCtrlCExecutesControlledPause: the first Ctrl+C executes the
// real controlled Pause (the typed PauseWorkflowCommand changes the
// Runtime); the second Ctrl+C is the Force Stop (EscalateStop) and
// quits.
func TestModelCtrlCExecutesControlledPause(t *testing.T) {
	fx := newTUIFixture(t)
	ref := &appRef{fx: fx, scripts: []string{planScript(fx.next("p"))}}
	ctx := context.Background()
	a, err := ref.open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Execute(ctx, app.CreateWorkflowCommand{Name: "calculator", Provider: "fake", ConfirmDirty: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Execute(ctx, app.GeneratePlanCommand{Workflow: ref.list()[0], Provider: "fake"}); err != nil {
		t.Fatal(err)
	}
	rec := &recordingController{ctrl: a}
	m := load(t, testModel(rec))

	// The first Ctrl+C requests the controlled Pause: the typed command
	// closes dispatch and stops the managed processes.
	m = press(t, m, KeyCtrlCRune, tea.ModCtrl)
	if m.stop != stopFirstCtrlC {
		t.Fatalf("stop = %d, want first-ctrl-c", m.stop)
	}
	if !rec.hasExecuted(app.PauseWorkflowCommand{}) {
		t.Fatalf("the first Ctrl+C did not execute the controlled pause: %v", rec.executed)
	}
	view, err := a.Query(ctx, app.StatusQuery{Workflow: ref.list()[0]})
	if err != nil {
		t.Fatal(err)
	}
	if st := view.(app.StatusView); st.Runtime != model.RuntimePaused {
		t.Fatalf("runtime after the first Ctrl+C = %s, want PAUSED", st.Runtime)
	}

	// The second Ctrl+C is the Force Stop: it escalates the controlled
	// stop and quits.
	m2, cmd := m.Update(tea.KeyPressMsg{Code: KeyCtrlCRune, Mod: tea.ModCtrl})
	if rec.escalated != 1 {
		t.Fatalf("the second Ctrl+C did not call EscalateStop: %d", rec.escalated)
	}
	if _, ok := m2.(Model); !ok {
		t.Fatalf("the second Ctrl+C changed the model type")
	}
	if cmd == nil {
		t.Fatal("the second Ctrl+C did not quit")
	}
}

// TestModelQIsOrdinaryInput: q does not enter the controlled-stop protocol,
// even while a Foreground Runner is active. Ctrl+C owns controlled stop.
func TestModelQIsOrdinaryInput(t *testing.T) {
	m := newModel(Dependencies{})
	m.running = true
	m.page = PageExecution
	m.selected = "wf-1"

	next, cmd := m.Update(tea.KeyPressMsg{Code: KeyQuit})
	got := next.(Model)
	if cmd != nil || got.page != PageExecution || got.stop != stopIdle || !got.running {
		t.Fatalf("q changed controlled-stop state: page=%v stop=%v running=%v cmd=%v", got.page, got.stop, got.running, cmd != nil)
	}
}

func TestActionPreviewRuntimeCommandsAcknowledgeWorkspaceProjection(t *testing.T) {
	tests := []struct {
		name       string
		action     app.MenuAction
		want       app.Command
		runtime    model.RuntimeStatus
		legalKind  model.WorkflowCommandKind
		legalLabel string
	}{
		{
			name:      "resume",
			action:    app.MenuActionResume,
			want:      app.ResumeWorkflowCommand{Workflow: "wf-1"},
			runtime:   model.RuntimeRunning,
			legalKind: model.PauseWorkflow, legalLabel: "Pause",
		},
		{
			name:      "pause",
			action:    app.MenuActionPause,
			want:      app.PauseWorkflowCommand{Workflow: "wf-1"},
			runtime:   model.RuntimePaused,
			legalKind: model.ResumeWorkflow, legalLabel: "Resume",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := &recordingController{ctrl: &actionPreviewAckController{
				view: app.WorkspaceView{
					Selected:     "wf-1",
					Workflows:    []app.WorkflowSummary{{ID: "wf-1", Name: "calculator", Runtime: tt.runtime}},
					Lifecycle:    &app.WorkflowLifecycleView{Status: app.StatusView{Workflow: "wf-1", Runtime: tt.runtime}},
					LegalActions: []app.LegalAction{{Kind: tt.legalKind, Label: tt.legalLabel}},
				},
			}}
			m := newModel(Dependencies{})
			m.ctrl = ctrl
			m.selected = "wf-1"
			m.page = PageActionPreview
			m.navigation = NavigationStack{Frames: []NavigationFrame{
				{Layer: LayerHome, Page: PageWorkspace},
				{Layer: LayerWorkflowMenu, Page: PageWorkflowMenu, Workflow: "wf-1"},
				{Layer: LayerActionPreview, Page: PageActionPreview, Workflow: "wf-1"},
			}}
			m.workflowMenuPreviewItem = MenuItem{Action: tt.action, Label: tt.name}
			m.actionPreviewed = true

			next, cmd := m.handleActionPreviewKey(tea.KeyPressMsg{Code: tea.KeyEnter})
			if cmd == nil {
				t.Fatal("confirmed action returned no command")
			}
			m = next
			if got := m.commandState.ackPage; got != PageWorkspace {
				t.Fatalf("ack page = %v, want Workspace", got)
			}

			raw := cmd()
			msg, ok := raw.(commandDoneMsg)
			if !ok {
				t.Fatalf("command result = %T, want commandDoneMsg", raw)
			}
			m, _ = m.applyCommand(msg)
			if !m.commandInFlight() {
				t.Fatal("command was not in flight before projection acknowledgement")
			}
			if len(ctrl.executed) != 1 || !reflect.DeepEqual(ctrl.executed[0], tt.want) {
				t.Fatalf("executed = %#v, want %#v", ctrl.executed, tt.want)
			}

			projection := m.queryProjectionMsgAt(
				PageWorkspace,
				app.ProjectWorkspaceQuery{Selected: "wf-1"},
				m.nextProjectionID(),
				m.commandState.pending,
			).(projectionMsg)
			m, _ = m.applyProjection(projection)
			if m.commandInFlight() {
				t.Fatal("matching Workspace projection left command in flight")
			}
			if m.workspace.Lifecycle == nil || m.workspace.Lifecycle.Runtime != tt.runtime {
				t.Fatalf("refreshed lifecycle = %+v, want runtime %s", m.workspace.Lifecycle, tt.runtime)
			}
			if !hasAction(m.workspace.Actions, map[model.WorkflowCommandKind]Action{
				model.ResumeWorkflow: ActionResume,
				model.PauseWorkflow:  ActionPause,
			}[tt.legalKind]) {
				t.Fatalf("refreshed legal actions = %v, want %s", m.workspace.Actions, tt.legalLabel)
			}
		})
	}
}

func TestApplyExecuteBindsExactAttemptFactsFromPreview(t *testing.T) {
	rec := &recordingController{ctrl: &actionPreviewAckController{view: app.WorkspaceView{
		Selected:  "wf-1",
		Workflows: []app.WorkflowSummary{{ID: "wf-1", Runtime: model.RuntimeSucceeded}},
		Lifecycle: &app.WorkflowLifecycleView{Status: app.StatusView{Workflow: "wf-1", Runtime: model.RuntimeSucceeded}},
	}}}
	m := newModel(Dependencies{})
	m.ctrl = rec
	m.selected = "wf-1"
	m.page = PageTerminal
	m.terminal.Section = SectionApply
	attempt := model.ApplyAttempt{
		ID: "apply-7", TargetHead: "target-1", IntegrationHead: "integration-1",
		Preflight:     model.ArtifactRef{Workflow: "wf-1", Type: model.ArtifactReport, Revision: 4, Hash: "preflight-1"},
		PreflightHash: "preflight-1", Fingerprint: "fingerprint-1", StagingHead: "staging-1",
		Status: model.ApplyAwaitingConfirmation,
	}
	m.terminal.ApplyPreview = renderApplyAttempt(attempt)
	updated, _ := m.applyCommand(commandDoneMsg{cmd: app.PrepareApplyCommand{Workflow: "wf-1"}, out: app.Outcome{Workflow: "wf-1", Apply: &attempt}})
	updated.terminal.Previewed = true
	_, cmd := updated.handleTerminalKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatalf("confirmed apply command = %v executed=%v", cmd != nil, rec.executed)
	}
	_ = cmd()
	if len(rec.executed) != 1 {
		t.Fatalf("confirmed apply executed=%v", rec.executed)
	}
	got, ok := rec.executed[0].(app.ExecuteApplyCommand)
	if !ok {
		t.Fatalf("command = %T, want ExecuteApplyCommand", rec.executed[0])
	}
	fields := []struct{ name, want string }{
		{"AttemptID", "apply-7"}, {"TargetHead", "target-1"}, {"IntegrationHead", "integration-1"},
		{"PreflightHash", "preflight-1"}, {"Fingerprint", "fingerprint-1"},
	}
	v := reflect.ValueOf(got)
	for _, field := range fields {
		fv := v.FieldByName(field.name)
		if !fv.IsValid() || fv.String() != field.want {
			t.Fatalf("ExecuteApplyCommand.%s = %v, want %q", field.name, fv, field.want)
		}
	}
	preflight := v.FieldByName("Preflight")
	if !preflight.IsValid() || preflight.FieldByName("Revision").Int() != 4 || preflight.FieldByName("Hash").String() != "preflight-1" {
		t.Fatalf("ExecuteApplyCommand.Preflight = %v, want exact preview ref", preflight)
	}
}

func TestStageLocalMutationsOpenActionPreviewBeforeExecute(t *testing.T) {
	tests := []struct {
		name  string
		page  Page
		key   rune
		setup func(*Model)
	}{
		{name: "discussion finish", page: PageDiscussion, key: tea.KeyEnter, setup: func(m *Model) {
			m.discussion = DiscussionPage{Loaded: true, Session: "sess-1", Actions: []DiscussionReturnAction{ReturnFinish}}
		}},
		{name: "execution resume", page: PageExecution, key: 'r', setup: func(m *Model) {
			m.workspace.Actions = []Action{ActionResume}
		}},
		{name: "blocked resume", page: PageBlocked, key: 'r', setup: func(m *Model) {
			m.workspace.Actions = []Action{ActionResume}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &recordingController{ctrl: &actionPreviewAckController{}}
			m := newModel(Dependencies{})
			m.ctrl, m.selected, m.page = rec, "wf-1", tt.page
			m.navigation = NavigationStack{Frames: []NavigationFrame{{Layer: LayerHome, Page: PageWorkspace}, {Layer: LayerWorkflowMenu, Page: PageWorkflowMenu, Workflow: "wf-1"}, {Layer: LayerStageWorkspace, Page: tt.page, Workflow: "wf-1"}}}
			tt.setup(&m)
			updated, _ := m.Update(tea.KeyPressMsg{Code: tt.key})
			got := updated.(Model)
			if len(rec.executed) != 0 || got.page != PageActionPreview || got.actionPreviewed {
				t.Fatalf("first stage mutation = page %v previewed=%v executed=%v, want action preview only", got.page, got.actionPreviewed, rec.executed)
			}
		})
	}
}

func TestStartRunnerActionPreviewEscReturnsToWorkflowMenu(t *testing.T) {
	m := newModel(Dependencies{})
	m.selected = "wf-1"
	m.page = PageActionPreview
	m.workflowMenuPreviewItem = MenuItem{Action: app.MenuActionStartRunner, Label: "Start Runner"}
	m.actionPreviewed = true
	m.navigation = NavigationStack{Frames: []NavigationFrame{
		{Layer: LayerHome, Page: PageWorkspace},
		{Layer: LayerWorkflowMenu, Page: PageWorkflowMenu, Workflow: "wf-1"},
		{Layer: LayerActionPreview, Page: PageActionPreview, Workflow: "wf-1"},
	}}
	updated, _ := m.handleActionPreviewKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	if got := updated.navigation.Current(); got.Layer != LayerStageWorkspace || got.Page != PageExecution {
		t.Fatalf("Start Runner current frame = %+v, want Execution stage", got)
	}
	if got := updated.navigation.ParentPage(); got != PageWorkflowMenu {
		t.Fatalf("Start Runner parent page = %v, want Workflow Menu", got)
	}
}

func TestStageResumeConfirmationKeepsExistingExecutionParent(t *testing.T) {
	m := newModel(Dependencies{})
	m.selected = "wf-1"
	m.page = PageActionPreview
	m.workflowMenuPreviewItem = MenuItem{Action: app.MenuActionResume, Label: "Resume"}
	m.actionPreviewed = true
	m.navigation = NavigationStack{Frames: []NavigationFrame{
		{Layer: LayerHome, Page: PageWorkspace},
		{Layer: LayerWorkflowMenu, Page: PageWorkflowMenu, Workflow: "wf-1"},
		{Layer: LayerStageWorkspace, Page: PageExecution, Workflow: "wf-1"},
		{Layer: LayerActionPreview, Page: PageActionPreview, Workflow: "wf-1"},
	}}
	updated, cmd := m.handleActionPreviewKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil || len(updated.navigation.Frames) != 3 || updated.navigation.Current().Page != PageExecution {
		t.Fatalf("stage Resume stack=%+v page=%v cmd=%v, want existing Execution parent", updated.navigation.Frames, updated.page, cmd != nil)
	}
}

func TestActionPreviewConfirmationTracesEveryTypedActionOnce(t *testing.T) {
	actions := []struct {
		name   string
		action app.MenuAction
		setup  func(*Model)
	}{
		{name: "apply", action: app.MenuActionApply},
		{name: "cleanup", action: app.MenuActionCleanup},
		{name: "adopt workspace", action: app.MenuActionAdoptWorkspace},
		{name: "start discussion", action: app.MenuActionStartDiscussion},
		{name: "continue discussion", action: app.MenuActionContinueDiscussion, setup: func(m *Model) {
			m.discussion.Session = "sess-1"
		}},
		{name: "finish discussion", action: app.MenuActionFinishDiscussion, setup: func(m *Model) {
			m.discussion.Session = "sess-1"
		}},
		{name: "switch discussion", action: app.MenuActionSwitchDiscussion, setup: func(m *Model) {
			m.discussion.Session = "sess-1"
			m.discussion.Provider = "fake"
			m.workspace.Health.Providers = []app.ProviderHealth{{Name: "other", Compatible: true}}
		}},
		{name: "resume", action: app.MenuActionResume},
		{name: "pause", action: app.MenuActionPause},
		{name: "start runner", action: app.MenuActionStartRunner},
	}

	for _, tc := range actions {
		t.Run(tc.name, func(t *testing.T) {
			var log bytes.Buffer
			m := newModel(Dependencies{OperationLog: &log})
			m.ctrl = &actionPreviewAckController{}
			m.selected = "wf-1"
			m.page = PageActionPreview
			m.workflowMenuPreviewItem = MenuItem{Action: tc.action, Label: tc.name}
			m.actionPreviewed = true
			m.navigation = NavigationStack{Frames: []NavigationFrame{
				{Layer: LayerHome, Page: PageWorkspace},
				{Layer: LayerWorkflowMenu, Page: PageWorkflowMenu, Workflow: "wf-1"},
				{Layer: LayerActionPreview, Page: PageActionPreview, Workflow: "wf-1"},
			}}
			if tc.setup != nil {
				tc.setup(&m)
			}

			updated, cmd := m.handleActionPreviewKey(tea.KeyPressMsg{Code: tea.KeyEnter})
			if cmd == nil {
				t.Fatal("confirmation returned no command")
			}
			var confirms []operationLogEntry
			for _, line := range strings.Split(strings.TrimSpace(log.String()), "\n") {
				if line == "" {
					continue
				}
				var entry operationLogEntry
				if err := json.Unmarshal([]byte(line), &entry); err != nil {
					t.Fatal(err)
				}
				if entry.Action == uiActionActionPreviewConfirm {
					confirms = append(confirms, entry)
				}
			}
			if len(confirms) != 1 {
				t.Fatalf("action preview confirmations = %d, want 1; log=%s", len(confirms), log.String())
			}
			if confirms[0].Workflow != "wf-1" || confirms[0].OperationID == "" {
				t.Fatalf("confirmation binding = %+v, want workflow and operation", confirms[0])
			}
			_ = updated
		})
	}
}

type actionPreviewAckController struct {
	view app.WorkspaceView
}

func (c *actionPreviewAckController) Execute(context.Context, app.Command) (app.Outcome, error) {
	return app.Outcome{}, nil
}

func (c *actionPreviewAckController) Query(_ context.Context, q app.Query) (app.View, error) {
	if _, ok := q.(app.ProjectWorkspaceQuery); ok {
		return c.view, nil
	}
	return nil, model.InvalidInputFault("unexpected query")
}

func (*actionPreviewAckController) DriveOnce(context.Context, model.WorkflowID) (app.DriveOutcome, error) {
	return app.DriveOutcome{}, nil
}

func (*actionPreviewAckController) EscalateStop() {}

func TestHierarchicalOperationTraceRecordsSafeUIActions(t *testing.T) {
	var log bytes.Buffer
	m := newModel(Dependencies{OperationLog: &log})
	m.ctrl = &workflowMenuController{view: app.WorkflowMenuView{
		Workflow: "wf-1",
		Entries: []app.WorkflowMenuEntry{{
			ID: "resume", Kind: app.MenuEntryAction, Label: "Resume",
			Action: app.MenuActionResume,
		}},
		DefaultIndex: 0,
	}}
	m.selected = "wf-1"
	m.workspace.Rows = []WorkflowRow{{Kind: WorkflowRowExisting, ID: "wf-1", Name: "calculator"}}
	m.workflowMenuModel = WorkflowMenuModel{
		Workflow: "wf-1",
		Loaded:   true,
		Items: []MenuItem{{
			SourceIndex: 0,
			Kind:        app.MenuEntryAction,
			Label:       "Resume",
			Action:      app.MenuActionResume,
		}},
	}
	m.workflowMenu = app.WorkflowMenuView{
		Workflow: "wf-1",
		Entries: []app.WorkflowMenuEntry{{
			ID:     "resume",
			Kind:   app.MenuEntryAction,
			Label:  "Resume",
			Action: app.MenuActionResume,
		}},
	}

	// Exercise the actual root dispatch for palette open/execute and menu
	// selection. The arbitrary text must never be copied into the trace.
	m = step(t, m, tea.KeyPressMsg{Text: "/"})
	m = step(t, m, tea.KeyPressMsg{Text: "rm -rf /"})
	m = press(t, m, tea.KeyEsc, 0)
	m = step(t, m, tea.KeyPressMsg{Text: "/"})
	m = step(t, m, tea.KeyPressMsg{Text: "exit"})
	m = press(t, m, tea.KeyEnter, 0)
	m = press(t, m, tea.KeyEnter, 0)
	m = press(t, m, tea.KeyEnter, 0)
	m = press(t, m, tea.KeyEnter, 0)
	m = press(t, m, tea.KeyEnter, 0)
	m, _ = m.popNavigation()

	var entries []operationLogEntry
	for _, line := range strings.Split(strings.TrimSpace(log.String()), "\n") {
		if line == "" {
			continue
		}
		var entry operationLogEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("decode operation log: %v", err)
		}
		entries = append(entries, entry)
	}
	wantActions := []string{
		"command_palette.open",
		"command_palette.execute",
		"navigation.push",
		"workflow_menu.query",
		"workflow_menu.select",
		"action_preview.open",
		"action_preview.confirm",
		"navigation.pop",
	}
	seen := make(map[string]operationLogEntry)
	for _, entry := range entries {
		if entry.Action != "" {
			seen[entry.Action] = entry
		}
		if strings.Contains(log.String(), "rm -rf /") {
			t.Fatalf("operation trace leaked arbitrary palette input: %q", log.String())
		}
	}
	for _, action := range wantActions {
		entry, ok := seen[action]
		if !ok {
			t.Fatalf("operation trace missing %q: %+v", action, entries)
		}
		if action != "command_palette.open" && action != "command_palette.execute" && entry.Workflow != "wf-1" {
			t.Fatalf("operation %q workflow = %q, want wf-1", action, entry.Workflow)
		}
		if action == "action_preview.confirm" && entry.OperationID == "" {
			t.Fatalf("operation %q has no bound operation id: %+v", action, entry)
		}
	}
}

func TestSlashOpensOnlyOutsideTextInput(t *testing.T) {
	m := newModel(Dependencies{})
	m.page = PageWorkspace
	m = step(t, m, tea.KeyPressMsg{Text: "/"})
	if !m.commandPalette.Open {
		t.Fatal("slash did not open Command Palette")
	}

	m.page = PageCreate
	m.commandPalette = NewCommandPalette()
	m.createConfirm = false
	m.createInput = ""
	m = step(t, m, tea.KeyPressMsg{Text: "/"})
	if m.commandPalette.Open || !strings.Contains(m.createInput, "/") {
		t.Fatalf("slash was not literal in text input: palette=%+v input=%q", m.commandPalette, m.createInput)
	}
}

func TestCommandPaletteEscRestoresPriorPageAndSelection(t *testing.T) {
	m := newModel(Dependencies{})
	m.page = PageWorkflowMenu
	m.selected = "wf-1"
	m.workflowMenuIndex = 2
	m.workflowMenuModel.Selected = 2
	m.navigation = NavigationStack{Frames: []NavigationFrame{
		{Layer: LayerHome, Page: PageWorkspace},
		{Layer: LayerWorkflowMenu, Page: PageWorkflowMenu, Workflow: "wf-1"},
	}}

	m = step(t, m, tea.KeyPressMsg{Text: "/"})
	m = step(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
	if m.page != PageWorkflowMenu || m.workflowMenuIndex != 2 || m.workflowMenuModel.Selected != 2 {
		t.Fatalf("palette key leaked into prior page: page=%v index=%d selected=%d", m.page, m.workflowMenuIndex, m.workflowMenuModel.Selected)
	}
	m = step(t, m, tea.KeyPressMsg{Code: tea.KeyEsc})
	if m.commandPalette.Open || m.page != PageWorkflowMenu || m.selected != "wf-1" || m.workflowMenuIndex != 2 || m.workflowMenuModel.Selected != 2 {
		t.Fatalf("palette Esc did not restore state: page=%v selected=%q index=%d menu=%d open=%v", m.page, m.selected, m.workflowMenuIndex, m.workflowMenuModel.Selected, m.commandPalette.Open)
	}
}

func TestIdleExitCommandQuitsTUI(t *testing.T) {
	m := newModel(Dependencies{})
	m.page = PageWorkflowMenu
	m = step(t, m, tea.KeyPressMsg{Text: "/"})
	updated, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	got := updated.(Model)
	if cmd == nil {
		t.Fatal("idle /exit Enter did not return tea.Quit")
	}
	if got.commandPalette.Open {
		t.Fatal("idle /exit left the command palette open")
	}
}

func TestExitCommandDoesNotQuitWhileApplicationCommandIsInFlight(t *testing.T) {
	m := newModel(Dependencies{})
	m.commandState = &commandState{inFlight: true, generation: 3, pending: 3, ackPage: PageWorkspace, workflow: "wf-1"}
	m.page = PageWorkspace
	m = step(t, m, tea.KeyPressMsg{Text: "/"})
	updated, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	got := updated.(Model)
	if cmd != nil || got.page != PageWorkspace || got.commandPalette.Open {
		t.Fatalf("/exit bypassed in-flight command: page=%v palette=%v cmd=%v", got.page, got.commandPalette.Open, cmd != nil)
	}
	if !strings.Contains(got.status, "command") {
		t.Fatalf("in-flight /exit did not leave a diagnostic status: %q", got.status)
	}
}

func TestRunningExitWaitsForRunnerDoneAfterControlledPause(t *testing.T) {
	ctrl := &recordingController{ctrl: &migrationController{}}
	m := newModel(Dependencies{})
	m.ctrl = ctrl
	m.page = PageExecution
	m.prevPage = PageExecution
	m.selected = "wf-1"
	m.runnerWorkflow = "wf-1"
	m.running = true
	cancelled := false
	m.runCancel = func() { cancelled = true }

	m = step(t, m, tea.KeyPressMsg{Text: "/"})
	updated, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = updated.(Model)
	if cmd != nil || m.page != PagePauseExit || m.prevPage != PageExecution || !m.running {
		t.Fatalf("running /exit did not open pause preview: page=%v prev=%v running=%v cmd=%v", m.page, m.prevPage, m.running, cmd != nil)
	}

	updated, cmd = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = updated.(Model)
	if cmd == nil || m.stop != stopPauseAndExit || !m.pauseCommandPending || !cancelled {
		t.Fatalf("pause preview Enter did not request controlled stop: stop=%v pending=%v cancelled=%v cmd=%v", m.stop, m.pauseCommandPending, cancelled, cmd != nil)
	}

	updated, quit := m.Update(runnerDoneMsg{res: foreground.Result{Reason: foreground.StopCancelled}})
	m = updated.(Model)
	if quit != nil {
		t.Fatal("runnerDoneMsg quit before PauseWorkflowCommand acknowledgement")
	}
	if m.running {
		t.Fatal("runnerDoneMsg left runner active")
	}
	updated, quit = m.applyCommand(commandDoneMsg{
		cmd:        app.PauseWorkflowCommand{Workflow: "wf-1"},
		generation: m.commandState.pending,
	})
	if quit == nil {
		t.Fatal("PauseWorkflowCommand acknowledgement did not quit after runner join")
	}
}

func TestRunningExitPreviewUsesOnlyEnter(t *testing.T) {
	m := newModel(Dependencies{})
	m.page = PagePauseExit
	m.prevPage = PageExecution
	m.running = true
	m.runnerWorkflow = "wf-1"
	m.runCancel = func() {}

	for _, key := range []rune{'q', 'y', 'n'} {
		updated, cmd := m.Update(tea.KeyPressMsg{Code: key, Text: string(key)})
		got := updated.(Model)
		if cmd != nil || got.page != PagePauseExit || got.stop != stopIdle || !got.running {
			t.Fatalf("%q acted as an exit confirmation: page=%v stop=%v running=%v cmd=%v", key, got.page, got.stop, got.running, cmd != nil)
		}
	}
}

func TestTask7RenderedCancelAndMigrationCopyDoesNotAdvertiseConfirmationKey(t *testing.T) {
	m := newModel(Dependencies{})
	m.ready, m.width, m.height = true, 100, 24

	m.page = PageCancel
	got := render(m)
	for _, forbidden := range []string{"Enter confirm", "Enter to choose", "confirm: no", "y confirm", "n default", "[y/N]"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("Cancel surface advertises stale confirmation copy %q:\n%s", forbidden, got)
		}
	}

	m.page = PageMigration
	m.migrationConfirm = migrationConfirmPrepare
	got = render(m)
	for _, forbidden := range []string{"Enter confirm", "Enter to choose", "y confirm", "n default", "[y/N]"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("Migration surface advertises stale confirmation copy %q:\n%s", forbidden, got)
		}
	}
}

func TestRenderOpenCommandPaletteKeepsUnderlyingPageBounded(t *testing.T) {
	m := newModel(Dependencies{})
	m.ready, m.width, m.height = true, 100, 24
	m.workspace = sampleWorkspaceViewModel()
	m.commandPalette = NewCommandPalette()
	m.commandPalette.Open = true
	m.commandPalette.Input = "/"

	got := render(m)
	if !strings.Contains(got, "WORKSPACE") || !strings.Contains(got, "/exit") || !strings.Contains(got, "COMMAND PALETTE") {
		t.Fatalf("open palette frame lost underlying page or palette:\n%s", got)
	}
	lines := strings.Split(got, "\n")
	if len(lines) > m.height {
		t.Fatalf("open palette frame has %d rows, want <= %d", len(lines), m.height)
	}
	for i, line := range lines {
		if gotWidth := lipgloss.Width(line); gotWidth > m.width {
			t.Fatalf("open palette line %d has width %d, want <= %d: %q", i, gotWidth, m.width, line)
		}
	}
}

// ---------------------------------------------------------------------------
// Task 6: Foreground Runner ownership
// ---------------------------------------------------------------------------

// blockingController drives the Foreground Runner into a bounded wait: its
// DriveOnce returns a DriveWaiting outcome whose channel never closes, so
// the Runner blocks until its run context is cancelled (design §12.1).
type blockingController struct {
	executed []app.Command
	wait     chan struct{}
}

func (c *blockingController) Execute(_ context.Context, cmd app.Command) (app.Outcome, error) {
	c.executed = append(c.executed, cmd)
	return app.Outcome{Workflow: "wf-1"}, nil
}

func (c *blockingController) Query(_ context.Context, q app.Query) (app.View, error) {
	if _, ok := q.(app.ProjectWorkspaceQuery); !ok {
		return nil, model.InvalidInputFault("unexpected query")
	}
	return app.WorkspaceView{
		Selected:  "wf-1",
		Workflows: []app.WorkflowSummary{{ID: "wf-1", Runtime: model.RuntimeRunning}},
		Lifecycle: &app.WorkflowLifecycleView{
			Status: app.StatusView{Workflow: "wf-1", Stage: model.StageExecution, Runtime: model.RuntimeRunning},
		},
		Health: app.HealthView{GitAvailable: true},
	}, nil
}

func (c *blockingController) DriveOnce(_ context.Context, _ model.WorkflowID) (app.DriveOutcome, error) {
	return app.DriveOutcome{Kind: app.DriveWaiting, Wait: c.wait}, nil
}

func (*blockingController) EscalateStop() {}

// runBatchMessages runs a Batch command's sub-commands to completion and
// returns their messages. The synchronous test harness cannot run a
// blocking Runner, so the cancellation tests run it in a goroutine.
func runBatchMessages(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	var results []tea.Msg
	switch batch := msg.(type) {
	case tea.BatchMsg:
		for _, c := range batch {
			if c != nil {
				results = append(results, c())
			}
		}
	default:
		results = append(results, msg)
	}
	return results
}

// hasRunnerStopped reports whether the messages contain the Runner's
// terminal result with the given stop reason.
func hasRunnerStopped(msgs []tea.Msg, reason foreground.StopReason) bool {
	for _, msg := range msgs {
		if rd, ok := msg.(runnerDoneMsg); ok && rd.err == nil {
			return rd.res.Reason == reason
		}
	}
	return false
}

// TestRunnerStartOwnsState: the root Model owns the Foreground Runner
// lifecycle (design §11, Task 6): startRunner returns the updated Model
// with running, runCancel, eventCh and the subscription set, and the
// runner-done terminal path clears them exactly once.
func TestRunnerStartOwnsState(t *testing.T) {
	ctrl := &executionController{runtime: model.RuntimeRunning}
	m := load(t, testModel(&recordingController{ctrl: ctrl}))
	m.selected = "wf-1"

	m2, runCmd := m.startRunner()
	if !m2.running || m2.runCancel == nil || m2.eventCh == nil {
		t.Fatalf("runner ownership after start: running=%v runCancel=%v eventCh=%v",
			m2.running, m2.runCancel == nil, m2.eventCh == nil)
	}
	if runCmd == nil {
		t.Fatal("startRunner returned no runner command")
	}

	// A terminal run clears the ownership state exactly once.
	m3 := runCmds(t, m2, runCmd)
	if m3.running || m3.runCancel != nil || m3.eventCh != nil {
		t.Fatalf("runner ownership after done: running=%v runCancel=%v eventCh=%v",
			m3.running, m3.runCancel != nil, m3.eventCh != nil)
	}
}

// TestRunnerDuplicateStartRefused: starting a Run while one is already
// active is refused (Task 6): no second runner command and no second
// subscription (the event channel is unchanged).
func TestRunnerDuplicateStartRefused(t *testing.T) {
	ctrl := &executionController{runtime: model.RuntimeRunning}
	m := load(t, testModel(&recordingController{ctrl: ctrl}))
	m.selected = "wf-1"

	m, firstCmd := m.startRunner()
	firstCh := m.eventCh
	if firstCmd == nil {
		t.Fatal("the first start produced no runner command")
	}

	m2, secondCmd := m.startRunner()
	if secondCmd != nil {
		t.Fatal("duplicate start issued a second runner command")
	}
	if m2.eventCh != firstCh {
		t.Fatal("duplicate start replaced the event subscription")
	}
	if !m2.running {
		t.Fatal("duplicate start cleared the running state")
	}
	if !strings.Contains(m2.status, "already active") {
		t.Fatalf("duplicate start status = %q", m2.status)
	}
}

// TestRunnerRunKeyRefusesDuplicate: pressing r while a Runner is active
// refuses the second start (the Execution page never spawns a second
// runner or a second subscription).
func TestRunnerRunKeyRefusesDuplicate(t *testing.T) {
	ctrl := &executionController{runtime: model.RuntimeRunning}
	m := load(t, testModel(&recordingController{ctrl: ctrl}))
	m.selected = "wf-1"
	m.page = PageExecution

	m, _ = m.startRunner()
	m2 := press(t, m, 'r', 0)
	if !strings.Contains(m2.status, "already active") {
		t.Fatalf("r on an active runner status = %q", m2.status)
	}
	if ctrl.driveCalls != 0 {
		t.Fatalf("duplicate r drove the runner again (drive calls = %d)", ctrl.driveCalls)
	}
}

// TestModelCtrlCCancelsRunnerOnPause: the first Ctrl+C requests the
// controlled Pause AND cancels the real Runner (the run context is
// cancelled), so the runner stops with StopCancelled instead of driving
// on.
func TestModelCtrlCCancelsRunnerOnPause(t *testing.T) {
	ctrl := &blockingController{wait: make(chan struct{})}
	rec := &recordingController{ctrl: ctrl}
	m := load(t, testModel(rec))
	m.selected = "wf-1"
	m.page = PageExecution

	m, runCmd := m.startRunner()
	done := make(chan []tea.Msg, 1)
	go func() {
		done <- runBatchMessages(runCmd)
	}()

	// The first Ctrl+C requests the controlled pause and cancels the
	// runner.
	m2 := press(t, m, KeyCtrlCRune, tea.ModCtrl)
	if m2.stop != stopFirstCtrlC {
		t.Fatalf("stop = %d, want first-ctrl-c", m2.stop)
	}
	if !rec.hasExecuted(app.PauseWorkflowCommand{}) {
		t.Fatalf("the first Ctrl+C did not request the controlled pause: %v", rec.executed)
	}

	select {
	case msgs := <-done:
		if !hasRunnerStopped(msgs, foreground.StopCancelled) {
			t.Fatalf("the runner did not stop with StopCancelled: %#v", msgs)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the runner was not cancelled by the first Ctrl+C")
	}
}

// TestModelCtrlCSecondForceStopCleansUp: the second Ctrl+C escalates the
// controlled stop to the force-kill phase (EscalateStop), then quits only
// after runnerDoneMsg proves the runner ownership is cleaned up.
func TestModelCtrlCSecondForceStopCleansUp(t *testing.T) {
	ctrl := &executionController{runtime: model.RuntimeRunning}
	rec := &recordingController{ctrl: ctrl}
	m := load(t, testModel(rec))
	m.selected = "wf-1"

	m, _ = m.startRunner()
	m2 := press(t, m, KeyCtrlCRune, tea.ModCtrl)
	if m2.stop != stopFirstCtrlC {
		t.Fatalf("stop = %d, want first-ctrl-c", m2.stop)
	}
	m3, quitCmd := m2.Update(tea.KeyPressMsg{Code: KeyCtrlCRune, Mod: tea.ModCtrl})
	if rec.escalated != 1 {
		t.Fatalf("the second Ctrl+C did not escalate: %d", rec.escalated)
	}
	if quitCmd != nil {
		t.Fatal("the second Ctrl+C quit before runner completion")
	}
	m4, quitCmd := m3.(Model).Update(runnerDoneMsg{res: foreground.Result{Reason: foreground.StopCancelled}})
	if quitCmd == nil {
		t.Fatal("the second Ctrl+C did not quit after runner completion")
	}
	mm := m4.(Model)
	if mm.running || mm.runCancel != nil || mm.eventCh != nil {
		t.Fatalf("the second Ctrl+C left runner ownership: running=%v runCancel=%v eventCh=%v",
			mm.running, mm.runCancel != nil, mm.eventCh != nil)
	}
}

func TestPauseFailureAfterForceStopLeavesDiagnosableExitState(t *testing.T) {
	m := newModel(Dependencies{})
	m.ready, m.width, m.height = true, 100, 24
	m.stop = stopPauseAndExit
	m.quitAfterRunner = true
	m.pauseCommandPending = true
	m.commandState = &commandState{inFlight: true, generation: 9, pending: 9, ackPage: PageWorkspace}

	got, quitCmd := m.applyCommand(commandDoneMsg{
		cmd:        app.PauseWorkflowCommand{Workflow: "wf-1"},
		generation: 9,
		err:        model.InvalidInputFault("pause failed"),
	})
	if quitCmd != nil || got.page != PagePauseExit || got.stop != stopIdle || got.quitAfterRunner || got.pauseCommandPending {
		t.Fatalf("pause failure quit or lost recovery state: page=%v stop=%v quitAfterRunner=%v pausePending=%v cmd=%v", got.page, got.stop, got.quitAfterRunner, got.pauseCommandPending, quitCmd != nil)
	}
	if !strings.Contains(got.status, "pause failed") || !strings.Contains(render(got), "retry") {
		t.Fatalf("pause failure lost error/recovery copy: status=%q render=%s", got.status, render(got))
	}
}

// TestRunnerDoneClearsEventChannel pins the applyRunnerDone terminal path:
// a runner error resets running, runCancel, AND the event channel (no
// stale subscription field is left on the Model).
func TestRunnerDoneClearsEventChannel(t *testing.T) {
	ctrl := &executionController{runtime: model.RuntimeRunning}
	m := load(t, testModel(&recordingController{ctrl: ctrl}))
	m.selected = "wf-1"

	m, runCmd := m.startRunner()
	m = runCmds(t, m, runCmd)
	if m.running || m.runCancel != nil || m.eventCh != nil {
		t.Fatalf("runner done left ownership: running=%v runCancel=%v eventCh=%v",
			m.running, m.runCancel != nil, m.eventCh != nil)
	}
}

// TestRunnerRendererErrorJoinsRunner: a renderer failure (an error message)
// requests cancellation but keeps Runner ownership until runnerDoneMsg proves
// the goroutine and event subscription have unwound (design §16).
func TestRunnerRendererErrorClearsOwnership(t *testing.T) {
	ctrl := &executionController{runtime: model.RuntimeRunning}
	m := load(t, testModel(&recordingController{ctrl: ctrl}))
	m.selected = "wf-1"

	m, _ = m.startRunner()
	m2 := step(t, m, model.InvalidInputFault("renderer exploded"))
	if !m2.running || m2.runCancel == nil || m2.eventCh == nil || !m2.quitAfterRunner {
		t.Fatalf("renderer error did not preserve runner join: running=%v runCancel=%v eventCh=%v quitAfterRunner=%v",
			m2.running, m2.runCancel != nil, m2.eventCh != nil, m2.quitAfterRunner)
	}
	m3, cmd := m2.Update(runnerDoneMsg{res: foreground.Result{Reason: foreground.StopCancelled}})
	if cmd == nil {
		t.Fatal("renderer error did not quit after runner completion")
	}
	if m3.(Model).running || m3.(Model).runCancel != nil || m3.(Model).eventCh != nil {
		t.Fatal("runner ownership survived renderer-error completion")
	}
}

func TestRunnerDoneWaitsForPauseCommand(t *testing.T) {
	m := testModel(&recordingController{ctrl: &executionController{runtime: model.RuntimeRunning}})
	m.selected = "wf-1"
	m.running = true
	m.stop = stopPauseAndExit
	m.pauseCommandPending = true

	m2, cmd := m.applyRunnerDone(runnerDoneMsg{res: foreground.Result{Reason: foreground.StopCancelled}})
	if cmd != nil {
		t.Fatal("runner completion quit before the pause command completed")
	}
	if m2.running || !m2.pauseCommandPending || m2.stop != stopPauseAndExit {
		t.Fatalf("runner completion lost the pause join: running=%v pausePending=%v stop=%v", m2.running, m2.pauseCommandPending, m2.stop)
	}

	m3, cmd := m2.applyCommand(commandDoneMsg{cmd: app.PauseWorkflowCommand{Workflow: "wf-1"}})
	if cmd == nil {
		t.Fatal("pause completion did not finish the pending exit")
	}
	if m3.stop != stopPauseAndExit || m3.pauseCommandPending {
		t.Fatalf("pause completion corrupted stop state: stop=%v pausePending=%v", m3.stop, m3.pauseCommandPending)
	}
}

// TestPumpEventsNilChannel is the nil-channel regression test: after a
// terminal path clears eventCh (applyRunnerDone, clearRunner), a pump
// command that is still in flight must terminate with eventsClosedMsg
// instead of blocking forever on a nil channel (a leaked goroutine). The
// timeout guard turns the pre-fix hang into a test failure rather than
// freezing the suite.
func TestPumpEventsNilChannel(t *testing.T) {
	ctrl := &executionController{runtime: model.RuntimeRunning}
	m := testModel(&recordingController{ctrl: ctrl})
	m.eventCh = nil

	cmd := m.pumpEvents("", 0)
	if cmd == nil {
		t.Fatal("pumpEvents returned a nil command")
	}
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	select {
	case msg := <-done:
		if _, ok := msg.(eventsClosedMsg); !ok {
			t.Fatalf("pumpEvents on a nil channel yielded %T, want eventsClosedMsg", msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pumpEvents on a nil channel hung (blocked on the nil channel)")
	}
}

// TestRunnerEventInactiveNoRepump is the stale-event regression test: a
// runnerEventMsg arriving after the runner is no longer active (the
// renderer-error and the runner's event send come from different
// goroutines, so an in-flight event can arrive after clearRunner) must be
// applied to the Execution page but MUST NOT re-pump — re-pumping there
// captures the cleared nil channel and leaks the pump goroutine.
func TestRunnerEventInactiveNoRepump(t *testing.T) {
	m := testModel(&recordingController{ctrl: &executionController{runtime: model.RuntimeRunning}})
	m.running = false
	m.eventCh = nil

	upd, cmd := m.Update(runnerEventMsg{ev: model.Event{Seq: 1, Kind: model.EventRunStarted, Workflow: "wf-1", Text: "run started"}})
	if cmd != nil {
		t.Fatalf("runnerEventMsg while the runner is inactive re-pumped: cmd = %v", cmd)
	}
	mm := upd.(Model)
	if mm.running {
		t.Fatal("runnerEventMsg while inactive set the running flag")
	}
	if len(mm.execution.Log) != 1 {
		t.Fatalf("inactive runnerEventMsg was not applied to the Execution page: log = %v", mm.execution.Log)
	}
}

// TestRunnerEventActiveRepumps guards the normal-flow counterpart: while
// the runner IS active, a runnerEventMsg still re-pumps (the Execution
// page keeps consuming committed events until the runner terminal path).
func TestRunnerEventActiveRepumps(t *testing.T) {
	ch := make(chan model.Event, 1)
	ch <- model.Event{Seq: 2, Kind: model.EventNodeSucceeded, Workflow: "wf-1", Node: "n1"}
	m := testModel(&recordingController{ctrl: &executionController{runtime: model.RuntimeRunning}})
	m.running = true
	m.eventCh = ch

	upd, cmd := m.Update(runnerEventMsg{ev: model.Event{Seq: 1, Kind: model.EventRunStarted, Workflow: "wf-1", Text: "run started"}})
	if cmd == nil {
		t.Fatal("runnerEventMsg while active did not re-pump")
	}
	mm := upd.(Model)
	if !mm.running {
		t.Fatal("runnerEventMsg while active cleared the running flag")
	}
	// The re-pump command reads the next committed event.
	if msg := cmd(); msg == nil {
		t.Fatal("the re-pump command produced no message")
	}
}

// createWithDiscussion drives the requirement discussion setup through
// the Application: create, prepare the native session (managed bootstrap
// binds the Provider's own session id), the Bridge return persists the
// process exit facts and moves the Session to INTERACTIVE_IDLE, freeze the
// Change Set, and finish with the managed structured resume producing the
// strict handoff. Returns the workflow id.
func (fx *tuiFixture) createWithDiscussion(ctx context.Context, a *app.Application) model.WorkflowID {
	fx.t.Helper()
	out, err := a.Execute(ctx, app.CreateWorkflowCommand{Name: "calculator", Provider: "fake", ConfirmDirty: true})
	if err != nil {
		fx.t.Fatalf("create: %v", err)
	}
	wf := out.Workflow
	prep, err := a.Execute(ctx, app.PrepareNativeDiscussionCommand{Workflow: wf, Provider: "fake"})
	if err != nil {
		fx.t.Fatalf("prepare native discussion: %v", err)
	}
	if prep.Native == nil {
		fx.t.Fatal("prepare carried no native bridge request")
	}
	// The Bridge return revalidates the binding and moves the Session to
	// INTERACTIVE_IDLE.
	if _, err := a.Execute(ctx, app.NativeDiscussionReturnCommand{
		Workflow: wf, Session: prep.SessionID,
		Exit:            process.Exit{Code: 0, Fact: process.FactProcessExit},
		Provider:        "fake",
		ProviderSession: prep.Native.ProviderSession,
	}); err != nil {
		fx.t.Fatalf("native discussion return: %v", err)
	}
	frozen, err := a.Execute(ctx, app.FreezeDiscussionCommand{Workflow: wf, Session: prep.SessionID})
	if err != nil {
		fx.t.Fatalf("freeze: %v", err)
	}
	_ = frozen.ChangeSet.Ref
	// Finish drives the managed structured resume on the same provider
	// session that produces the strict handoff from the user's decisions.
	if _, err := a.Execute(ctx, app.FinishDiscussionCommand{
		Workflow: wf, Session: prep.SessionID,
		Decisions: []byte(`{` + handoffContentFields + `}`),
	}); err != nil {
		fx.t.Fatalf("finish: %v", err)
	}
	return wf
}

// approvePlan approves the active plan through the Application.
func (fx *tuiFixture) approvePlan(ctx context.Context, a *app.Application, wf model.WorkflowID) error {
	fx.t.Helper()
	if _, err := a.Execute(ctx, app.GeneratePlanCommand{Workflow: wf, Provider: "fake"}); err != nil {
		return err
	}
	if _, err := a.Execute(ctx, app.CheckPlanCommand{Workflow: wf, Provider: "fake"}); err != nil {
		return err
	}
	view, err := a.Query(ctx, app.PlanQuery{Workflow: wf})
	if err != nil {
		return err
	}
	pv := view.(app.PlanView)
	_, err = a.Execute(ctx, app.ApprovePlanCommand{Workflow: wf, Revision: pv.Revision, Hash: pv.Hash})
	return err
}

// ---------------------------------------------------------------------------
// Return Page: Continue Same Session / Switch Agent launch the native bridge
// ---------------------------------------------------------------------------

// nativeReturnController is the Return Page seam: Execute returns an Outcome
// that carries the managed Native Bridge request facts for the native
// discussion commands, so applyCommand can launch the blocking-exec adapter.
// With native=false the Outcome carries no Native request (the reload
// fallback case).
type nativeReturnController struct {
	executed []app.Command
	native   bool
}

func (c *nativeReturnController) Execute(_ context.Context, cmd app.Command) (app.Outcome, error) {
	c.executed = append(c.executed, cmd)
	out := app.Outcome{Workflow: "wf-1"}
	if c.native {
		out.SessionID = "sess-1"
		out.Native = &app.NativeBridgeRequest{
			Workflow:        "wf-1",
			Session:         "sess-1",
			Provider:        "fake",
			ProviderSession: "prov-sess-1",
			Worktree:        "/cflow/projects/p/wf-1/workspace",
		}
	}
	return out, nil
}

func (c *nativeReturnController) Query(_ context.Context, q app.Query) (app.View, error) {
	if _, ok := q.(app.ProjectWorkspaceQuery); !ok {
		return nil, model.InvalidInputFault("unexpected query")
	}
	return app.WorkspaceView{
		Selected:  "wf-1",
		Workflows: []app.WorkflowSummary{{ID: "wf-1", Runtime: model.RuntimePending}},
		Lifecycle: &app.WorkflowLifecycleView{Status: app.StatusView{Workflow: "wf-1", Stage: model.StageRequirementDiscussion, Runtime: model.RuntimePending}},
		Health: app.HealthView{GitAvailable: true, Providers: []app.ProviderHealth{
			{Name: "fake", Compatible: true},
			{Name: "alt", Compatible: true},
		}},
	}, nil
}

func (*nativeReturnController) DriveOnce(context.Context, model.WorkflowID) (app.DriveOutcome, error) {
	return app.DriveOutcome{}, nil
}

func (*nativeReturnController) EscalateStop() {}

// returnPage builds the root Model on the native discussion Return Page
// with the full action set (Continue is the default selection).
func returnPage(t *testing.T, ctrl controller) Model {
	t.Helper()
	m := newModel(Dependencies{})
	m.ctrl = ctrl
	m = load(t, m)
	m.selected = "wf-1"
	m.page = PageDiscussion
	m.discussion = DiscussionPage{
		Loaded:   true,
		Session:  "sess-1",
		Provider: "fake",
		Actions:  []DiscussionReturnAction{ReturnContinue, ReturnFinish, ReturnSwitch, ReturnPause, ReturnCancel},
	}
	return m
}

// activateReturnAction presses Enter on the current Return action and runs
// the issued typed Application Command to its commandDoneMsg, then returns
// the command applyCommand produced for that result — WITHOUT running it, so
// the test can assert whether it is the native bridge exec or the reload.
func activateReturnAction(t *testing.T, m Model) (Model, tea.Cmd) {
	t.Helper()
	upd, keyCmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = upd.(Model)
	if keyCmd != nil || m.page != PageActionPreview || m.actionPreviewed {
		t.Fatalf("first Enter on the Return page = page %v previewed=%v cmd=%v, want preview", m.page, m.actionPreviewed, keyCmd != nil)
	}
	upd, keyCmd = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = upd.(Model)
	if keyCmd == nil {
		t.Fatal("second Enter on the Return preview produced no command")
	}
	doneMsg := keyCmd()
	done, ok := doneMsg.(commandDoneMsg)
	if !ok {
		t.Fatalf("Enter on the Return page produced %T, want commandDoneMsg", doneMsg)
	}
	m2, execCmd := m.Update(done)
	return m2.(Model), execCmd
}

// isNativeExecMsg reports whether the command is the Bubble Tea blocking-exec
// adapter of the Native Session Bridge: calling it yields the tea.execMsg the
// Program consumes to suspend the renderer and attach the terminal streams.
func isNativeExecMsg(cmd tea.Cmd) bool {
	if cmd == nil {
		return false
	}
	msg := cmd()
	typ := reflect.TypeOf(msg)
	return typ != nil && typ.Name() == "execMsg"
}

// TestModelReturnContinueLaunchesNativeExec proves the Continue Same Session
// action (the default-selected Return action) launches the native bridge
// exec: after the controller's Execute carries the managed Native request,
// applyCommand must produce the blocking-exec adapter (never a plain reload
// that leaves the re-armed Session's RUNNING process phantom).
func TestModelReturnContinueLaunchesNativeExec(t *testing.T) {
	ctrl := &nativeReturnController{native: true}
	m := returnPage(t, ctrl)
	if len(m.discussion.Actions) == 0 || m.discussion.Actions[0] != ReturnContinue {
		t.Fatalf("Continue is not the default Return action: %+v", m.discussion.Actions)
	}
	_, execCmd := activateReturnAction(t, m)
	if len(ctrl.executed) != 1 {
		t.Fatalf("Enter on Continue executed %v", ctrl.executed)
	}
	if _, ok := ctrl.executed[0].(app.ContinueNativeDiscussionCommand); !ok {
		t.Fatalf("Continue command type = %T", ctrl.executed[0])
	}
	if !isNativeExecMsg(execCmd) {
		t.Fatal("Continue did not launch the native bridge exec")
	}
}

func TestPreferredProviderUsesConfiguredClaudeBeforeFake(t *testing.T) {
	got := preferredProvider([]app.ProviderHealth{
		{Name: "claude", Compatible: true},
		{Name: "fake", Compatible: true},
	})
	if got != "claude" {
		t.Fatalf("preferredProvider = %q, want claude", got)
	}
}

func TestPreferredProviderFallsBackToFake(t *testing.T) {
	got := preferredProvider([]app.ProviderHealth{
		{Name: "claude", Compatible: false},
		{Name: "fake", Compatible: true},
	})
	if got != "fake" {
		t.Fatalf("preferredProvider = %q, want fake", got)
	}
}

// TestModelReturnSwitchLaunchesNativeExec proves the Switch Agent action also
// launches the native bridge exec (the switched Session's interactive turn
// must run in the terminal, exactly like the Prepare case).
func TestModelReturnSwitchLaunchesNativeExec(t *testing.T) {
	ctrl := &nativeReturnController{native: true}
	m := returnPage(t, ctrl)
	m = press(t, m, tea.KeyDown, 0) // Finish
	m = press(t, m, tea.KeyDown, 0) // Switch Agent
	if m.discussion.Actions[m.discussion.Selected] != ReturnSwitch {
		t.Fatalf("selected Return action = %v, want switch-agent", m.discussion.Actions[m.discussion.Selected])
	}
	_, execCmd := activateReturnAction(t, m)
	if len(ctrl.executed) != 1 {
		t.Fatalf("Enter on Switch executed %v", ctrl.executed)
	}
	if _, ok := ctrl.executed[0].(app.SwitchAgentCommand); !ok {
		t.Fatalf("Switch command type = %T", ctrl.executed[0])
	}
	if !isNativeExecMsg(execCmd) {
		t.Fatal("Switch did not launch the native bridge exec")
	}
}

// TestModelReturnContinueWithoutNativeReloads guards the fallback: when the
// command outcome carries no Native bridge request, applyCommand must fall
// through to the plain projection reload — never launch an exec with a nil
// request and never wedge the Session.
func TestModelReturnContinueWithoutNativeReloads(t *testing.T) {
	ctrl := &nativeReturnController{native: false}
	m := returnPage(t, ctrl)
	_, execCmd := activateReturnAction(t, m)
	if execCmd == nil {
		t.Fatal("Continue without a native request produced no reload")
	}
	if isNativeExecMsg(execCmd) {
		t.Fatal("Continue without a native bridge request launched the exec")
	}
	msg := execCmd()
	if _, ok := msg.(tea.BatchMsg); !ok {
		t.Fatalf("Continue fallback produced %T, want the projection reload batch", msg)
	}
}
