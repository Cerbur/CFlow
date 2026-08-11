package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

func TestNewCommandPaletteRegistersOnlyExit(t *testing.T) {
	p := NewCommandPalette()
	if p.Open {
		t.Fatal("new command palette is open")
	}
	if len(p.Commands) != 1 || p.Commands[0].Name != "/exit" || p.Commands[0].Description != "Exit CFlow" {
		t.Fatalf("commands = %+v, want only /exit", p.Commands)
	}
}

func TestCommandPaletteEnterSelectsExit(t *testing.T) {
	p := NewCommandPalette()
	p.Open = true
	p.Input = "/"

	p, event := p.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if event != CommandPaletteExit {
		t.Fatalf("Enter event = %v, want exit", event)
	}
	if p.Open {
		t.Fatal("selected command palette remained open")
	}
}

func TestCommandPaletteEscClosesWithoutSelectionChange(t *testing.T) {
	p := NewCommandPalette()
	p.Open = true
	p.Input = "/ex"
	p.Selected = 0

	p, event := p.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	if event != CommandPaletteClose || p.Open || p.Input != "/ex" || p.Selected != 0 {
		t.Fatalf("Esc result = palette=%+v event=%v", p, event)
	}
}

func TestRenderCommandPaletteIsCenteredAndBounded(t *testing.T) {
	p := NewCommandPalette()
	p.Open = true
	p.Input = "/"
	for _, tc := range []struct {
		width, height int
	}{
		{160, 45}, {120, 30}, {100, 24}, {80, 24}, {60, 18}, {88, 6}, {100, 6}, {120, 6},
	} {
		frame := RenderCommandPalette(p, tc.width, tc.height)
		lines := strings.Split(frame, "\n")
		if len(lines) > tc.height {
			t.Fatalf("palette at %dx%d has %d rows", tc.width, tc.height, len(lines))
		}
		for _, line := range lines {
			if got := lipgloss.Width(line); got > tc.width {
				t.Fatalf("palette at %dx%d has row width %d: %q", tc.width, tc.height, got, line)
			}
		}
		if !strings.Contains(frame, "/exit") || !strings.Contains(frame, "Enter") || !strings.Contains(frame, "Esc") {
			t.Fatalf("palette at %dx%d lost command affordances:\n%s", tc.width, tc.height, frame)
		}
	}
}
