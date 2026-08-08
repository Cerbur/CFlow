// Package tui is the full-screen Bubble Tea interface of CFlow (design
// §1: the TUI is the default entry point on an interactive terminal). It
// renders the read-only project workspace and drives the explicit user
// confirmations; it never decides lifecycle transitions itself.
package tui

import (
	"context"
	"fmt"
	"io"
	"os"

	tea "charm.land/bubbletea/v2"

	"cflow.local/cflow/internal/cli"
)

// Dependencies is what the TUI needs to run: the same command-tree
// Dependencies plus the optional input/output streams (tests inject a
// Fake terminal; production uses the process's stdin/stdout).
type Dependencies struct {
	CLI     cli.Dependencies
	In      io.Reader
	Out     io.Writer
	Err     io.Writer
	Program *tea.Program // nil: the Run call creates the default program
}

// Run launches the full-screen TUI and returns when the user quits.
// Run is the top-level entry the bare `cflow` command calls on an
// interactive terminal; it never mutates a Workflow by itself.
func Run(ctx context.Context, deps Dependencies) error {
	prog := deps.Program
	if prog == nil {
		in := deps.In
		out := deps.Out
		if in == nil {
			in = os.Stdin
		}
		if out == nil {
			out = os.Stdout
		}
		prog = tea.NewProgram(newModel(), tea.WithInput(in), tea.WithOutput(out))
	}
	model, err := prog.Run()
	if err != nil {
		return err
	}
	if m, ok := model.(Model); ok && m.err != nil {
		return m.err
	}
	return ctx.Err()
}

// Model is the root TUI model. Task 9 renders a minimal idle screen;
// Task 10 fills the read-only project workspace.
type Model struct {
	width  int
	height int
	ready  bool
	err    error
}

// NewModel returns the initial root model.
func NewModel() Model { return newModel() }

func newModel() Model {
	return Model{}
}

// Init is the initial command: nothing to do at startup.
func (m Model) Init() tea.Cmd { return nil }

// Update handles one message: window resize, quit, and errors.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.ready = true
		return m, nil
	case tea.KeyPressMsg:
		if IsQuit(msg) || IsCtrlC(msg) {
			return m, tea.Quit
		}
		return m, nil
	case error:
		m.err = msg
		return m, nil
	}
	return m, nil
}

// View renders the current screen.
func (m Model) View() tea.View {
	v := tea.NewView(render(m))
	v.AltScreen = true
	return v
}

// render is the pure screen renderer of the root model.
func render(m Model) string {
	if m.err != nil {
		return fmt.Sprintf("cflow: %v\n\npress q to quit", m.err)
	}
	if !m.ready {
		return "cflow\n\npress q to quit"
	}
	return "cflow  (project workspace loads next)\n\npress q to quit"
}
