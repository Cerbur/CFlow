package tui

import (
	"context"
	"testing"

	tea "charm.land/bubbletea/v2"

	"cflow.local/cflow/internal/app"
	"cflow.local/cflow/internal/model"
)

func TestNavigationStackHomeMenuStageEsc(t *testing.T) {
	stack := NavigationStack{
		Frames: []NavigationFrame{{Layer: LayerHome, Page: PageWorkspace}},
	}
	stack = stack.Push(NavigationFrame{
		Layer: LayerWorkflowMenu, Page: PageWorkflowMenu, Workflow: "wf-1",
	})
	stack = stack.Push(NavigationFrame{
		Layer: LayerStageWorkspace, Page: PageDiscussion, Workflow: "wf-1",
	})

	if got := stack.Current().Page; got != PageDiscussion {
		t.Fatalf("current page = %v", got)
	}
	var ok bool
	stack, ok = stack.Pop()
	if !ok || stack.Current().Page != PageWorkflowMenu {
		t.Fatalf("first pop = %+v, ok=%v", stack, ok)
	}
	stack, ok = stack.Pop()
	if !ok || stack.Current().Page != PageWorkspace {
		t.Fatalf("second pop = %+v, ok=%v", stack, ok)
	}
	_, ok = stack.Pop()
	if ok {
		t.Fatal("Home pop unexpectedly exited the TUI")
	}
}

func TestModelHomeEscIsNoOp(t *testing.T) {
	m := newModel(Dependencies{})
	m.selected = "wf-1"

	next, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	got := next.(Model)
	if cmd != nil {
		t.Fatal("Home Esc returned a command, including tea.Quit")
	}
	if got.page != PageWorkspace || got.selected != m.selected || len(got.navigation.Frames) != 1 {
		t.Fatalf("Home Esc changed model: page=%v selected=%q navigation=%+v", got.page, got.selected, got.navigation)
	}
}

func TestModelParentReturnRestoresWorkflowMenuIndexWithoutExecute(t *testing.T) {
	ctrl := &navigationController{}
	m := newModel(Dependencies{})
	m.ctrl = ctrl
	m.selected = "wf-1"
	m.page = PageWorkflowMenu
	m.workflowMenu = app.WorkflowMenuView{
		Workflow: "wf-1",
		Entries: []app.WorkflowMenuEntry{
			{ID: "stage", Kind: app.MenuEntryReadonly, Route: app.MenuRouteCurrentStage},
			{ID: "stage-details", Kind: app.MenuEntryReadonly, Route: app.MenuRouteCurrentStage},
		},
	}
	m.workflowMenuModel = MapWorkflowMenu(m.workflowMenu)
	m.workflowMenuIndex = 1
	m.workflowMenuModel.Selected = 1
	m.navigation = NavigationStack{Frames: []NavigationFrame{
		{Layer: LayerHome, Page: PageWorkspace},
		{Layer: LayerWorkflowMenu, Page: PageWorkflowMenu, Workflow: "wf-1"},
	}}

	next, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd != nil {
		t.Fatal("readonly navigation returned a command")
	}
	m = next.(Model)
	if m.page != PageReadonlyWorkspace || m.navigation.Current().Layer != LayerReadonlyWorkspace {
		t.Fatalf("menu Enter route = page %v frame %+v", m.page, m.navigation.Current())
	}

	next, cmd = m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	if cmd != nil {
		t.Fatal("parent return returned a command")
	}
	m = next.(Model)
	if m.page != PageWorkflowMenu || m.workflowMenuIndex != 1 {
		t.Fatalf("parent return = page %v index %d", m.page, m.workflowMenuIndex)
	}
	if ctrl.executes != 0 {
		t.Fatalf("navigation called Execute %d times", ctrl.executes)
	}
}

func TestModelHomeEnterPushesWorkflowMenuWithoutExecute(t *testing.T) {
	ctrl := &navigationController{}
	m := newModel(Dependencies{})
	m.ctrl = ctrl
	m.selected = "wf-1"

	next, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("Home Enter did not request the WorkflowMenu projection")
	}
	m = next.(Model)
	if m.page != PageWorkflowMenu || m.navigation.Current().Workflow != "wf-1" {
		t.Fatalf("Home Enter = page %v frame %+v", m.page, m.navigation.Current())
	}
	if ctrl.executes != 0 {
		t.Fatalf("Home Enter called Execute %d times", ctrl.executes)
	}
}

func TestQuitClassificationDoesNotTreatQAsExit(t *testing.T) {
	if IsQuit(tea.KeyPressMsg{Code: 'q'}) {
		t.Fatal("q is still classified as an exit key")
	}
	m := newModel(Dependencies{})
	next, cmd := m.Update(tea.KeyPressMsg{Code: 'q'})
	if cmd != nil || next.(Model).page != PageWorkspace {
		t.Fatalf("q changed root state: page=%v cmd=%v", next.(Model).page, cmd != nil)
	}
}

func TestNestedNavigationLegacyKeysPreserveStack(t *testing.T) {
	for _, code := range []rune{tea.KeyTab, tea.KeyLeft, tea.KeyRight} {
		m := nestedNavigationModel()
		next, cmd := m.Update(tea.KeyPressMsg{Code: code})
		got := next.(Model)
		if cmd != nil {
			t.Fatalf("key %q returned a legacy navigation command", code)
		}
		if got.page != PageDiscussion || got.navigation.Current().Page != PageDiscussion {
			t.Fatalf("key %q desynchronized page/frame: page=%v frame=%+v", code, got.page, got.navigation.Current())
		}
	}

	m := nestedNavigationModel()
	next, cmd := m.Update(tea.KeyPressMsg{Code: 'b'})
	got := next.(Model)
	if cmd != nil || got.page != PageWorkspace || len(got.navigation.Frames) != 1 || got.navigation.Current().Layer != LayerHome {
		t.Fatalf("b did not reset navigation: page=%v stack=%+v cmd=%v", got.page, got.navigation, cmd != nil)
	}

	next, cmd = got.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	got = next.(Model)
	if cmd == nil || got.page != PageWorkflowMenu || got.navigation.Current().Layer != LayerWorkflowMenu {
		t.Fatalf("Enter after b did not start a fresh menu route: page=%v stack=%+v", got.page, got.navigation)
	}
}

func TestNestedNavigationEscPopsExactlyOneFrame(t *testing.T) {
	m := nestedNavigationModel()

	next, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	m = next.(Model)
	if cmd != nil || m.page != PageWorkflowMenu || m.navigation.Current().Layer != LayerWorkflowMenu {
		t.Fatalf("first Esc = page %v stack=%+v cmd=%v", m.page, m.navigation, cmd != nil)
	}
	next, cmd = m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	m = next.(Model)
	if cmd != nil || m.page != PageWorkspace || len(m.navigation.Frames) != 1 {
		t.Fatalf("second Esc = page %v stack=%+v cmd=%v", m.page, m.navigation, cmd != nil)
	}
}

func TestWorkflowMenuRoutesNonStageActionsToInertPreview(t *testing.T) {
	cases := []struct {
		name   string
		action app.MenuAction
		layer  WorkspaceLayer
		page   Page
	}{
		{name: "readonly", action: app.MenuActionNone, layer: LayerReadonlyWorkspace, page: PageReadonlyWorkspace},
		{name: "start discussion", action: app.MenuActionStartDiscussion, layer: LayerStageWorkspace, page: PageDiscussion},
		{name: "continue discussion", action: app.MenuActionContinueDiscussion, layer: LayerStageWorkspace, page: PageDiscussion},
		{name: "inspect blocked", action: app.MenuActionInspectBlocked, layer: LayerStageWorkspace, page: PageBlocked},
		{name: "resume", action: app.MenuActionResume, layer: LayerActionPreview, page: PageActionPreview},
		{name: "start runner", action: app.MenuActionStartRunner, layer: LayerActionPreview, page: PageActionPreview},
		{name: "pause", action: app.MenuActionPause, layer: LayerActionPreview, page: PageActionPreview},
		{name: "cancel", action: app.MenuActionCancel, layer: LayerStageWorkspace, page: PageCancel},
		{name: "migrate", action: app.MenuActionMigrate, layer: LayerStageWorkspace, page: PageMigration},
		{name: "apply", action: app.MenuActionApply, layer: LayerStageWorkspace, page: PageTerminal},
		{name: "cleanup", action: app.MenuActionCleanup, layer: LayerStageWorkspace, page: PageTerminal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := &navigationController{}
			m := newModel(Dependencies{})
			m.ctrl = ctrl
			m.selected = "wf-1"
			m.page = PageWorkflowMenu
			m.workflowMenu = app.WorkflowMenuView{
				Workflow: "wf-1",
				Entries:  []app.WorkflowMenuEntry{{Kind: app.MenuEntryAction, Action: tc.action}},
			}
			if tc.name == "readonly" {
				m.workflowMenu.Entries[0].Kind = app.MenuEntryReadonly
			}
			m.workflowMenuModel = MapWorkflowMenu(m.workflowMenu)
			m.navigation = NavigationStack{Frames: []NavigationFrame{
				{Layer: LayerHome, Page: PageWorkspace},
				{Layer: LayerWorkflowMenu, Page: PageWorkflowMenu, Workflow: "wf-1"},
			}}

			next, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			got := next.(Model)
			if ctrl.executes != 0 {
				t.Fatalf("menu Enter executed work: cmd=%v executes=%d", cmd != nil, ctrl.executes)
			}
			if got.navigation.Current().Layer != tc.layer || got.page != tc.page {
				t.Fatalf("route = layer %v page %v, want layer %v page %v", got.navigation.Current().Layer, got.page, tc.layer, tc.page)
			}
		})
	}
}

func nestedNavigationModel() Model {
	m := newModel(Dependencies{})
	m.selected = "wf-1"
	m.page = PageDiscussion
	m.navigation = NavigationStack{Frames: []NavigationFrame{
		{Layer: LayerHome, Page: PageWorkspace},
		{Layer: LayerWorkflowMenu, Page: PageWorkflowMenu, Workflow: "wf-1", Index: 2},
		{Layer: LayerStageWorkspace, Page: PageDiscussion, Workflow: "wf-1"},
	}}
	return m
}

type navigationController struct {
	executes int
}

func (c *navigationController) Execute(context.Context, app.Command) (app.Outcome, error) {
	c.executes++
	return app.Outcome{}, nil
}

func (*navigationController) Query(context.Context, app.Query) (app.View, error) {
	return nil, nil
}

func (*navigationController) DriveOnce(context.Context, model.WorkflowID) (app.DriveOutcome, error) {
	return app.DriveOutcome{}, nil
}

func (*navigationController) EscalateStop() {}
