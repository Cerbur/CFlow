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
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"

	yaml "go.yaml.in/yaml/v3"

	"cflow.local/cflow/internal/agent"
	"cflow.local/cflow/internal/artifact"
	"cflow.local/cflow/internal/compile"
	"cflow.local/cflow/internal/gitflow"
	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/platform"
	"cflow.local/cflow/internal/schedule"
	"cflow.local/cflow/internal/store"
)

// nodeDispatchBudget bounds one Node's effect chain. An agent-task chain
// runs: the allocation, the Worktree creation, the coding Session, the
// Session settle, the Task gate result, and the settle Decision. A verify
// chain runs: the allocation, the Verification run, the Reviewer Session,
// and the settle. A merge chain runs: the allocation, the merge, and (on
// conflict) the rollback and the settle.
const nodeDispatchBudget = 8

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
// budgeted retries of every agent-task Node, plus one Attempt per verify,
// merge, and final-verify Node (design 12: the total-run budget must
// permit every allocation; Task 18 adds the Final Verify Node).
func (p *dispatchPlan) runBudget() int {
	total := 0
	for _, n := range p.nodes {
		switch n.kind {
		case model.NodeAgentTask:
			total += 1 + n.retry
		case model.NodeVerify, model.NodeMerge, model.NodeFinalVerify:
			total++
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
// Artifact, route, command, and commit-policy facts), the immutable
// routing and budget inputs must still match the approval (design 20.1),
// and then one dispatch pass runs. A protocol-drift fault of the pass
// closes the Dispatch Gate and pauses the Workflow (PRD 失败分类: a
// regenerated Dry Run and Execution Approval are required).
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
	// The Workspace Adoption Gate (TUI task 6, design 8.4): an Execution
	// Approval bound to a frozen Change Set may not schedule normal Tasks
	// until the Workspace was adopted — no Task may be created from an
	// unadopted candidate Head (verified_workspace_head is the only legal
	// Task base).
	if facts.ChangeSetHash != "" && view.State.Workflow.VerifiedWorkspaceHead == "" {
		return Outcome{}, model.NewFault(model.CodeWorkspaceAdoptionRequired,
			"the workspace has not been adopted; run AdoptWorkspaceCommand before dispatch")
	}
	approved, err := a.verifyApprovedRouting(ctx, wf, facts)
	if err != nil {
		return Outcome{}, err
	}
	out, err := a.dispatchPass(ctx, st, wf, plan, restricted, approved)
	if err != nil {
		if code, ok := model.CodeOf(err); ok {
			if code == model.CodeProviderBindingChanged || code == model.CodeProviderProtocolUnsupported {
				// Protocol drift closes the Dispatch Gate: no later
				// dispatch may cross the failed pre-pass until a
				// regenerated Dry Run and Execution Approval. The pause
				// is best-effort; the drift fault itself is the command
				// error.
				_ = a.pauseDispatch(ctx, st, wf, restricted)
			}
		}
		return Outcome{}, err
	}
	return out, nil
}

// verifyApprovedRouting re-resolves the immutable routing and budget
// inputs without any detection and compares them to the approval facts
// (design 20.1, PRD fail-closed #3-4): an edited configuration, Spec
// route, prompt, or registry revision since the Execution Approval is
// APPROVAL_INPUT_CHANGED and requires a successor Dry Run and Execution
// Approval. This gate runs before the CAS pre-pass, so a configuration
// drift pauses before any executable probe. It returns the parsed
// approved policy the pass attaches to its Runtime.
func (a *Application) verifyApprovedRouting(ctx context.Context, wf model.WorkflowID, facts *model.ExecutionFacts) (*agent.RoutingPolicySet, error) {
	if facts == nil || facts.RoutingHash == "" || facts.BudgetHash == "" {
		return nil, model.NewFault(model.CodeApprovalInputChanged,
			"the execution approval did not bind routing and budget inputs; re-approve before dispatch")
	}
	approved, err := a.approvedRoutingPolicy(ctx, wf)
	if err != nil {
		return nil, err
	}
	fresh, err := a.resolveRoutingInputs(ctx, wf, false)
	if err != nil {
		return nil, err
	}
	if !agent.ContentEqual(approved, fresh) {
		return nil, model.NewFault(model.CodeApprovalInputChanged,
			"the resolved routing inputs changed since the execution approval; re-approve before dispatch")
	}
	budgetBody, err := a.resolveBudgetInputs(ctx, wf)
	if err != nil {
		return nil, err
	}
	store, err := a.artifactStore(wf)
	if err != nil {
		return nil, err
	}
	bref, err := store.Resolve(ctx, artifact.ResolveRequest{WorkflowID: wf, Type: model.ArtifactBudgetPolicy})
	if err != nil {
		return nil, model.NewFault(model.CodeApprovalInputChanged,
			"the budget policy is missing; re-run the execution dry run")
	}
	approvedBudget, err := store.Get(ctx, bref)
	if err != nil {
		return nil, err
	}
	// The stored body is the Artifact Store's canonical serialization
	// (map keys sorted), so the comparison is JSON-semantic, never
	// byte-wise.
	if !jsonBodiesEqual(approvedBudget, budgetBody) {
		return nil, model.NewFault(model.CodeApprovalInputChanged,
			"the resolved budget inputs changed since the execution approval; re-approve before dispatch")
	}
	return approved, nil
}

// jsonBodiesEqual reports whether two JSON bodies are semantically
// identical (the canonical serialization of the Artifact Store reorders
// object keys deterministically).
func jsonBodiesEqual(a, b []byte) bool {
	var va, vb any
	if json.Unmarshal(a, &va) != nil || json.Unmarshal(b, &vb) != nil {
		return false
	}
	return reflect.DeepEqual(va, vb)
}

// pauseDispatch commits the ordinary RUNNING→PAUSED control after a
// protocol drift closed the Dispatch Gate (design 6.1): dispatch closes
// and no managed process may start until the user regenerates the Dry
// Run and Execution Approval.
func (a *Application) pauseDispatch(ctx context.Context, st *store.Store, wf model.WorkflowID, restricted bool) error {
	_, err := a.runDecisionLoop(ctx, st, wf, PauseWorkflowCommand{Workflow: wf},
		model.WorkflowCommandInput{Kind: model.PauseWorkflow, Workflow: wf}, restricted)
	return err
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
			dn.specID = n.SpecID
			if s, ok := specByID[n.SpecID]; ok && s.Route != nil {
				dn.route = s.Route.Provider
			}
		case "merge":
			dn.kind = model.NodeMerge
			dn.specID = n.SpecID
			if s, ok := specByID[n.SpecID]; ok && s.Route != nil {
				dn.route = s.Route.Provider
			}
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
// execution graph when the aggregate has none, runs the dispatch CAS
// pre-pass against the approved routing policy (Task 16), computes the
// eligible set with the pure Scheduler, applies the pairwise static
// compatibility judgment, allocates every selected Node — committing each
// RUNNING Attempt before submitting its Effects — and then runs the
// selected Nodes' effect chains concurrently (Task 16 live parallelism:
// different Tasks may run their Provider Sessions on different Providers
// at the same time). Pause, Cancel, Quiesce, and Safety Stop share the
// committed Dispatch Gate with allocation, so no start can cross a
// committed closure (PRD 已确认：并行失败后的 Quiescing).
func (a *Application) dispatchPass(ctx context.Context, st *store.Store, wf model.WorkflowID, plan *dispatchPlan, restricted bool, routing *agent.RoutingPolicySet) (Outcome, error) {
	// The pass context: a user interruption (first Ctrl+C) or a detected
	// Commit Policy drift cancels it; every chain derives from it, so all
	// active Sessions abort together and no new external action starts
	// after the stop request.
	passCtx, passCancel := context.WithCancel(ctx)
	defer passCancel()
	a.mu.Lock()
	a.passCancel = passCancel
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.passCancel = nil
		a.mu.Unlock()
	}()
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
		// The dispatch CAS pre-pass (PRD 约束 306, design 14.2): every
		// Provider the approved policies reference is re-detected and
		// Compare-and-Swapped against its approved binding before any
		// Attempt is allocated or any Provider model request starts
		// (only the read-only version probes of detection run). A drift
		// closes the Dispatch Gate with
		// PROVIDER_PROTOCOL_BINDING_CHANGED; the verified detections are
		// cached for the pass, so every Start/Resume compares the same
		// verified identity.
		if routing != nil {
			rt.SetRoutingPolicy(routing)
		}
		if err := rt.VerifyBindings(ctx); err != nil {
			return Outcome{}, err
		}
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

	// Phase 1 (serialized): the selected Nodes' Resource Locks are taken
	// and held for the whole RUNNING lifetime of each Node — the
	// OS-level lifetime of a declared lock matches the state-model
	// declaration, so actual concurrent use can never overlap a declared
	// lock (ledger obligation from Task 12 re-review; the lock files live
	// in CFLOW_HOME/locks and are shared by every workflow, so two
	// workflows' RUNNING Attempts sharing a declared lock are excluded at
	// the OS level too). A merge Node takes the Integration/Apply Lock
	// before its Resource Locks (fixed lock order, design 18.1): the
	// serial --no-ff merges of the trusted delivery chain never overlap.
	type nodeChain struct {
		id        model.NodeID
		route     string
		baseHead  string
		node      *dispatchNode
		holds     *platform.Hold
		integHold *platform.Hold
	}
	var chains []nodeChain
	for _, id := range selected {
		node := plan.node(id)
		// The Final Verify Node's Reviewer Session runs on the approved
		// final-verification route of the Execution Approval (Task 18:
		// the independent Final Reviewer's route is bound by the policy,
		// never invented at dispatch).
		route := node.route
		if node.kind == model.NodeFinalVerify {
			if rb, ok := routingPrimaryBinding(routing, model.PurposeFinalVerification); ok {
				route = rb.Provider
			}
		}
		var integHold *platform.Hold
		if node.kind == model.NodeMerge {
			ls, err := a.lockSet()
			if err != nil {
				return Outcome{}, err
			}
			integHold, err = ls.Integration(ctx, a.project.Key)
			if err != nil {
				return Outcome{}, lockFault(err)
			}
		}
		holds, err := a.acquireResourceLocks(ctx, node.locks)
		if err != nil {
			if integHold != nil {
				integHold.Release()
			}
			return Outcome{}, err
		}
		chains = append(chains, nodeChain{
			id: id, route: route, baseHead: baseHead, node: node,
			holds: holds, integHold: integHold,
		})
	}

	// Phase 2 (live parallelism): every chain commits its RUNNING Attempt
	// before its Effects submit (design 12: an in-memory queued goroutine
	// is not an in-flight Attempt), and the chains then run concurrently.
	// The aggregate transactions stay serialized through the pass
	// transaction mutex, so no chain ever observes a stale aggregate; the
	// Provider runs themselves are concurrent. The pass waits for every
	// chain and surfaces the first failure; each chain releases its locks
	// when it settles.
	var wg sync.WaitGroup
	errs := make(chan error, len(chains))
	for _, c := range chains {
		c := c
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer c.holds.Release()
			if c.integHold != nil {
				defer c.integHold.Release()
			}
			if err := a.runNodeDispatch(passCtx, st, wf, c.id, c.route, c.baseHead, c.node, rt); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			return Outcome{}, err
		}
	}
	// The pass is over: cancel the pass context BEFORE the post-stop
	// settle so the monitor goroutines die before the scan. A monitor
	// that kept polling would re-arm the drift snapshot with post-stop
	// pre-heads (the config drift is unchanged), and the next dispatch
	// pass would settle with stale pre-heads — falsely quarantining a
	// legitimate successor session or re-opening a duplicate
	// COMMIT_POLICY gate after a replacement approval.
	passCancel()
	// A detected Commit Policy drift settles after the pass: the window
	// scan feeds the quarantine or the confirmation gate (PRD steps 6-7).
	if a.policyDriftPending() {
		if serr := a.settlePolicyDrift(ctx, st, wf); serr != nil {
			return Outcome{}, serr
		}
	}
	return Outcome{Workflow: wf, Stage: state.Workflow.Stage, Runtime: state.Workflow.Runtime,
		Findings: state.Findings}, nil
}

// routingPrimaryBinding resolves the primary (first) approved binding of
// one Purpose inside the approved routing policy ("" when the policy has
// none): the route the dispatch allocates a purpose's Session on.
func routingPrimaryBinding(set *agent.RoutingPolicySet, purpose model.AgentPurpose) (agent.RouteBinding, bool) {
	if set == nil {
		return agent.RouteBinding{}, false
	}
	for _, p := range set.Policies {
		if p.Purpose != purpose || len(p.Bindings) == 0 {
			continue
		}
		return p.Bindings[0], true
	}
	return agent.RouteBinding{}, false
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
// blocking Finding in the Node's scope and the total-run budget also
// defer. Verify Nodes run in their own Task Worktrees (no static
// conflict); Merge Nodes are serial: at most one merge Node per pass and
// never while another merge is RUNNING (design 15.5, 18.1); the Final
// Verify Node dispatches once every Merge Node is SUCCEEDED (Task 18;
// the Scheduler's dependency edges already gate it).
func (a *Application) selectBatch(state model.State, plan *dispatchPlan, eligible []model.NodeID) []model.NodeID {
	var selected []model.NodeID
	mergeSelected := false
	mergeRunning := false
	for _, n := range state.Nodes {
		if n.Kind == model.NodeMerge && n.Status == model.NodeRunning {
			mergeRunning = true
			break
		}
	}
	for _, id := range eligible {
		node := plan.node(id)
		if node == nil {
			continue
		}
		switch node.kind {
		case model.NodeAgentTask, model.NodeVerify, model.NodeFinalVerify, model.NodeCheckpoint:
		case model.NodeMerge:
			if mergeRunning || mergeSelected {
				continue
			}
			mergeSelected = true
		default:
			continue
		}
		if blockingFinding(state, node.id) {
			continue
		}
		if len(state.Attempts) >= plan.runBudget() {
			continue
		}
		if node.kind != model.NodeAgentTask {
			selected = append(selected, id)
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
// and must be discarded if the gate closes — then the Node's chain runs:
// the Task Worktree creation and the coding Session for an agent-task
// Node, the deterministic Verification and the independent Reviewer
// Session for a verify Node, and the serial --no-ff Integration merge
// (with its conflict rollback) for a merge Node. When an agent-task
// coding Session settles, the Application runs the Task Commit/Clean/
// Scope gate and feeds the AttemptEnded result; every Decision
// revalidates the committed Dispatch Gate.
func (a *Application) runNodeDispatch(ctx context.Context, st *store.Store, wf model.WorkflowID, node model.NodeID, route, baseHead string, planNode *dispatchNode, rt *agent.Runtime) error {
	// A budgeted retry of an automatic fallback reuses the persisted
	// successor Session: its row carries supersedes_session_id and the
	// fallback Provider, so the successor Attempt dispatches on the
	// approved fallback binding and keeps the lineage (design 14.4 —
	// the successor Session the Runtime allocated at the fallback is the
	// same Session the successor Attempt starts).
	session, dispatchRoute := model.SessionID(a.ids(model.IDSession)), route
	view, err := st.View(ctx, store.StoreQuery{})
	if err != nil {
		return err
	}
	if ready := readyAttemptOfState(view.State, node); ready != nil && ready.Session != "" {
		if s := sessionOfState(view.State, ready.Session); s != nil {
			session = s.ID
			if s.Provider != "" {
				dispatchRoute = s.Provider
			}
		}
	}
	processID := model.ProcessID(a.ids(model.IDProcess))
	input := model.Input(model.DispatchInput{
		Node:     node,
		Session:  session,
		Route:    dispatchRoute,
		BaseHead: baseHead,
		Process:  processID,
	})
	// The node's command Input is fixed before the loop: every Effect of
	// the node chain (worktree creation, the coding Session, ...) executes
	// against the same DispatchInput (the effect Result feeds the next
	// Decision through `input`, but never replaces the command context the
	// executors read).
	dispatchCmd := input
	executed := map[string]struct{}{}
	gateFed := false
	// The pass may be cancelled mid-chain (user Ctrl+C or a detected
	// Commit Policy drift): the RUNNING Attempt then settles INTERRUPTED
	// without Retry charge and the stop converges through the Kernel. The
	// settle transactions and the process-stop Effects continue on a
	// context that is not cancelled, so the aggregate transactions and
	// the two-phase stop complete.
	settleCtx := ctx
	for iter := 0; iter < nodeDispatchBudget; iter++ {
		cd, err := a.transactPass(ctx, st, input)
		if err != nil {
			if ctx.Err() != nil {
				// The pass was interrupted between Effects: settle the
				// RUNNING Attempt and complete the controlled stop.
				if a.interruptChain(context.WithoutCancel(ctx), st, wf, node, rt) {
					return nil
				}
			}
			return err
		}
		if iter == 0 {
			// The RUNNING Attempt committed: only now is the Node running.
			a.probeStep("attempt:" + string(node) + ":commit")
		}
		if cd.Decision.Effect == nil {
			// No further Effect: when an agent-task coding Session settled
			// and its Attempt is still RUNNING, the Application runs the
			// Task gate and feeds the AttemptEnded result (design 15.4).
			if planNode != nil && planNode.kind == model.NodeAgentTask && !gateFed && a.attemptRunning(ctx, wf, node) {
				result, err := a.taskGateResult(ctx, wf, node, planNode.writeScope)
				if err != nil {
					return a.interruptedOr(err, ctx, settleCtx, st, wf, node, rt)
				}
				gateFed = true
				input = result
				continue
			}
			// The Final Verify chain settled: the independent Final
			// Reviewer passed, so the Application feeds the exact-evidence
			// completion Decision in the same chain (Task 18, PRD 最终验
			// 收). The Kernel refuses with EVIDENCE_SUBJECT_CHANGED when
			// the Integration HEAD moved after the review bound it.
			if planNode != nil && planNode.kind == model.NodeFinalVerify && !gateFed && a.finalVerifySucceeded(ctx, wf, node) {
				gateFed = true
				input = model.CompleteWorkflowInput{}
				continue
			}
			// The completion Decision committed: the immutable Final
			// Report Artifact follows it (PRD 最终验收: 生成 final-report.md).
			// A chain that settled otherwise (a failed Final Verify or
			// review) writes nothing.
			if planNode != nil && planNode.kind == model.NodeFinalVerify {
				if merr := a.writeFinalReportIfCompleted(ctx, wf, st); merr != nil {
					return merr
				}
			}
			return nil
		}
		id := intentIdentity(cd.Decision.Effect)
		if _, dup := executed[id]; dup {
			return model.InvariantFault(fmt.Errorf("repeated identical uncompleted effect intent %s", id))
		}
		executed[id] = struct{}{}
		// The Commit Policy monitor runs while the commit-capable Session
		// is active (PRD step 5: no slower than once per second). It
		// starts at the chain's first Effect — before the Worktree
		// creation and the Session — so its first recompute observes the
		// drift before any Commit can land inside the window.
		if planNode != nil && planNode.kind == model.NodeAgentTask {
			switch cd.Decision.Effect.(type) {
			case model.ProviderStartIntent, model.TaskWorktreeCreateIntent:
				go a.monitorPolicy(ctx, wf)
			}
		}
		result, err := a.executeEffect(ctx, cd.Decision.Effect, false, wf, DispatchCommand{Workflow: wf}, dispatchCmd, rt)
		if err != nil {
			if ctx.Err() != nil {
				// The pass was interrupted (user Ctrl+C or a detected
				// Commit Policy drift): settle the RUNNING Attempt as
				// INTERRUPTED (never charged) and complete the controlled
				// stop of the pass on the non-cancelled settle context.
				if a.interruptChain(context.WithoutCancel(ctx), st, wf, node, rt) {
					return nil
				}
			}
			return err
		}
		if err := validateEffectResult(cd.Decision.Effect, result); err != nil {
			if ctx.Err() != nil {
				// The pass was interrupted: a result that no longer matches
				// its Intent is the interruption's symptom (e.g. the Worktree
				// could not be observed under the cancelled context). The
				// RUNNING Attempt settles INTERRUPTED through the stop
				// protocol, never as a fabricated failure.
				if a.interruptChain(context.WithoutCancel(ctx), st, wf, node, rt) {
					return nil
				}
			}
			return err
		}
		input = model.EffectResultInput(result)
	}
	return model.InvariantFault(fmt.Errorf("node dispatch exceeded the effect bound"))
}

// interruptedOr settles the chain's RUNNING Attempt when a pass
// interruption aborted the gate, and reports nil when the stop completed.
func (a *Application) interruptedOr(err error, ctx, settleCtx context.Context, st *store.Store, wf model.WorkflowID, node model.NodeID, rt *agent.Runtime) error {
	if ctx.Err() != nil && a.interruptChain(context.WithoutCancel(ctx), st, wf, node, rt) {
		return nil
	}
	return err
}

// interruptChain settles the chain's RUNNING Attempt as interrupted when
// the pass was cancelled and completes the controlled stop of the pass:
// the typed interrupted result opens the stop (gate close, Run STOPPING,
// CONTROLLED_STOP_REQUESTED or the COMMIT_POLICY_SAFETY_STOP_REQUESTED
// intent) and the process-stop Effects are executed to convergence — all
// on the non-cancelled settle context. It reports whether the chain may
// finish (the stop completed or nothing was running).
func (a *Application) interruptChain(settleCtx context.Context, st *store.Store, wf model.WorkflowID, node model.NodeID, rt *agent.Runtime) bool {
	view, err := st.View(settleCtx, store.StoreQuery{})
	if err != nil {
		return false
	}
	att := runningAttemptOfState(view.State, node)
	if att == nil {
		return true // nothing running: the chain is already done
	}
	// The interrupted Attempt records its Worktree end facts (observed on
	// the settle context): the resume re-verification compares the
	// successor's Worktree HEAD/status/Dirty Fingerprint against them
	// (PRD 已确认：Ctrl+C 两阶段有限停止 step 7), so the end evidence must
	// exist. A non-coding chain's Worktree observation fails closed to
	// empty facts, which the reuse check treats as "no prior evidence".
	head, dirty := a.driftEndFacts(settleCtx, wf, node)
	input := model.EffectResultInput{
		Kind:                model.AttemptEnded,
		Attempt:             att.Key,
		Outcome:             model.OutcomeInterrupted,
		FailureCode:         a.policyDriftCode(),
		EndHead:             head,
		EndDirtyFingerprint: dirty,
	}
	executed := map[string]struct{}{}
	for iter := 0; iter < nodeDispatchBudget; iter++ {
		cd, err := a.transactPass(settleCtx, st, input)
		if err != nil {
			return false
		}
		if cd.Decision.Effect == nil {
			return true
		}
		id := intentIdentity(cd.Decision.Effect)
		if _, dup := executed[id]; dup {
			return false
		}
		executed[id] = struct{}{}
		result, err := a.executeEffect(settleCtx, cd.Decision.Effect, false, wf, DispatchCommand{Workflow: wf}, input, rt)
		if err != nil {
			return false
		}
		input = model.EffectResultInput(result)
	}
	return false
}

// attemptRunning reports whether one Node's Attempt is still RUNNING.
// The read goes through the already-open write Store: the mutation lock
// batch is held, so no second Schema Lock may be taken (design 18.1).
func (a *Application) attemptRunning(ctx context.Context, wf model.WorkflowID, node model.NodeID) bool {
	view, err := a.writeStoreView(ctx, wf)
	if err != nil {
		return false
	}
	return runningAttemptOfState(view.State, node) != nil
}

// finalVerifySucceeded reports whether the Final Verify Node settled
// SUCCEEDED and the Workflow is at the FINAL_VERIFICATION stage (the
// completion Decision may be fed). The read goes through the
// already-open write Store (design 18.1).
func (a *Application) finalVerifySucceeded(ctx context.Context, wf model.WorkflowID, node model.NodeID) bool {
	view, err := a.writeStoreView(ctx, wf)
	if err != nil {
		return false
	}
	st := view.State
	if st.Workflow.Stage != model.StageFinalVerification || st.Workflow.Runtime != model.RuntimeRunning {
		return false
	}
	n := st.Nodes[node]
	return n != nil && n.Kind == model.NodeFinalVerify && n.Status == model.NodeSucceeded
}

// validateTaskNode checks that the named Node exists in the installed
// graph and is an agent-task Node (PRD 必须提供的 CLI: `cflow retry
// <task-id>` refuses an unknown or non-task identity before any
// dispatch).
func (a *Application) validateTaskNode(ctx context.Context, wf model.WorkflowID, node model.NodeID) error {
	if node == "" {
		return model.InvalidInputFault("a task id is required")
	}
	view, err := a.readAggregate(ctx, wf, store.StoreQuery{})
	if err != nil {
		return orCtx(ctx, err)
	}
	if view.State.Workflow.ID == "" {
		return model.InvalidInputFault("no such workflow: " + string(wf))
	}
	n := view.State.Nodes[node]
	if n == nil || n.Kind != model.NodeAgentTask {
		return model.InvalidInputFault("unknown task " + string(node))
	}
	return nil
}

// writeStoreView reads the current aggregate through the already-open
// write Store of one workflow (no new lock; the mutation lock batch is
// held by the caller, design 18.1).
func (a *Application) writeStoreView(ctx context.Context, wf model.WorkflowID) (store.StoreView, error) {
	a.mu.Lock()
	st := a.stores[wf]
	a.mu.Unlock()
	if st == nil {
		return store.StoreView{}, model.InvariantFault(fmt.Errorf("no write store is open for workflow %s", wf))
	}
	return st.View(ctx, store.StoreQuery{})
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
// codingSessionInput is the coding Session's input block: the approved
// Spec set, the Verification Catalog, the Task Worktree location, and —
// for the successor Session of an automatic fallback — the immutable
// redacted Context Bundle handoff of the LOST original (design 14.4,
// PRD 已确认：Session Resume 失败与跨 Provider 上下文交接). The bundle
// carries the auditable handoff only; it is never a credential or an
// unredacted transcript.
type codingSessionInput struct {
	Spec     string `json:"spec"`
	Catalog  string `json:"catalog"`
	Worktree string `json:"worktree"`
	// ContextBundle is the redacted handoff of the superseded LOST
	// Session (nil for a brand-new Session).
	ContextBundle *agent.ContextBundle `json:"context_bundle,omitempty"`
}

func (a *Application) codingSessionInput(ctx context.Context, wf model.WorkflowID, node model.NodeID) any {
	store, err := a.artifactStore(wf)
	if err != nil {
		return nil
	}
	body := readArtifact(ctx, store, wf, model.ArtifactSpec)
	bodies, err := splitSpecSetBody(body)
	if err != nil {
		return nil
	}
	// The Compiler names each Task node "task-<spec-id>"; hand the coding
	// Session ONLY its own Spec so a multi-Spec workflow (Task 12) can
	// never be implemented under the wrong Spec — a provider agent that
	// receives the whole Spec set may attribute a sibling Spec to its
	// Task.
	specID := strings.TrimPrefix(string(node), "task-")
	nodeSpec := ""
	for _, b := range bodies {
		s, err := compile.ParseSpec(b)
		if err != nil {
			continue
		}
		if s.ID == specID {
			nodeSpec = string(b)
			break
		}
	}
	if nodeSpec == "" {
		return nil
	}
	return &codingSessionInput{
		Spec:     nodeSpec,
		Catalog:  string(readArtifact(ctx, store, wf, model.ArtifactCatalog)),
		Worktree: a.taskWorktreePath(wf, node),
	}
}
