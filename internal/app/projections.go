package app

// Read projection builders, workflow enumeration, and the events export
// (design 20, 21). Same-package split of the Application seam: no public
// seam added.

import (
	"context"
	"os"
	"path/filepath"
	"sort"

	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/observe"
	"cflow.local/cflow/internal/security"
)

// resolveQueryWorkflow resolves the optional workflow of a Query: an
// explicit ID wins; otherwise the project's single workflow; with none, an
// empty project-level projection; with several, the caller must name one.
func (a *Application) resolveQueryWorkflow(explicit model.WorkflowID) (model.WorkflowID, error) {
	if explicit != "" {
		return explicit, nil
	}
	switch ids := a.knownWorkflows(); len(ids) {
	case 0:
		return "", nil
	case 1:
		return ids[0], nil
	default:
		return "", model.InvalidInputFault("multiple workflows: a workflow id is required")
	}
}

// resolveMutationWorkflow resolves the optional workflow of a mutation: an
// explicit ID wins; otherwise the project's single workflow.
func (a *Application) resolveMutationWorkflow(explicit model.WorkflowID) (model.WorkflowID, error) {
	if explicit != "" {
		return explicit, nil
	}
	switch ids := a.knownWorkflows(); len(ids) {
	case 0:
		return "", model.InvalidInputFault("no workflow")
	case 1:
		return ids[0], nil
	default:
		return "", model.InvalidInputFault("multiple workflows: a workflow id is required")
	}
}

// knownWorkflows enumerates the project's workflows: the persisted
// workflow directories plus the Stores opened this session.
func (a *Application) knownWorkflows() []model.WorkflowID {
	set := map[model.WorkflowID]struct{}{}
	if entries, err := os.ReadDir(a.workflowsDir()); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				set[model.WorkflowID(e.Name())] = struct{}{}
			}
		}
	}
	a.mu.Lock()
	for wf := range a.known {
		set[wf] = struct{}{}
	}
	a.mu.Unlock()
	ids := make([]model.WorkflowID, 0, len(set))
	for wf := range set {
		ids = append(ids, wf)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func (a *Application) workflowDir(wf model.WorkflowID) string {
	return filepath.Join(a.home, "projects", a.project.Key, "workflows", string(wf))
}

func (a *Application) workflowsDir() string {
	return filepath.Join(a.home, "projects", a.project.Key, "workflows")
}

// ensureWorkflowDir creates the workflow directory chain 0700 through the
// security guard (design 19.1). Mutations only; the events export lives
// there (design 21).
func (a *Application) ensureWorkflowDir(wf model.WorkflowID) error {
	for _, dir := range []string{
		filepath.Join(a.home, "projects"),
		filepath.Join(a.home, "projects", a.project.Key),
		a.workflowsDir(),
		a.workflowDir(wf),
	} {
		if _, err := os.Stat(dir); err == nil {
			continue
		}
		if err := security.CreateSensitiveDir(dir); err != nil {
			return err
		}
	}
	return nil
}

// exportEvents appends one committed Event window to the workflow's
// events.jsonl audit export (design 21).
func (a *Application) exportEvents(ctx context.Context, wf model.WorkflowID, events []model.Event) error {
	if err := a.ensureWorkflowDir(wf); err != nil {
		return err
	}
	return observe.ExportEvents(filepath.Join(a.workflowDir(wf), "events.jsonl"), events, a.redaction)
}

func workflowSummary(st model.State) WorkflowSummary {
	return WorkflowSummary{
		ID: st.Workflow.ID, Stage: st.Workflow.Stage, Runtime: st.Workflow.Runtime,
		TargetBranch: st.Workflow.TargetBranch, BaseCommit: st.Workflow.BaseCommit,
	}
}

func statusView(st model.State) StatusView {
	v := StatusView{
		Workflow: st.Workflow.ID, Stage: st.Workflow.Stage, Runtime: st.Workflow.Runtime,
		TargetBranch: st.Workflow.TargetBranch, BaseCommit: st.Workflow.BaseCommit,
		IntegrationBranch: st.Workflow.IntegrationBranch,
		IntegrationHead:   st.Workflow.IntegrationHead,
		Findings:          st.Findings,
		Processes:         st.Processes,
	}
	for i := range st.Runs {
		if !st.Runs[i].Status.IsTerminal() {
			r := st.Runs[i]
			v.Run = &r
			break
		}
	}
	return v
}

func inspectView(st model.State, pending []string) InspectView {
	return InspectView{
		Status: statusView(st), Plan: st.Plan,
		Nodes:           nodeSlice(st.Nodes),
		Attempts:        attemptSlice(st.Attempts),
		Approvals:       st.Approvals,
		Sessions:        st.Sessions,
		Runs:            st.Runs,
		Quarantines:     st.Quarantines,
		ApplyAttempts:   st.ApplyAttempts,
		CleanupAttempts: st.CleanupAttempts,
		PendingEffects:  pending,
	}
}

func nodeSlice(nodes map[model.NodeID]*model.Node) []model.Node {
	ids := make([]model.NodeID, 0, len(nodes))
	for id := range nodes {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	out := make([]model.Node, 0, len(nodes))
	for _, id := range ids {
		out = append(out, *nodes[id])
	}
	return out
}

func attemptSlice(attempts map[model.AttemptKey]*model.Attempt) []model.Attempt {
	keys := make([]model.AttemptKey, 0, len(attempts))
	for k := range attempts {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Node != keys[j].Node {
			return keys[i].Node < keys[j].Node
		}
		return keys[i].Number < keys[j].Number
	})
	out := make([]model.Attempt, 0, len(attempts))
	for _, k := range keys {
		out = append(out, *attempts[k])
	}
	return out
}
