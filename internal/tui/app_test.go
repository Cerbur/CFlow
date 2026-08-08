package tui

// Root TUI model tests (TUI task 9): the minimal Model handles resize,
// quits on q and Ctrl+C, and surfaces errors; the render is pure.

import (
	"testing"

	tea "charm.land/bubbletea/v2"
)

// TestModelResizeAndQuit: a WindowSizeMsg marks the model ready; q and
// Ctrl+C quit; any other key keeps the model running.
func TestModelResizeAndQuit(t *testing.T) {
	m := newModel()
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m2, ok := updated.(Model)
	if !ok {
		t.Fatalf("update returned %T, want Model", updated)
	}
	if !m2.ready || m2.width != 80 || m2.height != 24 {
		t.Fatalf("model after resize = %+v", m2)
	}

	// q quits.
	updated, cmd := m2.Update(tea.KeyPressMsg{Code: KeyQuit})
	if _, ok := updated.(Model); !ok {
		t.Fatalf("quit update changed the model type")
	}
	if cmd == nil {
		t.Fatal("q produced no quit command")
	}

	// Ctrl+C quits too.
	m3 := m2
	_, cmd = m3.Update(tea.KeyPressMsg{Code: KeyCtrlCRune, Mod: tea.ModCtrl})
	if cmd == nil {
		t.Fatal("Ctrl+C produced no quit command")
	}
}

// TestModelIgnoresOtherKeys: navigation and typing keys never quit or
// error.
func TestModelIgnoresOtherKeys(t *testing.T) {
	m := newModel()
	m.ready = true
	for _, code := range []rune{'a', tea.KeyEnter, tea.KeyUp, tea.KeyDown} {
		updated, cmd := m.Update(tea.KeyPressMsg{Code: code})
		if cmd != nil {
			t.Fatalf("key %q produced a command: %v", code, cmd)
		}
		if _, ok := updated.(Model); !ok {
			t.Fatalf("key %q changed the model type", code)
		}
	}
}

// TestModelSurfacesError: an error message is rendered and does not
// quit.
func TestModelSurfacesError(t *testing.T) {
	m := newModel()
	m.ready = true
	updated, cmd := m.Update(testErr("boom"))
	if cmd != nil {
		t.Fatalf("an error produced a command: %v", cmd)
	}
	m2 := updated.(Model)
	if m2.err == nil || m2.err.Error() != "boom" {
		t.Fatalf("model error = %v", m2.err)
	}
	if got := render(m2); !contains(got, "boom") {
		t.Fatalf("render = %q, want the error surfaced", got)
	}
}

// TestRenderBeforeReady: before the first resize the screen renders an
// idle prompt.
func TestRenderBeforeReady(t *testing.T) {
	got := render(newModel())
	if !contains(got, "cflow") {
		t.Fatalf("idle render = %q", got)
	}
}

type testErr string

func (e testErr) Error() string { return string(e) }

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
