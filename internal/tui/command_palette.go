package tui

import (
	"strings"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
)

// GlobalCommand is a bounded command-palette entry. Command names are
// labels in an explicit registry; they are never passed to a shell.
type GlobalCommand struct {
	Name        string
	Description string
}

// CommandPaletteModel owns only the transient global command UI state.
type CommandPaletteModel struct {
	Open     bool
	Input    string
	Selected int
	Commands []GlobalCommand
}

// CommandPaletteEvent is the root action emitted by the pure palette model.
type CommandPaletteEvent uint8

const (
	CommandPaletteNone CommandPaletteEvent = iota
	CommandPaletteClose
	CommandPaletteExit
)

// NewCommandPalette creates the bounded global command registry.
func NewCommandPalette() CommandPaletteModel {
	return CommandPaletteModel{
		Commands: []GlobalCommand{{Name: "/exit", Description: "Exit CFlow"}},
	}
}

// Update handles keys while the palette owns the input. It emits one of the
// explicit palette events and never forwards a key to the underlying page.
func (p CommandPaletteModel) Update(msg tea.KeyPressMsg) (CommandPaletteModel, CommandPaletteEvent) {
	if !p.Open {
		return p, CommandPaletteNone
	}

	switch msg.Code {
	case tea.KeyEsc:
		p.Open = false
		return p, CommandPaletteClose
	case tea.KeyEnter:
		commands := p.filteredCommands()
		if p.Selected >= 0 && p.Selected < len(commands) && commands[p.Selected].Name == "/exit" {
			p.Open = false
			return p, CommandPaletteExit
		}
		return p, CommandPaletteNone
	case tea.KeyUp:
		p.Selected = movePaletteSelection(p.Selected, len(p.filteredCommands()), -1)
		return p, CommandPaletteNone
	case tea.KeyDown:
		p.Selected = movePaletteSelection(p.Selected, len(p.filteredCommands()), 1)
		return p, CommandPaletteNone
	case tea.KeyBackspace, tea.KeyDelete:
		if p.Input != "" {
			_, size := utf8.DecodeLastRuneInString(p.Input)
			p.Input = p.Input[:len(p.Input)-size]
		}
		p.Selected = movePaletteSelection(p.Selected, len(p.filteredCommands()), 0)
		return p, CommandPaletteNone
	}
	if msg.Text != "" {
		p.Input += msg.Text
		p.Selected = movePaletteSelection(p.Selected, len(p.filteredCommands()), 0)
	}
	return p, CommandPaletteNone
}

func movePaletteSelection(selected, count, delta int) int {
	if count <= 0 {
		return 0
	}
	selected = max(0, min(selected, count-1))
	if delta < 0 {
		selected = (selected + count - 1) % count
	} else if delta > 0 {
		selected = (selected + 1) % count
	}
	return selected
}

func (p CommandPaletteModel) filteredCommands() []GlobalCommand {
	query := strings.ToLower(strings.TrimSpace(p.Input))
	if query == "" || query == "/" {
		return append([]GlobalCommand(nil), p.Commands...)
	}
	filtered := make([]GlobalCommand, 0, len(p.Commands))
	for _, command := range p.Commands {
		if strings.Contains(strings.ToLower(command.Name), query) || strings.Contains(strings.ToLower(command.Description), query) {
			filtered = append(filtered, command)
		}
	}
	return filtered
}

// RenderCommandPalette renders a bounded command box. The root renderer
// centers this box over the current page; its dimensions are used here to
// clamp both the box and its rows for responsive terminals.
func RenderCommandPalette(p CommandPaletteModel, width, height int) string {
	if !p.Open || width <= 0 || height <= 0 {
		return ""
	}

	commands := p.filteredCommands()
	selected := movePaletteSelection(p.Selected, len(commands), 0)
	input := p.Input
	if input == "" {
		input = "/"
	}
	row := ""
	if len(commands) == 0 {
		row = "  no matching commands"
	} else {
		command := commands[selected]
		row = "▸ " + command.Name + "  " + command.Description
	}

	content := []string{
		"COMMAND PALETTE  " + input,
		row,
		"↑↓ Navigate · Enter Select · Esc Close",
	}
	boxWidth := max(1, min(width, max(24, paletteContentWidth(content))))
	if boxWidth <= 2 {
		return workspaceFitStyledLine("/", width)
	}
	inner := boxWidth - 2
	if height >= 5 {
		lines := []string{
			"┌" + strings.Repeat("─", inner) + "┐",
			"│" + workspaceFitStyledLine(content[0], inner) + "│",
			"│" + workspaceFitStyledLine(content[1], inner) + "│",
			"│" + workspaceFitStyledLine(content[2], inner) + "│",
			"└" + strings.Repeat("─", inner) + "┘",
		}
		return strings.Join(lines, "\n")
	}

	// Keep the action row visible when the terminal is shorter than a framed
	// box. This is intentionally a presentation clamp; palette behavior does
	// not depend on the viewport size.
	compact := make([]string, 0, height)
	for _, line := range content {
		compact = append(compact, workspaceFitStyledLine(line, width))
		if len(compact) == height {
			break
		}
	}
	return strings.Join(compact, "\n")
}

func paletteContentWidth(lines []string) int {
	width := 0
	for _, line := range lines {
		if got := lipglossWidth(line); got > width {
			width = got
		}
	}
	return width + 2
}

// overlayCommandPalette places the bounded palette over the current page.
// Rows outside the palette retain the page; the palette owns all visible
// input while open.
func overlayCommandPalette(base, palette string, width, height int) string {
	if palette == "" || width <= 0 || height <= 0 {
		return base
	}
	baseLines := strings.Split(base, "\n")
	for len(baseLines) < height {
		baseLines = append(baseLines, "")
	}
	if len(baseLines) > height {
		baseLines = baseLines[:height]
	}
	paletteLines := strings.Split(palette, "\n")
	paletteWidth := 0
	for _, line := range paletteLines {
		paletteWidth = max(paletteWidth, lipglossWidth(line))
	}
	top := max(0, (height-len(paletteLines))/2)
	left := max(0, (width-paletteWidth)/2)
	for i, line := range paletteLines {
		row := top + i
		if row >= len(baseLines) {
			break
		}
		baseLines[row] = strings.Repeat(" ", left) + workspaceFitStyledLine(line, max(1, width-left))
		baseLines[row] = workspaceFitStyledLine(baseLines[row], width)
	}
	return strings.Join(baseLines, "\n")
}
