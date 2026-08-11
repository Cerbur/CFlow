// Package tui is the full-screen Bubble Tea interface of CFlow (design
// §1: the TUI is the default entry point on an interactive terminal). It
// renders the read-only project workspace and drives the explicit user
// confirmations; it never decides lifecycle transitions itself.
package tui

import tea "charm.land/bubbletea/v2"

// Key bindings (design §6.1: navigation only updates UI selection; the
// mutation commands and confirmations are typed and explicit).
const (
	// KeyQuit is retained as the rune used by older callers and fixtures.
	// It is not an exit classification: q is ordinary input.
	KeyQuit rune = 'q'
	// KeyCtrlCRune is the Ctrl+C key rune (the first Ctrl+C requests the
	// controlled Pause; the second is the Force Stop of an active Runner).
	KeyCtrlCRune rune = 'c'
)

// KeyPress is one typed key press message.
type KeyPress tea.KeyPressMsg

// IsQuit always reports false. Normal exit is routed by the bounded /exit
// command palette (Task 7), while Ctrl+C retains controlled-stop semantics.
func IsQuit(tea.KeyMsg) bool { return false }

// IsCtrlC reports whether a key press is Ctrl+C.
func IsCtrlC(msg tea.KeyMsg) bool {
	if p, ok := msg.(tea.KeyPressMsg); ok {
		return p.Key().Code == KeyCtrlCRune && p.Key().Mod == tea.ModCtrl
	}
	return false
}

// IsSlash reports the global command-palette opener. Text-input pages must
// check their ownership before calling this helper.
func IsSlash(msg tea.KeyMsg) bool {
	p, ok := msg.(tea.KeyPressMsg)
	return ok && (p.Text == "/" || p.Key().Code == '/')
}

// IsEnter reports whether a key press is Enter.
func IsEnter(msg tea.KeyMsg) bool {
	if p, ok := msg.(tea.KeyPressMsg); ok {
		return p.Key().Code == tea.KeyEnter
	}
	return false
}

// IsUp / IsDown / IsLeft / IsRight report the navigation keys.
func IsUp(msg tea.KeyMsg) bool {
	p, ok := msg.(tea.KeyPressMsg)
	return ok && p.Key().Code == tea.KeyUp
}

func IsDown(msg tea.KeyMsg) bool {
	p, ok := msg.(tea.KeyPressMsg)
	return ok && p.Key().Code == tea.KeyDown
}

func IsLeft(msg tea.KeyMsg) bool {
	p, ok := msg.(tea.KeyPressMsg)
	return ok && p.Key().Code == tea.KeyLeft
}

func IsRight(msg tea.KeyMsg) bool {
	p, ok := msg.(tea.KeyPressMsg)
	return ok && p.Key().Code == tea.KeyRight
}
