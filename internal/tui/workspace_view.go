package tui

// The Workspace view is a pure, responsive projection of the Runtime facts.
// It does not query, execute commands, or own selection state.

import (
	"fmt"
	"strings"

	"cflow.local/cflow/internal/app"
	"cflow.local/cflow/internal/model"
	"charm.land/lipgloss/v2"
)

type workspaceLayout uint8

const (
	layoutWide workspaceLayout = iota
	layoutMedium
	layoutCompact
	layoutMinimal
)

// RenderWorkspace renders the main workbench within the requested viewport.
// It is a pure View function: only the mapped ViewModel and terminal
// dimensions affect its output. Root-owned transient status is overlaid by
// app.go without becoming a WorkspaceViewModel field.
func RenderWorkspace(m WorkspaceViewModel, width, height int) string {
	if width <= 0 {
		width = 80
	}
	if height <= 0 {
		height = 1
	}

	switch workspaceLayoutFor(width, height) {
	case layoutWide:
		return renderWideWorkspace(m, width, height)
	case layoutMedium:
		return renderMediumWorkspace(m, width, height)
	case layoutCompact:
		return renderCompactWorkspace(m, width, height)
	default:
		return renderMinimalWorkspace(m, width, height)
	}
}

func workspaceLayoutFor(width, height int) workspaceLayout {
	// A bordered panel needs enough rows for its frame, primary context, and
	// footer. Below sixteen rows, Minimal preserves stable context and the
	// action footer instead of rendering a bounded-but-empty panel.
	if height < 16 {
		return layoutMinimal
	}
	if width >= 120 && height >= 28 {
		return layoutWide
	}
	if width >= 88 {
		return layoutMedium
	}
	// A compact panel is useful only when there is enough room for its
	// context and footer. Smaller viewports use the border-free minimal mode.
	if width >= 60 && height >= 12 {
		return layoutCompact
	}
	return layoutMinimal
}

func renderWideWorkspace(m WorkspaceViewModel, width, height int) string {
	header := renderWorkspaceHeaderLines(m, width)
	footer := renderWorkspaceFooter(m, "", width)
	bodyHeight := max(4, height-len(header)-1)

	available := width - 2*2 // two two-cell column gaps
	leftWidth := clamp(24, available/4, 34)
	rightWidth := clamp(28, available/4, 36)
	middleWidth := available - leftWidth - rightWidth
	if middleWidth < 32 {
		return renderMediumWorkspace(m, width, height)
	}

	left := workspacePanelWithHeight("WORKFLOWS", renderWorkflowLines(m), leftWidth, bodyHeight)
	middle := workspacePanelWithHeight("WORKSPACE", renderLifecycleLines(m), middleWidth, bodyHeight)
	right := workspacePanelWithHeight("INSPECTOR", renderInspectorLines(m), rightWidth, bodyHeight)
	return joinWorkspaceSections(header, joinWorkspaceColumns([]string{left, middle, right}, []int{leftWidth, middleWidth, rightWidth}), footer, width, height)
}

func renderMediumWorkspace(m WorkspaceViewModel, width, height int) string {
	header := renderWorkspaceHeaderLines(m, width)
	footer := renderWorkspaceFooter(m, "", width)
	bodyHeight := max(4, height-len(header)-1)

	available := width - 2 // one two-cell column gap
	leftWidth := clamp(24, available/3, 34)
	mainWidth := available - leftWidth
	if mainWidth < 32 {
		return renderCompactWorkspace(m, width, height)
	}

	left := workspacePanelWithHeight("WORKFLOWS", renderWorkflowLines(m), leftWidth, bodyHeight)
	mainLines := renderMediumLines(m)
	main := workspacePanelWithHeight("WORKSPACE", mainLines, mainWidth, bodyHeight)
	return joinWorkspaceSections(header, joinWorkspaceColumns([]string{left, main}, []int{leftWidth, mainWidth}), footer, width, height)
}

func renderMediumLines(m WorkspaceViewModel) []string {
	if m.Lifecycle == nil {
		lines := renderLifecycleLines(m)
		lines = append(lines, "", workspaceTheme.Muted.Render("SUMMARY"), "no selection")
		return lines
	}

	lc := m.Lifecycle
	lines := []string{
		workspaceTheme.Heading.Render("workflow " + workflowLabel(lc.Name, lc.ID)),
		"STAGE    " + workspaceValueOr(strings.ToUpper(string(lc.Stage)), "UNKNOWN"),
		"RUNTIME  " + workspaceValueOr(strings.ToUpper(string(lc.Runtime)), "UNKNOWN"),
		workspaceTheme.Muted.Render("LIFECYCLE  " + compactProgress(lc.Stage)),
		"",
		workspaceTheme.Muted.Render("LEGAL ACTIONS · actions:"),
	}
	if len(m.Actions) == 0 {
		lines = append(lines, "none projected")
	} else {
		for _, action := range m.Actions {
			lines = append(lines, workspaceTheme.Healthy.Render("→ "+string(action)))
		}
	}
	if lc.Blocked {
		lines = append(lines, workspaceTheme.Attention.Render("! blocked · inspect findings"))
	}
	if lc.Adopted {
		lines = append(lines, workspaceTheme.Healthy.Render("✓ workspace adopted"))
	}

	lines = append(lines, "", workspaceTheme.Muted.Render("SUMMARY"))
	lines = append(lines, mediumSummaryLines(lc, m.Health)...)
	return lines
}

func mediumSummaryLines(lc *LifecycleItem, health app.HealthView) []string {
	lines := []string{
		"TARGET     " + workspaceValueOr(lc.Target, "unavailable"),
		"WORKSPACE  " + workspaceValueOr(workspaceShortHead(lc.Head), "not verified"),
	}
	if lc.Plan != nil {
		status := workspaceValueOr(string(lc.Plan.Status), "unknown")
		if lc.Plan.Approved {
			status = "approved"
		}
		lines = append(lines, fmt.Sprintf("PLAN       rev %d · %s · %s", lc.Plan.Revision, status, workspaceShortHead(lc.Plan.Hash)))
	}
	lines = append(lines, "HEALTH     "+healthSummary(health))
	return lines
}

func renderCompactWorkspace(m WorkspaceViewModel, width, height int) string {
	header := renderWorkspaceHeaderLines(m, width)
	footer := renderWorkspaceFooter(m, "", width)
	bodyHeight := max(4, height-len(header)-1)
	body := workspacePanelWithHeight("WORKSPACE", renderCompactLines(m), width, bodyHeight)
	return joinWorkspaceSections(header, body, footer, width, height)
}

func renderMinimalWorkspace(m WorkspaceViewModel, width, height int) string {
	workflow := "no workflow selected"
	stage := "stage unavailable"
	runtime := "runtime unavailable"
	if m.Lifecycle != nil {
		workflow = workflowLabel(m.Lifecycle.Name, m.Lifecycle.ID)
		stage = workspaceValueOr(strings.ToUpper(string(m.Lifecycle.Stage)), "stage unavailable")
		runtime = workspaceValueOr(strings.ToUpper(string(m.Lifecycle.Runtime)), "runtime unavailable")
	}
	lines := []string{
		workspaceTheme.Brand.Render("CFLOW") + " · " + workflow,
		"WORKFLOWS · " + minimalWorkflowRows(m),
		"WORKSPACE · " + stage + " · " + runtime,
		renderWorkspaceFooter(m, "", width),
	}
	return boundWorkspaceLines(lines, width, height, true)
}

func joinWorkspaceSections(header []string, body, footer string, width, height int) string {
	lines := make([]string, 0, len(header)+1+len(strings.Split(body, "\n"))+1)
	lines = append(lines, header...)
	lines = append(lines, strings.Split(body, "\n")...)
	lines = append(lines, footer)
	return boundWorkspaceLines(lines, width, height, true)
}

func boundWorkspaceLines(lines []string, width, height int, keepFooter bool) string {
	if width <= 0 {
		width = 1
	}
	if height <= 0 {
		height = 1
	}
	bounded := make([]string, len(lines))
	for i, line := range lines {
		bounded[i] = workspaceFitStyledLine(line, width)
	}
	if len(bounded) > height {
		if keepFooter && height == 1 {
			// At the smallest viewport the footer is the only stable action
			// surface that can still be shown; preserve it instead of the
			// context rows so it never silently disappears.
			bounded = bounded[len(bounded)-1:]
		} else if keepFooter && height >= 2 {
			body := append([]string(nil), bounded[:height-1]...)
			body[height-2] = workspaceFitStyledLine(body[height-2], width)
			bounded = append(body, bounded[len(bounded)-1])
		} else {
			bounded = bounded[:height]
		}
	}
	return strings.Join(bounded, "\n")
}

func renderWorkspaceHeaderLines(m WorkspaceViewModel, width int) []string {
	root := workspaceValueOr(m.Project.Root, "project not loaded")
	project := workspaceValueOr(m.Project.Name, "unnamed project")
	workflow := "no workflow selected"
	target := "target unavailable"
	if m.Lifecycle != nil {
		workflow = workflowLabel(m.Lifecycle.Name, m.Lifecycle.ID)
		target = workspaceValueOr(m.Lifecycle.Target, target)
	}
	context := fmt.Sprintf("project: %s · %s · target %s · %s", project, root, target, workflow)
	return []string{
		workspaceFitStyledLine(workspaceTheme.Brand.Render("CFLOW · CFlow")+"  "+context, width),
		workspaceFitStyledLine(workspaceTheme.Muted.Render(healthSummary(m.Health)), width),
	}
}

// renderWorkspaceHeader is retained for small internal callers that expect a
// single string; the root page uses the height-aware header lines directly.
func renderWorkspaceHeader(m WorkspaceViewModel, width int) string {
	return strings.Join(renderWorkspaceHeaderLines(m, width), "\n")
}

func renderWorkflowLines(m WorkspaceViewModel) []string {
	lines := []string{
		workspaceTheme.Muted.Render("PROJECT") + "  " + workspaceValueOr(m.Project.Name, "(unnamed)"),
		workspaceTheme.Muted.Render("workflows:"),
	}
	for i, row := range m.Rows {
		marker := "•"
		lineStyle := workspaceTheme.Muted
		if i == m.SelectedRow {
			marker = "▸"
			lineStyle = workspaceTheme.Selected
		}
		if row.Kind == WorkflowRowNew {
			lines = append(lines, lineStyle.Render(marker+" "+row.Name))
			continue
		}
		lines = append(lines, lineStyle.Render(marker+" "+workflowLabel(row.Name, row.ID)))
		lines = append(lines, "  "+strings.ToLower(workspaceValueOr(string(row.Stage), "stage"))+" · "+strings.ToLower(workspaceValueOr(string(row.Runtime), "runtime")))
	}
	if len(m.Workflows) == 0 {
		lines = append(lines, workspaceTheme.Muted.Render("  no existing workflows"))
	}
	return lines
}

func minimalWorkflowRows(m WorkspaceViewModel) string {
	if m.SelectedRow < 0 || m.SelectedRow >= len(m.Rows) {
		return "• NEW WORKFLOW"
	}
	row := m.Rows[m.SelectedRow]
	if row.Kind == WorkflowRowNew {
		return "▸ NEW WORKFLOW"
	}
	return "• NEW WORKFLOW · ▸ " + workflowLabel(row.Name, row.ID)
}

type lifecycleStep struct {
	Label  string
	Stages []model.WorkflowStage
}

// lifecycleSteps are presentation stages. The Runtime's authoritative
// WorkflowStage values intentionally collapse into the user-facing phases;
// Cleanup is a visual post-completion affordance and has no WorkflowStage.
var lifecycleSteps = []lifecycleStep{
	{Label: "Discuss", Stages: []model.WorkflowStage{model.StageRequirementDiscussion}},
	{Label: "Plan", Stages: []model.WorkflowStage{model.StagePlanGeneration, model.StagePlanCheck}},
	{Label: "Define", Stages: []model.WorkflowStage{model.StageSpecGeneration, model.StageWorkflowGeneration}},
	{Label: "Execute", Stages: []model.WorkflowStage{model.StageExecution}},
	{Label: "Report", Stages: []model.WorkflowStage{model.StageFinalVerification}},
	{Label: "Apply", Stages: []model.WorkflowStage{model.StageCompleted}},
	{Label: "Cleanup"},
}

func lifecycleStepIndex(stage model.WorkflowStage) int {
	for i, step := range lifecycleSteps {
		for _, candidate := range step.Stages {
			if stage == candidate {
				return i
			}
		}
	}
	return -1
}

func renderLifecycleLines(m WorkspaceViewModel) []string {
	if m.Lifecycle == nil {
		return []string{
			workspaceTheme.Heading.Render("New Workflow"),
			"",
			workspaceTheme.Muted.Render("Press Enter to open the Create Workspace."),
		}
	}
	lc := m.Lifecycle
	lines := []string{
		workspaceTheme.Heading.Render("workflow " + workflowLabel(lc.Name, lc.ID)),
		"STAGE    " + workspaceValueOr(strings.ToUpper(string(lc.Stage)), "UNKNOWN"),
		"RUNTIME  " + workspaceValueOr(strings.ToUpper(string(lc.Runtime)), "UNKNOWN"),
		"",
		workspaceTheme.Muted.Render("LIFECYCLE PROGRESS"),
	}
	activeStep := lifecycleStepIndex(lc.Stage)
	for i, step := range lifecycleSteps {
		marker := "○"
		style := workspaceTheme.Muted
		if i == activeStep {
			marker = "●"
			style = workspaceTheme.Selected
		}
		lines = append(lines, style.Render(marker+" "+step.Label))
	}
	lines = append(lines, "", workspaceTheme.Muted.Render("LEGAL ACTIONS · actions:"))
	if len(m.Actions) == 0 {
		lines = append(lines, workspaceTheme.Muted.Render("none projected"))
	} else {
		for _, action := range m.Actions {
			lines = append(lines, workspaceTheme.Healthy.Render("→ "+string(action)))
		}
	}
	if lc.Blocked {
		lines = append(lines, "", workspaceTheme.Attention.Render("! blocked · inspect findings"))
	}
	if lc.Adopted {
		lines = append(lines, "", workspaceTheme.Healthy.Render("✓ workspace adopted"))
	}
	return lines
}

func renderCompactLines(m WorkspaceViewModel) []string {
	if m.Lifecycle == nil {
		return []string{
			workspaceTheme.Muted.Render("WORKFLOWS · ▸ NEW WORKFLOW"),
			"",
			workspaceTheme.Heading.Render("No workflow selected"),
			workspaceTheme.Muted.Render("Press Enter to open the Create Workspace."),
			"",
			workspaceTheme.Muted.Render("LEGAL ACTIONS"),
			"none projected",
		}
	}
	lc := m.Lifecycle
	lines := []string{
		workspaceTheme.Muted.Render("WORKFLOWS · • NEW WORKFLOW · ▸ " + workflowLabel(m.Selected.Name, m.Selected.ID)),
		"",
		workspaceTheme.Heading.Render(workflowLabel(lc.Name, lc.ID)),
		"STAGE    " + workspaceValueOr(strings.ToUpper(string(lc.Stage)), "UNKNOWN"),
		"RUNTIME  " + workspaceValueOr(strings.ToUpper(string(lc.Runtime)), "UNKNOWN"),
		workspaceTheme.Muted.Render("LIFECYCLE  " + compactProgress(lc.Stage)),
	}
	if lc.Blocked {
		lines = append(lines, workspaceTheme.Attention.Render("! blocked · inspect findings"))
	}
	lines = append(lines, "", workspaceTheme.Muted.Render("LEGAL ACTIONS"))
	if len(m.Actions) == 0 {
		lines = append(lines, "none projected")
	} else {
		for _, action := range m.Actions {
			lines = append(lines, workspaceTheme.Healthy.Render("→ "+string(action)))
		}
	}
	lines = append(lines, "", workspaceTheme.Muted.Render("inspector: summary"), "TARGET     "+workspaceValueOr(lc.Target, "unavailable"), "WORKSPACE  "+workspaceValueOr(workspaceShortHead(lc.Head), "not verified"))
	if lc.Plan != nil {
		status := workspaceValueOr(string(lc.Plan.Status), "unknown")
		if lc.Plan.Approved {
			status = "approved"
		}
		lines = append(lines, fmt.Sprintf("PLAN       rev %d · %s · %s", lc.Plan.Revision, status, workspaceShortHead(lc.Plan.Hash)))
	}
	return lines
}

func compactProgress(stage model.WorkflowStage) string {
	total := len(lifecycleSteps)
	index := lifecycleStepIndex(stage)
	if index < 0 {
		return "?/" + fmt.Sprint(total)
	}
	return fmt.Sprintf("%d/%d · %s", index+1, total, lifecycleSteps[index].Label)
}

func renderInspectorLines(m WorkspaceViewModel) []string {
	if m.Lifecycle == nil {
		return []string{
			workspaceTheme.Muted.Render("inspector:"),
			workspaceTheme.Muted.Render("no selection"),
			"",
			workspaceTheme.Muted.Render(healthSummary(m.Health)),
		}
	}
	lc := m.Lifecycle
	lines := []string{
		workspaceTheme.Muted.Render("inspector:"),
		workspaceTheme.Muted.Render("STATUS"),
		workspaceValueOr(string(lc.Runtime), "unknown"),
		"",
		workspaceTheme.Muted.Render("TARGET"),
		workspaceValueOr(lc.Target, "unavailable"),
		"",
		workspaceTheme.Muted.Render("WORKSPACE HEAD"),
		workspaceValueOr(workspaceShortHead(lc.Head), "not verified"),
	}
	if lc.Plan != nil {
		status := workspaceValueOr(string(lc.Plan.Status), "unknown")
		if lc.Plan.Approved {
			status = "approved"
		}
		lines = append(lines, "", workspaceTheme.Muted.Render("PLAN"), fmt.Sprintf("revision %d · %s", lc.Plan.Revision, status), "hash "+workspaceShortHead(lc.Plan.Hash))
	}
	lines = append(lines, "", workspaceTheme.Muted.Render("HEALTH"), healthSummary(m.Health))
	return lines
}

func renderWorkspaceFooter(m WorkspaceViewModel, status string, width int) string {
	if width <= 0 {
		width = 1
	}

	// Home exposes only hierarchical navigation. Runtime legal actions remain
	// visible in the central Workspace summary and are entered through the
	// selected workflow's menu, never advertised as direct Home shortcuts.
	parts := []string{"/ command"}
	if status != "" {
		remaining := width - footerWidth(parts) - 2
		if remaining < lipglossWidth("status: ") && width >= lipglossWidth("/  status:") {
			parts = []string{"/"}
			remaining = width - footerWidth(parts) - 2
		}
		if remaining >= lipglossWidth("status:") {
			parts = append(parts, workspaceTruncateText("status: "+workspaceSingleLine(status), remaining))
		}
	}
	controls := []string{"Enter open", "Esc back", "↑↓ navigate"}
	switch {
	case width < 20:
		controls = nil
	case width < 60:
		controls = []string{"Enter", "Esc", "↑↓"}
	}
	for _, control := range controls {
		if footerPartsWidth(parts, control) <= width {
			parts = append(parts, control)
		}
	}

	// At widths below the textual command hint, retain a one-character actionable
	// affordance instead of allowing optional content to consume the row.
	if width < lipglossWidth("/ command") {
		parts = []string{"/"}
	}
	return workspaceTheme.KeyHint.Render(workspaceTruncateText(strings.Join(parts, "  "), width))
}

func workspaceActionHints(actions []Action) []string {
	parts := make([]string, 0, 4)
	if hasAction(actions, ActionResume) {
		parts = append(parts, "r resume")
	}
	if hasAction(actions, ActionPause) {
		parts = append(parts, "p pause")
	}
	if hasAction(actions, ActionCancel) {
		parts = append(parts, "x cancel")
	}
	if hasAction(actions, ActionMigrate) {
		parts = append(parts, "m migrate")
	}
	return parts
}

func footerWidth(parts []string) int {
	return lipglossWidth(strings.Join(parts, "  "))
}

func footerPartsWidth(parts []string, extra string) int {
	if len(parts) == 0 {
		return lipglossWidth(extra)
	}
	return lipglossWidth(strings.Join(append(append([]string(nil), parts...), extra), "  "))
}

func lipglossWidth(s string) int { return lipgloss.Width(s) }

func healthSummary(h app.HealthView) string {
	git := "Git !"
	gitStyle := workspaceTheme.Attention
	if h.GitAvailable {
		git = "Git ✓"
		gitStyle = workspaceTheme.Healthy
	}
	parts := []string{gitStyle.Render(git)}
	if len(h.Providers) == 0 {
		parts = append(parts, workspaceTheme.Muted.Render("Provider: none"))
	} else {
		for _, provider := range h.Providers {
			name := workspaceValueOr(provider.Name, "provider")
			mark := "!"
			style := workspaceTheme.Attention
			if provider.Compatible {
				mark = "✓"
				style = workspaceTheme.Healthy
			}
			parts = append(parts, style.Render(name+" "+mark))
		}
	}
	return "health: " + strings.Join(parts, " · ")
}

func healthText(h app.HealthView) string {
	if h.GitAvailable {
		return "git ok"
	}
	return "git unavailable"
}

func actionsText(actions []Action) []string {
	out := make([]string, 0, len(actions))
	for _, action := range actions {
		out = append(out, string(action))
	}
	return out
}
