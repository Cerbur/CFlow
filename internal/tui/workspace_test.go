package tui

// Workspace screen tests: the three-column Home layout renders the
// workflow column, the central workspace, and the inspector; a
// narrow terminal collapses the inspector below the main column.

import (
	"fmt"
	"strings"
	"testing"

	"cflow.local/cflow/internal/app"
	"cflow.local/cflow/internal/model"

	"charm.land/lipgloss/v2"
)

// TestRenderWorkspaceWide: the wide layout shows all three columns.
func TestRenderWorkspaceWide(t *testing.T) {
	m := sampleWorkspaceViewModel()
	got := RenderWorkspace(m, 120, 45)
	for _, want := range []string{"project:", "workflows:", "calculator", "workflow calculator (wf-1)", "actions:", "inspector:"} {
		if !strings.Contains(got, want) {
			t.Fatalf("wide render misses %q:\n%s", want, got)
		}
	}
	for _, line := range strings.Split(got, "\n") {
		if width := lipgloss.Width(line); width > 120 {
			t.Fatalf("wide render line has width %d > 120: %q", width, line)
		}
	}
}

func TestRenderWorkspaceUsesWorkbenchFrame(t *testing.T) {
	got := RenderWorkspace(sampleWorkspaceViewModel(), 120, 45)
	for _, want := range []string{
		"CFlow",
		"WORKFLOWS",
		"WORKSPACE",
		"INSPECTOR",
		"Provider:",
		"↑↓ navigate",
		"│",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("workbench render misses %q:\n%s", want, got)
		}
	}
}

// TestRenderWorkspaceNarrow: Compact below the narrow width keeps the
// inspector facts as an inline read-only summary rather than a full panel.
func TestRenderWorkspaceNarrow(t *testing.T) {
	m := sampleWorkspaceViewModel()
	got := RenderWorkspace(m, 80, 24)
	if !strings.Contains(got, "inspector: summary") {
		t.Fatalf("compact render misses the inline inspector summary:\n%s", got)
	}
	if strings.Contains(got, "INSPECTOR") {
		t.Fatalf("compact render unexpectedly contains the full inspector panel:\n%s", got)
	}
	for _, line := range strings.Split(got, "\n") {
		if width := lipgloss.Width(line); width > 80 {
			t.Fatalf("narrow render line has width %d > 80: %q", width, line)
		}
	}
}

func TestRenderWorkspaceResponsiveBoundsAndStructure(t *testing.T) {
	cases := []struct {
		name   string
		width  int
		height int
		want   []string
		avoid  []string
	}{
		{name: "wide-large", width: 160, height: 45, want: []string{"WORKFLOWS", "WORKSPACE", "INSPECTOR"}},
		{name: "wide-threshold", width: 120, height: 30, want: []string{"WORKFLOWS", "WORKSPACE", "INSPECTOR"}},
		{name: "medium", width: 100, height: 24, want: []string{"WORKFLOWS", "WORKSPACE", "SUMMARY", "TARGET", "WORKSPACE"}},
		{name: "compact", width: 80, height: 24, want: []string{"WORKSPACE", "STAGE", "RUNTIME", "LEGAL ACTIONS"}, avoid: []string{"INSPECTOR"}},
		{name: "compact-small", width: 60, height: 18, want: []string{"WORKSPACE", "STAGE", "RUNTIME", "LEGAL ACTIONS"}, avoid: []string{"INSPECTOR"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RenderWorkspace(longWorkspaceViewModel(), tc.width, tc.height)
			assertWorkspaceBounds(t, got, tc.width, tc.height)
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Fatalf("render misses %q at %dx%d:\n%s", want, tc.width, tc.height, got)
				}
			}
			for _, avoid := range tc.avoid {
				if strings.Contains(got, avoid) {
					t.Fatalf("render unexpectedly contains %q at %dx%d:\n%s", avoid, tc.width, tc.height, got)
				}
			}
			assertCompletePanelBorders(t, got)
		})
	}
}

func TestRenderWorkspaceHomeRowsAtTargetSizes(t *testing.T) {
	for _, tc := range []struct {
		width  int
		height int
	}{
		{width: 160, height: 45},
		{width: 120, height: 30},
		{width: 100, height: 24},
		{width: 80, height: 24},
		{width: 60, height: 18},
		{width: 88, height: 6},
		{width: 100, height: 6},
		{width: 120, height: 6},
	} {
		t.Run(fmt.Sprintf("%dx%d", tc.width, tc.height), func(t *testing.T) {
			frame := RenderWorkspace(longWorkspaceViewModel(), tc.width, tc.height)
			visible := visibleTerminalText(frame)
			if strings.Contains(frame, "←→ lifecycle") {
				t.Fatal("Home still advertises lifecycle navigation")
			}
			if !strings.Contains(visible, "NEW WORKFLOW") {
				t.Fatal("Home misses the New Workflow row")
			}
			if !strings.Contains(visible, "WORKSPACE") {
				t.Fatal("Home misses the central workspace panel")
			}
			assertWorkspaceBounds(t, frame, tc.width, tc.height)
		})
	}
}

func TestRenderWorkspaceCompactPreservesBlockedFact(t *testing.T) {
	m := sampleWorkspaceViewModel()
	m.Lifecycle.Blocked = true
	m.Lifecycle.Runtime = model.RuntimeRunning

	got := RenderWorkspace(m, 80, 24)
	if !strings.Contains(got, "blocked · inspect findings") {
		t.Fatalf("compact render hides authoritative blocked fact:\n%s", got)
	}
}

func TestRenderWorkspaceUsesAuthoritativeStageForLifecycleProgress(t *testing.T) {
	m := longWorkspaceViewModel()
	wide := RenderWorkspace(m, 120, 30)
	if !strings.Contains(wide, "● Define") {
		t.Fatalf("workflow-generation stage did not activate Define: %s", wide)
	}
	compact := RenderWorkspace(m, 80, 24)
	if !strings.Contains(compact, "LIFECYCLE  3/7 · Define") {
		t.Fatalf("workflow-generation stage did not map to compact Define progress: %s", compact)
	}

	m.Lifecycle.Stage = model.StageCompleted
	completed := RenderWorkspace(m, 80, 24)
	if !strings.Contains(completed, "LIFECYCLE  6/7 · Apply") {
		t.Fatalf("completed stage did not map to Apply progress: %s", completed)
	}
}

func TestRenderWorkspaceFallsBackToMinimalBeforePanelsLosePrimaryContext(t *testing.T) {
	for _, tc := range []struct {
		width  int
		height int
	}{
		{width: 88, height: 7},
		{width: 60, height: 12},
	} {
		got := RenderWorkspace(longWorkspaceViewModel(), tc.width, tc.height)
		assertWorkspaceBounds(t, got, tc.width, tc.height)
		if strings.ContainsAny(got, "╭╮├┤╰╯") {
			t.Fatalf("unsafe threshold rendered a panel at %dx%d:\n%s", tc.width, tc.height, got)
		}
		if !strings.Contains(got, "/") {
			t.Fatalf("minimal threshold lost footer at %dx%d:\n%s", tc.width, tc.height, got)
		}
	}
}

func TestRenderWorkspaceFooterKeepsCommandHintAtTinyWidths(t *testing.T) {
	m := sampleWorkspaceViewModel()
	for _, width := range []int{1, 4, 5, 6} {
		got := renderWorkspaceFooter(m, "", width)
		if !strings.Contains(got, "/") {
			t.Fatalf("footer at width %d lost command affordance: %q", width, got)
		}
		if gotWidth := lipgloss.Width(got); gotWidth > width {
			t.Fatalf("footer width %d > %d: %q", gotWidth, width, got)
		}
	}
}

func TestRenderWorkspaceSmallPanelViewportsStayMinimal(t *testing.T) {
	for _, tc := range []struct {
		width  int
		height int
	}{
		{width: 88, height: 6},
		{width: 100, height: 6},
		{width: 120, height: 6},
	} {
		t.Run(fmt.Sprintf("%dx%d", tc.width, tc.height), func(t *testing.T) {
			got := RenderWorkspace(longWorkspaceViewModel(), tc.width, tc.height)
			assertWorkspaceBounds(t, got, tc.width, tc.height)
			if strings.ContainsAny(got, "╭╮├┤╰╯") {
				t.Fatalf("small viewport contains a partial panel at %dx%d:\n%s", tc.width, tc.height, got)
			}
		})
	}
}

func TestWorkspaceFitStyledLineIsANSIUnicodeAndNewlineAware(t *testing.T) {
	cases := []struct {
		name  string
		input string
		width int
	}{
		{name: "under", input: "abc", width: 8},
		{name: "exact", input: "abc", width: 3},
		{name: "over", input: "abcdefgh", width: 5},
		{name: "cjk", input: "项目状态", width: 8},
		{name: "cjk-truncated-boundary", input: "项目状态", width: 4},
		{name: "emoji", input: "👩‍💻 ready", width: 8},
		{name: "emoji-truncated", input: "👩‍💻 ready", width: 4},
		{name: "tab", input: "a\tb", width: 8},
		{name: "ansi", input: workspaceTheme.Selected.Render("selected"), width: 12},
		{name: "ansi-truncated", input: workspaceTheme.Selected.Render("selected content"), width: 8},
		{name: "newline", input: "first\nsecond", width: 12},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := workspaceFitStyledLine(tc.input, tc.width)
			if strings.ContainsAny(got, "\r\n") {
				t.Fatalf("helper returned multiple rows: %q", got)
			}
			if gotWidth := lipgloss.Width(got); gotWidth != tc.width {
				t.Fatalf("width = %d, want %d: %q", gotWidth, tc.width, got)
			}
		})
	}
	if got := workspaceTruncateText("first\nsecond", 8); strings.ContainsAny(got, "\r\n") {
		t.Fatalf("truncate helper returned multiple rows: %q", got)
	}
}

func TestWorkspacePanelKeepsBorderColumnsAlignedAfterUnicodeTruncation(t *testing.T) {
	got := workspacePanelWithHeight("标题", []string{"项目状态"}, 12, 7)
	for i, line := range strings.Split(got, "\n") {
		if gotWidth := lipgloss.Width(line); gotWidth != 12 {
			t.Fatalf("panel row %d width = %d, want 12: %q", i, gotWidth, line)
		}
	}
}

func TestWorkspaceFooterUsesOnlyHomeNavigationHints(t *testing.T) {
	m := longWorkspaceViewModel()
	m.Actions = []Action{ActionResume, ActionPause, ActionCancel, ActionMigrate}
	for _, width := range []int{60, 80} {
		got := renderWorkspaceFooter(m, "", width)
		for _, want := range []string{"/ command", "Enter open", "Esc back", "↑↓ navigate"} {
			if !strings.Contains(got, want) {
				t.Fatalf("footer at width %d misses Home hint %q: %q", width, want, got)
			}
		}
		for _, forbidden := range []string{"q quit", "r resume", "p pause", "x cancel", "m migrate", "n create", "←→ lifecycle"} {
			if strings.Contains(got, forbidden) {
				t.Fatalf("footer at width %d retains legacy hint %q: %q", width, forbidden, got)
			}
		}
		if gotWidth := lipgloss.Width(got); gotWidth > width {
			t.Fatalf("footer width %d > %d: %q", gotWidth, width, got)
		}
	}
}

func TestRenderWorkspaceMinimalViewportHasNoPartialPanel(t *testing.T) {
	for _, tc := range []struct {
		width  int
		height int
	}{
		{width: 59, height: 11},
		{width: 40, height: 5},
	} {
		got := RenderWorkspace(longWorkspaceViewModel(), tc.width, tc.height)
		assertWorkspaceBounds(t, got, tc.width, tc.height)
		if strings.ContainsAny(got, "╭╮├┤╰╯") {
			t.Fatalf("minimal render contains a partial panel at %dx%d:\n%s", tc.width, tc.height, got)
		}
	}
}

func TestRenderWorkspaceDoesNotInventLegalActions(t *testing.T) {
	got := RenderWorkspace(sampleWorkspaceViewModel(), 160, 45)
	if !strings.Contains(got, "→ resume") {
		t.Fatalf("projected resume action missing:\n%s", got)
	}
	for _, forbidden := range []string{"→ pause", "→ cancel", "→ layout-migration"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("unprojected action %q rendered:\n%s", forbidden, got)
		}
	}
}

func assertWorkspaceBounds(t *testing.T, output string, width, height int) {
	t.Helper()
	if gotHeight := lipgloss.Height(output); gotHeight > height {
		t.Fatalf("render height %d > %d:\n%s", gotHeight, height, output)
	}
	for i, line := range strings.Split(output, "\n") {
		if gotWidth := lipgloss.Width(line); gotWidth > width {
			t.Fatalf("line %d width %d > %d: %q", i, gotWidth, width, line)
		}
	}
}

func assertCompletePanelBorders(t *testing.T, output string) {
	t.Helper()
	for i, line := range strings.Split(output, "\n") {
		switch {
		case strings.Contains(line, "╭") && !strings.Contains(line, "╮"):
			t.Fatalf("opening panel border is incomplete on line %d: %q", i, line)
		case strings.Contains(line, "├") && !strings.Contains(line, "┤"):
			t.Fatalf("separator panel border is incomplete on line %d: %q", i, line)
		case strings.Contains(line, "╰") && !strings.Contains(line, "╯"):
			t.Fatalf("closing panel border is incomplete on line %d: %q", i, line)
		}
	}
}

func longWorkspaceViewModel() WorkspaceViewModel {
	return MapWorkspace(app.WorkspaceView{
		Project:  app.ProjectView{Key: "项目-这是一个非常长的项目标识", Root: "/Users/example/非常/长/的/项目/路径/with/a/long/segment", Name: "仓库工作区"},
		Selected: "wf-长名称",
		Workflows: []app.WorkflowSummary{
			{ID: "wf-长名称", Name: "这是一个非常长的 Workflow 名称", Stage: model.StageWorkflowGeneration, Runtime: model.RuntimeRunning},
			{ID: "wf-blocked", Name: "blocked workflow", Stage: model.StageExecution, Runtime: model.RuntimeBlocked},
		},
		Lifecycle: &app.WorkflowLifecycleView{
			Status: app.StatusView{
				Workflow: "wf-长名称", Name: "这是一个非常长的 Workflow 名称", Stage: model.StageWorkflowGeneration,
				Runtime: model.RuntimeRunning, TargetBranch: "feature/这是一条很长的目标分支名称", VerifiedWorkspaceHead: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			},
			Plan: &app.PlanView{PlanStatus: model.PlanApproved, Revision: 42, Approved: true, Hash: "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"},
		},
		Health: app.HealthView{
			GitAvailable: true,
			Providers:    []app.ProviderHealth{{Name: "非常长的 Provider 名称", Compatible: true, Executable: "/usr/local/bin/provider-with-a-very-long-name", CLIVersion: "v2026.08.11"}},
		},
		LegalActions: []app.LegalAction{{Label: "Resume", Kind: model.ResumeWorkflow}},
	})
}

// TestRenderWorkspaceEmpty: the empty workspace renders a hint without
// panicking.
func TestRenderWorkspaceEmpty(t *testing.T) {
	m := MapWorkspace(app.WorkspaceView{})
	got := RenderWorkspace(m, 120, 45)
	if !strings.Contains(got, "no existing workflows") || !strings.Contains(got, "NEW WORKFLOW") {
		t.Fatalf("empty render = %q", got)
	}
}

// TestMapWorkspaceIncludesLayoutMigrationLegalAction proves the TUI
// exposes migration only when the Application's authoritative
// LegalActions projection permits it.
func TestMapWorkspaceIncludesLayoutMigrationLegalAction(t *testing.T) {
	m := MapWorkspace(app.WorkspaceView{
		Selected:     "wf-legacy",
		Workflows:    []app.WorkflowSummary{{ID: "wf-legacy", Runtime: model.RuntimePaused}},
		Lifecycle:    &app.WorkflowLifecycleView{Status: app.StatusView{Workflow: "wf-legacy"}},
		LegalActions: []app.LegalAction{{Label: "Migrate layout", Hint: "layout-migration"}},
	})
	found := false
	for _, action := range m.Actions {
		found = found || action == Action("layout-migration")
	}
	if !found {
		t.Fatalf("migration legal action missing: %v", m.Actions)
	}
}

// TestWorkspaceNavigationOnlyUpdatesSelection: navigation keys update
// only the UI selection; no Execute is ever called (the mapping is pure).
func TestWorkspaceNavigationOnlyUpdatesSelection(t *testing.T) {
	m := sampleWorkspaceViewModel()
	if m.Selected.ID != "wf-1" {
		t.Fatalf("selection = %s", m.Selected.ID)
	}
	// The selection follows the workflow column order; the model is a
	// pure function of the projection, so re-mapping with a different
	// selection changes only the selection.
	m2 := MapWorkspace(app.WorkspaceView{
		Selected: "wf-2",
		Workflows: []app.WorkflowSummary{
			{ID: "wf-1", Runtime: model.RuntimeRunning},
			{ID: "wf-2", Runtime: model.RuntimeBlocked},
		},
		Lifecycle: &app.WorkflowLifecycleView{Status: app.StatusView{Workflow: "wf-2"}},
	})
	if m2.Selected.ID != "wf-2" {
		t.Fatalf("selection after navigation = %s", m2.Selected.ID)
	}
}

func sampleWorkspaceViewModel() WorkspaceViewModel {
	return MapWorkspace(app.WorkspaceView{
		Project:  app.ProjectView{Key: "k", Root: "/r", Name: "repo"},
		Selected: "wf-1",
		Workflows: []app.WorkflowSummary{
			{ID: "wf-1", Name: "calculator", Runtime: model.RuntimePaused},
			{ID: "wf-2", Runtime: model.RuntimeBlocked},
		},
		Lifecycle: &app.WorkflowLifecycleView{
			Status: app.StatusView{
				Workflow: "wf-1", Name: "calculator", Stage: model.StageWorkflowGeneration, Runtime: model.RuntimePaused,
				TargetBranch: "main",
			},
			Plan: &app.PlanView{PlanStatus: model.PlanApproved, Revision: 1, Approved: true},
		},
		Health: app.HealthView{GitAvailable: true},
		LegalActions: []app.LegalAction{
			{Label: "Resume", Kind: model.ResumeWorkflow},
		},
	})
}

// TestMapDiscussionReturn: the Return Page maps the app projection and
// renders every legal action; Finish freezes the Change Set.
func TestMapDiscussionReturn(t *testing.T) {
	p := MapDiscussionReturn(app.DiscussionReturnView{
		Workflow: "wf-1", Session: "sess-1", Provider: "fake",
		ChangeSet: &model.ArtifactRef{Workflow: "wf-1", Type: model.ArtifactChangeSet, Revision: 1, Hash: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"},
		Actions:   []string{"continue", "finish", "switch-agent", "pause", "cancel"},
	})
	if p.Session != "sess-1" || p.Provider != "fake" {
		t.Fatalf("page = %+v", p)
	}
	if len(p.Actions) != 5 {
		t.Fatalf("actions = %+v", p.Actions)
	}
	got := RenderDiscussionReturn(p)
	for _, want := range []string{"Finish", "Continue Same Session", "change set: rev 1"} {
		if !strings.Contains(got, want) {
			t.Fatalf("return page misses %q:\n%s", want, got)
		}
	}
}
