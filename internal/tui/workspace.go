package tui

// The workspace screen is intentionally rendered as a fixed workbench:
// context header, three scan-friendly columns, and a persistent status bar.
// Keeping this layout here makes the visual contract testable without
// starting Bubble Tea or touching the Runtime.

import (
	"fmt"
	"strings"

	"cflow.local/cflow/internal/app"
)

const (
	narrowWidth = 100
	ansiReset   = "\033[0m"
	ansiMuted   = "\033[90m"
	ansiWhite   = "\033[97m"
	ansiBlue    = "\033[94m"
	ansiGreen   = "\033[92m"
	ansiYellow  = "\033[93m"
)

// RenderWorkspace renders the full workbench. Narrow screens retain the
// same information architecture and stack the inspector below the main
// view instead of squeezing it into unreadable columns.
func RenderWorkspace(m WorkspaceModel, width int) string {
	if width <= 0 {
		width = 80
	}
	if width < narrowWidth {
		return renderNarrowWorkspace(m, width)
	}

	leftWidth := clamp(25, width/4, 32)
	rightWidth := clamp(27, width/4, 34)
	middleWidth := width - leftWidth - rightWidth - 2
	if middleWidth < 30 {
		return renderNarrowWorkspace(m, width)
	}

	header := renderWorkspaceHeader(m, width)
	left := panel("WORKFLOWS", renderWorkflowLines(m), leftWidth)
	middle := panel("LIFECYCLE", renderLifecycleLines(m), middleWidth)
	right := panel("INSPECTOR", renderInspectorLines(m), rightWidth)
	body := joinColumns([]string{left, middle, right}, []int{leftWidth, middleWidth, rightWidth})
	footer := renderWorkspaceFooter(m, width)
	return header + "\n" + body + "\n" + footer
}

func renderNarrowWorkspace(m WorkspaceModel, width int) string {
	if width < 44 {
		width = 44
	}
	header := renderWorkspaceHeader(m, width)
	main := panel("WORKFLOWS", renderWorkflowLines(m), width)
	lifecycle := panel("LIFECYCLE", renderLifecycleLines(m), width)
	inspector := panel("INSPECTOR", renderInspectorLines(m), width)
	return header + "\n" + main + "\n" + lifecycle + "\n" + inspector + "\n" + renderWorkspaceFooter(m, width)
}

func renderWorkspaceHeader(m WorkspaceModel, width int) string {
	root := m.Project.Root
	if root == "" {
		root = "(project not loaded)"
	}
	workflow := "no workflow selected"
	target := ""
	if m.Lifecycle != nil {
		workflow = string(m.Lifecycle.ID)
		target = m.Lifecycle.Target
	}
	if target == "" {
		target = "target branch unknown"
	}
	title := fmt.Sprintf("CFlow · %s · %s · %s", root, target, workflow)
	return ansi(ansiWhite, fitLine(title, width)) + "\n" + ansi(ansiMuted, strings.Repeat("-", width))
}

func renderWorkflowLines(m WorkspaceModel) []string {
	lines := []string{
		"project: " + valueOr(m.Project.Name, "(unnamed)"),
		ansi(ansiMuted, "workflows:"),
		"",
	}
	for _, w := range m.Workflows {
		mark := "○"
		color := ansiMuted
		if w.ID == m.Selected.ID {
			mark = "●"
			color = ansiWhite
		}
		lines = append(lines, ansi(color, fmt.Sprintf("%s %s", mark, w.ID)))
		lines = append(lines, "  "+strings.ToLower(string(w.Stage))+" · "+strings.ToLower(string(w.Runtime)))
	}
	if len(m.Workflows) == 0 {
		lines = append(lines, ansi(ansiMuted, "  no workflows yet"))
	}
	lines = append(lines, "", ansi(ansiBlue, "[n] New workflow"), "", ansi(ansiMuted, "LIFECYCLE"))
	for _, stage := range []string{"Discuss", "Plan", "Define", "Execute", "Report", "Apply", "Cleanup"} {
		mark := "○"
		color := ansiMuted
		if m.Lifecycle != nil && strings.EqualFold(string(m.Lifecycle.Stage), stage) {
			mark = "●"
			color = ansiBlue
		}
		lines = append(lines, ansi(color, fmt.Sprintf("%s %s", mark, stage)))
	}
	lines = append(lines, "", ansi(ansiMuted, "VIEWS"), "Overview", "Artifacts", "Events", "Settings")
	lines = append(lines, "", ansi(ansiMuted, "health: "+healthText(m.Health)))
	return lines
}

func renderLifecycleLines(m WorkspaceModel) []string {
	if m.Lifecycle == nil {
		return []string{"select a workflow to inspect its lifecycle"}
	}
	lc := m.Lifecycle
	lines := []string{
		fmt.Sprintf("workflow %s", lc.ID),
		fmt.Sprintf("%s · %s", strings.ToUpper(string(lc.Stage)), strings.ToUpper(string(lc.Runtime))),
		"",
		ansi(ansiMuted, "TASK GRAPH"),
	}
	if lc.Plan != nil {
		status := string(lc.Plan.Status)
		if lc.Plan.Approved {
			status = "approved"
		}
		lines = append(lines, fmt.Sprintf("Plan revision %d · %s", lc.Plan.Revision, status))
	}
	if lc.Adopted {
		lines = append(lines, ansi(ansiGreen, "✓ workspace adopted")+" · "+shortHead(lc.Head))
	} else if lc.Head != "" {
		lines = append(lines, ansi(ansiYellow, "● candidate")+" · "+shortHead(lc.Head))
	}
	if lc.Blocked {
		lines = append(lines, ansi(ansiYellow, "BLOCKED · inspect findings"))
	}
	lines = append(lines, "", ansi(ansiMuted, "LEGAL ACTIONS"), "actions:")
	if len(m.Actions) == 0 {
		lines = append(lines, "none")
	} else {
		lines = append(lines, actionsText(m.Actions)...)
	}
	return lines
}

func renderInspectorLines(m WorkspaceModel) []string {
	if m.Lifecycle == nil {
		return []string{"inspector:", "-", "", "no selection"}
	}
	lc := m.Lifecycle
	lines := []string{
		"inspector:",
		"S01 · workflow",
		"",
		ansi(ansiMuted, "STATUS"),
		string(lc.Runtime),
		"",
		ansi(ansiMuted, "TARGET"),
		valueOr(lc.Target, "-"),
		"",
		ansi(ansiMuted, "WORKSPACE"),
		valueOr(lc.Head, "pending"),
		"",
		ansi(ansiMuted, "ROUTE"),
		"runtime · approved",
	}
	if lc.Plan != nil {
		lines = append(lines, "", ansi(ansiMuted, "PLAN HASH"), shortHead(lc.Plan.Hash))
	}
	return lines
}

func renderWorkspaceFooter(m WorkspaceModel, width int) string {
	providers := []string{"Provider: runtime ✓"}
	for _, p := range m.Health.Providers {
		mark := "✓"
		if !p.Compatible {
			mark = "!"
		}
		providers = append(providers, p.Name+" "+mark)
	}
	return ansi(ansiMuted, fitLine(strings.Join(providers, " · ")+"    ↑↓ Navigate · tab Focus · q Back/Exit", width))
}

func actionsText(actions []Action) []string {
	out := make([]string, 0, len(actions))
	for _, action := range actions {
		out = append(out, string(action))
	}
	return out
}

func panel(title string, lines []string, width int) string {
	if width < 4 {
		width = 4
	}
	body := make([]string, 0, len(lines)+2)
	body = append(body, ansi(ansiWhite, title))
	body = append(body, ansi(ansiMuted, strings.Repeat("-", width-2)))
	for _, line := range lines {
		body = append(body, line)
	}
	height := len(body)
	var b strings.Builder
	b.WriteString("+" + strings.Repeat("-", width-2) + "+")
	for _, line := range body {
		b.WriteString("\n|" + fitStyledLine(line, width-2) + "|")
	}
	b.WriteString("\n+" + strings.Repeat("-", width-2) + "+")
	_ = height
	return b.String()
}

func joinColumns(columns []string, widths []int) string {
	rows := make([][]string, len(columns))
	maxRows := 0
	for i, column := range columns {
		rows[i] = strings.Split(column, "\n")
		if len(rows[i]) > maxRows {
			maxRows = len(rows[i])
		}
	}
	for i := range rows {
		for len(rows[i]) < maxRows {
			bottom := rows[i][len(rows[i])-1]
			rows[i] = append(rows[i][:len(rows[i])-1], "", bottom)
		}
	}
	var b strings.Builder
	for row := 0; row < maxRows; row++ {
		if row > 0 {
			b.WriteByte('\n')
		}
		for i := range columns {
			if i > 0 {
				b.WriteString(" | ")
			}
			line := ""
			if row < len(rows[i]) {
				line = rows[i][row]
			}
			b.WriteString(fitStyledLine(line, widths[i]))
		}
	}
	return b.String()
}

func fitLine(s string, width int) string {
	if width <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) > width {
		if width == 1 {
			return string(runes[:1])
		}
		return string(runes[:width-1]) + "…"
	}
	return s + strings.Repeat(" ", width-len(runes))
}

func fitStyledLine(s string, width int) string {
	plain := stripANSI(s)
	padded := fitLine(plain, width)
	if len(s) == len(plain) {
		return padded
	}
	return s + strings.Repeat(" ", max(0, width-len([]rune(plain)))) + ansiReset
}

func stripANSI(s string) string {
	for _, code := range []string{ansiReset, ansiMuted, ansiWhite, ansiBlue, ansiGreen, ansiYellow} {
		s = strings.ReplaceAll(s, code, "")
	}
	return s
}

func ansi(code, s string) string { return code + s + ansiReset }

func valueOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func healthText(h app.HealthView) string {
	if h.GitAvailable {
		return "git ok"
	}
	return "git unavailable"
}

func shortHead(head string) string {
	if len(head) <= 12 {
		return head
	}
	return head[:12]
}

func clamp(minimum, value, maximum int) int {
	if value < minimum {
		return minimum
	}
	if value > maximum {
		return maximum
	}
	return value
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
