package app

// The execution dispatch machinery (Task 12, design 12): the approved
// execution plan (nodes and edges of the compiled Dynamic Workflow plus
// the approved Spec facts), the graph install, the pure Scheduler pass,
// the pairwise static compatibility judgment (PRD 并行安全判断), and the
// serialized per-Node allocation that commits each RUNNING Attempt before
// submitting its Effects. Same-package split of the Application seam: no
// public seam added.

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	yaml "go.yaml.in/yaml/v3"

	"cflow.local/cflow/internal/agent"
	"cflow.local/cflow/internal/artifact"
	"cflow.local/cflow/internal/compile"
	"cflow.local/cflow/internal/decision"
	"cflow.local/cflow/internal/gitflow"
	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/platform"
	"cflow.local/cflow/internal/schedule"
	"cflow.local/cflow/internal/store"
)

// nodeDispatchBudget bounds one Node's effect chain: the allocation
// Decision, the Task Worktree result, the coding Session result, and the
// settle Decision.
const nodeDispatchBudget = 4

// dispatchNode is one installed Node of the approved execution plus the
// approved Task facts the dispatch needs (route, scopes, resource locks).
type dispatchNode struct {
	id         model.NodeID
	kind       model.NodeKind
	specID     string
	deps       []model.NodeID
	retry      int
	timeout    int
	route      string
	locks      []string
	writeScope []string
	readScope  []string
}

// dispatchPlan is the app's view of the approved execution: every node of
// the compiled Dynamic Workflow with its skeleton edges, plus the Spec
// facts, plus the approval references the binding check compares.
type dispatchPlan struct {
	nodes        []dispatchNode
	totalRuns    int
	workflowHash string
	specHash     string
}

// node returns one plan node (nil when unknown).
func (p *dispatchPlan) node(id model.NodeID) *dispatchNode {
	for i := range p.nodes {
		if p.nodes[i].id == id {
			return &p.nodes[i]
		}
	}
	return nil
}

// runBudget is the approved total-run budget: the initial run plus the
// budgeted retries of every agent-task Node (design 12: the total-run
// budget must permit every allocation).
func (p *dispatchPlan) runBudget() int {
	total := 0
	for _, n := range p.nodes {
		if n.kind == model.NodeAgentTask {
			total += 1 + n.retry
		}
	}
	return total
}

// installNodes maps the plan onto the kernel's graph install input.
func (p *dispatchPlan) installNodes() []model.InstallNode {
	out := make([]model.InstallNode, 0, len(p.nodes))
	for _, n := range p.nodes {
		out = append(out, model.InstallNode{
			ID:           n.id,
			Kind:         n.kind,
			Dependencies: append([]model.NodeID(nil), n.deps...),
			RetryBudget:  n.retry,
		})
	}
	return out
}

// executeDispatch is the DispatchCommand's pass: the plan is derived from
// the approved execution artifacts, the current artifact references must
// match the approval facts (design 12: readiness binds the current
// Artifact, route, command, and commit-policy facts), and then one
// dispatch pass runs.
func (a *Application) executeDispatch(ctx context.Context, st *store.Store, wf model.WorkflowID, restricted bool) (Outcome, error) {
	plan, err := a.executionPlan(ctx, wf)
	if err != nil {
		return Outcome{}, err
	}
	view, err := st.View(ctx, store.StoreQuery{})
	if err != nil {
		return Outcome{}, err
	}
	facts := view.State.Workflow.ExecutionFacts
	if facts == nil || facts.WorkflowHash == "" || facts.WorkflowHash != plan.workflowHash ||
		len(facts.SpecHashes) == 0 || facts.SpecHashes[0] != plan.specHash {
		return Outcome{}, model.NewFault(model.CodeApprovalInputChanged,
			"the execution artifacts no longer match the approval facts; re-approve before dispatch")
	}
	return a.dispatchPass(ctx, st, wf, plan, restricted)
}

// executionPlan derives the dispatch plan from the approved execution
// artifacts: the compiled Dynamic Workflow (nodes and edges) and the
// approved Spec set (routes, scopes, resource locks, budgets).
func (a *Application) executionPlan(ctx context.Context, wf model.WorkflowID) (*dispatchPlan, error) {
	store, err := a.artifactStore(wf)
	if err != nil {
		return nil, err
	}
	resolve := func(typ model.ArtifactType) (model.ArtifactRef, []byte, error) {
		ref, err := store.Resolve(ctx, artifact.ResolveRequest{WorkflowID: wf, Type: typ})
		if err != nil {
			return model.ArtifactRef{}, nil, err
		}
		body, err := store.Get(ctx, ref)
		if err != nil {
			return model.ArtifactRef{}, nil, err
		}
		return ref, body, nil
	}
	workflowRef, workflowBody, err := resolve(model.ArtifactWorkflow)
	if err != nil {
		return nil, err
	}
	specRef, specBody, err := resolve(model.ArtifactSpec)
	if err != nil {
		return nil, err
	}
	wfIR, err := compile.ParseWorkflow(workflowBody)
	if err != nil {
		return nil, err
	}
	specs, err := a.parseSpecSet(specBody)
	if err != nil {
		return nil, err
	}
	specByID := map[string]compile.Spec{}
	for _, s := range specs {
		specByID[s.ID] = s
	}
	incoming := map[string][]string{}
	for _, e := range wfIR.Edges {
		incoming[e.To] = append(incoming[e.To], e.From)
	}
	plan := &dispatchPlan{workflowHash: workflowRef.Hash, specHash: specRef.Hash}
	for _, n := range wfIR.Nodes {
		dn := dispatchNode{
			id:    model.NodeID(n.ID),
			deps:  idsOf(incoming[n.ID]),
			retry: n.MaxRetry,
		}
		switch n.Type {
		case "agent_task":
			dn.kind = model.NodeAgentTask
			dn.specID = n.SpecID
			dn.timeout = n.TimeoutSeconds
			if s, ok := specByID[n.SpecID]; ok {
				if s.Route != nil {
					dn.route = s.Route.Provider
				}
				dn.locks = append([]string(nil), s.Locks...)
				dn.writeScope = append([]string(nil), s.WriteScope...)
				dn.readScope = append([]string(nil), s.ReadScope...)
			}
			plan.totalRuns += 1 + n.MaxRetry
		case "verify":
			dn.kind = model.NodeVerify
		case "merge":
			dn.kind = model.NodeMerge
		case "checkpoint":
			dn.kind = model.NodeCheckpoint
		case "final_verify":
			dn.kind = model.NodeFinalVerify
		default:
			return nil, model.InvariantFault(fmt.Errorf("compiled workflow carries unknown node type %q", n.Type))
		}
		plan.nodes = append(plan.nodes, dn)
	}
	return plan, nil
}

// idsOf converts a node-id slice to the model identity slice.
func idsOf(ids []string) []model.NodeID {
	out := make([]model.NodeID, 0, len(ids))
	for _, id := range ids {
		out = append(out, model.NodeID(id))
	}
	return out
}

// splitSpecSetBody splits one Spec Artifact body (a spec object or a
// spec-set sequence, both schema-validated on Put) into the per-Spec
// canonical bodies the Compiler consumes (the multi-Spec pipeline, Task
// 12).
func splitSpecSetBody(body []byte) ([][]byte, error) {
	var raw []map[string]any
	if err := yaml.Unmarshal(body, &raw); err != nil {
		return nil, model.InvalidInputFault("spec artifact body is not a spec set")
	}
	if len(raw) == 0 {
		return nil, model.InvalidInputFault("spec artifact body is empty")
	}
	out := make([][]byte, 0, len(raw))
	for _, m := range raw {
		if m == nil {
			return nil, model.InvalidInputFault("spec artifact body carries an empty spec")
		}
		b, err := yaml.Marshal(m)
		if err != nil {
			return nil, model.InvariantFault(fmt.Errorf("spec body cannot be serialized"))
		}
		out = append(out, b)
	}
	return out, nil
}

// parseSpecSet parses and validates every Spec of the spec-set body.
func (a *Application) parseSpecSet(body []byte) ([]compile.Spec, error) {
	bodies, err := splitSpecSetBody(body)
	if err != nil {
		return nil, err
	}
	specs := make([]compile.Spec, 0, len(bodies))
	for _, b := range bodies {
		s, err := compile.ParseSpec(b)
		if err != nil {
			return nil, err
		}
		specs = append(specs, s)
	}
	return specs, nil
}

// dispatchPass is one allocation pass (design 12): it installs the
// execution graph when the aggregate has none, computes the eligible set
// with the pure Scheduler, applies the pairwise static compatibility
// judgment, and allocates every selected Node — committing each RUNNING
// Attempt before submitting its Effects. Pause, Cancel, Quiesce, and
// Safety Stop share the committed Dispatch Gate with allocation, so no
// start can cross a committed closure (PRD 已确认：并行失败后的 Quiescing).
func (a *Application) dispatchPass(ctx context.Context, st *store.Store, wf model.WorkflowID, plan *dispatchPlan, restricted bool) (Outcome, error) {
	view, err := st.View(ctx, store.StoreQuery{})
	if err != nil {
		return Outcome{}, err
	}
	if len(view.State.Nodes) == 0 {
		if _, err := a.runDecisionLoop(ctx, st, wf, DispatchCommand{Workflow: wf},
			model.GraphInstallInput{Nodes: plan.installNodes()}, restricted); err != nil {
			return Outcome{}, err
		}
		view, err = st.View(ctx, store.StoreQuery{})
		if err != nil {
			return Outcome{}, err
		}
	}
	state := view.State
	rt, err := a.agentRuntime(ctx, state)
	if err != nil {
		return Outcome{}, err
	}
	if rt != nil {
		defer rt.Close()
	}

	// The verified Integration HEAD is observed at readiness and fixed as
	// the immutable Task Base of every Task this pass allocates (PRD
	// Worktree 策略: Task Base = current verified Integration HEAD).
	baseHead, err := a.observedIntegrationHead(ctx, wf)
	if err != nil {
		return Outcome{}, err
	}

	snapshot := a.graphSnapshot(state, plan)
	policy := model.DispatchPolicy{MaxConcurrency: a.concurrencyCap()}
	sched := (schedule.Scheduler{}).Next(snapshot, policy)
	selected := a.selectBatch(state, plan, sched.Eligible)

	for _, id := range selected {
		node := plan.node(id)
		holds, err := a.acquireResourceLocks(ctx, node.locks)
		if err != nil {
			return Outcome{}, err
		}
		err = a.runNodeDispatch(ctx, st, wf, id, node.route, baseHead, rt)
		holds.Release()
		if err != nil {
			return Outcome{}, err
		}
	}
	return Outcome{Workflow: wf, Stage: state.Workflow.Stage, Runtime: state.Workflow.Runtime,
		Findings: state.Findings}, nil
}

// graphSnapshot builds the pure Scheduler input from the persisted Node
// state plus the approved skeleton edges and the Run Dispatch Gate.
func (a *Application) graphSnapshot(state model.State, plan *dispatchPlan) model.GraphSnapshot {
	// The Dispatch Gate belongs to the active Run (the first non-terminal
	// Run, the same rule the Kernel's allocation uses): the Scheduler
	// reads exactly the gate the allocation decisions revalidate, so no
	// start can cross a committed closure.
	gate := false
	for i := range state.Runs {
		if state.Runs[i].Status.IsTerminal() {
			continue
		}
		gate = state.Runs[i].Status == model.RunRunning && state.Runs[i].DispatchGate
		break
	}
	nodes := make([]model.GraphNode, 0, len(state.Nodes))
	for id, n := range state.Nodes {
		var deps []model.NodeID
		if pn := plan.node(id); pn != nil {
			deps = pn.deps
		}
		nodes = append(nodes, model.GraphNode{
			ID:           id,
			Kind:         n.Kind,
			Status:       n.Status,
			Dependencies: deps,
			Branch:       n.Branch,
		})
	}
	return model.GraphSnapshot{Nodes: nodes, DispatchGateOpen: gate}
}

// selectBatch applies the static parallel-safety judgment (PRD 并行安全判
// 断) plus the app-level readiness facts: two Tasks may only dispatch in
// the same pass when no mutual dependency exists (the DAG guarantees it),
// their write scopes are disjoint, and their resource locks are disjoint.
// The Agent can only downgrade parallel to serial, never override a
// static conflict; a conflicting Task is deferred to a later pass. The
// judgment extends to the RUNNING aggregate: a RUNNING agent-task Node
// holds its declared resource locks and write scopes for its Attempt's
// RUNNING lifetime, so a Task deferred in an earlier pass cannot dispatch
// into them in a later pass — two RUNNING Attempts with overlapping locks
// or scopes never coexist in the state model (review fix #1; the state
// Task 13's gates consume must never show the violation). An open
// blocking Finding in the Node's scope, the total-run budget, and the
// unsupported kinds (verify/merge/checkpoint/final-verify dispatch
// arrives with Task 13) also defer.
func (a *Application) selectBatch(state model.State, plan *dispatchPlan, eligible []model.NodeID) []model.NodeID {
	var selected []model.NodeID
	for _, id := range eligible {
		node := plan.node(id)
		if node == nil || node.kind != model.NodeAgentTask {
			continue
		}
		if blockingFinding(state, node.id) {
			continue
		}
		if len(state.Attempts) >= plan.runBudget() {
			continue
		}
		conflict := false
		for _, other := range selected {
			if tasksConflict(plan.node(other), node) {
				conflict = true
				break
			}
		}
		if !conflict {
			for nid, n := range state.Nodes {
				if n.Status != model.NodeRunning || n.Kind != model.NodeAgentTask {
					continue
				}
				if rn := plan.node(nid); rn != nil && tasksConflict(rn, node) {
					conflict = true
					break
				}
			}
		}
		if conflict {
			continue
		}
		selected = append(selected, id)
	}
	return selected
}

// blockingFinding reports whether an open blocking Finding covers the
// Node's scope (workflow- or run-wide, or the Node itself).
func blockingFinding(state model.State, node model.NodeID) bool {
	for _, f := range state.Findings {
		if !f.Blocking {
			continue
		}
		switch f.Scope {
		case model.ScopeWorkflow, model.ScopeRun:
			return true
		case model.ScopeNode, model.ScopeAttempt:
			if f.Subject == string(node) {
				return true
			}
		}
	}
	return false
}

// tasksConflict is the static judgment: a shared resource lock or
// overlapping write scopes make two Tasks incompatible in one pass (PRD
// 并行安全判断).
func tasksConflict(x, y *dispatchNode) bool {
	for _, lx := range x.locks {
		for _, ly := range y.locks {
			if lx == ly {
				return true
			}
		}
	}
	return scopesOverlap(x.writeScope, y.writeScope)
}

// scopesOverlap reports whether any write-scope entry of a overlaps any
// entry of b (directory-prefix containment after normalizing trailing
// glob markers; the same semantics the Compiler's validation uses).
func scopesOverlap(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			x = normalizeScope(x)
			y = normalizeScope(y)
			if x == y || strings.HasPrefix(x, y+"/") || strings.HasPrefix(y, x+"/") {
				return true
			}
		}
	}
	return false
}

func normalizeScope(entry string) string {
	return strings.TrimSuffix(strings.TrimSuffix(entry, "/"), "/**")
}

// acquireResourceLocks takes the sorted Resource Locks of one Task (design
// 18.1: Resource Locks are acquired in lexicographic order; one Hold
// releases the batch). A Task without declared locks holds nothing.
func (a *Application) acquireResourceLocks(ctx context.Context, names []string) (*platform.Hold, error) {
	if len(names) == 0 {
		return nil, nil
	}
	ls, err := a.lockSet()
	if err != nil {
		return nil, err
	}
	hold, err := ls.Resource(ctx, names...)
	if err != nil {
		return nil, lockFault(err)
	}
	return hold, nil
}

// runNodeDispatch allocates one Node (design 12): the RUNNING Attempt
// commits first — a Node is considered running only after its Attempt row
// commits, and an in-memory queued goroutine is not an in-flight Attempt
// and must be discarded if the gate closes — then the Task Worktree
// creation runs, then the coding Session starts inside it, then the
// Session settles. Every Decision revalidates the committed Dispatch
// Gate.
func (a *Application) runNodeDispatch(ctx context.Context, st *store.Store, wf model.WorkflowID, node model.NodeID, route, baseHead string, rt *agent.Runtime) error {
	view, err := st.View(ctx, store.StoreQuery{})
	if err != nil {
		return err
	}
	version := view.AggregateVersion
	input := model.Input(model.DispatchInput{
		Node:     node,
		Session:  model.SessionID(a.ids(model.IDSession)),
		Route:    route,
		BaseHead: baseHead,
	})
	executed := map[string]struct{}{}
	for iter := 0; iter < nodeDispatchBudget; iter++ {
		cd, err := st.Transact(ctx, version, func(state model.State) (model.Decision, error) {
			return decision.Decide(state, input)
		})
		if err != nil {
			return err
		}
		version = cd.Version
		if iter == 0 {
			// The RUNNING Attempt committed: only now is the Node running.
			a.probeStep("attempt:" + string(node) + ":commit")
		}
		if cd.Decision.Effect == nil {
			return nil
		}
		id := intentIdentity(cd.Decision.Effect)
		if _, dup := executed[id]; dup {
			return model.InvariantFault(fmt.Errorf("repeated identical uncompleted effect intent %s", id))
		}
		executed[id] = struct{}{}
		result, err := a.executeEffect(ctx, cd.Decision.Effect, false, wf, DispatchCommand{Workflow: wf}, input, rt)
		if err != nil {
			return err
		}
		if err := validateEffectResult(cd.Decision.Effect, result); err != nil {
			return err
		}
		input = model.EffectResultInput(result)
	}
	return model.InvariantFault(fmt.Errorf("node dispatch exceeded the effect bound"))
}

// taskWorktreePath is the deterministic Task Worktree location of one
// Node (PRD 全局目录结构: worktrees/<project-key>/<workflow-id>/tasks/<node>).
func (a *Application) taskWorktreePath(wf model.WorkflowID, node model.NodeID) string {
	return filepath.Join(a.home, "worktrees", a.project.Key, string(wf), "tasks", string(node))
}

// observedIntegrationHead is the current verified Integration HEAD: the
// Runtime observes the Integration Worktree's HEAD through the GitFlow
// seam at readiness (the workflows row persists the Integration Branch;
// the HEAD is a git fact the Runtime re-observes, PRD Worktree 策略).
func (a *Application) observedIntegrationHead(ctx context.Context, wf model.WorkflowID) (string, error) {
	if a.git == nil {
		return "", model.InvariantFault(fmt.Errorf("git seam is not configured for this application"))
	}
	path := filepath.Join(a.home, "worktrees", a.project.Key, string(wf), "integration")
	facts, err := a.git.Observe(ctx, gitflow.GitStatus{Dir: path})
	if err != nil {
		return "", err
	}
	st, ok := facts.(gitflow.StatusFacts)
	if !ok {
		return "", model.InvariantFault(fmt.Errorf("git status observation has an unexpected type"))
	}
	if st.Head == "" {
		return "", model.InvariantFault(fmt.Errorf("the integration worktree has no verified head"))
	}
	return st.Head, nil
}

// codingSessionInput builds the coding Session's input block from only
// the approved facts: the Spec set, the Verification Catalog, and the
// Task Worktree location (PRD Worktree 策略; the implementation prompt's
// input contract).
func (a *Application) codingSessionInput(ctx context.Context, wf model.WorkflowID, node model.NodeID) any {
	store, err := a.artifactStore(wf)
	if err != nil {
		return nil
	}
	return struct {
		Spec     string `json:"spec"`
		Catalog  string `json:"catalog"`
		Worktree string `json:"worktree"`
	}{
		Spec:     string(readArtifact(ctx, store, wf, model.ArtifactSpec)),
		Catalog:  string(readArtifact(ctx, store, wf, model.ArtifactCatalog)),
		Worktree: a.taskWorktreePath(wf, node),
	}
}
