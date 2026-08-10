package tui

import "charm.land/lipgloss/v2"

// WorkspaceTheme contains the visual tokens for the main workspace. Keeping
// these values together prevents page routing and projection code from
// accumulating presentation decisions.
var workspaceTheme = struct {
	Brand      lipgloss.Style
	Heading    lipgloss.Style
	Muted      lipgloss.Style
	Selected   lipgloss.Style
	Healthy    lipgloss.Style
	Attention  lipgloss.Style
	Danger     lipgloss.Style
	Border     lipgloss.Style
	PanelTitle lipgloss.Style
	KeyHint    lipgloss.Style
}{
	Brand:      lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#8AB4F8")),
	Heading:    lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#E8EAED")),
	Muted:      lipgloss.NewStyle().Foreground(lipgloss.Color("#8A929C")),
	Selected:   lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#D7E7FF")).Background(lipgloss.Color("#19324D")),
	Healthy:    lipgloss.NewStyle().Foreground(lipgloss.Color("#79C99B")),
	Attention:  lipgloss.NewStyle().Foreground(lipgloss.Color("#E7C66A")),
	Danger:     lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#E98282")),
	Border:     lipgloss.NewStyle().Foreground(lipgloss.Color("#536170")),
	PanelTitle: lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#B7C7D9")),
	KeyHint:    lipgloss.NewStyle().Foreground(lipgloss.Color("#AAB7C4")),
}

const (
	ansiReset  = "\033[0m"
	ansiMuted  = "\033[90m"
	ansiWhite  = "\033[97m"
	ansiBlue   = "\033[94m"
	ansiGreen  = "\033[92m"
	ansiYellow = "\033[93m"

	// narrowWidth is retained for the execution workbench's existing layout.
	narrowWidth = 100
)
