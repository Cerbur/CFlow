package decision

import (
	"fmt"

	"cflow.local/cflow/internal/model"
)

// decideWorkflow handles the closed set of user Workflow mutation
// Commands (design 6.1). Ordinary control allows only RUNNING→PAUSED,
// RUNNING→BLOCKED, PAUSED→RUNNING, and BLOCKED→RUNNING (PRD 状态机与持久化
// 模型); Cancel is a separate recoverable protocol (design 17.4).
func decideWorkflow(state model.State, in model.WorkflowCommandInput) (model.Decision, error) {
	switch in.Kind {
	case model.CreateWorkflow:
		return decideCreateWorkflow(state, in)
	case model.StartWorkflow:
		return decideStart(state, in)
	case model.PauseWorkflow:
		return decidePause(state, in)
	case model.ResumeWorkflow:
		return decideResume(state, in)
	case model.CancelWorkflow:
		return decideCancel(state, in)
	default:
		return model.Decision{}, model.InvalidInputFault("unsupported workflow command")
	}
}

// decideCreateWorkflow records the Workflow with the user branch and base
// commit fixed at creation. The Target Branch is modified only by a later
// explicit protected Apply; this Decision fixes it and no later Decision
// carries a different value (design 7.3 invariant 11).
func decideCreateWorkflow(state model.State, in model.WorkflowCommandInput) (model.Decision, error) {
	if state.Workflow.ID != "" {
		return model.Decision{}, model.InvalidInputFault("workflow already exists")
	}
	if !in.Workflow.Valid() || !in.Project.Valid() {
		return model.Decision{}, model.InvalidInputFault("workflow creation requires opaque workflow and project identities")
	}
	if in.TargetBranch == "" || in.BaseCommit == "" {
		return model.Decision{}, model.InvalidInputFault("workflow creation requires a target branch and base commit")
	}
	b := &builder{state: state}
	b.mutate(wfMut(state, model.StageRequirementDiscussion, model.RuntimePending, nil))
	b.event(model.EventWorkflowCreated, "", model.AttemptKey{}, "", "workflow created")
	return b.decision(), nil
}

// decideStart starts the first Run: PENDING→RUNNING with an open dispatch
// gate. Only the pre-start PENDING status may be started; every later
// transition uses Resume.
func decideStart(state model.State, in model.WorkflowCommandInput) (model.Decision, error) {
	if state.Workflow.ID == "" {
		return model.Decision{}, model.InvalidInputFault("no workflow to start")
	}
	if state.Workflow.Runtime != model.RuntimePending {
		return model.Decision{}, model.InvalidInputFault("workflow can only start from PENDING")
	}
	b := &builder{state: state}
	b.mutate(wfMutStatus(state, model.RuntimeRunning))
	b.mutate(model.RunAppendMutation{Run: newRun(state, model.RunRunning, true)})
	b.event(model.EventRunStarted, "", model.AttemptKey{}, "", "run started")
	b.event(model.EventWorkflowStarted, "", model.AttemptKey{}, "", "workflow started")
	return b.decision(), nil
}

// decidePause applies the ordinary RUNNING→PAUSED control: dispatch
// closes and each managed process is stopped through the controlled-stop
// protocol (one Effect per Decision).
func decidePause(state model.State, in model.WorkflowCommandInput) (model.Decision, error) {
	if state.Workflow.ID == "" {
		return model.Decision{}, model.InvalidInputFault("no workflow to pause")
	}
	if state.Workflow.Runtime != model.RuntimeRunning {
		return model.Decision{}, model.InvalidInputFault("workflow can only pause from RUNNING")
	}
	b := &builder{state: state}
	b.mutate(wfMutStatus(state, model.RuntimePaused))
	b.event(model.EventWorkflowPaused, "", model.AttemptKey{}, "", "workflow paused")
	if run := activeRun(state); run != nil && !run.Status.IsTerminal() {
		b.mutate(model.RunMutation{ID: run.ID, Status: model.RunStopping, DispatchGate: false})
		b.event(model.EventRunStopped, "", model.AttemptKey{}, "", "run stopping")
		stopRunningProcesses(b, state)
	}
	return b.decision(), nil
}

// decideResume reopens dispatch with a new Run record; a resume never
// resurrects a failed Node or reopens a terminal Attempt (PRD 状态机与持久
// 化模型).
func decideResume(state model.State, in model.WorkflowCommandInput) (model.Decision, error) {
	if state.Workflow.ID == "" {
		return model.Decision{}, model.InvalidInputFault("no workflow to resume")
	}
	if state.Workflow.Runtime != model.RuntimePaused && state.Workflow.Runtime != model.RuntimeBlocked {
		return model.Decision{}, model.InvalidInputFault("workflow can only resume from PAUSED or BLOCKED")
	}
	b := &builder{state: state}
	b.mutate(wfMutStatus(state, model.RuntimeRunning))
	b.mutate(model.RunAppendMutation{Run: newRun(state, model.RunRunning, true)})
	b.event(model.EventRunStarted, "", model.AttemptKey{}, "", "run started")
	b.event(model.EventWorkflowResumed, "", model.AttemptKey{}, "", "workflow resumed")
	return b.decision(), nil
}

// decidePlanApproval accepts one exact checked Plan Revision and hash.
// A mismatch is APPROVAL_INPUT_CHANGED with no mutation; a checker pass
// alone never approves (CONTEXT.md: Plan Approval, design 7.3 invariant
// 2).
func decidePlanApproval(state model.State, in model.PlanApprovalInput) (model.Decision, error) {
	if state.Plan == nil {
		return model.Decision{}, model.InvalidInputFault("no plan to approve")
	}
	if state.Plan.Status != model.PlanChecked {
		return model.Decision{}, model.InvalidInputFault("plan must be CHECKED before approval")
	}
	if state.Workflow.Stage != model.StagePlanCheck {
		return model.Decision{}, model.InvalidInputFault("plan approval requires the PLAN_CHECK stage")
	}
	if state.Plan.Artifact != in.PlanRef || state.Plan.Hash != in.Hash {
		return model.Decision{}, model.NewFault(model.CodeApprovalInputChanged, "plan revision or hash changed since it was checked")
	}
	b := &builder{state: state}
	b.mutate(model.PlanMutation{Status: model.PlanApproved})
	b.mutate(model.ApprovalAppendMutation{Approval: model.Approval{
		ID:   model.ApprovalID(fmt.Sprintf("approval-%d", len(state.Approvals)+1)),
		Kind: model.ApprovalPlan,
		Seq:  state.NextEventSeq,
		Refs: []model.ArtifactRef{in.PlanRef},
	}})
	b.mutate(wfMut(state, model.StageSpecGeneration, state.Workflow.Runtime, state.Workflow.CancelIntent))
	b.event(model.EventPlanApproved, "", model.AttemptKey{}, "", "plan approved")
	b.event(model.EventStageChanged, "", model.AttemptKey{}, "", "stage changed to SPEC_GENERATION")
	return b.decision(), nil
}

// decideExecutionApproval accepts one exact set of execution Artifacts,
// routing, budgets, and commit-policy facts. Every input hash must match
// the active ExecutionFacts; any mismatch is APPROVAL_INPUT_CHANGED and
// keeps the Workflow paused for a regenerated preview (PRD 约束 34,
// design 7.3 invariant 3).
func decideExecutionApproval(state model.State, in model.ExecutionApprovalInput) (model.Decision, error) {
	facts := state.Workflow.ExecutionFacts
	if facts == nil {
		return model.Decision{}, model.InvalidInputFault("no execution facts awaiting approval")
	}
	if state.Workflow.Stage != model.StageWorkflowGeneration {
		return model.Decision{}, model.InvalidInputFault("execution approval requires the WORKFLOW_GENERATION stage")
	}
	if !facts.Matches(in.PlanHash, in.SpecHashes, in.CatalogHash, in.WorkflowHash, in.RoutingHash, in.BudgetHash, in.CommitPolicyHash) {
		return model.Decision{}, model.NewFault(model.CodeApprovalInputChanged, "execution facts changed since the approval preview")
	}
	b := &builder{state: state}
	b.mutate(model.ApprovalAppendMutation{Approval: model.Approval{
		ID:          model.ApprovalID(fmt.Sprintf("approval-%d", len(state.Approvals)+1)),
		Kind:        model.ApprovalExecution,
		Seq:         state.NextEventSeq,
		Refs:        []model.ArtifactRef{{Workflow: state.Workflow.ID, Type: model.ArtifactWorkflow, Revision: 1, Hash: in.WorkflowHash}},
		Fingerprint: facts.Fingerprint,
	}})
	b.mutate(wfMut(state, model.StageExecution, state.Workflow.Runtime, state.Workflow.CancelIntent))
	b.event(model.EventExecutionApproved, "", model.AttemptKey{}, "", "execution approved")
	b.event(model.EventStageChanged, "", model.AttemptKey{}, "", "stage changed to EXECUTION")
	return b.decision(), nil
}
