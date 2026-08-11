package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"cflow.local/cflow/internal/app"
	"cflow.local/cflow/internal/model"
)

// MenuItem is the TUI copy of one Application-owned menu entry. SourceIndex
// keeps the Application ordering and default selection observable without
// making the TUI responsible for inventing entries.
type MenuItem struct {
	SourceIndex int
	ID          string
	Group       app.MenuGroup
	Kind        app.MenuEntryKind
	Label       string
	Route       app.MenuRoute
	Action      app.MenuAction
}

// WorkflowMenuModel is pure presentation and selection state for one loaded
// WorkflowMenuView. It contains no Runtime authority and no command input.
type WorkflowMenuModel struct {
	Workflow model.WorkflowID
	Name     string
	Stage    model.WorkflowStage
	Runtime  model.RuntimeStatus
	Items    []MenuItem
	Selected int
	Loaded   bool
}

// MapWorkflowMenu copies only the entries supplied by Application. The
// selected item is the Application's DefaultIndex, bounded for defensive
// rendering of a malformed or empty projection.
func MapWorkflowMenu(view app.WorkflowMenuView) WorkflowMenuModel {
	items := make([]MenuItem, 0, len(view.Entries))
	for i, entry := range view.Entries {
		items = append(items, MenuItem{
			SourceIndex: i,
			ID:          entry.ID,
			Group:       entry.Group,
			Kind:        entry.Kind,
			Label:       entry.Label,
			Route:       entry.Route,
			Action:      entry.Action,
		})
	}
	selected := view.DefaultIndex
	if len(items) == 0 {
		selected = 0
	} else if selected < 0 || selected >= len(items) {
		selected = 0
	}
	return WorkflowMenuModel{
		Workflow: view.Workflow,
		Name:     view.Name,
		Stage:    view.Stage,
		Runtime:  view.Runtime,
		Items:    items,
		Selected: selected,
		Loaded:   true,
	}
}

// RenderWorkflowMenu renders a loaded menu, or its stable loading state.
func RenderWorkflowMenu(menu WorkflowMenuModel) string {
	return RenderWorkflowMenuState(menu, "")
}

// RenderWorkflowMenuState renders loading and query-error states without
// showing stale or unavailable actions.
func RenderWorkflowMenuState(menu WorkflowMenuModel, errText string) string {
	var b strings.Builder
	b.WriteString("WORKFLOW MENU\n\n")
	fmt.Fprintf(&b, "workflow: %s\nstage: %s\nruntime: %s\n", valueOr(string(menu.Name), string(menu.Workflow)), menu.Stage, menu.Runtime)
	if errText != "" {
		fmt.Fprintf(&b, "\nERROR\n%s\n", errText)
	} else if !menu.Loaded {
		b.WriteString("\nLOADING\nworkflow menu facts…\n")
	} else if len(menu.Items) == 0 {
		b.WriteString("\n(no entries)\n")
	} else {
		b.WriteString("\n")
		lastGroup := app.MenuGroup(255)
		for i, item := range menu.Items {
			if item.Group != lastGroup {
				if lastGroup != app.MenuGroup(255) {
					b.WriteString("\n")
				}
				b.WriteString(menuGroupLabel(item.Group) + "\n")
				lastGroup = item.Group
			}
			marker := "  "
			if i == menu.Selected {
				marker = "▸ "
			}
			fmt.Fprintf(&b, "%s%s\n", marker, strings.ToUpper(item.Label))
		}
	}
	b.WriteString("\n↑↓ select  Enter open  Esc back  / commands")
	return b.String()
}

func menuGroupLabel(group app.MenuGroup) string {
	switch group {
	case app.MenuGroupContinue:
		return "CONTINUE"
	case app.MenuGroupView:
		return "VIEW"
	case app.MenuGroupControl:
		return "CONTROL"
	default:
		return "MENU"
	}
}

func (m Model) selectedWorkflowMenuItem() (MenuItem, bool) {
	menu := m.workflowMenuModel
	if !menu.Loaded && m.workflowMenu.Workflow == m.selected && len(m.workflowMenu.Entries) > 0 {
		menu = MapWorkflowMenu(m.workflowMenu)
		if m.workflowMenuIndex >= 0 && m.workflowMenuIndex < len(menu.Items) {
			menu.Selected = m.workflowMenuIndex
		}
	}
	if !menu.Loaded || m.workflowMenu.Workflow != m.selected {
		return MenuItem{}, false
	}
	idx := menu.Selected
	if idx < 0 || idx >= len(menu.Items) {
		return MenuItem{}, false
	}
	return menu.Items[idx], true
}

func (m Model) moveWorkflowMenuSelection(delta int) Model {
	if !m.workflowMenuModel.Loaded || len(m.workflowMenuModel.Items) == 0 {
		return m
	}
	idx := m.workflowMenuModel.Selected + delta
	if idx < 0 {
		idx = len(m.workflowMenuModel.Items) - 1
	}
	if idx >= len(m.workflowMenuModel.Items) {
		idx = 0
	}
	m.workflowMenuModel.Selected = idx
	m.workflowMenuIndex = idx
	m.navigation = m.navigation.withCurrentIndex(idx)
	return m
}

func (m *Model) restoreHomeWorkflowRow() {
	if m == nil {
		return
	}
	for i, row := range m.workspace.Rows {
		if row.Kind == WorkflowRowExisting && row.ID == m.selected {
			m.workspace.SelectedRow = i
			return
		}
	}
	if m.selected == "" && len(m.workspace.Rows) > 0 && m.workspace.Rows[0].Kind == WorkflowRowNew {
		m.workspace.SelectedRow = 0
	}
}

// routeWorkflowMenuItem selects an existing page and, where that page has an
// existing projection, requests facts through Query. It never calls Execute.
func (m Model) routeWorkflowMenuItem(item MenuItem) (Model, tea.Cmd) {
	m.workflowMenuIndex = item.SourceIndex
	m.workflowMenuPreviewItem = item
	switch item.Action {
	case app.MenuActionStartDiscussion, app.MenuActionContinueDiscussion:
		m.discussion = DiscussionPage{}
		m = m.pushNavigation(NavigationFrame{Layer: LayerStageWorkspace, Page: PageDiscussion, Workflow: m.selected})
		return m, m.queryCmd(PageDiscussion, app.DiscussionReturnQuery{Workflow: m.selected})
	case app.MenuActionCancel:
		m.cancel = app.CancelSummaryView{}
		m = m.pushNavigation(NavigationFrame{Layer: LayerStageWorkspace, Page: PageCancel, Workflow: m.selected})
		return m, m.queryCmd(PageCancel, app.CancelSummaryQuery{Workflow: m.selected})
	case app.MenuActionMigrate:
		m.migration = app.MigrationPreviewView{}
		m.migrationConfirm = migrationConfirmNone
		m = m.pushNavigation(NavigationFrame{Layer: LayerStageWorkspace, Page: PageMigration, Workflow: m.selected})
		return m, m.queryCmd(PageMigration, app.LayoutMigrationPreviewQuery{Workflow: m.selected})
	case app.MenuActionResume, app.MenuActionPause, app.MenuActionStartRunner:
		m = m.pushNavigation(NavigationFrame{Layer: LayerActionPreview, Page: PageActionPreview, Workflow: m.selected})
		return m, nil
	case app.MenuActionApply, app.MenuActionCleanup:
		m.terminal = NewTerminalModel()
		if item.Action == app.MenuActionApply {
			m.terminal.Section = SectionApply
		} else {
			m.terminal.Section = SectionCleanup
		}
		m = m.pushNavigation(NavigationFrame{LayerStageWorkspace, PageTerminal, m.selected, item.SourceIndex})
		return m, m.queryCmd(PageTerminal, app.ProjectWorkspaceQuery{Selected: m.selected})
	case app.MenuActionInspectBlocked:
		m = m.pushNavigation(NavigationFrame{LayerStageWorkspace, PageBlocked, m.selected, item.SourceIndex})
		return m, m.queryCmd(PageBlocked, app.ProjectWorkspaceQuery{Selected: m.selected})
	}

	switch item.Route {
	case app.MenuRouteExecutionApproval:
		m.approval = ApprovalModel{Preview: m.preview}
		m = m.pushNavigation(NavigationFrame{Layer: LayerStageWorkspace, Page: PageExecutionApproval, Workflow: m.selected, Index: item.SourceIndex})
		return m, m.queryCmd(PageExecutionApproval, app.ExecutionPreviewQuery{Workflow: m.selected})
	case app.MenuRouteExecution:
		m = m.pushNavigation(NavigationFrame{Layer: LayerStageWorkspace, Page: PageExecution, Workflow: m.selected, Index: item.SourceIndex})
		return m, m.queryCmd(PageExecution, app.ProjectWorkspaceQuery{Selected: m.selected})
	case app.MenuRouteDiscussion:
		m.discussion = DiscussionPage{}
		m = m.pushNavigation(NavigationFrame{Layer: LayerStageWorkspace, Page: PageDiscussion, Workflow: m.selected, Index: item.SourceIndex})
		return m, m.queryCmd(PageDiscussion, app.DiscussionReturnQuery{Workflow: m.selected})
	case app.MenuRouteCancel:
		m.cancel = app.CancelSummaryView{}
		m = m.pushNavigation(NavigationFrame{Layer: LayerStageWorkspace, Page: PageCancel, Workflow: m.selected, Index: item.SourceIndex})
		return m, m.queryCmd(PageCancel, app.CancelSummaryQuery{Workflow: m.selected})
	case app.MenuRouteMigration:
		m.migration = app.MigrationPreviewView{}
		m = m.pushNavigation(NavigationFrame{Layer: LayerStageWorkspace, Page: PageMigration, Workflow: m.selected, Index: item.SourceIndex})
		return m, m.queryCmd(PageMigration, app.LayoutMigrationPreviewQuery{Workflow: m.selected})
	case app.MenuRouteApply, app.MenuRouteCleanup:
		m.terminal = NewTerminalModel()
		switch item.Route {
		case app.MenuRouteApply:
			m.terminal.Section = SectionApply
		case app.MenuRouteCleanup:
			m.terminal.Section = SectionCleanup
		}
		m = m.pushNavigation(NavigationFrame{Layer: LayerStageWorkspace, Page: PageTerminal, Workflow: m.selected, Index: item.SourceIndex})
		return m, m.queryCmd(PageTerminal, app.ProjectWorkspaceQuery{Selected: m.selected})
	case app.MenuRouteCurrentStage, app.MenuRoutePlan, app.MenuRouteSpecs, app.MenuRouteCatalog,
		app.MenuRouteDAG, app.MenuRouteTaskGraph, app.MenuRouteLogs, app.MenuRouteReport:
		m.readonly = ReadonlyWorkspaceModel{
			Workflow: m.selected,
			Name:     valueOr(m.workflowMenuModel.Name, string(m.selected)),
			Stage:    m.workflowMenuModel.Stage,
			Runtime:  m.workflowMenuModel.Runtime,
			Route:    item.Route,
			Label:    item.Label,
		}
		m = m.pushNavigation(NavigationFrame{Layer: LayerReadonlyWorkspace, Page: PageReadonlyWorkspace, Workflow: m.selected, Index: item.SourceIndex})
		query, ok := readonlyQueryForRoute(item.Route, m.selected, m.deps.CLI.Build)
		if !ok {
			m.readonly.Loaded = true
			m.readonly.Error = "no authoritative query is available"
			return m, nil
		}
		return m, m.queryCmd(PageReadonlyWorkspace, query)
	}
	m = m.pushNavigation(NavigationFrame{LayerReadonlyWorkspace, PageReadonlyWorkspace, m.selected, item.SourceIndex})
	return m, nil
}

func renderWorkflowActionPreview(menu WorkflowMenuModel, item MenuItem) string {
	var b strings.Builder
	b.WriteString("ACTION PREVIEW\n\n")
	fmt.Fprintf(&b, "workflow: %s\nstage: %s\nruntime: %s\n\n", valueOr(menu.Name, string(menu.Workflow)), menu.Stage, menu.Runtime)
	if item.Label != "" {
		fmt.Fprintf(&b, "selected action: %s\n", item.Label)
	}
	b.WriteString("\nNo mutation has been issued.\nEnter is reserved for the later action confirmation.\n\n↑↓ select  Enter confirm  Esc back  / commands")
	return b.String()
}
