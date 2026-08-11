package tui

import (
	"cflow.local/cflow/internal/app"
	"cflow.local/cflow/internal/model"
)

// WorkspaceLayer is a UI-only navigation level. It never represents or
// changes a Runtime lifecycle state.
type WorkspaceLayer uint8

const (
	LayerHome WorkspaceLayer = iota
	LayerWorkflowMenu
	LayerReadonlyWorkspace
	LayerStageWorkspace
	LayerActionPreview
	LayerCreateWorkspace
	LayerCreatePreview
)

// NavigationFrame preserves the UI state needed to return to one parent.
type NavigationFrame struct {
	Layer    WorkspaceLayer
	Page     Page
	Workflow model.WorkflowID
	Index    int
}

// NavigationStack is the UI-only Home -> Menu -> Workspace route. Value
// operations copy the frame slice so Model copies cannot alias navigation
// mutations through a shared backing array.
type NavigationStack struct {
	Frames []NavigationFrame
}

// Current returns the visible frame. An empty stack safely resolves to Home.
func (s NavigationStack) Current() NavigationFrame {
	if len(s.Frames) == 0 {
		return NavigationFrame{Layer: LayerHome, Page: PageWorkspace}
	}
	return s.Frames[len(s.Frames)-1]
}

// Push returns a stack with frame as its current route.
func (s NavigationStack) Push(frame NavigationFrame) NavigationStack {
	frames := make([]NavigationFrame, len(s.Frames), len(s.Frames)+1)
	copy(frames, s.Frames)
	frames = append(frames, frame)
	return NavigationStack{Frames: frames}
}

// Pop returns the parent stack. Home is a stable root and cannot be popped.
func (s NavigationStack) Pop() (NavigationStack, bool) {
	if len(s.Frames) <= 1 {
		return s, false
	}
	frames := make([]NavigationFrame, len(s.Frames)-1)
	copy(frames, s.Frames[:len(s.Frames)-1])
	return NavigationStack{Frames: frames}, true
}

// ParentPage returns the page one frame below Current. At Home it remains Home.
func (s NavigationStack) ParentPage() Page {
	if len(s.Frames) < 2 {
		return s.Current().Page
	}
	return s.Frames[len(s.Frames)-2].Page
}

func (s NavigationStack) withCurrentIndex(index int) NavigationStack {
	if len(s.Frames) == 0 {
		return s
	}
	frames := make([]NavigationFrame, len(s.Frames))
	copy(frames, s.Frames)
	frames[len(frames)-1].Index = index
	return NavigationStack{Frames: frames}
}

func (m Model) pushNavigation(frame NavigationFrame) Model {
	if m.navigation.Current().Layer == LayerWorkflowMenu {
		m.navigation = m.navigation.withCurrentIndex(m.workflowMenuIndex)
	}
	m.navigation = m.navigation.Push(frame)
	m.page = frame.Page
	if frame.Workflow != "" {
		m.selected = frame.Workflow
	}
	return m
}

func (m Model) popNavigation() (Model, bool) {
	stack, ok := m.navigation.Pop()
	if !ok {
		return m, false
	}
	m.navigation = stack
	parent := stack.Current()
	m.page = parent.Page
	if parent.Workflow != "" {
		m.selected = parent.Workflow
	}
	if parent.Layer == LayerWorkflowMenu {
		m.workflowMenuIndex = parent.Index
	}
	return m, true
}

func (m Model) enterWorkflowMenu() Model {
	if m.selected == "" {
		return m
	}
	return m.pushNavigation(NavigationFrame{
		Layer: LayerWorkflowMenu, Page: PageWorkflowMenu, Workflow: m.selected,
	})
}

func (m Model) enterSelectedWorkflowMenuRoute() Model {
	if m.workflowMenu.Workflow != m.selected ||
		m.workflowMenuIndex < 0 || m.workflowMenuIndex >= len(m.workflowMenu.Entries) {
		return m
	}
	entry := m.workflowMenu.Entries[m.workflowMenuIndex]
	return m.pushNavigation(navigationFrameForMenuEntry(m.selected, entry))
}

func navigationFrameForMenuEntry(workflow model.WorkflowID, entry app.WorkflowMenuEntry) NavigationFrame {
	frame := NavigationFrame{Layer: LayerReadonlyWorkspace, Page: PageReadonlyWorkspace, Workflow: workflow}
	if entry.Kind == app.MenuEntryReadonly {
		return frame
	}

	frame.Layer = LayerActionPreview
	frame.Page = PageActionPreview
	switch entry.Action {
	case app.MenuActionStartDiscussion, app.MenuActionContinueDiscussion:
		frame.Layer, frame.Page = LayerStageWorkspace, PageDiscussion
	case app.MenuActionInspectBlocked:
		frame.Layer, frame.Page = LayerStageWorkspace, PageBlocked
	case app.MenuActionCancel:
		frame.Page = PageCancel
	case app.MenuActionMigrate:
		frame.Page = PageMigration
	case app.MenuActionApply, app.MenuActionCleanup:
		frame.Page = PageTerminal
	}
	return frame
}
