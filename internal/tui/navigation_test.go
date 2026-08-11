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
			{ID: "plan", Kind: app.MenuEntryReadonly, Route: app.MenuRoutePlan},
		},
	}
	m.workflowMenuIndex = 1
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
	if cmd != nil {
		t.Fatal("Home Enter returned a command before Task 5 menu loading")
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
