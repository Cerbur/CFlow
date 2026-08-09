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
// installed, or for a DAG with dangling dependencies. A Replacement
// install (after a Replacement Execution Approval) keeps every existing
// Node identity whose kind and dependencies are unchanged — the
// persisted state is the incremental recovery — and appends only the new
// Nodes (PRD 已确认：未污染兄弟 Task 增量恢复 step 3: a changed definition
// must use a new Node id, never a "recovered" old one).
func decideGraphInstall(state model.State, in model.GraphInstallInput) (model.Decision, error) {
	if state.Workflow.ID == "" {
		return model.Decision{}, model.InvalidInputFault("no workflow to install an execution graph for")
	}
	if state.Workflow.Stage != model.StageExecution {
		return model.Decision{}, model.InvalidInputFault("the execution graph can only be installed at the EXECUTION stage")
	}
	replacement := len(state.Nodes) > 0
	if replacement && (!in.Replacement || !replacementApproved(state)) {
		return model.Decision{}, model.InvalidInputFault(
			"the execution graph is already installed; only an approved replacement may extend it")
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
				if old := state.Nodes[d]; old == nil {
					return model.Decision{}, model.InvalidInputFault(
						"node " + string(n.ID) + " depends on " + string(d) + ", which is not part of the graph")
				}
			}
		}
	}
	sorted := append([]model.InstallNode(nil), in.Nodes...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	b := &builder{state: state}
	for _, n := range sorted {
		if old := state.Nodes[n.ID]; old != nil {
			// Replacement re-install: the logical Node identity must match
			// exactly; any definition change is a new Node.
			if old.Kind != n.Kind || !sameDependencies(old.Dependencies, n.Dependencies) {
				return model.Decision{}, model.InvalidInputFault(
					"replacement node " + string(n.ID) + " changes the definition of an existing node; use a new id")
			}
			continue
		}
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

// sameDependencies reports whether two dependency lists are equal as sets
// with the same order.
func sameDependencies(a, b []model.NodeID) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// decideDispatch is one serialized allocation routed by Node kind
// (design 12): agent-task Nodes run the coding chain, verify Nodes the
// deterministic Verification and the independent Review, and merge Nodes
// the serial --no-ff Integration merge (Task 13). The Workspace Adoption
// Gate closes the scheduler before any Node starts (Task 4, design 8.2):
// when the Execution Approval bound a frozen Change Set and the Workspace
// has not been adopted yet, no normal Task may be created from an
// unverified candidate Head (verified_workspace_head is the only legal
// Task base), and the Kernel refuses with WORKSPACE_ADOPTION_REQUIRED
// without mutating anything.
func decideDispatch(state model.State, in model.DispatchInput) (model.Decision, error) {
	if state.Workflow.ExecutionFacts != nil && state.Workflow.ExecutionFacts.ChangeSetHash != "" &&
		state.Workflow.VerifiedWorkspaceHead == "" {
		return model.Decision{}, model.NewFault(model.CodeWorkspaceAdoptionRequired,
			"the workspace has not been adopted; no task may start from an unverified candidate head")
	}
	node := state.Nodes[in.Node]
	if node == nil {
		return model.Decision{}, model.InvalidInputFault("unknown node " + string(in.Node))
	}
	switch node.Kind {
	case model.NodeAgentTask:
		return decideDispatchAgentTask(state, in)
	case model.NodeVerify:
		return decideVerifyDispatch(state, in)
	case model.NodeMerge:
		return decideMergeDispatch(state, in)
	case model.NodeFinalVerify:
		return decideFinalVerifyDispatch(state, in)
	case model.NodeCheckpoint:
		return decideCheckpointDispatch(state, in)
	default:
		return model.Decision{}, model.InvalidInputFault(
			"node kind " + string(node.Kind) + " cannot be dispatched by this build")
	}
}

// decideCheckpointDispatch settles one observation checkpoint of the
// approved DAG (Task 18): a checkpoint is a passive observation point
// whose inputs are the verified Merge outputs, so once every dependency
// succeeded the checkpoint records the deterministic "checkpoint
// reached" fact and a non-blocking Finding — the checkpoint Agent
// Session dispatch lands with a later task. It never claims a Provider
// run and never touches the Integration state, so the delivery chain
// (the Final Verify after every Merge) is never permanently gated by an
// observation point.
func decideCheckpointDispatch(state model.State, in model.DispatchInput) (model.Decision, error) {
	if state.Workflow.ID == "" {
		return model.Decision{}, model.InvalidInputFault("no workflow to dispatch")
	}
	switch state.Workflow.Stage {
	case model.StageExecution, model.StageFinalVerification:
	default:
		return model.Decision{}, model.InvalidInputFault(
			"checkpoint dispatch requires the EXECUTION or FINAL_VERIFICATION stage")
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
	if node.Kind != model.NodeCheckpoint {
		return model.Decision{}, model.InvalidInputFault(
			"node kind " + string(node.Kind) + " is not a checkpoint node")
	}
	switch node.Status {
	case model.NodePending, model.NodeReady:
	default:
		return model.Decision{}, model.InvalidInputFault(
			"node " + string(node.ID) + " cannot be allocated from status " + string(node.Status))
	}
	b := &builder{state: state}
	b.mutate(model.NodeStatusMutation{Node: node.ID, Status: model.NodeSucceeded, RetryCharged: node.RetryCharged})
	b.event(model.EventNodeSucceeded, node.ID, model.AttemptKey{}, "", "checkpoint reached")
	b.mutate(model.FindingAppendMutation{Finding: model.Finding{
		ID:       model.FindingID(fmt.Sprintf("finding-%d", len(state.Findings)+1)),
		Code:     model.CodeNotYetAvailable,
		Scope:    model.ScopeWorkflow,
		Subject:  string(node.ID),
		Blocking: false,
		Text:     "checkpoint observation reached; the checkpoint agent session dispatch lands with a later task",
		Seq:      state.NextEventSeq,
	}})
	b.event(model.EventFindingOpened, node.ID, model.AttemptKey{}, model.CodeNotYetAvailable, "checkpoint observation recorded")
	return b.decision(), nil
}

// decideDispatchAgentTask is the agent-task allocation. The
// Application computed the eligible set with the pure Scheduler; this
// Decision revalidates the committed aggregate — the Run is Running with
// the Dispatch Gate open, the Node is PENDING or READY with budget, the
// Session identity is fresh — and commits the RUNNING Attempt, the Task
// Base at readiness, the coding Session, and the Task Worktree creation
// Effect together. A closed gate refuses with DISPATCH_GATE_CLOSED and
// mutates nothing: an in-memory queued goroutine is never an in-flight
// Attempt.
func decideDispatchAgentTask(state model.State, in model.DispatchInput) (model.Decision, error) {
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
	// The Retry Budget bounds automatic successor Attempts: the initial
	// Attempt is never charged and each budgeted retry charges one, so a
	// READY successor whose charge equals the budget is the last allowed
	// Attempt and still dispatches; a charge beyond the budget cannot
	// exist (failures beyond it are blocking).
	if node.Status == model.NodeReady && node.RetryCharged > node.RetryBudget {
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
	// The prior terminal Attempt determines the successor's role: a
	// DIRTY_TASK_WORKTREE failure reuses the exact Task Branch/Worktree
	// with an independent Repair Session (PRD 已确认：DIRTY_TASK_WORKTREE 原
	// 地 Repair), and an interrupted Attempt with a resumable Provider
	// Session prefers resuming the original Session (PRD 已确认：Ctrl+C 两阶
	// 段有限停止 step 6). A budgeted retry of an automatic fallback reuses
	// the persisted successor Session (design 14.4) — the allocation never
	// creates a second fresh Session row, so the "one successor per Lost
	// original" lineage stays intact in the Store.
	session := in.Session
	reuse := false
	resume := false
	purpose := model.PurposeImplementation
	if node.Status == model.NodeReady {
		prior := highestTerminalAttempt(state, node.ID)
		if s := findSessionState(state, session); s != nil && s.Status == model.SessionStarting {
			if s.Purpose != model.PurposeImplementation {
				return model.Decision{}, model.NewFault(model.CodeSessionIndependenceViolation,
					"the reused successor session's purpose does not match the task")
			}
			reuse = true
		} else if prior != nil && prior.Status == model.AttemptInterrupted && prior.Session != "" {
			if s := findSessionState(state, prior.Session); s != nil &&
				s.Status == model.SessionInterrupted && s.ProviderSessionID != "" {
				// Prefer the original Session of the interrupted Attempt.
				session = prior.Session
				purpose = s.Purpose
				resume = true
			}
		}
	}
	if !reuse && !resume {
		if prior := highestTerminalAttempt(state, node.ID); prior != nil &&
			prior.FailureCode == model.CodeDirtyTaskWorktree {
			// The dirty repair runs an independent Repair Session.
			purpose = model.PurposeRepair
		}
		if err := validateFreshSession(state, session); err != nil {
			return model.Decision{}, err
		}
	}

	number := nextAttemptNumber(state, node.ID)
	key := model.AttemptKey{Node: node.ID, Number: number}
	b := &builder{state: state}
	b.mutate(model.NodeStatusMutation{Node: node.ID, Status: model.NodeRunning, RetryCharged: node.RetryCharged})
	b.event(model.EventNodeReady, node.ID, key, "", "node ready")
	b.event(model.EventNodeStarted, node.ID, key, "", "node started")
	if !reuse && !resume {
		// The Session row precedes the Attempt row (the node_attempts
		// session_id foreign key references it).
		b.mutate(model.SessionAppendMutation{Session: model.Session{
			ID:      session,
			Purpose: purpose,
			Status:  model.SessionStarting,
		}, Provider: in.Route})
	}
	b.mutate(model.AttemptAppendMutation{Attempt: model.Attempt{
		Key:       key,
		Session:   session,
		Status:    model.AttemptRunning,
		StartHead: base,
		StartedAt: state.Now,
	}})
	b.event(model.EventAttemptCreated, node.ID, key, "", "attempt created")
	if in.Process != "" {
		// The managed-process ledger: the chain's Process is RUNNING with
		// the Session (the controlled stop may stop it, design 13.3).
		b.mutate(model.ProcessAppendMutation{Process: model.ProcessRecord{
			ID: in.Process, Session: session, Purpose: purpose,
			Status: model.ProcessStatusRunning, StartedAt: state.Now,
		}})
	}
	if node.Status == model.NodePending {
		// The Task Base is the current verified Integration HEAD at
		// readiness, recorded once and immutable (PRD Worktree 策略).
		b.mutate(model.TaskMutation{Node: node.ID, BaseCommit: base})
		b.effect(model.TaskWorktreeCreateIntent{
			Node:     node.ID,
			Branch:   taskBranch(state.Workflow.ID, node.ID),
			BaseHead: base,
		})
	} else if resume {
		// The successor resumes the original Provider Session of the
		// interrupted Attempt on the same Task Branch/Worktree.
		b.effect(model.ProviderResumeIntent{
			Session: session,
			Purpose: purpose,
			Process: in.Process,
		})
	} else {
		// Budgeted retry: the Task Worktree already exists from the first
		// allocation, so no worktree creation Effect is emitted — the
		// coding Session starts directly inside it from the recorded Base.
		b.effect(model.ProviderStartIntent{
			Session: session,
			Purpose: purpose,
			Route:   in.Route,
			Node:    node.ID,
			Process: in.Process,
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
		Process: processOfAttempt(state, attempt.Key),
	})
	return b.decision(), nil
}

// processOfAttempt is the RUNNING managed Process bound to one Attempt's
// Session ("" when the chain carries none).
func processOfAttempt(state model.State, key model.AttemptKey) model.ProcessID {
	attempt := state.Attempts[key]
	if attempt == nil {
		return ""
	}
	for _, p := range state.Processes {
		if p.Session == attempt.Session && p.Status == model.ProcessStatusRunning {
			return p.ID
		}
	}
	return ""
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
