package tui

// ViewModel mapping tests (TUI task 10): MapWorkspace maps the aggregate
// workspace projection to the renderable model — selection, per-row legal
// actions, lifecycle, and health — without ever calling Execute.

import (
	"testing"

	"cflow.local/cflow/internal/app"
	"cflow.local/cflow/internal/model"
)

// TestMapWorkspace is the TUI task 10 failure test: a paused workflow's
// workspace row resolves as the selection with the resume action.
func TestMapWorkspace(t *testing.T) {
	vm := MapWorkspace(app.WorkspaceView{
		Project: app.ProjectView{Key: "k", Root: "/r", Name: "repo"},
		Workflows: []app.WorkflowSummary{
			{ID: "wf-1", Runtime: model.RuntimePaused},
		},
		Selected: "wf-1",
		Lifecycle: &app.WorkflowLifecycleView{
			Status: app.StatusView{Workflow: "wf-1", Runtime: model.RuntimePaused},
		},
		LegalActions: []app.LegalAction{
			{Label: "Resume", Kind: model.ResumeWorkflow},
		},
	})
	if vm.Selected.ID != "wf-1" || vm.Selected.Action != ActionResume {
		t.Fatalf("%+v", vm.Selected)
	}
	if len(vm.Workflows) != 1 || vm.Workflows[0].Action != ActionResume {
		t.Fatalf("workflows = %+v", vm.Workflows)
	}
	if vm.Lifecycle == nil || vm.Lifecycle.ID != "wf-1" {
		t.Fatalf("lifecycle = %+v", vm.Lifecycle)
	}
	if len(vm.Actions) == 0 {
		t.Fatalf("a paused workflow must offer the resume action")
	}
}

// TestMapWorkspaceSelectsFirstWhenEmpty: no explicit selection resolves
// to the first workflow.
func TestMapWorkspaceSelectsFirstWhenEmpty(t *testing.T) {
	vm := MapWorkspace(app.WorkspaceView{
		Workflows: []app.WorkflowSummary{
			{ID: "wf-1", Runtime: model.RuntimeRunning},
			{ID: "wf-2", Runtime: model.RuntimeBlocked},
		},
	})
	if vm.Selected.ID != "wf-1" || vm.Selected.Action != ActionPause {
		t.Fatalf("selected = %+v", vm.Selected)
	}
	if vm.Workflows[1].Action != ActionInspect {
		t.Fatalf("blocked row action = %s, want inspect", vm.Workflows[1].Action)
	}
}

// TestMapWorkspaceEmptyWorkspace: no workflows map to an empty model
// without a selection.
func TestMapWorkspaceEmptyWorkspace(t *testing.T) {
	vm := MapWorkspace(app.WorkspaceView{})
	if vm.Selected.ID != "" || len(vm.Workflows) != 0 {
		t.Fatalf("empty workspace mapped to %+v", vm)
	}
}

// TestMapWorkspaceLifecycleAndHealth: the lifecycle and health facts map
// through unchanged.
func TestMapWorkspaceLifecycleAndHealth(t *testing.T) {
	vm := MapWorkspace(app.WorkspaceView{
		Selected: "wf-1",
		Workflows: []app.WorkflowSummary{{ID: "wf-1", Runtime: model.RuntimeSucceeded}},
		Lifecycle: &app.WorkflowLifecycleView{
			Status: app.StatusView{
				Workflow: "wf-1", Stage: model.StageCompleted, Runtime: model.RuntimeSucceeded,
				TargetBranch: "main", VerifiedWorkspaceHead: "0123456789abcdef",
			},
			Adopted: true,
			Plan: &app.PlanView{
				PlanStatus: model.PlanApproved, Revision: 1, Hash: "h", Approved: true,
			},
		},
		Health: app.HealthView{
			GitAvailable: true,
			Providers:    []app.ProviderHealth{{Name: "fake", Compatible: true}},
		},
	})
	if vm.Lifecycle == nil || !vm.Lifecycle.Adopted || vm.Lifecycle.Head == "" ||
		vm.Lifecycle.Plan == nil || !vm.Lifecycle.Plan.Approved {
		t.Fatalf("lifecycle = %+v", vm.Lifecycle)
	}
	if !vm.Health.GitAvailable || len(vm.Health.Providers) != 1 {
		t.Fatalf("health = %+v", vm.Health)
	}
}
