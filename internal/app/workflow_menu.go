package app

// The authoritative Workflow Menu projection (2026-08-12 navigation
// design §3.2 and §8): Application translates aggregate, Artifact, and
// LegalActions facts into bounded typed entries. The TUI renders and routes
// these entries; it never infers actions from Stage or Runtime strings.

import (
	"context"

	"cflow.local/cflow/internal/artifact"
	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/store"
)

// queryWorkflowMenu projects the menu for the exact selected Workflow.
func (a *Application) queryWorkflowMenu(ctx context.Context, q WorkflowMenuQuery) (View, error) {
	if q.Workflow == "" {
		return nil, model.InvalidInputFault("workflow menu requires a workflow")
	}
	agg, err := a.readAggregate(ctx, q.Workflow, store.StoreQuery{})
	if err != nil {
		return nil, orCtx(ctx, err)
	}
	if agg.State.Workflow.ID == "" {
		return nil, model.InvalidInputFault("no such workflow: " + string(q.Workflow))
	}

	st := agg.State
	st.Workflow.Name = a.workflowDisplayName(st)
	status := statusView(st)

	continueEntries, controlEntries, err := a.workflowMenuActions(ctx, q.Workflow, legalActions(status))
	if err != nil {
		return nil, orCtx(ctx, err)
	}
	viewEntries, err := a.workflowMenuReadonlyEntries(ctx, q.Workflow, agg)
	if err != nil {
		return nil, orCtx(ctx, err)
	}

	entries := make([]WorkflowMenuEntry, 0, len(continueEntries)+len(viewEntries)+len(controlEntries))
	entries = append(entries, continueEntries...)
	entries = append(entries, viewEntries...)
	entries = append(entries, controlEntries...)

	menu := WorkflowMenuView{
		Workflow: q.Workflow,
		Name:     status.Name,
		Stage:    status.Stage,
		Runtime:  status.Runtime,
		Entries:  entries,
	}
	menu.DefaultIndex = workflowMenuDefault(entries, status.Runtime)
	return menu, nil
}

// workflowMenuReadonlyEntries emits only routes backed by current facts.
// Current Stage is always the first read-only entry. The Event Log exists
// when the authoritative aggregate carries at least one event.
func (a *Application) workflowMenuReadonlyEntries(ctx context.Context, wf model.WorkflowID, agg store.StoreView) ([]WorkflowMenuEntry, error) {
	entries := []WorkflowMenuEntry{{
		ID: "current-stage", Group: MenuGroupView, Kind: MenuEntryReadonly,
		Label: "Current Stage", Route: MenuRouteCurrentStage,
	}}
	st := agg.State
	if st.Plan != nil && st.Plan.Revision > 0 && st.Plan.Hash != "" {
		entries = append(entries, WorkflowMenuEntry{
			ID: "plan", Group: MenuGroupView, Kind: MenuEntryReadonly,
			Label: "Plan / Evidence", Route: MenuRoutePlan,
		})
	}
	if facts := st.Workflow.ExecutionFacts; facts != nil {
		// Specs, Catalog, and DAG all reuse the existing bounded
		// ExecutionPreviewQuery in the TUI. Expose them only when that query's
		// complete authority precondition is present; partial hash facts do not
		// justify inventing an artifact view.
		previewFacts := facts.PlanHash != "" && len(facts.SpecHashes) > 0 &&
			facts.CatalogHash != "" && facts.WorkflowHash != ""
		if previewFacts {
			entries = append(entries, WorkflowMenuEntry{
				ID: "specs", Group: MenuGroupView, Kind: MenuEntryReadonly,
				Label: "Specs", Route: MenuRouteSpecs,
			})
			entries = append(entries, WorkflowMenuEntry{
				ID: "catalog", Group: MenuGroupView, Kind: MenuEntryReadonly,
				Label: "Verification Catalog", Route: MenuRouteCatalog,
			})
			entries = append(entries, WorkflowMenuEntry{
				ID: "workflow-dag", Group: MenuGroupView, Kind: MenuEntryReadonly,
				Label: "Workflow DAG", Route: MenuRouteDAG,
			})
			entries = append(entries, WorkflowMenuEntry{
				ID: "execution-preview", Group: MenuGroupView, Kind: MenuEntryReadonly,
				Label: "Execution Preview", Route: MenuRouteExecutionApproval,
			})
		}
	}
	if len(st.Nodes) > 0 {
		entries = append(entries, WorkflowMenuEntry{
			ID: "task-graph", Group: MenuGroupView, Kind: MenuEntryReadonly,
			Label: "Task Graph", Route: MenuRouteTaskGraph,
		})
	}
	if len(agg.Events) > 0 || agg.NextEventSeq > 1 {
		entries = append(entries, WorkflowMenuEntry{
			ID: "event-log", Group: MenuGroupView, Kind: MenuEntryReadonly,
			Label: "Event Log", Route: MenuRouteLogs,
		})
	}
	report, err := a.workflowMenuHasFinalReport(ctx, wf, st)
	if err != nil {
		return nil, err
	}
	if report {
		entries = append(entries, WorkflowMenuEntry{
			ID: "final-report", Group: MenuGroupView, Kind: MenuEntryReadonly,
			Label: "Final Report", Route: MenuRouteReport,
		})
	}
	return entries, nil
}

func (a *Application) workflowMenuHasFinalReport(ctx context.Context, wf model.WorkflowID, st model.State) (bool, error) {
	if st.Workflow.Stage != model.StageCompleted || st.Workflow.Runtime != model.RuntimeSucceeded {
		return false, nil
	}
	artifacts, err := a.artifactStore(wf)
	if err != nil {
		return false, err
	}
	ref, err := artifacts.Resolve(ctx, artifact.ResolveRequest{WorkflowID: wf, Type: model.ArtifactReport})
	if err != nil {
		if artifact.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	if ref.Revision < 1 || ref.Hash == "" {
		return false, nil
	}
	// Commit preflight evidence and the completion report share
	// ArtifactReport revisions. A later Apply preflight may therefore be the
	// latest revision even though an earlier immutable completion report
	// still exists. Enumerate verified revisions and exclude every preflight
	// revision represented by the authoritative aggregate.
	preflights := workflowMenuPreflightRevisions(st)
	for revision := ref.Revision; revision >= 1; revision-- {
		if _, isPreflight := preflights[revision]; isPreflight {
			continue
		}
		candidate, err := artifacts.Resolve(ctx, artifact.ResolveRequest{
			WorkflowID: wf, Type: model.ArtifactReport, Revision: revision,
		})
		if err != nil {
			if artifact.IsNotFound(err) {
				continue
			}
			return false, err
		}
		if candidate.Hash != "" {
			return true, nil
		}
	}
	return false, nil
}

func workflowMenuPreflightRevisions(st model.State) map[int]struct{} {
	revisions := make(map[int]struct{})
	if facts := st.Workflow.ExecutionFacts; facts != nil && facts.PreflightRevision > 0 {
		revisions[facts.PreflightRevision] = struct{}{}
	}
	for _, approval := range st.Approvals {
		if approval.PreflightRevision > 0 {
			revisions[approval.PreflightRevision] = struct{}{}
		}
	}
	for _, attempt := range st.ApplyAttempts {
		if attempt.Preflight.Revision > 0 {
			revisions[attempt.Preflight.Revision] = struct{}{}
		}
	}
	return revisions
}

// workflowMenuActions translates only entries already admitted by
// legalActions. Discussion Start versus Continue comes from the existing
// DiscussionReturn projection, including its resumable-session facts.
func (a *Application) workflowMenuActions(ctx context.Context, wf model.WorkflowID, actions []LegalAction) ([]WorkflowMenuEntry, []WorkflowMenuEntry, error) {
	var continueEntries, controlEntries []WorkflowMenuEntry
	for _, legal := range actions {
		entry, ok, err := a.workflowMenuAction(ctx, wf, legal)
		if err != nil {
			return nil, nil, err
		}
		if !ok {
			continue
		}
		switch entry.Group {
		case MenuGroupContinue:
			continueEntries = append(continueEntries, entry)
		case MenuGroupControl:
			controlEntries = append(controlEntries, entry)
		}
	}
	return continueEntries, controlEntries, nil
}

func (a *Application) workflowMenuAction(ctx context.Context, wf model.WorkflowID, legal LegalAction) (WorkflowMenuEntry, bool, error) {
	if legal.Hint == "discussion" {
		view, err := a.queryDiscussionReturn(ctx, DiscussionReturnQuery{Workflow: wf})
		if err != nil {
			return WorkflowMenuEntry{}, false, err
		}
		facts := view.(DiscussionReturnView)
		for _, action := range facts.Actions {
			if action == "continue" {
				return WorkflowMenuEntry{
					ID: "continue-discussion", Group: MenuGroupContinue, Kind: MenuEntryAction,
					Label: "Continue Native Discussion", Route: MenuRouteDiscussion,
					Action: MenuActionContinueDiscussion,
				}, true, nil
			}
		}
		return WorkflowMenuEntry{
			ID: "start-discussion", Group: MenuGroupContinue, Kind: MenuEntryAction,
			Label: "Start Native Discussion", Route: MenuRouteDiscussion,
			Action: MenuActionStartDiscussion,
		}, true, nil
	}
	if legal.Hint == "blocked" {
		return WorkflowMenuEntry{
			ID: "inspect-blocked", Group: MenuGroupContinue, Kind: MenuEntryAction,
			Label: "Inspect Blocked Workflow", Route: MenuRouteCurrentStage,
			Action: MenuActionInspectBlocked,
		}, true, nil
	}
	if legal.Hint == "layout-migration" {
		return WorkflowMenuEntry{
			ID: "migrate-layout", Group: MenuGroupControl, Kind: MenuEntryAction,
			Label: "Migrate Layout", Route: MenuRouteMigration, Action: MenuActionMigrate,
		}, true, nil
	}

	switch legal.Kind {
	case model.StartWorkflow:
		return WorkflowMenuEntry{
			ID: "start-runner", Group: MenuGroupContinue, Kind: MenuEntryAction,
			Label: "Start Runner", Route: MenuRouteExecution, Action: MenuActionStartRunner,
		}, true, nil
	case model.ResumeWorkflow:
		return WorkflowMenuEntry{
			ID: "resume", Group: MenuGroupContinue, Kind: MenuEntryAction,
			Label: "Resume", Route: MenuRouteCurrentStage, Action: MenuActionResume,
		}, true, nil
	case model.PauseWorkflow:
		return WorkflowMenuEntry{
			ID: "pause", Group: MenuGroupControl, Kind: MenuEntryAction,
			Label: "Pause Workflow", Route: MenuRouteCurrentStage, Action: MenuActionPause,
		}, true, nil
	case model.CancelWorkflow:
		return WorkflowMenuEntry{
			ID: "cancel", Group: MenuGroupControl, Kind: MenuEntryAction,
			Label: "Cancel Workflow", Route: MenuRouteCancel, Action: MenuActionCancel,
		}, true, nil
	}
	return WorkflowMenuEntry{}, false, nil
}

func workflowMenuDefault(entries []WorkflowMenuEntry, runtime model.RuntimeStatus) int {
	if runtime == model.RuntimeBlocked {
		for i, entry := range entries {
			if entry.Kind == MenuEntryAction && entry.Action == MenuActionInspectBlocked {
				return i
			}
		}
	}
	for i, entry := range entries {
		if entry.Kind == MenuEntryAction {
			return i
		}
	}
	for i, entry := range entries {
		if entry.ID == "current-stage" {
			return i
		}
	}
	return 0
}
