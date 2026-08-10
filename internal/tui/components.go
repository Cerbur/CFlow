package tui

import (
	"fmt"
	"strings"

	"cflow.local/cflow/internal/model"
	"charm.land/lipgloss/v2"
	xansi "github.com/charmbracelet/x/ansi"
)

// workspaceSingleLine normalizes component input to one terminal row. A
// component must never let an embedded newline change the layout row count.
func workspaceSingleLine(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.ReplaceAll(s, "\t", " ")
}

// workspaceFitStyledLine bounds one terminal row using ANSI- and
// Unicode-aware cell widths. It is used by every workspace component before
// columns are joined.
func workspaceFitStyledLine(s string, width int) string {
	if width <= 0 {
		return ""
	}
	s = workspaceSingleLine(s)
	visibleWidth := lipgloss.Width(s)
	if visibleWidth > width {
		s = xansi.Truncate(s, width, "…")
		visibleWidth = lipgloss.Width(s)
	}
	return s + strings.Repeat(" ", max(0, width-visibleWidth))
}

func workspaceFitLine(s string, width int) string { return workspaceFitStyledLine(s, width) }

func workspaceTruncateText(s string, width int) string {
	if width <= 0 {
		return ""
	}
	return xansi.Truncate(workspaceSingleLine(s), width, "…")
}

func stripANSI(s string) string {
	for _, code := range []string{ansiReset, ansiMuted, ansiWhite, ansiBlue, ansiGreen, ansiYellow} {
		s = strings.ReplaceAll(s, code, "")
	}
	return s
}

func boundedStatusLine(status string, width int) string {
	return workspaceTruncateText("status: "+status, max(1, width))
}

func ansiStyle(style lipgloss.Style, s string) string { return style.Render(s) }

// ansi is kept as a small compatibility helper for the other TUI pages. New
// Workspace rendering uses the named theme tokens above.
func ansi(code, s string) string { return code + s + ansiReset }

func workspacePanel(title string, lines []string, width int) string {
	return workspacePanelWithHeight(title, lines, width, 0)
}

// panelWithHeight renders a complete panel or a shorter complete panel when
// the viewport is constrained. It never returns a partial border.
func workspacePanelWithHeight(title string, lines []string, width, height int) string {
	width = max(4, width)
	inner := width - 2
	content := make([]string, 0, len(lines))
	if height > 0 {
		contentHeight := max(0, height-4)
		if len(lines) > contentHeight {
			content = append(content, lines[:contentHeight]...)
		} else {
			content = append(content, lines...)
			for len(content) < contentHeight {
				content = append(content, "")
			}
		}
	} else {
		content = append(content, lines...)
	}

	rows := make([]string, 0, len(content)+3)
	rows = append(rows,
		workspaceTheme.Border.Render("╭"+strings.Repeat("─", inner)+"╮"),
		workspaceTheme.PanelTitle.Render("│")+workspaceFitStyledLine(workspaceTheme.PanelTitle.Render(title), inner)+workspaceTheme.PanelTitle.Render("│"),
		workspaceTheme.Border.Render("├"+strings.Repeat("─", inner)+"┤"),
	)
	for _, line := range content {
		rows = append(rows, workspaceTheme.Border.Render("│")+workspaceFitStyledLine(line, inner)+workspaceTheme.Border.Render("│"))
	}
	rows = append(rows, workspaceTheme.Border.Render("╰"+strings.Repeat("─", inner)+"╯"))

	// The opening/title rows above already have exactly width cells; the
	// explicit final pass protects styled CJK and custom component content.
	for i := range rows {
		rows[i] = workspaceFitStyledLine(rows[i], width)
	}
	return strings.Join(rows, "\n")
}

func joinWorkspaceColumns(columns []string, widths []int) string {
	if len(columns) == 0 || len(columns) != len(widths) {
		return ""
	}
	rows := make([][]string, len(columns))
	maxRows := 0
	for i, column := range columns {
		rows[i] = strings.Split(column, "\n")
		if len(rows[i]) > maxRows {
			maxRows = len(rows[i])
		}
	}
	var b strings.Builder
	for row := 0; row < maxRows; row++ {
		if row > 0 {
			b.WriteByte('\n')
		}
		for i := range columns {
			if i > 0 {
				b.WriteString("  ")
			}
			line := ""
			if row < len(rows[i]) {
				line = rows[i][row]
			}
			b.WriteString(workspaceFitStyledLine(line, widths[i]))
		}
	}
	return b.String()
}

func workflowLabel(name string, id model.WorkflowID) string {
	if strings.TrimSpace(name) == "" {
		return string(id)
	}
	return fmt.Sprintf("%s (%s)", name, id)
}

func valueOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func workspaceValueOr(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func workspaceShortHead(head string) string { return workspaceTruncateText(head, 12) }

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

// Legacy helpers below are intentionally kept for the existing Execution and
// lifecycle pages. The Workspace refresh uses workspace-prefixed helpers so
// its Lip Gloss framing cannot change those pages' established rendering.
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

func panel(title string, lines []string, width int) string {
	if width < 4 {
		width = 4
	}
	body := make([]string, 0, len(lines)+2)
	body = append(body, ansi(ansiWhite, title))
	body = append(body, ansi(ansiMuted, strings.Repeat("-", width-2)))
	body = append(body, lines...)
	var b strings.Builder
	b.WriteString("+" + strings.Repeat("-", width-2) + "+")
	for _, line := range body {
		b.WriteString("\n|" + legacyFitStyledLine(line, width-2) + "|")
	}
	b.WriteString("\n+" + strings.Repeat("-", width-2) + "+")
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
			b.WriteString(legacyFitStyledLine(line, widths[i]))
		}
	}
	return b.String()
}

func legacyFitStyledLine(s string, width int) string {
	plain := stripANSI(s)
	if len([]rune(plain)) > width {
		return fitLine(plain, width)
	}
	padded := fitLine(plain, width)
	if len(s) == len(plain) {
		return padded
	}
	return s + strings.Repeat(" ", max(0, width-len([]rune(plain)))) + ansiReset
}

func shortHead(head string) string {
	if len(head) <= 12 {
		return head
	}
	return head[:12]
}
