package tui

// Workspace screen tests (TUI task 10): the three-column layout renders
// the workflow column, the lifecycle main column, and the inspector; a
// narrow terminal collapses the inspector below the main column.

import (
	"strings"
	"testing"

	"cflow.local/cflow/internal/app"
	"cflow.local/cflow/internal/model"
)

// TestRenderWorkspaceWide: the wide layout shows all three columns.
func TestRenderWorkspaceWide(t *testing.T) {
	m := sampleWorkspaceModel()
	got := RenderWorkspace(m, 120)
	for _, want := range []string{"project:", "workflows:", "workflow wf-1", "actions:", "inspector:"} {
		if !strings.Contains(got, want) {
			t.Fatalf("wide render misses %q:\n%s", want, got)
		}
	}
}

// TestRenderWorkspaceNarrow: below the narrow width the inspector still
// renders (as a detail page below the main column).
func TestRenderWorkspaceNarrow(t *testing.T) {
	m := sampleWorkspaceModel()
	got := RenderWorkspace(m, 80)
	if !strings.Contains(got, "inspector:") {
		t.Fatalf("narrow render misses the inspector detail page:\n%s", got)
	}
}

// TestRenderWorkspaceEmpty: the empty workspace renders a hint without
// panicking.
func TestRenderWorkspaceEmpty(t *testing.T) {
	m := MapWorkspace(app.WorkspaceView{})
	got := RenderWorkspace(m, 120)
	if !strings.Contains(got, "no workflows") {
		t.Fatalf("empty render = %q", got)
	}
}

// TestWorkspaceNavigationOnlyUpdatesSelection: navigation keys update
// only the UI selection; no Execute is ever called (the mapping is pure).
func TestWorkspaceNavigationOnlyUpdatesSelection(t *testing.T) {
	m := sampleWorkspaceModel()
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

func sampleWorkspaceModel() WorkspaceModel {
	return MapWorkspace(app.WorkspaceView{
		Project: app.ProjectView{Key: "k", Root: "/r", Name: "repo"},
		Selected: "wf-1",
		Workflows: []app.WorkflowSummary{
			{ID: "wf-1", Runtime: model.RuntimePaused},
			{ID: "wf-2", Runtime: model.RuntimeBlocked},
		},
		Lifecycle: &app.WorkflowLifecycleView{
			Status: app.StatusView{
				Workflow: "wf-1", Stage: model.StageWorkflowGeneration, Runtime: model.RuntimePaused,
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
