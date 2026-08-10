package tui

// ViewModel mapping tests (TUI task 10): MapWorkspace maps the aggregate
// workspace projection to the renderable model — selection, per-row legal
// actions, lifecycle, and health — without ever calling Execute.

import (
	"strings"
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
			{ID: "wf-1", Name: "calculator", Runtime: model.RuntimePaused},
		},
		Selected: "wf-1",
		Lifecycle: &app.WorkflowLifecycleView{
			Status: app.StatusView{Workflow: "wf-1", Name: "calculator", Runtime: model.RuntimePaused},
		},
		LegalActions: []app.LegalAction{
			{Label: "Resume", Kind: model.ResumeWorkflow},
		},
	})
	if vm.Selected.ID != "wf-1" || vm.Selected.Action != ActionResume {
		t.Fatalf("%+v", vm.Selected)
	}
	if vm.Selected.Name != "calculator" || vm.Lifecycle.Name != "calculator" {
		t.Fatalf("workflow name was not mapped: selected=%+v lifecycle=%+v", vm.Selected, vm.Lifecycle)
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
// to the first workflow. Row actions are NEVER inferred from the runtime
// status: without a Runtime LegalActions projection no row carries an
// action (Task 5: the TUI does not re-derive the state machine).
func TestMapWorkspaceSelectsFirstWhenEmpty(t *testing.T) {
	vm := MapWorkspace(app.WorkspaceView{
		Workflows: []app.WorkflowSummary{
			{ID: "wf-1", Runtime: model.RuntimeRunning},
			{ID: "wf-2", Runtime: model.RuntimeBlocked},
		},
	})
	if vm.Selected.ID != "wf-1" {
		t.Fatalf("selected = %+v", vm.Selected)
	}
	if vm.Selected.Action != ActionNone {
		t.Fatalf("running row action = %s, want none (no LegalActions projection)", vm.Selected.Action)
	}
	if vm.Workflows[1].Action != ActionNone {
		t.Fatalf("blocked row action = %s, want none (no LegalActions projection)", vm.Workflows[1].Action)
	}
}

func TestMapWorkspaceNormalizesStaleSelectionBeforeMappingActions(t *testing.T) {
	vm := MapWorkspace(app.WorkspaceView{
		Selected: "wf-removed",
		Workflows: []app.WorkflowSummary{
			{ID: "wf-1", Runtime: model.RuntimePaused},
			{ID: "wf-2", Runtime: model.RuntimeRunning},
		},
		Lifecycle:    &app.WorkflowLifecycleView{Status: app.StatusView{Workflow: "wf-1", Runtime: model.RuntimePaused}},
		LegalActions: []app.LegalAction{{Label: "Resume", Kind: model.ResumeWorkflow}},
	})
	if vm.Selected.ID != "wf-1" || vm.Selected.Action != ActionResume {
		t.Fatalf("stale selection did not normalize before action mapping: selected=%+v", vm.Selected)
	}
	if vm.Workflows[0].Action != ActionResume || vm.Workflows[1].Action != ActionNone {
		t.Fatalf("row actions after stale selection = %+v", vm.Workflows)
	}
}

func TestMapWorkspaceClearsSelectionWhenNoWorkflowsRemain(t *testing.T) {
	vm := MapWorkspace(app.WorkspaceView{
		Selected:     "wf-removed",
		Lifecycle:    &app.WorkflowLifecycleView{Status: app.StatusView{Workflow: "wf-removed", Runtime: model.RuntimePaused}},
		LegalActions: []app.LegalAction{{Label: "Resume", Kind: model.ResumeWorkflow}},
	})
	if vm.Selected.ID != "" || vm.Lifecycle != nil || len(vm.Actions) != 0 {
		t.Fatalf("empty workspace retained stale facts: selected=%+v lifecycle=%+v actions=%v", vm.Selected, vm.Lifecycle, vm.Actions)
	}
}

func TestMapWorkspaceDropsFactsForASelectionMismatchedLifecycle(t *testing.T) {
	vm := MapWorkspace(app.WorkspaceView{
		Selected:     "wf-removed",
		Workflows:    []app.WorkflowSummary{{ID: "wf-1", Runtime: model.RuntimePaused}},
		Lifecycle:    &app.WorkflowLifecycleView{Status: app.StatusView{Workflow: "wf-removed", Runtime: model.RuntimePaused}},
		LegalActions: []app.LegalAction{{Label: "Resume", Kind: model.ResumeWorkflow}},
	})
	if vm.Selected.ID != "wf-1" {
		t.Fatalf("selected workflow = %s, want wf-1", vm.Selected.ID)
	}
	if vm.Lifecycle != nil || len(vm.Actions) != 0 || vm.Selected.Action != ActionNone {
		t.Fatalf("mismatched lifecycle facts leaked into selected workflow: lifecycle=%+v actions=%v selected=%+v", vm.Lifecycle, vm.Actions, vm.Selected)
	}
}

// TestMapWorkspaceBlockedWithoutResume: a blocked workflow whose Runtime
// LegalActions contain NO Resume maps to no resume action, no resume key
// hint, and no resume text on the blocked page.
func TestMapWorkspaceBlockedWithoutResume(t *testing.T) {
	vm := MapWorkspace(app.WorkspaceView{
		Selected:  "wf-1",
		Workflows: []app.WorkflowSummary{{ID: "wf-1", Runtime: model.RuntimeBlocked}},
		Lifecycle: &app.WorkflowLifecycleView{
			Status:  app.StatusView{Workflow: "wf-1", Runtime: model.RuntimeBlocked},
			Blocked: true,
		},
		LegalActions: []app.LegalAction{{Label: "Inspect", Hint: "blocked"}},
	})
	if hasAction(vm.Actions, ActionResume) {
		t.Fatalf("blocked workflow without a resume legal action offered one: %v", vm.Actions)
	}
	if got := blockedHints(vm); strings.Contains(got, "r resume") {
		t.Fatalf("blocked hints hard-code resume: %q", got)
	}
	if got := RenderBlocked(vm); strings.Contains(got, "resume") {
		t.Fatalf("blocked render mentions resume: %q", got)
	}
}

// TestMapWorkspaceKeepsRuntimeResumeAction: when the Runtime LegalActions
// DO contain Resume the view model keeps the resume action verbatim (the
// TUI renders whatever the authoritative Runtime provides).
func TestMapWorkspaceKeepsRuntimeResumeAction(t *testing.T) {
	vm := MapWorkspace(app.WorkspaceView{
		Selected:  "wf-1",
		Workflows: []app.WorkflowSummary{{ID: "wf-1", Runtime: model.RuntimeBlocked}},
		Lifecycle: &app.WorkflowLifecycleView{
			Status:  app.StatusView{Workflow: "wf-1", Runtime: model.RuntimeBlocked},
			Blocked: true,
		},
		LegalActions: []app.LegalAction{{Label: "Resume", Kind: model.ResumeWorkflow}},
	})
	if !hasAction(vm.Actions, ActionResume) {
		t.Fatalf("the runtime resume legal action was dropped: %v", vm.Actions)
	}
	if got := blockedHints(vm); !strings.Contains(got, "r resume") {
		t.Fatalf("a runtime resume legal action must keep the resume hint: %q", got)
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
		Selected:  "wf-1",
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
