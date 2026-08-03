package decision

import (
	"fmt"

	"cflow.local/cflow/internal/model"
)

// decideComplete records the Workflow's completion (Task 18, PRD 最终验
// 收: 生成最终报告，Workflow Completed). Completion requires the exact
// Integration Commit evidence the independent Final Reviewer bound: the
// FINAL_VERIFICATION stage, every Node SUCCEEDED, no Blocking Finding,
// and the current Integration HEAD still equal to the head the Final
// Verify Attempt verified (EVIDENCE_SUBJECT_CHANGED with no mutation
// otherwise). It records COMPLETED/SUCCEEDED WITHOUT changing the Target
// Branch: the mutation carries the recorded Target Branch, Integration
// Branch, and Integration HEAD untouched.
func decideComplete(state model.State, in model.CompleteWorkflowInput) (model.Decision, error) {
	if state.Workflow.ID == "" {
		return model.Decision{}, model.InvalidInputFault("no workflow to complete")
	}
	if state.Workflow.Stage != model.StageFinalVerification {
		return model.Decision{}, model.InvalidInputFault("completion requires the FINAL_VERIFICATION stage")
	}
	if state.Workflow.Runtime != model.RuntimeRunning {
		return model.Decision{}, model.InvalidInputFault("completion requires a running workflow")
	}
	if hasBlockingFinding(state) {
		return model.Decision{}, model.InvalidInputFault("a blocking finding prevents completion")
	}
	// Every Node of the delivery chain must be SUCCEEDED; no Node may
	// remain PENDING, READY, RUNNING, or FAILED at completion.
	var finalVerify *model.Node
	for _, n := range state.Nodes {
		if n.Status != model.NodeSucceeded {
			return model.Decision{}, model.InvalidInputFault(
				"completion requires every node succeeded; " + string(n.ID) + " is " + string(n.Status))
		}
		if n.Kind == model.NodeFinalVerify {
			finalVerify = n
		}
	}
	if finalVerify == nil {
		return model.Decision{}, model.InvariantFault(fmt.Errorf("completion requires a final verify node"))
	}
	fv := succeededAttemptOf(state, finalVerify.ID)
	if fv == nil || len(fv.Evidence) == 0 {
		return model.Decision{}, model.InvalidInputFault(
			"completion requires the succeeded final verify attempt with evidence")
	}
	// The exact Integration Commit evidence: the head the Final Reviewer
	// bound must still be the current Integration HEAD (design 16.2:
	// completion is bound to the evidence subject it verified).
	if fv.StartHead == "" || fv.StartHead != state.Workflow.IntegrationHead {
		return model.Decision{}, model.NewFault(model.CodeEvidenceSubjectChanged,
			"the integration head moved after the final review; completion requires the exact verified head")
	}
	// Every Merge Node's succeeded Attempt carries commit evidence on the
	// recorded Integration Branch: the delivery chain's evidence subjects
	// are exact.
	for _, n := range state.Nodes {
		if n.Kind != model.NodeMerge {
			continue
		}
		ma := succeededAttemptOf(state, n.ID)
		if ma == nil {
			return model.Decision{}, model.InvalidInputFault(
				"completion requires a succeeded merge attempt for " + string(n.ID))
		}
		if !evidenceOn(ma.Evidence, model.EvidenceCommit, state.Workflow.IntegrationBranch) {
			return model.Decision{}, model.NewFault(model.CodeEvidenceSubjectChanged,
				"a merge attempt's commit evidence subject changed; completion requires the exact verified subjects")
		}
	}
	b := &builder{state: state}
	// Completion never changes the Target Branch: wfMut carries the
	// recorded Target Branch, Integration Branch, and Integration HEAD
	// untouched into the terminal COMPLETED/SUCCEEDED record.
	b.mutate(wfMut(state, model.StageCompleted, model.RuntimeSucceeded, nil))
	b.event(model.EventWorkflowSucceeded, "", model.AttemptKey{}, "", "workflow completed")
	if run := activeRun(state); run != nil && !run.Status.IsTerminal() {
		b.mutate(model.RunMutation{ID: run.ID, Status: model.RunSucceeded, DispatchGate: false})
		b.event(model.EventRunSucceeded, "", model.AttemptKey{}, "", "run succeeded")
	}
	return b.decision(), nil
}

// evidenceOn reports whether one evidence list carries a reference of the
// exact kind and subject.
func evidenceOn(list []model.EvidenceRef, kind model.EvidenceKind, subject string) bool {
	for _, e := range list {
		if e.Kind == kind && e.Subject == subject {
			return true
		}
	}
	return false
}

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
		closePriorRuns(b, state)
		b.mutate(model.RunAppendMutation{Run: newRun(state, model.RunRunning, true)})
		b.event(model.EventRunStarted, "", model.AttemptKey{}, "", "run started")
		b.event(model.EventWorkflowStarted, "", model.AttemptKey{}, "", "workflow started")
		rt = model.RuntimeRunning
	case model.RuntimePaused:
		closePriorRuns(b, state)
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
		closePriorRuns(b, state)
		b.mutate(model.RunAppendMutation{Run: newRun(state, model.RunRunning, true)})
		b.event(model.EventRunStarted, "", model.AttemptKey{}, "", "run started")
		b.event(model.EventWorkflowStarted, "", model.AttemptKey{}, "", "workflow started")
	case model.RuntimePaused:
		b.mutate(wfMutStatus(state, model.RuntimeRunning))
		closePriorRuns(b, state)
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
	closePriorRuns(b, state)
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
	run := activeRun(state)
	if hasBlockingFinding(state) || (run != nil && run.Status == model.RunQuiescing) {
		// A pause during Quiescing or with a blocking Finding converges to
		// BLOCKED — Ctrl+C never clears the original Finding (PRD 已确认：
		// 并行失败后的 Quiescing rule 6).
		b.mutate(wfMutStatus(state, model.RuntimeBlocked))
		b.event(model.EventWorkflowBlocked, "", model.AttemptKey{}, "", "workflow blocked")
	} else {
		b.mutate(wfMutStatus(state, model.RuntimePaused))
		b.event(model.EventWorkflowPaused, "", model.AttemptKey{}, "", "workflow paused")
	}
	transitioned := false
	if run != nil && !run.Status.IsTerminal() {
		if run.Status != model.RunStopping {
			// The controlled stop intent: dispatch closes and the managed
			// processes are stopped through the two-phase protocol.
			b.mutate(model.RunMutation{ID: run.ID, Status: model.RunStopping, DispatchGate: false, StopReason: model.CodeUserInterrupted})
			b.event(model.EventControlledStopRequested, "", model.AttemptKey{}, "", "stop requested")
			b.event(model.EventRunStopped, "", model.AttemptKey{}, "", "run stopping")
			stopRunningProcesses(b, state)
			transitioned = true
		}
	}
	// The stop may already be complete (no processes, no attempts in
	// flight): the Run converges INTERRUPTED in the same transaction.
	convergeStopping(b, state, model.AttemptKey{}, "", transitioned)
	return b.decision(), nil
}

// decideResume reopens dispatch with a new Run record; a resume never
// resurrects a failed Node or reopens a terminal Attempt (PRD 状态机与持久
// 化模型). A Run still mid-stop or mid-quiesce never resumes — Recovery
// settles the persisted stop first — and a pending Commit Policy
// confirmation must be bound before any commit-capable action resumes
// (PRD 已确认：执行期间 Commit Policy 漂移确认 step 7: resume must re-verify
// the fingerprint and must not skip the confirmation across a restart).
func decideResume(state model.State, in model.WorkflowCommandInput) (model.Decision, error) {
	if state.Workflow.ID == "" {
		return model.Decision{}, model.InvalidInputFault("no workflow to resume")
	}
	if state.Workflow.Runtime != model.RuntimePaused && state.Workflow.Runtime != model.RuntimeBlocked {
		return model.Decision{}, model.InvalidInputFault("workflow can only resume from PAUSED or BLOCKED")
	}
	if run := activeRun(state); run != nil &&
		(run.Status == model.RunStopping || run.Status == model.RunQuiescing) {
		return model.Decision{}, model.InvalidInputFault(
			"a stop or quiesce is still settling; resume is refused until it completes")
	}
	// The Commit Policy confirmation gate guards commit-capable execution
	// (PRD 已确认：执行期间 Commit Policy 漂移确认 steps 2 and 7): at
	// EXECUTION the latest Preflight Revision must be bound by an approval
	// before a resume may reopen dispatch. The pre-execution approval gates
	// (e.g. the paused Dry Run at WORKFLOW_GENERATION) resume normally.
	if state.Workflow.Stage == model.StageExecution && !policyConfirmed(state) {
		return model.Decision{}, model.NewFault(model.CodeCommitPolicyConfirmationRequired,
			"the exact new commit policy must be confirmed before resume")
	}
	b := &builder{state: state}
	b.mutate(wfMutStatus(state, model.RuntimeRunning))
	closePriorRuns(b, state)
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
// design 7.3 invariant 3). Only the paused Dry Run gate may be approved;
// the single append-only EXECUTION row binds every execution Artifact
// Revision and Hash together with the Commit Preflight Fingerprint, and
// only then is the Integration Worktree creation requested (PRD Worktree
// 策略: the Integration Ref is withheld until the Execution Approval).
func decideExecutionApproval(state model.State, in model.ExecutionApprovalInput) (model.Decision, error) {
	facts := state.Workflow.ExecutionFacts
	if facts == nil {
		return model.Decision{}, model.InvalidInputFault("no execution facts awaiting approval")
	}
	if state.Workflow.Stage != model.StageWorkflowGeneration {
		return model.Decision{}, model.InvalidInputFault("execution approval requires the WORKFLOW_GENERATION stage")
	}
	if state.Workflow.Runtime != model.RuntimePaused {
		return model.Decision{}, model.InvalidInputFault("execution approval requires the paused Dry Run gate")
	}
	if state.Plan == nil {
		return model.Decision{}, model.InvalidInputFault("execution approval requires the approved plan reference")
	}
	if !facts.Matches(in.PlanHash, in.SpecHashes, in.CatalogHash, in.WorkflowHash, in.RoutingHash, in.BudgetHash, in.CommitPolicyHash) {
		return model.Decision{}, model.NewFault(model.CodeApprovalInputChanged, "execution facts changed since the approval preview")
	}
	b := &builder{state: state}
	b.mutate(model.ApprovalAppendMutation{Approval: model.Approval{
		ID:   model.ApprovalID(fmt.Sprintf("approval-%d", len(state.Approvals)+1)),
		Kind: model.ApprovalExecution,
		Seq:  state.NextEventSeq,
		Refs: []model.ArtifactRef{
			{Workflow: state.Workflow.ID, Type: model.ArtifactPlan, Revision: state.Plan.Revision, Hash: facts.PlanHash},
			{Workflow: state.Workflow.ID, Type: model.ArtifactSpec, Revision: facts.SpecRevision, Hash: facts.SpecHashes[0]},
			{Workflow: state.Workflow.ID, Type: model.ArtifactCatalog, Revision: facts.CatalogRevision, Hash: facts.CatalogHash},
			{Workflow: state.Workflow.ID, Type: model.ArtifactWorkflow, Revision: facts.WorkflowRevision, Hash: facts.WorkflowHash},
		},
		Fingerprint:       facts.Fingerprint,
		PreflightRevision: facts.PreflightRevision,
	}})
	// The approval is the workflow's entry into EXECUTION: dispatch opens
	// with a fresh Run (closing every prior gate run) and the deterministic
	// Integration Branch is recorded.
	closePriorRuns(b, state)
	b.mutate(model.RunAppendMutation{Run: newRun(state, model.RunRunning, true)})
	b.mutate(wfWithIntegration(state, model.StageExecution, model.RuntimeRunning, integrationBranch(state.Workflow.ID)))
	b.event(model.EventRunStarted, "", model.AttemptKey{}, "", "run started")
	b.event(model.EventWorkflowResumed, "", model.AttemptKey{}, "", "workflow resumed into execution")
	b.event(model.EventExecutionApproved, "", model.AttemptKey{}, "", "execution approved")
	b.event(model.EventStageChanged, "", model.AttemptKey{}, "", "stage changed to EXECUTION")
	b.effect(model.IntegrationWorktreeCreateIntent{
		Workflow:   state.Workflow.ID,
		BaseCommit: state.Workflow.BaseCommit,
	})
	return b.decision(), nil
}

// integrationBranch is the deterministic CFlow-owned Integration Branch
// name (PRD Worktree 策略; the same derivation the workflow.yaml
// manifest records).
func integrationBranch(wf model.WorkflowID) string {
	return "cflow/" + string(wf) + "/integration"
}

// wfWithIntegration builds a WorkflowMutation carrying the Integration
// Branch (the first mutation that knows the workflow identity).
func wfWithIntegration(state model.State, stage model.WorkflowStage, rt model.RuntimeStatus, branch string) model.WorkflowMutation {
	m := wfMut(state, stage, rt, state.Workflow.CancelIntent)
	m.IntegrationBranch = branch
	return m
}

// ---------------------------------------------------------------------------
// Spec generation, Workflow compilation, and the Execution Dry Run gate
// ---------------------------------------------------------------------------

// decideSpecGeneration records the Runtime-assembled Verification
// Catalog Revision and starts the Spec Generation Session (PRD Agent
// 角色: SPEC_GENERATION splits the approved Plan into Specs that may
// reference existing command ids). The Catalog body was written by the
// Runtime directly (PRD 已确认：Workflow-local Verification Command
// Catalog: CFlow assembles the immutable Catalog Revision; the Kernel
// records its reference and binds the Session that may use it).
// Regeneration from WORKFLOW_GENERATION (the adjustment loop) records a
// successor Catalog Revision and re-opens the workflow for the Session.
func decideSpecGeneration(state model.State, in model.SpecGenerationInput) (model.Decision, error) {
	if state.Workflow.ID == "" {
		return model.Decision{}, model.InvalidInputFault("no workflow to generate specs for")
	}
	switch state.Workflow.Stage {
	case model.StageSpecGeneration, model.StageWorkflowGeneration:
	default:
		return model.Decision{}, model.InvalidInputFault("spec generation is not possible from stage " + string(state.Workflow.Stage))
	}
	if !planningRuntimeAllowed(state.Workflow.Runtime) {
		return model.Decision{}, model.InvalidInputFault("workflow cannot generate specs from " + string(state.Workflow.Runtime))
	}
	if err := validateProvider(in.Provider); err != nil {
		return model.Decision{}, err
	}
	if err := validateFreshSession(state, in.Session); err != nil {
		return model.Decision{}, err
	}
	ref := in.CatalogRef
	if ref.Type != model.ArtifactCatalog || ref.Revision < 1 || ref.Hash == "" {
		return model.Decision{}, model.InvalidInputFault("the verification catalog revision is required")
	}
	b := &builder{state: state}
	b.mutate(model.ArtifactRefMutation{
		Type: ref.Type, Revision: ref.Revision, Path: artifactRefPath(ref), Hash: ref.Hash,
	})
	startIfNeeded(b, state)
	b.mutate(model.SessionAppendMutation{Session: model.Session{
		ID:      in.Session,
		Purpose: model.PurposeSpecGeneration,
		Status:  model.SessionStarting,
	}, Provider: in.Provider})
	b.effect(model.ProviderStartIntent{
		Session: in.Session,
		Purpose: model.PurposeSpecGeneration,
		Route:   in.Provider,
	})
	return b.decision(), nil
}

// decideWorkflowCompilation starts the Workflow Optimization Session:
// the independent scheduling Agent proposes a restricted Patch IR the
// Compiler validates against the deterministic skeleton (PRD Agent 角色:
// WORKFLOW_OPTIMIZATION).
func decideWorkflowCompilation(state model.State, in model.WorkflowCompilationInput) (model.Decision, error) {
	if state.Workflow.ID == "" {
		return model.Decision{}, model.InvalidInputFault("no workflow to compile")
	}
	if state.Workflow.Stage != model.StageWorkflowGeneration {
		return model.Decision{}, model.InvalidInputFault("workflow compilation requires the WORKFLOW_GENERATION stage")
	}
	if !planningRuntimeAllowed(state.Workflow.Runtime) {
		return model.Decision{}, model.InvalidInputFault("workflow cannot compile from " + string(state.Workflow.Runtime))
	}
	if err := validateProvider(in.Provider); err != nil {
		return model.Decision{}, err
	}
	if err := validateFreshSession(state, in.Session); err != nil {
		return model.Decision{}, err
	}
	b := &builder{state: state}
	b.mutate(model.SessionAppendMutation{Session: model.Session{
		ID:      in.Session,
		Purpose: model.PurposeWorkflowOptimization,
		Status:  model.SessionStarting,
	}, Provider: in.Provider})
	b.effect(model.ProviderStartIntent{
		Session: in.Session,
		Purpose: model.PurposeWorkflowOptimization,
		Route:   in.Provider,
	})
	return b.decision(), nil
}

// decideExecutionDryRun records the freshly observed Commit Preflight
// Revision and pauses the Workflow at the Execution Approval gate. Every
// execution input must already be bound: the approved Plan, the Specs,
// the Verification Catalog, and the compiled Dynamic Workflow; a
// successful Preflight must exist (PRD 已确认：两个用户批准门 step 2).
func decideExecutionDryRun(state model.State, in model.ExecutionDryRunInput) (model.Decision, error) {
	if state.Workflow.ID == "" {
		return model.Decision{}, model.InvalidInputFault("no workflow for an execution dry run")
	}
	if state.Workflow.Stage != model.StageWorkflowGeneration {
		return model.Decision{}, model.InvalidInputFault("execution dry run requires the WORKFLOW_GENERATION stage")
	}
	if state.Workflow.Runtime != model.RuntimeRunning {
		return model.Decision{}, model.InvalidInputFault("execution dry run requires a running workflow")
	}
	facts := state.Workflow.ExecutionFacts
	if facts == nil || facts.PlanHash == "" || len(facts.SpecHashes) == 0 ||
		facts.CatalogHash == "" || facts.WorkflowHash == "" {
		return model.Decision{}, model.InvalidInputFault("execution inputs are incomplete; generate specs and compile the workflow first")
	}
	p := in.Preflight
	if p.Fingerprint == "" || p.EvidenceHash == "" || (p.ProbeRequired && !p.ProbeSuccess) {
		return model.Decision{}, model.InvalidInputFault("a successful commit preflight is required before the execution dry run")
	}
	// The Kernel assigns the next immutable Preflight Revision from the
	// aggregate; the report Artifact the Application wrote uses the same
	// deterministic derivation. The resolved routing and budget policy
	// Revisions the Application wrote become active references in the
	// same transaction that pauses the gate, so the Execution Approval
	// preview binds their hashes (Task 16, design 20.1).
	b := &builder{state: state}
	b.mutate(model.PreflightRecordMutation{
		Revision:          facts.PreflightRevision + 1,
		RepositoryContext: p.RepositoryContext,
		GitVersion:        p.GitVersion,
		Fingerprint:       p.Fingerprint,
		IdentityJSON:      p.IdentityJSON,
		SigningPolicyJSON: p.SigningPolicyJSON,
		ProbeStatus:       p.ProbeStatus,
		ArtifactPath:      p.ArtifactPath,
		ArtifactHash:      p.EvidenceHash,
	})
	if in.RoutingRef.Hash != "" {
		b.mutate(model.ArtifactRefMutation{
			Type:     model.ArtifactRoutingPolicy,
			Revision: in.RoutingRef.Revision,
			Path:     in.RoutingRef.String(),
			Hash:     in.RoutingRef.Hash,
		})
	}
	if in.BudgetRef.Hash != "" {
		b.mutate(model.ArtifactRefMutation{
			Type:     model.ArtifactBudgetPolicy,
			Revision: in.BudgetRef.Revision,
			Path:     in.BudgetRef.String(),
			Hash:     in.BudgetRef.Hash,
		})
	}
	b.mutate(wfMutStatus(state, model.RuntimePaused))
	b.event(model.EventWorkflowPaused, "", model.AttemptKey{}, "", "workflow paused for execution approval")
	// The gate pause closes the foreground Run (no processes exist at the
	// gate): exactly one Run stays active per foreground execution.
	if run := activeRun(state); run != nil && !run.Status.IsTerminal() {
		b.mutate(model.RunMutation{ID: run.ID, Status: model.RunInterrupted, DispatchGate: false})
		b.event(model.EventRunInterrupted, "", model.AttemptKey{}, "", "run interrupted at the approval gate")
	}
	return b.decision(), nil
}
