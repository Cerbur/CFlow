package tui

import (
	"fmt"
	"strings"

	"cflow.local/cflow/internal/app"
	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/observe"
)

// ReadonlyWorkspaceItem is one bounded fact in a read-only route. Label and
// Value are copied from an Application view; they are never interpreted as a
// command or a lifecycle transition.
type ReadonlyWorkspaceItem struct {
	Label string
	Value string
}

// ReadonlyInspector carries the subset of authoritative status facts useful
// beside every read-only route. Empty fields remain unavailable rather than
// being inferred from the selected route or from display text.
type ReadonlyInspector struct {
	TargetBranch    string
	WorkspaceBranch string
	WorkspaceHead   string
	PlanRevision    int
	PlanStatus      string
	PlanHash        string
}

// ReadonlyWorkspaceModel is the UI-only projection for one menu route. The
// Application view remains the authority; this model only bounds content and
// owns local list selection.
type ReadonlyWorkspaceModel struct {
	Workflow  model.WorkflowID
	Name      string
	Stage     model.WorkflowStage
	Runtime   model.RuntimeStatus
	Route     app.MenuRoute
	Label     string
	Items     []ReadonlyWorkspaceItem
	Selected  int
	Loaded    bool
	Error     string
	Inspector ReadonlyInspector
}

// readonlyQueryForRoute maps the closed menu route set to existing typed
// Application queries. There is deliberately no query for an invented
// Specs/Catalog/DAG view: the three artifact routes reuse the existing
// execution preview only after the Application has established that preview
// facts exist.
func readonlyQueryForRoute(route app.MenuRoute, workflow model.WorkflowID, build observe.BuildInfo) (app.Query, bool) {
	switch route {
	case app.MenuRouteCurrentStage:
		return app.StatusQuery{Workflow: workflow}, true
	case app.MenuRoutePlan:
		return app.PlanQuery{Workflow: workflow}, true
	case app.MenuRouteSpecs, app.MenuRouteCatalog, app.MenuRouteDAG:
		return app.ExecutionPreviewQuery{Workflow: workflow}, true
	case app.MenuRouteTaskGraph:
		return app.InspectQuery{Workflow: workflow}, true
	case app.MenuRouteLogs:
		return app.LogsQuery{Workflow: workflow, Limit: 200}, true
	case app.MenuRouteReport:
		return app.ReportQuery{Workflow: workflow, Build: build}, true
	default:
		return nil, false
	}
}

// MapReadonlyWorkspace copies one authoritative view into the bounded route
// model. A view that has no route-specific facts becomes an explicit,
// bounded unavailable state; no text is fabricated for it.
func MapReadonlyWorkspace(menu WorkflowMenuModel, route app.MenuRoute, view app.View) ReadonlyWorkspaceModel {
	result := ReadonlyWorkspaceModel{
		Workflow: menu.Workflow,
		Name:     valueOr(menu.Name, string(menu.Workflow)),
		Stage:    menu.Stage,
		Runtime:  menu.Runtime,
		Route:    route,
		Label:    readonlyRouteLabel(route),
		Loaded:   true,
	}

	var items []ReadonlyWorkspaceItem
	switch v := view.(type) {
	case app.StatusView:
		result.Stage, result.Runtime = v.Stage, v.Runtime
		result.Inspector = readonlyInspector(v)
		items = []ReadonlyWorkspaceItem{
			{Label: "stage", Value: string(v.Stage)},
			{Label: "runtime", Value: string(v.Runtime)},
		}
		if v.LayoutVersion > 0 {
			items = append(items, ReadonlyWorkspaceItem{Label: "layout", Value: fmt.Sprintf("v%d", v.LayoutVersion)})
		}
	case app.PlanView:
		result.Stage, result.Runtime = v.Stage, v.Runtime
		items = []ReadonlyWorkspaceItem{
			{Label: "revision", Value: fmt.Sprintf("%d", v.Revision)},
			{Label: "status", Value: valueOr(string(v.PlanStatus), "unavailable")},
			{Label: "hash", Value: valueOr(v.Hash, "unavailable")},
			{Label: "approved", Value: fmt.Sprintf("%t", v.Approved)},
		}
	case app.ExecutionPreviewView:
		result.Stage, result.Runtime = v.Stage, v.Runtime
		items = readonlyExecutionItems(route, v)
	case app.InspectView:
		result.Stage, result.Runtime = v.Status.Stage, v.Status.Runtime
		result.Inspector = readonlyInspector(v.Status)
		switch route {
		case app.MenuRouteTaskGraph:
			for _, node := range v.Nodes {
				items = append(items, ReadonlyWorkspaceItem{
					Label: string(node.ID),
					Value: fmt.Sprintf("%s · %s", node.Kind, node.Status),
				})
			}
		}
	case app.LogsView:
		for _, event := range v.Events {
			value := string(event.Kind)
			if event.Text != "" {
				value += " · " + event.Text
			}
			items = append(items, ReadonlyWorkspaceItem{Label: fmt.Sprintf("#%d", event.Seq), Value: value})
		}
	case app.ReportView:
		result.Stage, result.Runtime = v.Report.Workflow.Stage, v.Report.Workflow.Runtime
		result.Inspector = ReadonlyInspector{
			TargetBranch:    v.Report.Workflow.TargetBranch,
			WorkspaceBranch: v.Report.Workflow.WorkspaceBranch,
			WorkspaceHead:   v.Report.Workflow.VerifiedWorkspaceHead,
			PlanRevision:    v.Report.Workflow.PlanRevision,
		}
		items = append(items,
			ReadonlyWorkspaceItem{Label: "result", Value: valueOr(v.Report.Result, "unavailable")},
			ReadonlyWorkspaceItem{Label: "tasks", Value: fmt.Sprintf("%d completed / %d", v.Report.Summary.Completed, v.Report.Summary.Tasks)},
		)
		for _, line := range boundedReportLines(v.Markdown, 64) {
			items = append(items, ReadonlyWorkspaceItem{Label: "report", Value: line})
		}
	default:
		result.Error = "no authoritative view is available for this route"
	}
	if len(items) == 0 && result.Error == "" {
		items = []ReadonlyWorkspaceItem{{Label: "facts", Value: "unavailable"}}
	}
	result.Items = items
	return result
}

func readonlyProjectionMatches(route app.MenuRoute, workflow model.WorkflowID, view app.View) bool {
	switch route {
	case app.MenuRouteCurrentStage:
		v, ok := view.(app.StatusView)
		return ok && v.Workflow == workflow
	case app.MenuRoutePlan:
		v, ok := view.(app.PlanView)
		return ok && v.Workflow == workflow
	case app.MenuRouteSpecs, app.MenuRouteCatalog, app.MenuRouteDAG:
		v, ok := view.(app.ExecutionPreviewView)
		return ok && v.Workflow == workflow
	case app.MenuRouteTaskGraph:
		v, ok := view.(app.InspectView)
		return ok && v.Status.Workflow == workflow
	case app.MenuRouteLogs:
		_, ok := view.(app.LogsView)
		return ok
	case app.MenuRouteReport:
		v, ok := view.(app.ReportView)
		return ok && v.Report.Workflow.ID == workflow
	default:
		return false
	}
}

func readonlyRouteLabel(route app.MenuRoute) string {
	switch route {
	case app.MenuRouteCurrentStage:
		return "Current Stage"
	case app.MenuRoutePlan:
		return "Plan / Evidence"
	case app.MenuRouteSpecs:
		return "Specs"
	case app.MenuRouteCatalog:
		return "Verification Catalog"
	case app.MenuRouteDAG:
		return "Workflow DAG"
	case app.MenuRouteTaskGraph:
		return "Task Graph"
	case app.MenuRouteLogs:
		return "Event Log"
	case app.MenuRouteReport:
		return "Final Report"
	default:
		return "Read-only Workspace"
	}
}

func readonlyInspector(status app.StatusView) ReadonlyInspector {
	head := status.VerifiedWorkspaceHead
	if head == "" {
		head = status.CandidateWorkspaceHead
	}
	return ReadonlyInspector{
		TargetBranch:    status.TargetBranch,
		WorkspaceBranch: status.WorkspaceBranch,
		WorkspaceHead:   head,
		PlanRevision:    status.PlanRevision,
		PlanStatus:      valueOr(string(status.PlanStatus), ""),
		PlanHash:        status.PlanHash,
	}
}

func readonlyExecutionItems(route app.MenuRoute, view app.ExecutionPreviewView) []ReadonlyWorkspaceItem {
	items := make([]ReadonlyWorkspaceItem, 0, 8)
	switch route {
	case app.MenuRouteSpecs:
		if view.Spec != nil {
			items = append(items, ReadonlyWorkspaceItem{Label: "artifact", Value: view.Spec.String()})
		}
		for i, hash := range view.SpecHashes {
			items = append(items, ReadonlyWorkspaceItem{Label: fmt.Sprintf("spec %d", i+1), Value: hash})
		}
	case app.MenuRouteCatalog:
		if view.Catalog != nil {
			items = append(items, ReadonlyWorkspaceItem{Label: "artifact", Value: view.Catalog.String()})
		}
		items = append(items, ReadonlyWorkspaceItem{Label: "hash", Value: valueOr(view.CatalogHash, "unavailable")})
		for _, command := range view.CommandIdentities {
			items = append(items, ReadonlyWorkspaceItem{Label: command.CommandID, Value: command.Purpose})
		}
	case app.MenuRouteDAG:
		if view.WorkflowArtifact != nil {
			items = append(items, ReadonlyWorkspaceItem{Label: "artifact", Value: view.WorkflowArtifact.String()})
		}
		items = append(items, ReadonlyWorkspaceItem{Label: "hash", Value: valueOr(view.WorkflowHash, "unavailable")})
		for i, group := range view.ParallelGroups {
			items = append(items, ReadonlyWorkspaceItem{Label: fmt.Sprintf("parallel group %d", i+1), Value: strings.Join(group, ", ")})
		}
	}
	return items
}

func boundedReportLines(markdown string, limit int) []string {
	if markdown == "" {
		return nil
	}
	lines := strings.Split(strings.ReplaceAll(markdown, "\r\n", "\n"), "\n")
	if len(lines) > limit {
		lines = lines[:limit]
	}
	return lines
}

func moveReadonlySelection(m Model, delta int) Model {
	if len(m.readonly.Items) == 0 {
		return m
	}
	index := m.readonly.Selected + delta
	if index < 0 {
		index = len(m.readonly.Items) - 1
	}
	if index >= len(m.readonly.Items) {
		index = 0
	}
	m.readonly.Selected = index
	return m
}

// RenderReadonlyWorkspace renders bounded facts with a parent-return footer.
// It intentionally contains no mutation or confirmation affordance.
func RenderReadonlyWorkspace(m ReadonlyWorkspaceModel, width, height int) string {
	lines := []string{
		"WORKSPACE",
		"",
		"workflow: " + valueOr(m.Name, string(m.Workflow)),
		"stage: " + valueOr(string(m.Stage), "unavailable"),
		"runtime: " + valueOr(string(m.Runtime), "unavailable"),
		"",
		strings.ToUpper(m.Label),
	}
	if m.Error != "" {
		lines = append(lines, "unavailable: "+m.Error)
	} else if !m.Loaded {
		lines = append(lines, "loading authoritative facts…")
	} else {
		for i, item := range m.Items {
			marker := "  "
			if i == m.Selected {
				marker = "▸ "
			}
			lines = append(lines, marker+item.Label+": "+valueOr(item.Value, "unavailable"))
		}
	}

	inspector := []string{"", "INSPECTOR"}
	inspector = appendReadonlyInspector(inspector, "TARGET BRANCH", m.Inspector.TargetBranch)
	inspector = appendReadonlyInspector(inspector, "WORKSPACE BRANCH", m.Inspector.WorkspaceBranch)
	inspector = appendReadonlyInspector(inspector, "WORKSPACE HEAD", m.Inspector.WorkspaceHead)
	if m.Inspector.PlanRevision > 0 {
		inspector = append(inspector, fmt.Sprintf("PLAN: rev %d", m.Inspector.PlanRevision))
	}
	inspector = appendReadonlyInspector(inspector, "PLAN STATUS", m.Inspector.PlanStatus)
	inspector = appendReadonlyInspector(inspector, "PLAN HASH", m.Inspector.PlanHash)
	lines = append(lines, inspector...)
	lines = append(lines, "", "↑↓ browse  Esc back  / commands")
	return boundWorkspaceLines(lines, width, height, true)
}

func appendReadonlyInspector(lines []string, label, value string) []string {
	if value == "" {
		return lines
	}
	return append(lines, label+": "+value)
}
