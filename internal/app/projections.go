package app

// Read projection builders, workflow enumeration, and the events export
// (design 20, 21). Same-package split of the Application seam: no public
// seam added.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/observe"
	"cflow.local/cflow/internal/security"
	"cflow.local/cflow/internal/store"
)

// resolveQueryWorkflow resolves the optional workflow of a Query: an
// explicit ID wins; otherwise the project's single workflow; with none, an
// empty project-level projection; with several, the caller must name one.
func (a *Application) resolveQueryWorkflow(explicit model.WorkflowID) (model.WorkflowID, error) {
	if explicit != "" {
		return explicit, nil
	}
	ids, err := a.knownWorkflows(context.Background())
	if err != nil {
		return "", err
	}
	switch len(ids) {
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
	ids, err := a.knownWorkflows(context.Background())
	if err != nil {
		return "", err
	}
	switch len(ids) {
	case 0:
		return "", model.InvalidInputFault("no workflow")
	case 1:
		return ids[0], nil
	default:
		return "", model.InvalidInputFault("multiple workflows: a workflow id is required")
	}
}

// knownWorkflows enumerates the project's workflows from SQLite, the
// authoritative workflow state source (design §7). Artifact and worktree
// directories are external facts used by recovery, never workflow identity.
func (a *Application) knownWorkflows(ctx context.Context) ([]model.WorkflowID, error) {
	if _, err := os.Stat(a.dbPath); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	ls, err := a.lockSet()
	if err != nil {
		return nil, err
	}
	hold, err := ls.SchemaShared(ctx)
	if err != nil {
		return nil, lockFault(err)
	}
	defer hold.Release()
	st, err := store.Open(ctx, store.OpenOptions{
		Path: a.dbPath, ReadOnly: true, CflowVersion: a.cflowVer, Now: a.now,
	})
	if err != nil {
		return nil, err
	}
	defer st.Close()
	return st.ListWorkflowIDs(ctx, model.ProjectID(a.project.Key))
}

func (a *Application) workflowDir(ctx context.Context, wf model.WorkflowID) (string, error) {
	version, err := a.workflowLayout(ctx, wf)
	if err != nil {
		return "", err
	}
	switch version {
	case 1:
		return a.legacyWorkflowDir(wf), nil
	case 2:
		return a.layout.WorkflowRoot(wf), nil
	default:
		return "", model.InvariantFault(fmt.Errorf("workflow %s has unsupported layout version %d", wf, version))
	}
}

func (a *Application) legacyWorkflowDir(wf model.WorkflowID) string {
	return filepath.Join(a.workflowsDir(), string(wf))
}

func (a *Application) workflowsDir() string {
	return filepath.Join(a.home, "projects", a.project.Key, "workflows")
}

// ensureWorkflowDir creates the workflow directory chain 0700 through the
// security guard (design 19.1). Mutations only; the events export lives
// there (design 21).
func (a *Application) ensureWorkflowDir(ctx context.Context, wf model.WorkflowID) error {
	dirs := []string{
		filepath.Join(a.home, "projects"),
		filepath.Join(a.home, "projects", a.project.Key),
	}
	version, err := a.workflowLayout(ctx, wf)
	if err != nil {
		return err
	}
	if version == 2 {
		dirs = append(dirs, a.layout.WorkflowRoot(wf))
	} else if version == 1 {
		dirs = append(dirs, a.workflowsDir(), a.legacyWorkflowDir(wf))
	} else {
		return model.InvariantFault(fmt.Errorf("workflow %s has unsupported layout version %d", wf, version))
	}
	for _, dir := range dirs {
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
	if err := a.ensureWorkflowDir(ctx, wf); err != nil {
		return err
	}
	dir, err := a.workflowDir(ctx, wf)
	if err != nil {
		return err
	}
	return observe.ExportEvents(filepath.Join(dir, "events.jsonl"), events, a.redaction)
}

// workflowEvidenceDir selects the deterministic evidence root. Layout 2 is
// workflow-local; Layout 1 retains only the configured legacy global root.
func (a *Application) workflowEvidenceDir(ctx context.Context, wf model.WorkflowID) (string, error) {
	version, err := a.workflowLayout(ctx, wf)
	if err != nil {
		return "", err
	}
	switch version {
	case 1:
		return a.agent.EvidenceDir, nil
	case 2:
		return a.layout.EvidenceDir(wf), nil
	default:
		return "", model.InvariantFault(fmt.Errorf("workflow %s has unsupported layout version %d", wf, version))
	}
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
		LayoutVersion: st.Workflow.LayoutVersion,
		TargetBranch:  st.Workflow.TargetBranch, BaseCommit: st.Workflow.BaseCommit,
		IntegrationBranch:         st.Workflow.IntegrationBranch,
		IntegrationHead:           st.Workflow.IntegrationHead,
		WorkspacePath:             st.Workflow.WorkspacePath,
		WorkspaceBranch:           st.Workflow.WorkspaceBranch,
		CandidateWorkspaceHead:    st.Workflow.CandidateWorkspaceHead,
		VerifiedWorkspaceHead:     st.Workflow.VerifiedWorkspaceHead,
		WorkspaceDirtyFingerprint: st.Workflow.WorkspaceDirtyFingerprint,
		Findings:                  st.Findings,
		Processes:                 st.Processes,
	}
	if st.Plan != nil {
		v.PlanStatus = st.Plan.Status
		v.PlanApproved = st.Plan.Status == model.PlanApproved
		v.PlanRevision = st.Plan.Revision
		v.PlanHash = st.Plan.Hash
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

// queryPlan projects the active Plan Revision's review state.
func (a *Application) queryPlan(ctx context.Context, q PlanQuery) (View, error) {
	wf, err := a.resolveQueryWorkflow(q.Workflow)
	if err != nil {
		return nil, orCtx(ctx, err)
	}
	if wf == "" {
		return PlanView{}, nil
	}
	view, err := a.readAggregate(ctx, wf, store.StoreQuery{})
	if err != nil {
		return nil, orCtx(ctx, err)
	}
	if view.State.Workflow.ID == "" {
		return nil, model.InvalidInputFault("no such workflow: " + string(wf))
	}
	pv := PlanView{
		Workflow: view.State.Workflow.ID,
		Stage:    view.State.Workflow.Stage,
		Runtime:  view.State.Workflow.Runtime,
	}
	if view.State.Plan != nil {
		pv.PlanStatus = view.State.Plan.Status
		pv.Revision = view.State.Plan.Revision
		pv.Hash = view.State.Plan.Hash
		pv.Approved = view.State.Plan.Status == model.PlanApproved
	}
	return pv, nil
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
