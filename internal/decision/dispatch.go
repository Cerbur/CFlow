package decision

// The execution dispatch decisions (Task 12, design 12): the graph
// install, the serialized allocation, the Task Worktree creation result,
// and the settled coding Session. The pure Scheduler computes the
// eligible set from the GraphSnapshot; every allocation Decision
// revalidates the committed aggregate in the same transaction that
// commits the RUNNING Attempt, so no start can cross a committed Dispatch
// Gate closure (PRD 已确认：并行失败后的 Quiescing). The Kernel never
// infers readiness from Task display status.
//
// Same-package split of the decision package: no public seam added.

import (
	"fmt"
	"sort"

	"cflow.local/cflow/internal/model"
)

// decideGraphInstall installs the execution graph of the approved Dynamic
// Workflow: every Node starts PENDING with its skeleton dependencies, and
// agent-task Nodes carry their deterministic Task Branch. The install is
// refused outside EXECUTION, on a Workflow whose graph is already
// installed, or for a DAG with dangling dependencies.
func decideGraphInstall(state model.State, in model.GraphInstallInput) (model.Decision, error) {
	if state.Workflow.ID == "" {
		return model.Decision{}, model.InvalidInputFault("no workflow to install an execution graph for")
	}
	if state.Workflow.Stage != model.StageExecution {
		return model.Decision{}, model.InvalidInputFault("the execution graph can only be installed at the EXECUTION stage")
	}
	if len(state.Nodes) > 0 {
		return model.Decision{}, model.InvalidInputFault("the execution graph is already installed")
	}
	if len(in.Nodes) == 0 {
		return model.Decision{}, model.InvalidInputFault("the execution graph is empty")
	}
	byID := map[model.NodeID]bool{}
	for _, n := range in.Nodes {
		if n.ID == "" || !n.Kind.Valid() {
			return model.Decision{}, model.InvalidInputFault("the execution graph carries an invalid node")
		}
		if byID[n.ID] {
			return model.Decision{}, model.InvalidInputFault("the execution graph carries a duplicate node " + string(n.ID))
		}
		byID[n.ID] = true
		if n.RetryBudget < 0 {
			return model.Decision{}, model.InvalidInputFault("node " + string(n.ID) + " carries a negative retry budget")
		}
		for _, d := range n.Dependencies {
			if d == "" {
				return model.Decision{}, model.InvalidInputFault("node " + string(n.ID) + " carries an empty dependency")
			}
		}
	}
	for _, n := range in.Nodes {
		for _, d := range n.Dependencies {
			if !byID[d] {
				return model.Decision{}, model.InvalidInputFault(
					"node " + string(n.ID) + " depends on " + string(d) + ", which is not part of the graph")
			}
		}
	}
	sorted := append([]model.InstallNode(nil), in.Nodes...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	b := &builder{state: state}
	for _, n := range sorted {
		branch := ""
		if n.Kind == model.NodeAgentTask {
			branch = taskBranch(state.Workflow.ID, n.ID)
		}
		b.mutate(model.NodeAppendMutation{Node: model.Node{
			ID:           n.ID,
			Kind:         n.Kind,
			Status:       model.NodePending,
			Dependencies: append([]model.NodeID(nil), n.Dependencies...),
			Branch:       branch,
			RetryBudget:  n.RetryBudget,
		}})
	}
	b.event(model.EventGraphInstalled, "", model.AttemptKey{}, "", "execution graph installed")
	return b.decision(), nil
}

// decideDispatch is one serialized allocation (design 12). The
// Application computed the eligible set with the pure Scheduler; this
// Decision revalidates the committed aggregate — the Run is Running with
// the Dispatch Gate open, the Node is PENDING or READY with budget, the
// Session identity is fresh — and commits the RUNNING Attempt, the Task
// Base at readiness, the coding Session, and the Task Worktree creation
// Effect together. A closed gate refuses with DISPATCH_GATE_CLOSED and
// mutates nothing: an in-memory queued goroutine is never an in-flight
// Attempt.
func decideDispatch(state model.State, in model.DispatchInput) (model.Decision, error) {
	if state.Workflow.ID == "" {
		return model.Decision{}, model.InvalidInputFault("no workflow to dispatch")
	}
	if state.Workflow.Stage != model.StageExecution {
		return model.Decision{}, model.InvalidInputFault("dispatch requires the EXECUTION stage")
	}
	run := activeRun(state)
	if run == nil || run.Status != model.RunRunning || !run.DispatchGate {
		return model.Decision{}, model.NewFault(model.CodeDispatchGateClosed,
			"dispatch gate is closed; no new node may start")
	}
	node := state.Nodes[in.Node]
	if node == nil {
		return model.Decision{}, model.InvalidInputFault("unknown node " + string(in.Node))
	}
	if node.Kind != model.NodeAgentTask {
		return model.Decision{}, model.InvalidInputFault(
			"node kind " + string(node.Kind) + " cannot be dispatched by this build")
	}
	switch node.Status {
	case model.NodePending, model.NodeReady:
	default:
		return model.Decision{}, model.InvalidInputFault(
			"node " + string(node.ID) + " cannot be allocated from status " + string(node.Status))
	}
	if node.Status == model.NodeReady && node.RetryCharged >= node.RetryBudget {
		return model.Decision{}, model.InvalidInputFault("node " + string(node.ID) + " has exhausted its retry budget")
	}
	if in.Session == "" || in.Route == "" {
		return model.Decision{}, model.InvalidInputFault(
			"allocation requires a session identity and an approved route")
	}
	// The Task Base: the current verified Integration HEAD at readiness,
	// recorded once and immutable (PRD Worktree 策略). A first allocation
	// carries it from the Application; a budgeted retry reuses the
	// recorded Base — the freshly observed HEAD is ignored and the Task
	// never silently rebases onto a different baseline.
	base := in.BaseHead
	if node.Status == model.NodeReady {
		if node.BaseCommit == "" {
			return model.Decision{}, model.InvalidInputFault(
				"node " + string(node.ID) + " has no recorded task base to retry from")
		}
		base = node.BaseCommit
	} else if base == "" {
		return model.Decision{}, model.InvalidInputFault(
			"allocation requires the verified integration head")
	}
	if err := validateFreshSession(state, in.Session); err != nil {
		return model.Decision{}, err
	}

	number := nextAttemptNumber(state, node.ID)
	key := model.AttemptKey{Node: node.ID, Number: number}
	b := &builder{state: state}
	b.mutate(model.NodeStatusMutation{Node: node.ID, Status: model.NodeRunning, RetryCharged: node.RetryCharged})
	b.event(model.EventNodeReady, node.ID, key, "", "node ready")
	b.event(model.EventNodeStarted, node.ID, key, "", "node started")
	// The Session row precedes the Attempt row (the node_attempts
	// session_id foreign key references it).
	b.mutate(model.SessionAppendMutation{Session: model.Session{
		ID:      in.Session,
		Purpose: model.PurposeImplementation,
		Status:  model.SessionStarting,
	}, Provider: in.Route})
	b.mutate(model.AttemptAppendMutation{Attempt: model.Attempt{
		Key:       key,
		Session:   in.Session,
		Status:    model.AttemptRunning,
		StartHead: base,
		StartedAt: state.Now,
	}})
	b.event(model.EventAttemptCreated, node.ID, key, "", "attempt created")
	if node.Status == model.NodePending {
		// The Task Base is the current verified Integration HEAD at
		// readiness, recorded once and immutable (PRD Worktree 策略).
		b.mutate(model.TaskMutation{Node: node.ID, BaseCommit: base})
		b.effect(model.TaskWorktreeCreateIntent{
			Node:     node.ID,
			Branch:   taskBranch(state.Workflow.ID, node.ID),
			BaseHead: base,
		})
	} else {
		// Budgeted retry: the Task Worktree already exists from the first
		// allocation, so no worktree creation Effect is emitted — the
		// coding Session starts directly inside it from the recorded Base.
		b.effect(model.ProviderStartIntent{
			Session: in.Session,
			Purpose: model.PurposeImplementation,
			Route:   in.Route,
			Node:    node.ID,
		})
	}
	return b.decision(), nil
}

// nextAttemptNumber allocates the successor Attempt number of one Node
// deterministically: one past the highest persisted Attempt.
func nextAttemptNumber(state model.State, node model.NodeID) model.AttemptNumber {
	var max model.AttemptNumber
	for k := range state.Attempts {
		if k.Node == node && k.Number > max {
			max = k.Number
		}
	}
	return max + 1
}

// taskBranch is the deterministic CFlow-owned Task Branch of one Node
// (PRD Worktree 策略: the Task Branch and Worktree are created from the
// recorded Task Base and never rebase).
func taskBranch(wf model.WorkflowID, node model.NodeID) string {
	return "cflow/" + string(wf) + "/task-" + string(node)
}

// decideTaskWorktreeCreated records the created Task Worktree and
// requests the coding Session inside it. The route comes from the
// allocated Session's Provider, so the Session always runs on the
// approved route recorded at allocation.
func decideTaskWorktreeCreated(state model.State, in model.EffectResultInput) (model.Decision, error) {
	if in.WorktreePath == "" {
		return model.Decision{}, model.InvariantFault(fmt.Errorf("task worktree result carries no path"))
	}
	node := state.Nodes[in.Attempt.Node]
	if node == nil {
		return model.Decision{}, model.InvariantFault(fmt.Errorf("task worktree result references unknown node %s", in.Attempt.Node))
	}
	if node.Kind != model.NodeAgentTask {
		return model.Decision{}, model.InvariantFault(fmt.Errorf("task worktree result references a non-task node %s", in.Attempt.Node))
	}
	attempt := runningAttemptOf(state, in.Attempt.Node)
	if attempt == nil {
		return model.Decision{}, model.InvalidInputFault("node " + string(in.Attempt.Node) + " has no running attempt")
	}
	session := findSessionState(state, attempt.Session)
	if session == nil || session.Provider == "" {
		return model.Decision{}, model.InvariantFault(fmt.Errorf("allocated session carries no approved route"))
	}
	b := &builder{state: state}
	b.mutate(model.TaskMutation{Node: node.ID, WorktreePath: in.WorktreePath})
	b.effect(model.ProviderStartIntent{
		Session: attempt.Session,
		Purpose: model.PurposeImplementation,
		Route:   session.Provider,
		Node:    node.ID,
	})
	return b.decision(), nil
}

// runningAttemptOf returns the RUNNING Attempt of one Node (at most one
// can be running).
func runningAttemptOf(state model.State, node model.NodeID) *model.Attempt {
	for k, a := range state.Attempts {
		if k.Node == node && a.Status == model.AttemptRunning {
			return a
		}
	}
	return nil
}

// decideImplementationRunEnded settles one coding Session. The Coding
// Agent's output can never set lifecycle state (design 7.3 invariant 1):
// the Attempt's outcome is judged from Git evidence by the Commit gate
// (Task 13); here only the Session facts are recorded and the Attempt
// stays RUNNING with its start facts.
func decideImplementationRunEnded(state model.State, in model.EffectResultInput, created *model.Session) (model.Decision, error) {
	b := &builder{state: state}
	b.mutate(sessionEnd(state, created, in))
	return b.decision(), nil
}
