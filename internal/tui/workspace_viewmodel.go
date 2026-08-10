// Package tui is the full-screen Bubble Tea interface of CFlow (design
// §1: the TUI is the default entry point on an interactive terminal). It
// renders the read-only project workspace and drives the explicit user
// confirmations; it never decides lifecycle transitions itself.
package tui

// Workspace ViewModel: the pure mapping from the aggregate workspace
// projection to renderable data. It never calls Execute, queries the
// Application, or mutates anything; navigation only updates UI selection.

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
	Name    string
	Stage   model.WorkflowStage
	Runtime model.RuntimeStatus
	Blocked bool
	Action  Action
}

// LifecycleItem is the selected workflow's lifecycle facts the main
// column renders.
type LifecycleItem struct {
	ID      model.WorkflowID
	Name    string
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

// WorkspaceViewModel is the pure renderable workspace model.
// WorkspaceModel is retained as a source-compatible alias for lifecycle
// pages that consume the same read-only projection type.
type WorkspaceModel = WorkspaceViewModel

type WorkspaceViewModel struct {
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
func MapWorkspace(v app.WorkspaceView) WorkspaceViewModel {
	m := WorkspaceViewModel{
		Project:   v.Project,
		Workflows: make([]WorkflowItem, 0, len(v.Workflows)),
	}
	selected := v.Selected
	selectedFound := false
	for _, workflow := range v.Workflows {
		if workflow.ID == selected {
			selectedFound = true
			break
		}
	}
	if len(v.Workflows) == 0 {
		selected = ""
	} else if !selectedFound {
		selected = v.Workflows[0].ID
	}
	var projectedActions []Action
	if v.Lifecycle != nil && selected != "" && v.Lifecycle.Status.Workflow == selected {
		projectedActions = legalActionsOf(v.LegalActions)
	}
	for _, w := range v.Workflows {
		item := WorkflowItem{
			ID: w.ID, Name: w.Name, Stage: w.Stage, Runtime: w.Runtime,
			Blocked: w.Runtime == model.RuntimeBlocked,
		}
		// The row action is NEVER inferred from the stage/runtime strings
		// (design §5.3: the TUI does not re-derive the state machine). The
		// only authoritative actions are the selected workflow's Runtime
		// LegalActions; other rows have no per-row action.
		if w.ID == selected {
			item.Action = primaryActionOf(projectedActions)
		}
		m.Workflows = append(m.Workflows, item)
		if w.ID == selected {
			m.Selected = item
		}
	}
	if m.Selected.ID == "" && len(m.Workflows) > 0 {
		m.Selected = m.Workflows[0]
	}
	// Lifecycle facts and legal actions are authoritative only for the
	// normalized selected workflow. A stale projection must not render facts
	// or advertise actions for a removed/different workflow.
	if v.Lifecycle != nil && selected != "" && v.Lifecycle.Status.Workflow == selected {
		lc := &LifecycleItem{
			ID: v.Lifecycle.Status.Workflow, Name: v.Lifecycle.Status.Name, Stage: v.Lifecycle.Status.Stage,
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
		m.Actions = projectedActions
	}
	m.Health = v.Health
	return m
}

// primaryActionOf reduces the selected workflow's mapped legal actions to
// the single row action (the first in stable order), or ActionNone when
// the Runtime projects no legal action.
func primaryActionOf(actions []Action) Action {
	if len(actions) == 0 {
		return ActionNone
	}
	return actions[0]
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
