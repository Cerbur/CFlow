package tui

import (
	"context"
	"reflect"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"cflow.local/cflow/internal/app"
	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/observe"
)

func TestReadonlyMenuEntryIssuesOnlyQuery(t *testing.T) {
	ctrl := &readonlyController{}
	m := newModel(Dependencies{})
	m.ctrl = ctrl
	m.selected = "wf-1"
	m.page = PageWorkflowMenu
	m.workflowMenu = app.WorkflowMenuView{
		Workflow: "wf-1",
		Name:     "calculator",
		Stage:    model.StageRequirementDiscussion,
		Runtime:  model.RuntimePaused,
		Entries: []app.WorkflowMenuEntry{{
			ID: "stage", Group: app.MenuGroupView, Kind: app.MenuEntryReadonly,
			Route: app.MenuRouteCurrentStage, Label: "Current Stage",
		}},
	}
	m.workflowMenuModel = MapWorkflowMenu(m.workflowMenu)
	m.workflowMenuIndex = 0
	m.navigation = NavigationStack{Frames: []NavigationFrame{
		{Layer: LayerHome, Page: PageWorkspace},
		{Layer: LayerWorkflowMenu, Page: PageWorkflowMenu, Workflow: "wf-1"},
	}}

	updated, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = updated.(Model)
	if cmd == nil || len(ctrl.executed) != 0 {
		t.Fatalf("readonly Enter mutated or issued no query: cmd=%v executed=%v", cmd != nil, ctrl.executed)
	}
	msg, ok := cmd().(projectionMsg)
	if !ok || msg.err != nil {
		t.Fatalf("readonly query result = %#v", msg)
	}
	if len(ctrl.queries) != 1 {
		t.Fatalf("queries = %#v, want one query", ctrl.queries)
	}
	if _, ok := ctrl.queries[0].(app.StatusQuery); !ok {
		t.Fatalf("query = %T, want app.StatusQuery", ctrl.queries[0])
	}
}

func TestReadonlyRoutesUseOnlyExistingAuthoritativeQueries(t *testing.T) {
	cases := []struct {
		name  string
		route app.MenuRoute
		want  any
	}{
		{name: "current stage", route: app.MenuRouteCurrentStage, want: app.StatusQuery{}},
		{name: "plan evidence", route: app.MenuRoutePlan, want: app.PlanQuery{}},
		{name: "specs", route: app.MenuRouteSpecs, want: app.ExecutionPreviewQuery{}},
		{name: "catalog", route: app.MenuRouteCatalog, want: app.ExecutionPreviewQuery{}},
		{name: "dag", route: app.MenuRouteDAG, want: app.ExecutionPreviewQuery{}},
		{name: "task graph", route: app.MenuRouteTaskGraph, want: app.InspectQuery{}},
		{name: "event log", route: app.MenuRouteLogs, want: app.LogsQuery{}},
		{name: "final report", route: app.MenuRouteReport, want: app.ReportQuery{}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := &readonlyController{}
			m := readonlyMenuModel(ctrl, tc.route)
			updated, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			m = updated.(Model)
			if m.page != PageReadonlyWorkspace || cmd == nil {
				t.Fatalf("route page=%v cmd=%v, want readonly page and query", m.page, cmd != nil)
			}
			if len(ctrl.executed) != 0 {
				t.Fatalf("route executed mutation: %v", ctrl.executed)
			}
			_ = cmd()
			if len(ctrl.queries) != 1 {
				t.Fatalf("queries = %#v, want one query", ctrl.queries)
			}
			if got, want := reflect.TypeOf(ctrl.queries[0]), reflect.TypeOf(tc.want); got != want {
				t.Fatalf("query type = %v, want %v", got, want)
			}
		})
	}
}

func TestReadonlyProjectionRendersQueriedFacts(t *testing.T) {
	ctrl := &readonlyController{}
	m := readonlyMenuModel(ctrl, app.MenuRouteLogs)
	m.ready, m.width, m.height = true, 80, 24
	m = step(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if !m.readonly.Loaded || len(m.readonly.Items) != 1 {
		t.Fatalf("readonly projection = %+v, want one loaded event fact", m.readonly)
	}
	if got := render(m); !strings.Contains(got, "#1: WORKFLOW_CREATED") {
		t.Fatalf("render did not use the queried log fact:\n%s", got)
	}
	if len(ctrl.executed) != 0 {
		t.Fatalf("readonly projection executed mutation: %v", ctrl.executed)
	}
}

func TestReadonlyWorkspaceNavigatesLocallyAndEscUsesParentStack(t *testing.T) {
	m := readonlyMenuModel(&readonlyController{}, app.MenuRouteLogs)
	m.readonly = ReadonlyWorkspaceModel{
		Workflow: "wf-1",
		Name:     "calculator",
		Stage:    model.StageExecution,
		Runtime:  model.RuntimePaused,
		Route:    app.MenuRouteLogs,
		Label:    "Event Log",
		Loaded:   true,
		Items: []ReadonlyWorkspaceItem{
			{Label: "event 1"},
			{Label: "event 2"},
		},
		Selected: 0,
	}
	m.page = PageReadonlyWorkspace
	m.navigation = NavigationStack{Frames: []NavigationFrame{
		{Layer: LayerHome, Page: PageWorkspace},
		{Layer: LayerWorkflowMenu, Page: PageWorkflowMenu, Workflow: "wf-1", Index: 0},
		{Layer: LayerReadonlyWorkspace, Page: PageReadonlyWorkspace, Workflow: "wf-1", Index: 0},
	}}

	updated, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	m = updated.(Model)
	if cmd != nil || m.readonly.Selected != 1 {
		t.Fatalf("readonly local navigation = selected %d cmd=%v", m.readonly.Selected, cmd != nil)
	}
	updated, cmd = m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	m = updated.(Model)
	if cmd != nil || m.page != PageWorkflowMenu || m.navigation.Current().Layer != LayerWorkflowMenu {
		t.Fatalf("readonly Esc = page %v frame %+v cmd=%v", m.page, m.navigation.Current(), cmd != nil)
	}
}

func TestRenderReadonlyWorkspacePreservesFactsAndHasNoMutationFooter(t *testing.T) {
	workspace := ReadonlyWorkspaceModel{
		Workflow: "wf-1",
		Name:     "calculator",
		Stage:    model.StageExecution,
		Runtime:  model.RuntimeRunning,
		Route:    app.MenuRouteTaskGraph,
		Label:    "Task Graph",
		Loaded:   true,
		Inspector: ReadonlyInspector{
			TargetBranch:    "main",
			WorkspaceBranch: "cflow/wf-1/workspace",
			WorkspaceHead:   "abc123",
			PlanRevision:    2,
			PlanStatus:      string(model.PlanApproved),
			PlanHash:        "plan-hash",
		},
		Items: []ReadonlyWorkspaceItem{
			{Label: "task-s01", Value: "RUNNING"},
			{Label: "task-s02", Value: "PENDING"},
		},
		Selected: 0,
	}

	out := RenderReadonlyWorkspace(workspace, 80, 24)
	for _, want := range []string{"WORKSPACE", "workflow: calculator", "stage: EXECUTION", "runtime: RUNNING", "INSPECTOR", "TARGET BRANCH", "task-s01", "↑↓ browse", "Esc back"} {
		if !strings.Contains(out, want) {
			t.Fatalf("readonly render misses %q:\n%s", want, out)
		}
	}
	for _, forbidden := range []string{"Enter execute", "Execute", "Apply", "Cleanup", "y/n"} {
		if strings.Contains(out, forbidden) {
			t.Fatalf("readonly render advertises mutation %q:\n%s", forbidden, out)
		}
	}
	if got := lipgloss.Height(out); got > 24 {
		t.Fatalf("readonly render has %d rows, want <= 24", got)
	}
	for i, line := range strings.Split(out, "\n") {
		if got := lipgloss.Width(line); got > 80 {
			t.Fatalf("readonly row %d width %d > 80: %q", i, got, line)
		}
	}
}

func readonlyMenuModel(ctrl *readonlyController, route app.MenuRoute) Model {
	view := app.WorkflowMenuView{
		Workflow: "wf-1", Name: "calculator", Stage: model.StageExecution, Runtime: model.RuntimePaused,
		Entries:      []app.WorkflowMenuEntry{{ID: "readonly", Group: app.MenuGroupView, Kind: app.MenuEntryReadonly, Label: "Readonly", Route: route}},
		DefaultIndex: 0,
	}
	m := newModel(Dependencies{})
	m.ctrl = ctrl
	m.selected = "wf-1"
	m.page = PageWorkflowMenu
	m.workflowMenu = view
	m.workflowMenuModel = MapWorkflowMenu(view)
	m.workflowMenuIndex = 0
	m.navigation = NavigationStack{Frames: []NavigationFrame{
		{Layer: LayerHome, Page: PageWorkspace},
		{Layer: LayerWorkflowMenu, Page: PageWorkflowMenu, Workflow: "wf-1"},
	}}
	return m
}

type readonlyController struct {
	queries  []app.Query
	executed []app.Command
}

func (c *readonlyController) Execute(context.Context, app.Command) (app.Outcome, error) {
	c.executed = append(c.executed, nil)
	return app.Outcome{}, nil
}

func (c *readonlyController) Query(_ context.Context, q app.Query) (app.View, error) {
	c.queries = append(c.queries, q)
	switch q := q.(type) {
	case app.StatusQuery:
		return app.StatusView{Workflow: q.Workflow, Name: "calculator", Stage: model.StageExecution, Runtime: model.RuntimePaused, TargetBranch: "main"}, nil
	case app.PlanQuery:
		return app.PlanView{Workflow: q.Workflow, Stage: model.StagePlanCheck, Runtime: model.RuntimePaused, Revision: 2, Hash: "plan-hash", PlanStatus: model.PlanApproved, Approved: true}, nil
	case app.ExecutionPreviewQuery:
		return app.ExecutionPreviewView{Workflow: q.Workflow, Stage: model.StageExecution, Runtime: model.RuntimePaused, SpecHashes: []string{"spec-hash"}, CatalogHash: "catalog-hash", WorkflowHash: "workflow-hash"}, nil
	case app.InspectQuery:
		return app.InspectView{Status: app.StatusView{Workflow: q.Workflow, Stage: model.StageExecution, Runtime: model.RuntimePaused}, Nodes: []model.Node{{ID: "task-s01", Status: model.NodeRunning}}}, nil
	case app.LogsQuery:
		return app.LogsView{Events: []model.Event{{Seq: 1, Kind: model.EventWorkflowCreated}}}, nil
	case app.ReportQuery:
		return app.ReportView{Report: observe.Report{Workflow: observe.ReportWorkflow{ID: q.Workflow, Stage: model.StageCompleted, Runtime: model.RuntimeSucceeded}, Result: "PASSED"}, Markdown: "# Final Report"}, nil
	default:
		return nil, model.InvalidInputFault("unexpected query")
	}
}

func (*readonlyController) DriveOnce(context.Context, model.WorkflowID) (app.DriveOutcome, error) {
	return app.DriveOutcome{}, nil
}

func (*readonlyController) EscalateStop() {}
