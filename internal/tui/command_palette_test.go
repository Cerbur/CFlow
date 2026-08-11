package tui

import (
	"fmt"
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

func TestCommandPaletteFitsAllTargetSizes(t *testing.T) {
	p := CommandPaletteModel{
		Open:     true,
		Input:    "/",
		Selected: 0,
		Commands: []GlobalCommand{{Name: "/exit", Description: "Exit CFlow"}},
	}
	for _, tc := range []struct {
		width, height int
	}{
		{160, 45}, {120, 30}, {100, 24}, {80, 24}, {60, 18},
		{88, 6}, {100, 6}, {120, 6},
	} {
		t.Run(fmt.Sprintf("%dx%d", tc.width, tc.height), func(t *testing.T) {
			frame := RenderCommandPalette(p, tc.width, tc.height)
			if lipgloss.Height(frame) > tc.height {
				t.Fatalf("palette produced %d rows", lipgloss.Height(frame))
			}
			for i, line := range strings.Split(frame, "\n") {
				if got := lipgloss.Width(line); got > tc.width {
					t.Fatalf("row %d width=%d > %d: %q", i, got, tc.width, line)
				}
			}
			if !strings.Contains(frame, "/exit") || !strings.Contains(frame, "Enter") || !strings.Contains(frame, "Esc") {
				t.Fatalf("palette lost stable affordances:\n%s", frame)
			}

			base := strings.Repeat("base\n", tc.height)
			overlay := overlayCommandPalette(base, frame, tc.width, tc.height)
			if lipgloss.Height(overlay) > tc.height {
				t.Fatalf("overlay produced %d rows", lipgloss.Height(overlay))
			}
			for i, line := range strings.Split(overlay, "\n") {
				if got := lipgloss.Width(line); got > tc.width {
					t.Fatalf("overlay row %d width=%d > %d: %q", i, got, tc.width, line)
				}
			}
		})
	}
}

func TestRenderCommandPaletteDropsDescriptionBeforeCommandName(t *testing.T) {
	p := NewCommandPalette()
	p.Open = true
	p.Input = "/"

	frame := RenderCommandPalette(p, 16, 5)
	if !strings.Contains(frame, "/exit") {
		t.Fatalf("narrow palette lost the command name:\n%s", frame)
	}
	if strings.Contains(frame, "Exit") {
		t.Fatalf("narrow palette kept optional description while space was constrained:\n%s", frame)
	}
}

func TestRenderCommandPaletteOneRowKeepsExitCommand(t *testing.T) {
	p := NewCommandPalette()
	p.Open = true
	p.Input = "/"

	frame := RenderCommandPalette(p, 60, 1)
	if lipgloss.Height(frame) != 1 {
		t.Fatalf("one-row palette rendered %d rows: %q", lipgloss.Height(frame), frame)
	}
	if !strings.Contains(frame, "/exit") {
		t.Fatalf("one-row palette lost the selected command: %q", frame)
	}
	if lipgloss.Width(frame) > 60 {
		t.Fatalf("one-row palette exceeded width: %d", lipgloss.Width(frame))
	}
}

func TestOverlayCommandPaletteBoundsBaseAndPreservesItWhenClosed(t *testing.T) {
	base := "page 1\npage 2\npage 3\npage 4\npage 5\n" + strings.Repeat("项目状态", 20)
	p := NewCommandPalette()
	p.Open = true
	p.Input = "/"
	palette := RenderCommandPalette(p, 40, 6)

	open := overlayCommandPalette(base, palette, 40, 6)
	if lipgloss.Height(open) > 6 {
		t.Fatalf("open palette exceeded height: %d", lipgloss.Height(open))
	}
	for i, line := range strings.Split(open, "\n") {
		if got := lipgloss.Width(line); got > 40 {
			t.Fatalf("open palette row %d exceeded width: %d", i, got)
		}
	}
	if got := overlayCommandPalette(base, "", 40, 6); got != base {
		t.Fatalf("closed palette changed the underlying page: got %q want %q", got, base)
	}
}
