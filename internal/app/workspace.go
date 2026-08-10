package app

// The aggregate TUI workspace projection (design §1, TUI task 10): one
// bounded, read-only View carrying the Project identity, every Workflow
// summary, the selected Workflow's lifecycle facts, the provider/git
// health, and the legal actions of the selected Workflow — so the TUI
// renders one consistent frame from one Query, never several
// mutually-inconsistent Queries.

import (
	"context"
	"path/filepath"
	"sort"

	"cflow.local/cflow/internal/agent"
	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/store"
)

// queryWorkspace assembles the aggregate workspace projection.
func (a *Application) queryWorkspace(ctx context.Context, q ProjectWorkspaceQuery) (View, error) {
	view := WorkspaceView{
		Project: ProjectView{Key: a.project.Key, Root: a.project.Root, Name: filepath.Base(a.project.Root)},
	}
	ids, err := a.knownWorkflows(ctx)
	if err != nil {
		return nil, orCtx(ctx, err)
	}
	for _, wf := range ids {
		agg, err := a.readAggregate(ctx, wf, store.StoreQuery{})
		if err != nil {
			return nil, orCtx(ctx, err)
		}
		if agg.State.Workflow.ID == "" {
			continue // an orphaned workflow directory: Recovery's concern
		}
		st := agg.State
		st.Workflow.Name = a.workflowDisplayName(st)
		view.Workflows = append(view.Workflows, workflowSummary(st))
	}
	// The selected workflow: the explicit id, else the first workflow.
	selected := q.Selected
	if selected == "" && len(view.Workflows) > 0 {
		selected = view.Workflows[0].ID
	}
	view.Selected = selected
	if selected != "" {
		lifecycle, err := a.workspaceLifecycle(ctx, selected)
		if err != nil {
			return nil, orCtx(ctx, err)
		}
		view.Lifecycle = lifecycle
		view.LegalActions = legalActions(lifecycle.Status)
	}
	view.Health = a.workspaceHealth(ctx)
	return view, nil
}

// workspaceLifecycle projects the selected workflow's lifecycle facts.
func (a *Application) workspaceLifecycle(ctx context.Context, wf model.WorkflowID) (*WorkflowLifecycleView, error) {
	agg, err := a.readAggregate(ctx, wf, store.StoreQuery{})
	if err != nil {
		return nil, err
	}
	if agg.State.Workflow.ID == "" {
		return nil, model.InvalidInputFault("no such workflow: " + string(wf))
	}
	st := agg.State
	st.Workflow.Name = a.workflowDisplayName(st)
	lc := &WorkflowLifecycleView{
		Status:  statusView(st),
		Blocked: workflowBlocked(st),
		Adopted: st.Workflow.LayoutVersion >= 2 && st.Workflow.VerifiedWorkspaceHead != "",
	}
	if st.Plan != nil {
		lc.Plan = &PlanView{
			Workflow:   wf,
			Stage:      st.Workflow.Stage,
			Runtime:    st.Workflow.Runtime,
			PlanStatus: st.Plan.Status,
			Revision:   st.Plan.Revision,
			Hash:       st.Plan.Hash,
			Approved:   st.Plan.Status == model.PlanApproved,
		}
	}
	return lc, nil
}

// workflowBlocked reports whether the workflow carries a blocking
// Finding or a Blocked Runtime.
func workflowBlocked(st model.State) bool {
	if st.Workflow.Runtime == model.RuntimeBlocked {
		return true
	}
	for _, f := range st.Findings {
		if f.Blocking {
			return true
		}
	}
	return false
}

// legalActions is the deterministic legal-action list of one workflow's
// status (the workspace renders them; the explicit commands execute
// them). The actions never mutate by themselves.
func legalActions(st StatusView) []LegalAction {
	var actions []LegalAction
	if st.LayoutVersion == 1 {
		actions = append(actions, LegalAction{Label: "Migrate layout", Hint: "layout-migration"})
	}
	switch st.Runtime {
	case model.RuntimePaused:
		if st.Stage == model.StageWorkflowGeneration {
			actions = append(actions, LegalAction{Label: "Resume", Kind: model.ResumeWorkflow})
		} else {
			actions = append(actions, LegalAction{Label: "Resume", Kind: model.ResumeWorkflow})
		}
	case model.RuntimeBlocked:
		actions = append(actions, LegalAction{Label: "Inspect", Hint: "blocked"})
	case model.RuntimeRunning, model.RuntimeSucceeded:
		// A running or succeeded workflow may be paused or cancelled.
		actions = append(actions, LegalAction{Label: "Pause", Kind: model.PauseWorkflow})
		actions = append(actions, LegalAction{Label: "Cancel", Kind: model.CancelWorkflow})
	}
	if st.Stage == model.StageRequirementDiscussion || st.Stage == model.StagePlanGeneration {
		actions = append(actions, LegalAction{Label: "Discuss", Hint: "discussion"})
	}
	return actions
}

// workspaceHealth probes the read-only health of the workspace: the git
// seam availability and every configured Adapter's detection (a read-only
// version probe only; never a model request).
func (a *Application) workspaceHealth(ctx context.Context) HealthView {
	health := HealthView{GitAvailable: a.git != nil}
	names := make([]string, 0, len(a.agent.Adapters))
	for name := range a.agent.Adapters {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		ad := a.agent.Adapters[name]
		inst, err := ad.Detect(ctx)
		if err != nil {
			health.Providers = append(health.Providers, ProviderHealth{Name: name})
			continue
		}
		health.Providers = append(health.Providers, ProviderHealth{
			Name: name, Compatible: inst.Compatibility == agent.CompatibilitySupported,
			Executable: inst.ExecutablePath, CLIVersion: inst.CLIVersion,
		})
	}
	return health
}
