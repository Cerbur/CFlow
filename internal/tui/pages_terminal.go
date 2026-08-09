package tui

// The Terminal page (design §6.2, TUI task 14): the Final Report, the
// protected Apply Preview/Execute, and the Cleanup Dry Run/Manifest
// Confirmation. Every confirmation defaults to NO; Enter alone never
// executes a delivery or a cleanup.

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"cflow.local/cflow/internal/model"
)

// TerminalSection is the active section of the Terminal page.
type TerminalSection int

const (
	SectionReport TerminalSection = iota
	SectionApply
	SectionCleanup
)

// TerminalModel is the renderable Terminal page.
type TerminalModel struct {
	Section TerminalSection
	// Report is the final report summary (Task 18).
	Report string
	// ApplyPreview is the apply posture summary.
	ApplyPreview string
	// CleanupPreview is the cleanup manifest summary.
	CleanupPreview string
	// cleanupRef is the exact Manifest Ref of the produced Cleanup Dry
	// Run the explicit execution binds ("" until a manifest exists).
	cleanupRef *model.ArtifactRef
	// Confirmed is the explicit Yes/No confirmation state.
	Confirmed bool
	// Yes is the selected confirm answer (Enter alone never confirms).
	Yes bool
}

// NewTerminalModel returns the empty Terminal page.
func NewTerminalModel() TerminalModel { return TerminalModel{} }

// Init is the initial command (none).
func (m TerminalModel) Init() tea.Cmd { return nil }

// Update handles one message on the Terminal page. The quit keys belong
// to the root Model's controlled-stop protocol; the page only
// navigates its sections and the confirmation state.
func (m TerminalModel) Update(msg tea.Msg) (TerminalModel, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch {
		case IsLeft(msg):
			if m.Section > SectionReport {
				m.Section--
			}
		case IsRight(msg):
			if m.Section < SectionCleanup {
				m.Section++
			}
		case IsEnter(msg):
			// Enter alone never confirms a delivery or a cleanup.
			if !m.Confirmed {
				m.Confirmed = true
				m.Yes = false
			} else {
				m.Yes = !m.Yes
			}
		case msg.Code == 'y' || msg.Code == 'Y':
			m.Confirmed = true
			m.Yes = true
		case msg.Code == 'n' || msg.Code == 'N':
			m.Confirmed = true
			m.Yes = false
		}
	}
	return m, nil
}

// RenderTerminal renders the Terminal page.
func RenderTerminal(m TerminalModel) string {
	var b strings.Builder
	b.WriteString("terminal\n")
	sections := []string{"report", "apply", "cleanup"}
	for i, s := range sections {
		if i == int(m.Section) {
			b.WriteString(">")
		} else {
			b.WriteString(" ")
		}
		b.WriteString(s + "  ")
	}
	b.WriteString("\n\n")
	switch m.Section {
	case SectionReport:
		if m.Report != "" {
			fmt.Fprintf(&b, "%s\n", m.Report)
		} else {
			b.WriteString("final report: (not generated yet)\n")
		}
	case SectionApply:
		if m.ApplyPreview != "" {
			fmt.Fprintf(&b, "%s\n", m.ApplyPreview)
		} else {
			b.WriteString("apply: (no verified delivery pending)\n")
		}
	case SectionCleanup:
		if m.CleanupPreview != "" {
			fmt.Fprintf(&b, "%s\n", m.CleanupPreview)
		} else {
			b.WriteString("cleanup: (no manifest pending)\n")
		}
	}
	b.WriteString("\n")
	if !m.Confirmed {
		b.WriteString("confirm? Enter to choose, y/n, q to quit (defaults to no)\n")
	} else {
		mark := "no"
		if m.Yes {
			mark = "yes"
		}
		fmt.Fprintf(&b, "confirm: %s (Enter alone never confirms)\n", mark)
	}
	return b.String()
}

// RenderPauseExit renders the Pause and Exit prompt shown while an
// active Runner is running (q never quits directly, leaving processes).
func RenderPauseExit() string {
	var b strings.Builder
	b.WriteString("a workflow is running.\n")
	b.WriteString("Pause and Exit? [y/n]  (the workflow pauses through the controlled stop; processes never orphan)\n")
	return b.String()
}
