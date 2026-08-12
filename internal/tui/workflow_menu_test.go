package tui

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"cflow.local/cflow/internal/app"
	"cflow.local/cflow/internal/model"
)

func TestMapWorkflowMenuPreservesGroupsAndDefault(t *testing.T) {
	menu := MapWorkflowMenu(app.WorkflowMenuView{
		Workflow: "wf-1",
		Name:     "calculator",
		Stage:    model.StageRequirementDiscussion,
		Runtime:  model.RuntimePaused,
		Entries: []app.WorkflowMenuEntry{
			{ID: "resume", Group: app.MenuGroupContinue, Kind: app.MenuEntryAction, Label: "Resume Workflow", Action: app.MenuActionResume},
			{ID: "stage", Group: app.MenuGroupView, Kind: app.MenuEntryReadonly, Label: "Current Stage", Route: app.MenuRouteCurrentStage},
		},
		DefaultIndex: 0,
	})
	if menu.Selected != 0 || menu.Items[0].Group != app.MenuGroupContinue {
		t.Fatalf("menu = %+v", menu)
	}
	got := RenderWorkflowMenu(menu)
	if !strings.Contains(got, "CONTINUE") ||
		!strings.Contains(got, "CURRENT STAGE") || !strings.Contains(got, "calculator") {
		t.Fatalf("grouped menu render is incomplete: %s", got)
	}
	if strings.Count(got, "▸") != 1 {
		t.Fatalf("selected marker count = %d, render = %s", strings.Count(got, "▸"), got)
	}
}

func TestRenderWorkflowMenuLoadingAndErrorAreReadOnlyStates(t *testing.T) {
	menu := WorkflowMenuModel{Workflow: "wf-1", Name: "calculator"}
	loading := RenderWorkflowMenuState(menu, "")
	if !strings.Contains(loading, "LOADING") || !strings.Contains(loading, "calculator") {
		t.Fatalf("loading render = %s", loading)
	}
	errorView := RenderWorkflowMenuState(menu, "menu unavailable")
	if !strings.Contains(errorView, "menu unavailable") || strings.Contains(errorView, "Resume") {
		t.Fatalf("error render advertises or hides wrong state: %s", errorView)
	}
}

func TestHomeEnterLoadsWorkflowMenuWithQueryOnly(t *testing.T) {
	ctrl := &workflowMenuController{view: app.WorkflowMenuView{
		Workflow: "wf-1", Name: "calculator", Stage: model.StageRequirementDiscussion, Runtime: model.RuntimePaused,
		Entries: []app.WorkflowMenuEntry{{ID: "stage", Group: app.MenuGroupView, Kind: app.MenuEntryReadonly, Label: "Current Stage", Route: app.MenuRouteCurrentStage}},
	}}
	m := newModel(Dependencies{})
	m.ctrl = ctrl
	m.selected = "wf-1"
	m.workspace.Rows = []WorkflowRow{{Kind: WorkflowRowExisting, ID: "wf-1", Name: "calculator"}}

	next, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = next.(Model)
	if cmd == nil || m.page != PageWorkflowMenu || m.navigation.Current().Layer != LayerWorkflowMenu {
		t.Fatalf("Home Enter = page %v frame %+v cmd=%v", m.page, m.navigation.Current(), cmd != nil)
	}
	if len(ctrl.executed) != 0 {
		t.Fatalf("Home Enter executed commands: %v", ctrl.executed)
	}
	msg := cmd()
	projection, ok := msg.(projectionMsg)
	if !ok {
		t.Fatalf("menu query message = %T", msg)
	}
	query, ok := ctrl.queries[0].(app.WorkflowMenuQuery)
	if !ok || query.Workflow != "wf-1" {
		t.Fatalf("menu query = %#v", ctrl.queries)
	}
	if projection.err != nil {
		t.Fatalf("menu query error = %v", projection.err)
	}
}

func TestWorkflowMenuSelectionDoesNotExecute(t *testing.T) {
	ctrl := &workflowMenuController{}
	m := menuModel(ctrl,
		app.WorkflowMenuEntry{ID: "one", Group: app.MenuGroupContinue, Kind: app.MenuEntryAction, Label: "Resume", Action: app.MenuActionResume},
		app.WorkflowMenuEntry{ID: "two", Group: app.MenuGroupView, Kind: app.MenuEntryReadonly, Label: "Current Stage", Route: app.MenuRouteCurrentStage},
	)

	next, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	m = next.(Model)
	if cmd != nil || m.workflowMenuModel.Selected != 1 {
		t.Fatalf("down selection = %d cmd=%v", m.workflowMenuModel.Selected, cmd != nil)
	}
	next, cmd = m.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	m = next.(Model)
	if cmd != nil || m.workflowMenuModel.Selected != 0 || len(ctrl.executed) != 0 {
		t.Fatalf("up selection = %d cmd=%v executes=%d", m.workflowMenuModel.Selected, cmd != nil, len(ctrl.executed))
	}
}

func TestWorkflowMenuEnterRoutesReadOnlyAndPreviewWithoutExecute(t *testing.T) {
	cases := []struct {
		name       string
		action     app.MenuAction
		route      app.MenuRoute
		page       Page
		queryCheck func(t *testing.T, queries []app.Query)
	}{
		{name: "discussion", action: app.MenuActionContinueDiscussion, page: PageDiscussion, queryCheck: wantQuery[app.DiscussionReturnQuery]},
		{name: "cancel", action: app.MenuActionCancel, page: PageCancel, queryCheck: wantQuery[app.CancelSummaryQuery]},
		{name: "migration", action: app.MenuActionMigrate, page: PageMigration, queryCheck: wantQuery[app.LayoutMigrationPreviewQuery]},
		{name: "resume preview", action: app.MenuActionResume, page: PageActionPreview},
		{name: "pause preview", action: app.MenuActionPause, page: PageActionPreview},
		{name: "runner preview", action: app.MenuActionStartRunner, page: PageActionPreview},
		{name: "apply terminal", action: app.MenuActionApply, page: PageTerminal, queryCheck: wantWorkspaceQuery},
		{name: "cleanup terminal", action: app.MenuActionCleanup, page: PageTerminal, queryCheck: wantWorkspaceQuery},
		{name: "readonly", route: app.MenuRouteCurrentStage, page: PageReadonlyWorkspace, queryCheck: wantQuery[app.StatusQuery]},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := &workflowMenuController{}
			entry := app.WorkflowMenuEntry{ID: tc.name, Group: app.MenuGroupView, Kind: app.MenuEntryReadonly, Label: tc.name, Route: tc.route, Action: tc.action}
			if tc.action != app.MenuActionNone {
				entry.Kind = app.MenuEntryAction
			}
			m := menuModel(ctrl, entry)
			_, itemOK := m.selectedWorkflowMenuItem()
			if !itemOK {
				t.Fatal("menu item was not selected")
			}
			next, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			m = next.(Model)
			if m.page != tc.page || len(ctrl.executed) != 0 {
				t.Fatalf("route = page %v executes=%d, want page %v and no Execute", m.page, len(ctrl.executed), tc.page)
			}
			if tc.queryCheck == nil {
				if cmd != nil {
					t.Fatal("inert route returned a command")
				}
				return
			}
			if cmd == nil {
				t.Fatal("read-only route returned no query")
			}
			_ = cmd()
			tc.queryCheck(t, ctrl.queries)
		})
	}
}

func TestWorkflowMenuRoutesTask8StageEntriesToTypedPages(t *testing.T) {
	cases := []struct {
		name       string
		route      app.MenuRoute
		page       Page
		section    TerminalSection
		queryCheck func(t *testing.T, queries []app.Query)
	}{
		{name: "execution preview", route: app.MenuRouteExecutionApproval, page: PageExecutionApproval, queryCheck: wantQuery[app.ExecutionPreviewQuery]},
		{name: "task graph", route: app.MenuRouteTaskGraph, page: PageReadonlyWorkspace, queryCheck: wantQuery[app.InspectQuery]},
		{name: "report", route: app.MenuRouteReport, page: PageReadonlyWorkspace, queryCheck: wantQuery[app.ReportQuery]},
		{name: "apply", route: app.MenuRouteApply, page: PageTerminal, section: SectionApply, queryCheck: wantWorkspaceQuery},
		{name: "cleanup", route: app.MenuRouteCleanup, page: PageTerminal, section: SectionCleanup, queryCheck: wantWorkspaceQuery},
		{name: "plan evidence", route: app.MenuRoutePlan, page: PageReadonlyWorkspace, queryCheck: wantQuery[app.PlanQuery]},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := &workflowMenuController{}
			entry := app.WorkflowMenuEntry{ID: tc.name, Kind: app.MenuEntryReadonly, Label: tc.name, Route: tc.route}
			if tc.route == app.MenuRouteExecutionApproval {
				entry.Kind = app.MenuEntryAction
				entry.Action = app.MenuActionReviewExecution
			}
			m := menuModel(ctrl, entry)
			next, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			m = next.(Model)
			if m.page != tc.page || len(ctrl.executed) != 0 {
				t.Fatalf("route page=%v executes=%d, want page=%v and no Execute", m.page, len(ctrl.executed), tc.page)
			}
			if m.page == PageTerminal && m.terminal.Section != tc.section {
				t.Fatalf("terminal section=%v, want %v", m.terminal.Section, tc.section)
			}
			if cmd == nil {
				t.Fatal("stage route did not issue a read-only query")
			}
			_ = cmd()
			tc.queryCheck(t, ctrl.queries)
		})
	}
}

func TestReadonlyExecutionPreviewEntryCannotReachApprovalPage(t *testing.T) {
	ctrl := &workflowMenuController{}
	m := menuModel(ctrl, app.WorkflowMenuEntry{
		ID: "execution-preview", Group: app.MenuGroupView, Kind: app.MenuEntryReadonly,
		Label: "Execution Preview", Route: app.MenuRouteExecutionApproval,
	})
	next, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = next.(Model)
	if m.page == PageExecutionApproval {
		t.Fatal("a readonly execution-preview entry reached the mutation-capable approval page")
	}
	if len(ctrl.executed) != 0 || cmd != nil {
		t.Fatalf("malformed readonly execution-preview entry mutated or returned unexpected command: executes=%v cmd=%v", ctrl.executed, cmd != nil)
	}
}

func TestExecutionPreviewActionEntryOpensApprovalPreviewWithoutExecute(t *testing.T) {
	ctrl := &workflowMenuController{}
	m := menuModel(ctrl, app.WorkflowMenuEntry{
		ID: "execution-preview", Group: app.MenuGroupView, Kind: app.MenuEntryAction,
		Label: "Execution Preview", Route: app.MenuRouteExecutionApproval,
		Action: app.MenuActionReviewExecution,
	})
	next, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = next.(Model)
	if m.page != PageExecutionApproval || cmd == nil || len(ctrl.executed) != 0 {
		t.Fatalf("execution-preview action = page %v cmd=%v executes=%v", m.page, cmd != nil, ctrl.executed)
	}
}

func TestWorkflowMenuEscRestoresHomeWorkflowRow(t *testing.T) {
	ctrl := &workflowMenuController{}
	m := menuModel(ctrl,
		app.WorkflowMenuEntry{ID: "stage", Group: app.MenuGroupView, Kind: app.MenuEntryReadonly, Label: "Current Stage", Route: app.MenuRouteCurrentStage},
	)
	m.workspace.Rows = []WorkflowRow{
		{Kind: WorkflowRowNew, Name: "NEW WORKFLOW"},
		{Kind: WorkflowRowExisting, ID: "wf-other", Name: "other"},
		{Kind: WorkflowRowExisting, ID: "wf-1", Name: "calculator"},
	}
	next, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	m = next.(Model)
	if cmd != nil || m.page != PageWorkspace || m.navigation.Current().Layer != LayerHome {
		t.Fatalf("Esc = page %v frame %+v cmd=%v", m.page, m.navigation.Current(), cmd != nil)
	}
	if m.workspace.SelectedRow != 2 {
		t.Fatalf("restored Home row = %d, want workflow row 2", m.workspace.SelectedRow)
	}
}

func menuModel(ctrl *workflowMenuController, entries ...app.WorkflowMenuEntry) Model {
	view := app.WorkflowMenuView{Workflow: "wf-1", Name: "calculator", Stage: model.StageRequirementDiscussion, Runtime: model.RuntimePaused, Entries: entries, DefaultIndex: 0}
	m := newModel(Dependencies{})
	m.ctrl = ctrl
	m.selected = "wf-1"
	m.page = PageWorkflowMenu
	m.workflowMenu = view
	m.workflowMenuModel = MapWorkflowMenu(view)
	m.workflowMenuIndex = m.workflowMenuModel.Selected
	m.navigation = NavigationStack{Frames: []NavigationFrame{
		{Layer: LayerHome, Page: PageWorkspace},
		{Layer: LayerWorkflowMenu, Page: PageWorkflowMenu, Workflow: "wf-1"},
	}}
	return m
}

func wantWorkspaceQuery(t *testing.T, queries []app.Query) {
	t.Helper()
	if len(queries) != 1 {
		t.Fatalf("queries = %#v", queries)
	}
	q, ok := queries[0].(app.ProjectWorkspaceQuery)
	if !ok || q.Selected != "wf-1" {
		t.Fatalf("query = %#v", queries[0])
	}
}

func wantQuery[T app.Query](t *testing.T, queries []app.Query) {
	t.Helper()
	if len(queries) != 1 {
		t.Fatalf("queries = %#v", queries)
	}
	query, ok := queries[0].(T)
	if !ok {
		t.Fatalf("query = %#v, want %T", queries[0], *new(T))
	}
	switch q := any(query).(type) {
	case app.DiscussionReturnQuery:
		if q.Workflow != "wf-1" {
			t.Fatalf("query Workflow = %q, want wf-1", q.Workflow)
		}
	case app.CancelSummaryQuery:
		if q.Workflow != "wf-1" {
			t.Fatalf("query Workflow = %q, want wf-1", q.Workflow)
		}
	case app.LayoutMigrationPreviewQuery:
		if q.Workflow != "wf-1" {
			t.Fatalf("query Workflow = %q, want wf-1", q.Workflow)
		}
	case app.StatusQuery:
		if q.Workflow != "wf-1" {
			t.Fatalf("query Workflow = %q, want wf-1", q.Workflow)
		}
	case app.ExecutionPreviewQuery:
		if q.Workflow != "wf-1" {
			t.Fatalf("query Workflow = %q, want wf-1", q.Workflow)
		}
	case app.InspectQuery:
		if q.Workflow != "wf-1" {
			t.Fatalf("query Workflow = %q, want wf-1", q.Workflow)
		}
	case app.ReportQuery:
		if q.Workflow != "wf-1" {
			t.Fatalf("query Workflow = %q, want wf-1", q.Workflow)
		}
	case app.LogsQuery:
		if q.Workflow != "wf-1" {
			t.Fatalf("query Workflow = %q, want wf-1", q.Workflow)
		}
	case app.PlanQuery:
		if q.Workflow != "wf-1" {
			t.Fatalf("query Workflow = %q, want wf-1", q.Workflow)
		}
	}
}

type workflowMenuController struct {
	queries  []app.Query
	executed []app.Command
	view     app.View
}

func (c *workflowMenuController) Execute(context.Context, app.Command) (app.Outcome, error) {
	c.executed = append(c.executed, nil)
	return app.Outcome{}, nil
}

func (c *workflowMenuController) Query(_ context.Context, q app.Query) (app.View, error) {
	c.queries = append(c.queries, q)
	if c.view != nil {
		return c.view, nil
	}
	return app.WorkspaceView{}, nil
}

func (*workflowMenuController) DriveOnce(context.Context, model.WorkflowID) (app.DriveOutcome, error) {
	return app.DriveOutcome{}, nil
}

func (*workflowMenuController) EscalateStop() {}
