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
	// The Planning Snapshot Worktree is created at the recorded Base
	// Commit; the expected HEAD is fixed before the Effect (design 15.2,
	// 6.2 rule 6). The user's working tree is never touched.
	b.effect(model.PlanningWorktreeCreateIntent{Workflow: in.Workflow, BaseCommit: in.BaseCommit})
	return b.decision(), nil
}

// decideDiscussRequirement starts one requirement discussion turn (PRD
// 需求讨论交互): the Session is appended and the Provider run requested
// in one Decision. The first turn starts the Workflow from PENDING; a
// paused Workflow resumes for the turn. Every turn joins the discussion
// Session lineage: the new Session supersedes the latest planning
// Session.
func decideDiscussRequirement(state model.State, in model.DiscussRequirementInput) (model.Decision, error) {
	if state.Workflow.ID == "" {
		return model.Decision{}, model.InvalidInputFault("no workflow to discuss")
	}
	if state.Workflow.Stage != model.StageRequirementDiscussion {
		return model.Decision{}, model.InvalidInputFault("requirement discussion requires the REQUIREMENT_DISCUSSION stage")
	}
	if !planningRuntimeAllowed(state.Workflow.Runtime) {
		return model.Decision{}, model.InvalidInputFault("workflow cannot discuss from " + string(state.Workflow.Runtime))
	}
	if in.Text == "" || len(in.Text) > maxTurnText {
		return model.Decision{}, model.InvalidInputFault("requirement turn text must be non-empty and bounded")
	}
	if err := validateProvider(in.Provider); err != nil {
		return model.Decision{}, err
	}
	if err := validateFreshSession(state, in.Session); err != nil {
		return model.Decision{}, err
	}
	b := &builder{state: state}
	startIfNeeded(b, state)
	parent := latestPlanningSession(state)
	b.mutate(model.SessionAppendMutation{Session: model.Session{
		ID:         in.Session,
		Purpose:    model.PurposePlanning,
		Status:     model.SessionStarting,
		Supersedes: supersedesOf(parent),
	}, Provider: in.Provider})
	b.effect(model.ProviderStartIntent{
		Session:    in.Session,
		Purpose:    model.PurposePlanning,
		Route:      in.Provider,
		Supersedes: providerSessionOf(parent),
	})
	return b.decision(), nil
}

// decideGeneratePlan is the /finish transition (PRD Plan 生成): the
// Workflow moves to PLAN_GENERATION and a planner Session produces a new
// immutable Plan Revision. Re-generation after needs_revision or after an
// Approval (the user-driven adjustment loop) is the same Decision: the
// new Revision supersedes the old.
func decideGeneratePlan(state model.State, in model.GeneratePlanInput) (model.Decision, error) {
	if state.Workflow.ID == "" {
		return model.Decision{}, model.InvalidInputFault("no workflow to generate a plan for")
	}
	switch state.Workflow.Stage {
	case model.StageRequirementDiscussion, model.StagePlanGeneration,
		model.StagePlanCheck, model.StageSpecGeneration:
	default:
		return model.Decision{}, model.InvalidInputFault("plan generation is not possible from stage " + string(state.Workflow.Stage))
	}
	if !planningRuntimeAllowed(state.Workflow.Runtime) {
		return model.Decision{}, model.InvalidInputFault("workflow cannot generate a plan from " + string(state.Workflow.Runtime))
	}
	if err := validateProvider(in.Provider); err != nil {
		return model.Decision{}, err
	}
	if err := validateFreshSession(state, in.Session); err != nil {
		return model.Decision{}, err
	}
	b := &builder{state: state}
	// One WorkflowMutation carries the stage change and the started
	// runtime together: a second mutation would overwrite one of them.
	rt := state.Workflow.Runtime
	switch rt {
	case model.RuntimePending:
		b.mutate(model.RunAppendMutation{Run: newRun(state, model.RunRunning, true)})
		b.event(model.EventRunStarted, "", model.AttemptKey{}, "", "run started")
		b.event(model.EventWorkflowStarted, "", model.AttemptKey{}, "", "workflow started")
		rt = model.RuntimeRunning
	case model.RuntimePaused:
		b.mutate(model.RunAppendMutation{Run: newRun(state, model.RunRunning, true)})
		b.event(model.EventRunStarted, "", model.AttemptKey{}, "", "run started")
		b.event(model.EventWorkflowResumed, "", model.AttemptKey{}, "", "workflow resumed")
		rt = model.RuntimeRunning
	}
	b.mutate(wfMut(state, model.StagePlanGeneration, rt, state.Workflow.CancelIntent))
	b.event(model.EventStageChanged, "", model.AttemptKey{}, "", "stage changed to PLAN_GENERATION")
	parent := latestPlanningSession(state)
	b.mutate(model.SessionAppendMutation{Session: model.Session{
		ID:         in.Session,
		Purpose:    model.PurposePlanning,
		Status:     model.SessionStarting,
		Supersedes: supersedesOf(parent),
	}, Provider: in.Provider})
	b.effect(model.ProviderStartIntent{
		Session:    in.Session,
		Purpose:    model.PurposePlanning,
		Route:      in.Provider,
		Supersedes: providerSessionOf(parent),
	})
	return b.decision(), nil
}

// decideCheckPlan starts an independent plan-check Session (PRD Plan
// Check 交互). The Checker is a fresh Session with the plan-check
// purpose — never the Planner's Session (design 14.4, 7.3 invariant 2) —
// and the Plan moves DRAFT→CHECKING. A paused Workflow resumes for the
// check.
func decideCheckPlan(state model.State, in model.CheckPlanInput) (model.Decision, error) {
	if state.Workflow.ID == "" {
		return model.Decision{}, model.InvalidInputFault("no workflow to check")
	}
	if state.Workflow.Stage != model.StagePlanCheck {
		return model.Decision{}, model.InvalidInputFault("plan check requires the PLAN_CHECK stage")
	}
	if state.Plan == nil {
		return model.Decision{}, model.InvalidInputFault("no plan to check")
	}
	if state.Plan.Status != model.PlanDraft {
		return model.Decision{}, model.InvalidInputFault("plan must be DRAFT before check")
	}
	if !planningRuntimeAllowed(state.Workflow.Runtime) {
		return model.Decision{}, model.InvalidInputFault("workflow cannot check from " + string(state.Workflow.Runtime))
	}
	if err := validateProvider(in.Provider); err != nil {
		return model.Decision{}, err
	}
	if err := validateFreshSession(state, in.Session); err != nil {
		return model.Decision{}, err
	}
	b := &builder{state: state}
	b.mutate(model.PlanMutation{Status: model.PlanChecking})
	startIfNeeded(b, state)
	// The Checker never supersedes the Planner: plan-check is a different
	// role lineage, and a successor Session must keep the superseded
	// Session's purpose (the Runtime rejects any cross-purpose chain).
	b.mutate(model.SessionAppendMutation{Session: model.Session{
		ID:      in.Session,
		Purpose: model.PurposePlanCheck,
		Status:  model.SessionStarting,
	}, Provider: in.Provider})
	b.effect(model.ProviderStartIntent{
		Session: in.Session,
		Purpose: model.PurposePlanCheck,
		Route:   in.Provider,
	})
	return b.decision(), nil
}

// startIfNeeded opens the dispatch run for a planning command: the first
// command starts the Workflow from PENDING, a paused Workflow resumes.
func startIfNeeded(b *builder, state model.State) {
	switch state.Workflow.Runtime {
	case model.RuntimePending:
		b.mutate(wfMutStatus(state, model.RuntimeRunning))
		b.mutate(model.RunAppendMutation{Run: newRun(state, model.RunRunning, true)})
		b.event(model.EventRunStarted, "", model.AttemptKey{}, "", "run started")
		b.event(model.EventWorkflowStarted, "", model.AttemptKey{}, "", "workflow started")
	case model.RuntimePaused:
		b.mutate(wfMutStatus(state, model.RuntimeRunning))
		b.mutate(model.RunAppendMutation{Run: newRun(state, model.RunRunning, true)})
		b.event(model.EventRunStarted, "", model.AttemptKey{}, "", "run started")
		b.event(model.EventWorkflowResumed, "", model.AttemptKey{}, "", "workflow resumed")
	}
}

// planningRuntimeAllowed reports whether a planning Session may run: the
// Workflow must be able to open dispatch (PENDING, RUNNING, or PAUSED).
func planningRuntimeAllowed(r model.RuntimeStatus) bool {
	switch r {
	case model.RuntimePending, model.RuntimeRunning, model.RuntimePaused:
		return true
	}
	return false
}

// validateFreshSession rejects a Session identity the aggregate already
// carries: planning Sessions are allocated by the Application and fixed
// before the Effect (design 6.2 rule 6).
func validateFreshSession(state model.State, id model.SessionID) error {
	if !id.Valid() {
		return model.InvalidInputFault("a session identity is required")
	}
	for _, s := range state.Sessions {
		if s.ID == id {
			return model.InvalidInputFault("session identity is already in use")
		}
	}
	return nil
}

func validateProvider(provider string) error {
	if provider == "" || len(provider) > 128 {
		return model.InvalidInputFault("provider is required and bounded")
	}
	return nil
}

// latestPlanningSession is the lineage anchor: the most recent planning
// Session (a discussion turn or a plan generation).
func latestPlanningSession(state model.State) *model.Session {
	for i := len(state.Sessions) - 1; i >= 0; i-- {
		if state.Sessions[i].Purpose == model.PurposePlanning {
			return &state.Sessions[i]
		}
	}
	return nil
}

func supersedesOf(s *model.Session) model.SessionID {
	if s == nil {
		return ""
	}
	return s.ID
}

func providerSessionOf(s *model.Session) string {
	if s == nil {
		return ""
	}
	return s.ProviderSessionID
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
// 2). The compare-and-swap compares the user's exact input first: a
// revision or hash that no longer matches the active Plan is
// APPROVAL_INPUT_CHANGED regardless of the Plan's review status (the
// prior Approval is invalidated by any revision change).
func decidePlanApproval(state model.State, in model.PlanApprovalInput) (model.Decision, error) {
	if state.Plan == nil {
		return model.Decision{}, model.InvalidInputFault("no plan to approve")
	}
	if state.Plan.Artifact != in.PlanRef || state.Plan.Hash != in.Hash {
		return model.Decision{}, model.NewFault(model.CodeApprovalInputChanged, "plan revision or hash changed since it was checked")
	}
	if state.Plan.Status != model.PlanChecked {
		return model.Decision{}, model.InvalidInputFault("plan must be CHECKED before approval")
	}
	if state.Workflow.Stage != model.StagePlanCheck {
		return model.Decision{}, model.InvalidInputFault("plan approval requires the PLAN_CHECK stage")
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
