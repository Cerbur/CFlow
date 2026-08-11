package tui

// The Terminal page (design §6.2, TUI task 14): the Final Report, the
// protected Apply Preview/Execute, and the Cleanup Dry Run/Manifest
// flow. Each section requires Enter to preview its current facts and a
// second Enter to execute the typed command.

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
	// Previewed is true after the first Enter has displayed the exact preview
	// for the current section. Section changes invalidate it.
	Previewed bool
	// Confirmed is a transient second-Enter execution request.
	Confirmed bool
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
				m.Previewed = false
				m.Confirmed = false
			}
		case IsRight(msg):
			if m.Section < SectionCleanup {
				m.Section++
				m.Previewed = false
				m.Confirmed = false
			}
		case IsEnter(msg):
			if !m.Previewed {
				m.Previewed = true
			} else {
				m.Confirmed = true
			}
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
	if !m.Previewed {
		b.WriteString("Enter to preview the exact facts; Esc back\n")
	} else {
		b.WriteString("PREVIEW READY — Enter execute, Esc back\n")
	}
	return b.String()
}

// RenderPauseExit renders the Pause and Exit prompt shown while an
// active Runner is running (q never quits directly, leaving processes).
func RenderPauseExit() string {
	var b strings.Builder
	b.WriteString("a workflow is running.\n")
	b.WriteString("Pause and Exit? Enter execute, Esc back (the workflow pauses through the controlled stop; processes never orphan)\n")
	return b.String()
}
