// Package tui is the full-screen Bubble Tea interface of CFlow (design
// §1: the TUI is the default entry point on an interactive terminal). It
// renders the read-only project workspace and drives the explicit user
// confirmations; it never decides lifecycle transitions itself.
package tui

// ViewModel: the pure mapping from the aggregate workspace projection to
// the renderable workspace model (TUI task 10). The mapping never calls
// Execute and never mutates anything; navigation only updates the UI
// selection.

import (
	"sort"

	"cflow.local/cflow/internal/app"
	"cflow.local/cflow/internal/model"
)

// Action is one legal action the selected workflow may take right now.
type Action string

const (
	ActionNone    Action = ""
	ActionResume  Action = "resume"
	ActionPause   Action = "pause"
	ActionCancel  Action = "cancel"
	ActionAdopt   Action = "adopt"
	ActionInspect Action = "inspect"
	ActionDiscuss Action = "discuss"
	ActionMigrate Action = "layout-migration"
)

// WorkflowItem is one workspace row of the workflow column.
type WorkflowItem struct {
	ID      model.WorkflowID
	Stage   model.WorkflowStage
	Runtime model.RuntimeStatus
	Blocked bool
	Action  Action
}

// LifecycleItem is the selected workflow's lifecycle facts the main
// column renders.
type LifecycleItem struct {
	ID      model.WorkflowID
	Stage   model.WorkflowStage
	Runtime model.RuntimeStatus
	Target  string
	Plan    *PlanItem
	Blocked bool
	Adopted bool
	Head    string
}

// PlanItem is the active Plan revision summary.
type PlanItem struct {
	Status   model.PlanStatus
	Revision int
	Hash     string
	Approved bool
}

// WorkspaceModel is the pure renderable workspace model.
type WorkspaceModel struct {
	Project   app.ProjectView
	Selected  WorkflowItem
	Workflows []WorkflowItem
	Lifecycle *LifecycleItem
	Health    app.HealthView
	Actions   []Action
}

// MapWorkspace maps the aggregate workspace projection to the renderable
// model. Selection resolves to the explicit workflow or the first
// workflow; each workflow row carries the legal Action its Runtime
// permits.
func MapWorkspace(v app.WorkspaceView) WorkspaceModel {
	m := WorkspaceModel{
		Project:   v.Project,
		Workflows: make([]WorkflowItem, 0, len(v.Workflows)),
	}
	selected := v.Selected
	if selected == "" && len(v.Workflows) > 0 {
		selected = v.Workflows[0].ID
	}
	for _, w := range v.Workflows {
		item := WorkflowItem{
			ID: w.ID, Stage: w.Stage, Runtime: w.Runtime,
			Blocked: w.Runtime == model.RuntimeBlocked,
			Action:  actionFor(w.Runtime, w.Stage),
		}
		m.Workflows = append(m.Workflows, item)
		if w.ID == selected {
			m.Selected = item
		}
	}
	if m.Selected.ID == "" && len(m.Workflows) > 0 {
		m.Selected = m.Workflows[0]
	}
	if v.Lifecycle != nil {
		lc := &LifecycleItem{
			ID: v.Lifecycle.Status.Workflow, Stage: v.Lifecycle.Status.Stage,
			Runtime: v.Lifecycle.Status.Runtime, Target: v.Lifecycle.Status.TargetBranch,
			Blocked: v.Lifecycle.Blocked, Adopted: v.Lifecycle.Adopted,
			Head: v.Lifecycle.Status.VerifiedWorkspaceHead,
		}
		if v.Lifecycle.Plan != nil {
			lc.Plan = &PlanItem{
				Status: v.Lifecycle.Plan.PlanStatus, Revision: v.Lifecycle.Plan.Revision,
				Hash: v.Lifecycle.Plan.Hash, Approved: v.Lifecycle.Plan.Approved,
			}
		}
		m.Lifecycle = lc
		m.Actions = legalActionsOf(v.LegalActions)
	}
	m.Health = v.Health
	return m
}

// actionFor is the deterministic legal action of one workflow row.
func actionFor(rt model.RuntimeStatus, stage model.WorkflowStage) Action {
	switch rt {
	case model.RuntimePaused:
		return ActionResume
	case model.RuntimeBlocked:
		return ActionInspect
	case model.RuntimeRunning:
		return ActionPause
	case model.RuntimeSucceeded:
		return ActionCancel
	}
	if stage == model.StageRequirementDiscussion || stage == model.StagePlanGeneration {
		return ActionDiscuss
	}
	return ActionNone
}

// legalActionsOf reduces the app's legal-action list to the distinct
// renderable actions, in a stable order.
func legalActionsOf(actions []app.LegalAction) []Action {
	set := map[Action]bool{}
	for _, a := range actions {
		switch a.Kind {
		case model.ResumeWorkflow:
			set[ActionResume] = true
		case model.PauseWorkflow:
			set[ActionPause] = true
		case model.CancelWorkflow:
			set[ActionCancel] = true
		}
		if a.Hint == "discussion" {
			set[ActionDiscuss] = true
		}
		if a.Hint == "blocked" {
			set[ActionInspect] = true
		}
		if a.Hint == "layout-migration" {
			set[ActionMigrate] = true
		}
	}
	out := make([]Action, 0, len(set))
	for a := range set {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return string(out[i]) < string(out[j]) })
	return out
}
