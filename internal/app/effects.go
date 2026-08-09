package app

// The typed Effect dispatcher (design 6.3): the closed union of Effect
// Intents is executed by the Runtime after its Intent and expected facts
// committed (design 6.2 rule 2). Results are immutable evidence inputs to
// another Decision; an executor can never mark a Node, Attempt, Run, or
// Workflow successful (design 6.2 rule 5) — it only reports the typed
// facts, and the Kernel decides.
//
// Effects whose full semantics arrive with later tasks (ProviderCancel:
// Task 17; Cleanup*: Task 20) have typed executor stubs that fail closed
// without pretending to run the external operation. A stub firing is an
// invariant failure. The protected Apply (Task 19) executors live in the
// same-package apply.go split.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"cflow.local/cflow/internal/agent"
	"cflow.local/cflow/internal/artifact"
	"cflow.local/cflow/internal/gitflow"
	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/process"
)

// stopWaitBudget bounds one controlled stop's graceful Wait. The full
// two-phase Terminate/ForceKill policy with the 10s plus 2s budget
// arrives with Task 17 (design 13.3).
const stopWaitBudget = 12 * time.Second

// stopGraceBudget is the PRD drain window of the two-phase controlled
// stop (design 13.3: drain valid framed events for up to 10 seconds
// before terminating the process groups).
const stopGraceBudget = 10 * time.Second

// executeEffect runs one committed Effect Intent and returns the typed
// Result evidence. restricted commands may only stop and reconcile
// already managed processes (design 6.1). wf is the command's workflow
// identity, cmd the app Command (creation facts), input the command's
// kernel Input (the executor's prompt and structured-input context), and
// rt the per-command Agent Runtime (nil when the Application has no
// Agent configuration).
func (a *Application) executeEffect(ctx context.Context, intent model.EffectIntent, restricted bool, wf model.WorkflowID, cmd Command, input model.Input, rt *agent.Runtime) (model.EffectResultInput, error) {
	if restricted && !restrictedAllowed(intent) {
		return model.EffectResultInput{}, model.NewFault(model.CodeStateInvariantViolation,
			"restricted safety path cannot execute effect "+effectName(intent))
	}
	switch e := intent.(type) {
	case model.ManagedProcessStopIntent:
		return a.stopManagedProcess(ctx, e)
	case model.ArtifactWriteIntent:
		return a.artifactWrite(ctx, wf, e)
	case model.ProviderStartIntent:
		return a.providerStart(ctx, wf, e, input, rt)
	case model.ProviderResumeIntent:
		return a.providerResume(ctx, wf, e, input, rt)
	case model.NativeBootstrapIntent:
		return a.nativeBootstrap(ctx, wf, e, input, rt)
	case model.ProviderCancelIntent:
		// STUB (Task 17): the two-phase Provider stop protocol.
		return model.EffectResultInput{}, stubEffect(e)
	case model.WorkspaceWorktreeCreateIntent:
		return a.workspaceWorktreeCreate(ctx, e, cmd)
	case model.IntegrationWorktreeCreateIntent:
		return a.integrationWorktreeCreate(ctx, e)
	case model.TaskWorktreeCreateIntent:
		return a.taskWorktreeCreate(ctx, wf, e)
	case model.WorkflowCompileIntent:
		return a.workflowCompile(ctx, wf, e)
	case model.GitCommitInspectIntent:
		// STUB (Task 18): canonical Git commit facts for the report.
		return model.EffectResultInput{}, stubEffect(e)
	case model.GitAuditRefCreateIntent:
		return a.gitAuditRefCreate(ctx, e)
	case model.IntegrationMergeIntent:
		return a.integrationMerge(ctx, wf, e)
	case model.IntegrationRollbackIntent:
		return a.integrationRollback(ctx, wf, e)
	case model.WorkspaceMergeIntent:
		return a.workspaceMerge(ctx, wf, e)
	case model.WorkspaceRollbackIntent:
		return a.workspaceRollback(ctx, wf, e)
	case model.VerificationRunIntent:
		return a.verificationRun(ctx, wf, e)
	case model.ApplyStagingCreateIntent:
		return a.applyStagingCreate(ctx, wf, e, rt)
	case model.ApplyFastForwardIntent:
		return a.applyFastForward(ctx, wf, e)
	case model.CleanupWorktreeRemoveIntent:
		return a.cleanupWorktreeRemove(ctx, wf, e)
	case model.CleanupScratchRemoveIntent:
		return a.cleanupScratchRemove(ctx, wf, e)
	default:
		return model.EffectResultInput{}, model.InvariantFault(fmt.Errorf("unknown effect intent %T", intent))
	}
}

// stopManagedProcess runs the two-phase controlled stop of one managed
// process (design 13.3, PRD 已确认：Ctrl+C 两阶段有限停止): the Adapter
// Cancel (Interrupt) with the 10-second grace drain, Terminate the exact
// process group and wait 2 seconds, then ForceKill and inspect the exact
// PID/start-token identity. The second Ctrl+C (EscalateStop) cancels the
// stop context and skips the remaining grace. A matching identity still
// alive after the force-kill phase is the orphan fact the Result carries;
// the Kernel Blocks the Workflow and quarantines Project mutation. A
// managed Process without a supervised handle (an in-process fake
// Session) settled with its command context and is reported stopped.
func (a *Application) stopManagedProcess(ctx context.Context, intent model.ManagedProcessStopIntent) (model.EffectResultInput, error) {
	// The managed-process maps are written by bindProcess/unbindProcess
	// (a provider-start bind of another concurrent chain, Task 16 live
	// parallelism): the reads take the same lock so a stop effect never
	// races a concurrent map write.
	a.mu.Lock()
	handle, ok := a.procs[intent.Process]
	a.mu.Unlock()
	if !ok {
		// No supervised handle: the Session settled with its context; the
		// Kernel settles the process record from the typed stopped fact.
		return model.EffectResultInput{Kind: model.ProcessStopped, Process: intent.Process}, nil
	}
	// The stop runs on the Application's stop context — independent of the
	// command context (the first Ctrl+C cancels the command so the
	// Sessions abort; the stop itself keeps its grace) and cancelled by
	// the second Ctrl+C escalation.
	stopCtx, _ := a.stopContext(context.Background())
	exit, err := process.Stop(stopCtx, a.supervisor, handle, a.stopPolicy)
	orphan := false
	if err != nil && errors.Is(err, process.ErrNotReaped) {
		// The force-kill phase is over: the exact identity facts decide.
		// The read is guarded like the bind/unbind writes: a concurrent
		// provider-start bind may be rewriting the identity map.
		a.mu.Lock()
		id, ok := a.processIdentities[intent.Process]
		a.mu.Unlock()
		if ok {
			fact, ierr := a.supervisor.Inspect(context.Background(), id)
			if ierr == nil && fact.Running {
				orphan = true
			}
		}
	} else if err != nil {
		return model.EffectResultInput{}, err
	}
	_ = exit
	a.unbindProcess(intent.Process)
	return model.EffectResultInput{Kind: model.ProcessStopped, Process: intent.Process, Orphan: orphan}, nil
}

// restrictedAllowed reports whether one Effect may run on the restricted
// safety path: only stopping already managed processes (design 6.1). The
// path may never start a Provider, allocate a Retry, generate an
// Artifact, Merge, or Apply.
func restrictedAllowed(intent model.EffectIntent) bool {
	switch intent.(type) {
	case model.ManagedProcessStopIntent:
		return true
	}
	return false
}

// validateEffectResult checks the immutable Result evidence against the
// committed Intent before the Result Decision is applied (design 6.2 rule
// 3): the Result must reference exactly the Intent's external target.
func validateEffectResult(intent model.EffectIntent, r model.EffectResultInput) error {
	switch e := intent.(type) {
	case model.ManagedProcessStopIntent:
		if r.Kind != model.ProcessStopped || r.Process != e.Process {
			return model.InvariantFault(fmt.Errorf("process stop result does not match intent for process %s", e.Process))
		}
	case model.WorkspaceWorktreeCreateIntent:
		if r.Kind != model.WorkspaceWorktreeCreated {
			return model.InvariantFault(fmt.Errorf("workspace result does not match its intent"))
		}
	case model.PlanningWorktreeCreateIntent:
		if r.Kind != model.PlanningWorktreeCreated {
			return model.InvariantFault(fmt.Errorf("planning snapshot result does not match its intent"))
		}
	case model.ProviderStartIntent:
		if r.Kind == model.AttemptEnded && r.Outcome == model.OutcomeFailed &&
			(r.FailureCode == model.CodeDirtyWorktreeDrifted || r.FailureCode == model.CodeInterruptedWorktreeDrifted) {
			// The Worktree reuse CAS (PRD 已确认：DIRTY_TASK_WORKTREE 原地
			// Repair; 已确认：Ctrl+C 两阶段有限停止 step 7): the successor's
			// Worktree no longer matches the prior Attempt's end evidence,
			// so the Attempt fails closed BEFORE any Session starts. The
			// result references exactly the Intent's Attempt.
			return nil
		}
		if r.Kind != model.ProviderRunEnded || r.Session.ID != e.Session || r.Session.Purpose != e.Purpose {
			return model.InvariantFault(fmt.Errorf("provider run result does not match intent for session %s", e.Session))
		}
	case model.ProviderResumeIntent:
		if r.Kind == model.AttemptEnded && r.Outcome == model.OutcomeFailed &&
			(r.FailureCode == model.CodeDirtyWorktreeDrifted || r.FailureCode == model.CodeInterruptedWorktreeDrifted) {
			return nil // the Worktree reuse CAS failed the Attempt closed
		}
		if r.Kind != model.ProviderRunEnded || r.Session.ID != e.Session || r.Session.Purpose != e.Purpose {
			return model.InvariantFault(fmt.Errorf("provider resume result does not match intent for session %s", e.Session))
		}
	case model.NativeBootstrapIntent:
		if r.Kind != model.NativeBootstrapEstablished || r.Session.ID != e.Session {
			return model.InvariantFault(fmt.Errorf("native bootstrap result does not match intent for session %s", e.Session))
		}
		if r.Session.ProviderSessionID == "" {
			return model.InvariantFault(fmt.Errorf("native bootstrap result carries no provider session id"))
		}
	case model.ArtifactWriteIntent:
		if r.Kind != model.ArtifactWritten ||
			r.Artifact.Workflow != e.Ref.Workflow || r.Artifact.Type != e.Ref.Type {
			return model.InvariantFault(fmt.Errorf("artifact write result does not match intent for %s", e.Ref))
		}
		if e.Ref.Revision != 0 && r.Artifact.Revision != e.Ref.Revision {
			return model.InvariantFault(fmt.Errorf("artifact write result revision %d does not match intent revision %d",
				r.Artifact.Revision, e.Ref.Revision))
		}
		if r.Artifact.Revision < 1 || r.Artifact.Hash == "" {
			return model.InvariantFault(fmt.Errorf("artifact write result carries an incomplete reference"))
		}
	case model.WorkflowCompileIntent:
		if r.Kind != model.WorkflowCompiled {
			return model.InvariantFault(fmt.Errorf("workflow compilation result does not match its intent"))
		}
	case model.IntegrationWorktreeCreateIntent:
		if r.Kind != model.IntegrationWorktreeCreated || r.IntegrationHead == "" {
			return model.InvariantFault(fmt.Errorf("integration worktree result does not match its intent"))
		}
	case model.TaskWorktreeCreateIntent:
		if r.Kind != model.TaskWorktreeCreated || r.WorktreePath == "" {
			return model.InvariantFault(fmt.Errorf("task worktree result does not match its intent"))
		}
	case model.VerificationRunIntent:
		if r.Kind != model.VerificationRunEnded {
			return model.InvariantFault(fmt.Errorf("verification result does not match its intent"))
		}
	case model.IntegrationMergeIntent:
		if r.Kind != model.IntegrationMerged && r.Kind != model.IntegrationMergeFailed {
			return model.InvariantFault(fmt.Errorf("integration merge result does not match its intent"))
		}
		if r.Kind == model.IntegrationMergeFailed && r.PreMergeHead == "" {
			return model.InvariantFault(fmt.Errorf("integration merge failure carries no pre-merge head"))
		}
	case model.IntegrationRollbackIntent:
		if r.Kind != model.IntegrationRollbacked || r.Attempt != e.Attempt {
			return model.InvariantFault(fmt.Errorf("integration rollback result does not match its intent"))
		}
	case model.WorkspaceMergeIntent:
		if r.Kind != model.WorkspaceMerged && r.Kind != model.WorkspaceMergeFailed {
			return model.InvariantFault(fmt.Errorf("workspace merge result does not match its intent"))
		}
		if r.Kind == model.WorkspaceMergeFailed && r.PreMergeHead == "" {
			return model.InvariantFault(fmt.Errorf("workspace merge failure carries no pre-merge head"))
		}
	case model.WorkspaceRollbackIntent:
		if r.Kind != model.WorkspaceRollbacked || r.Attempt != e.Attempt {
			return model.InvariantFault(fmt.Errorf("workspace rollback result does not match its intent"))
		}
	case model.GitAuditRefCreateIntent:
		if r.Kind != model.GitAuditRefCreated {
			return model.InvariantFault(fmt.Errorf("audit ref result does not match its intent"))
		}
	case model.CleanupWorktreeRemoveIntent, model.CleanupScratchRemoveIntent:
		var att model.CleanupAttemptID
		var index int
		switch e := intent.(type) {
		case model.CleanupWorktreeRemoveIntent:
			att, index = e.Cleanup, e.Item
		case model.CleanupScratchRemoveIntent:
			att, index = e.Cleanup, e.Item
		}
		if (r.Kind != model.CleanupItemRemovedResult && r.Kind != model.CleanupItemFailedResult) ||
			r.CleanupAttempt != att || r.ItemIndex != index {
			return model.InvariantFault(fmt.Errorf("cleanup item result does not match its intent"))
		}
	}
	return nil
}

// intentIdentity is the stable identity of one Effect Intent: two Intents
// are identical when their external target is identical, so a Decision
// requesting the same uncompleted Intent twice is a kernel invariant
// failure (design 6.2).
func intentIdentity(i model.EffectIntent) string {
	return fmt.Sprintf("%T/%v", i, i)
}

func effectName(i model.EffectIntent) string {
	return fmt.Sprintf("%T", i)
}

// stubEffect is the fail-closed outcome of a typed Effect whose full
// semantics arrive with a later task (design 6.3). The stub never
// pretends to run the external operation.
func stubEffect(intent model.EffectIntent) error {
	return model.InvariantFault(fmt.Errorf("effect %s is not executable by this build", effectName(intent)))
}

// ---------------------------------------------------------------------------
// planning executors (design 6.3): GitFlow, Agent Runtime, Artifact Store
// ---------------------------------------------------------------------------

// workspaceWorktreeCreate creates the single long-lived Workspace
// Branch/Worktree at the recorded Base Head, Branch, and Path (design
// 8.1; TUI task 4) and writes the workflow.yaml static identity manifest
// (PRD Workflow 元信息). The user's target branch and working tree are
// never touched.
func (a *Application) workspaceWorktreeCreate(ctx context.Context, intent model.WorkspaceWorktreeCreateIntent, cmd Command) (model.EffectResultInput, error) {
	if a.git == nil {
		return model.EffectResultInput{}, model.InvariantFault(fmt.Errorf("git seam is not configured for this application"))
	}
	create, ok := cmd.(CreateWorkflowCommand)
	if !ok {
		return model.EffectResultInput{}, model.InvariantFault(fmt.Errorf("workspace effect outside workflow creation"))
	}
	if intent.Path == "" || intent.Branch == "" {
		return model.EffectResultInput{}, model.InvariantFault(fmt.Errorf("workspace create intent carries no path or branch"))
	}
	if err := a.ensureWorktreeParent(intent.Path); err != nil {
		return model.EffectResultInput{}, err
	}
	res, err := a.git.Execute(ctx, gitflow.CreateWorkspace{
		Branch:   intent.Branch,
		BaseHead: intent.BaseHead,
		Path:     intent.Path,
	})
	if err != nil {
		return model.EffectResultInput{}, err
	}
	ws, ok := res.(gitflow.WorkspaceWorktreeResult)
	if !ok {
		return model.EffectResultInput{}, model.InvariantFault(fmt.Errorf("workspace result has an unexpected type"))
	}
	if err := a.writeWorkflowManifest(ctx, intent.Workflow, create, ws); err != nil {
		return model.EffectResultInput{}, err
	}
	return model.EffectResultInput{Kind: model.WorkspaceWorktreeCreated}, nil
}

// planningWorktreeCreate creates the Planning Snapshot Worktree fixed at
func (a *Application) planningWorktreeCreate(ctx context.Context, intent model.PlanningWorktreeCreateIntent, cmd Command) (model.EffectResultInput, error) {
	if a.git == nil {
		return model.EffectResultInput{}, model.InvariantFault(fmt.Errorf("git seam is not configured for this application"))
	}
	create, ok := cmd.(CreateWorkflowCommand)
	if !ok {
		return model.EffectResultInput{}, model.InvariantFault(fmt.Errorf("planning snapshot effect outside workflow creation"))
	}
	path := a.planningWorktreePath(intent.Workflow)
	if err := a.ensureWorktreeParent(path); err != nil {
		return model.EffectResultInput{}, err
	}
	res, err := a.git.Execute(ctx, gitflow.CreatePlanningSnapshot{BaseCommit: intent.BaseCommit, Path: path})
	if err != nil {
		return model.EffectResultInput{}, err
	}
	snap, ok := res.(gitflow.PlanningSnapshotResult)
	if !ok {
		return model.EffectResultInput{}, model.InvariantFault(fmt.Errorf("planning snapshot result has an unexpected type"))
	}
	// Legacy Layout (schema 1): no Workspace facts are recorded. New
	// Workflows always carry Workspace facts through
	// WorkspaceWorktreeCreateIntent (Task 4).
	if err := a.writeLegacyWorkflowManifest(ctx, intent.Workflow, create, snap); err != nil {
		return model.EffectResultInput{}, err
	}
	return model.EffectResultInput{Kind: model.PlanningWorktreeCreated}, nil
}

// providerStart runs one Provider Session through the Agent Runtime and
// returns the settled Session facts plus the redacted artifact body the
// run produced. The Planning Snapshot's HEAD and Git-visible state are
// compared before and after every non-coding Session: any change makes
// the output invalid with UNEXPECTED_AGENT_MUTATION (PRD Worktree 策略).
func (a *Application) providerStart(ctx context.Context, wf model.WorkflowID, intent model.ProviderStartIntent, cmd model.Input, rt *agent.Runtime) (model.EffectResultInput, error) {
	if rt == nil {
		return model.EffectResultInput{}, model.InvariantFault(fmt.Errorf("agent runtime is not configured for this application"))
	}
	if intent.Purpose == model.PurposeImplementation {
		return a.codingProviderStart(ctx, wf, intent, cmd, rt)
	}
	if intent.Purpose == model.PurposeReview {
		if intent.Node == "" {
			// The Workspace Adoption Review (TUI task 6, design 8.4): the
			// independent Review of the frozen candidate Change Set inside
			// the Workspace, with no execution Attempt.
			return a.adoptionReviewProviderStart(ctx, wf, intent, cmd, rt)
		}
		// The independent Reviewer Session (design 16.2): a non-coding
		// Session inside the Task Worktree, bound to the exact
		// Commit/Catalog/evidence refs.
		return a.reviewProviderStart(ctx, wf, intent, cmd, rt)
	}
	if intent.Purpose == model.PurposeFinalVerification {
		// The independent Final Reviewer Session (Task 18, PRD 最终验收):
		// a non-coding Session inside the Integration Worktree, bound to
		// the exact Plan/Spec/Catalog/Workflow refs and the Integration
		// HEAD it verifies.
		return a.finalReviewProviderStart(ctx, wf, intent, cmd, rt)
	}
	if intent.Purpose == model.PurposeApplyVerification {
		// The independent Apply Verification Session (PRD 已确认：显式受保
		// 护 Apply step 4): a non-coding Session inside the Apply
		// Worktree, bound to the exact refs and the deterministic apply
		// verification manifest.
		return a.applyReviewProviderStart(ctx, wf, intent, cmd, rt)
	}
	prompt, ok := a.planningPrompt(cmd)
	if !ok {
		return model.EffectResultInput{}, model.InvalidInputFault("no embedded prompt for this planning command")
	}
	cwd, err := a.planningCWD(ctx, wf)
	if err != nil {
		return model.EffectResultInput{}, err
	}
	pre, err := a.observeSnapshot(ctx, cwd)
	if err != nil {
		return model.EffectResultInput{}, err
	}
	input, err := a.sessionInput(ctx, wf, cmd)
	if err != nil {
		return model.EffectResultInput{}, err
	}
	req := agent.StartRequest{
		Purpose:    intent.Purpose,
		Provider:   intent.Route,
		Prompt:     renderPrompt(prompt.Body, input),
		Input:      a.providerTypedInput(ctx, rt, intent.Purpose, intent.Route, input),
		CWD:        cwd,
		SessionID:  intent.Session,
		Supersedes: agent.ProviderSessionID(intent.Supersedes),
	}
	res, err := rt.Start(ctx, req)
	if err != nil {
		return model.EffectResultInput{}, err
	}
	if err := a.verifySnapshotUnchanged(ctx, cwd, pre); err != nil {
		return model.EffectResultInput{}, err
	}
	out, err := a.runOutcome(cmd, res)
	if err != nil {
		return model.EffectResultInput{}, err
	}
	// The Spec Generation Session's proposed commands are validated with
	// the Catalog policy and promoted into the successor immutable
	// Catalog Revision; the Kernel records the reference with the Spec
	// output (PRD 已确认：Workflow-local Verification Command Catalog).
	if intent.Purpose == model.PurposeSpecGeneration && res.Terminal != nil {
		ref, err := a.promoteCatalogProposals(ctx, wf, []byte(res.Terminal.Result), intent.Session)
		if err != nil {
			return model.EffectResultInput{}, err
		}
		out.CatalogRef = ref
	}
	return out, nil
}

// nativeBootstrap runs the managed Provider start/bootstrap of one native
// interactive discussion Session (design §9.1, TUI task 12): the Runtime
// establishes the Provider's own session identity from the validated
// session_started event, and the Result carries that exact identity for the
// Kernel to bind. The bootstrap never uses a CFlow Session id as the
// Provider identity, and it fails closed when the Provider returns no
// session id or the binding drifts.
func (a *Application) nativeBootstrap(ctx context.Context, wf model.WorkflowID, intent model.NativeBootstrapIntent, cmd model.Input, rt *agent.Runtime) (model.EffectResultInput, error) {
	if rt == nil {
		return model.EffectResultInput{}, model.InvariantFault(fmt.Errorf("agent runtime is not configured for this application"))
	}
	prompt, ok := a.planningPrompt(cmd)
	if !ok {
		return model.EffectResultInput{}, model.InvalidInputFault("no embedded prompt for the discussion bootstrap")
	}
	cwd, err := a.planningCWD(ctx, wf)
	if err != nil {
		return model.EffectResultInput{}, err
	}
	pre, err := a.observeSnapshot(ctx, cwd)
	if err != nil {
		return model.EffectResultInput{}, err
	}
	input, err := a.sessionInput(ctx, wf, cmd)
	if err != nil {
		return model.EffectResultInput{}, err
	}
	// The switch successor's bootstrap reads the superseded discussion's
	// immutable Context Bundle (design §9.4, TUI task 12): the bundle content
	// rides the managed bootstrap input so the successor Provider starts
	// with the prior discussion context. A missing bundle fails closed.
	if sw, ok := cmd.(model.SwitchAgentInput); ok && sw.Supersedes != "" {
		bundle, ok := rt.FallbackBundle(sw.Supersedes)
		if !ok {
			return model.EffectResultInput{}, model.NewFault(model.CodeStateInvariantViolation,
				"the switch successor's context bundle is not readable")
		}
		input = attachBundleInput(input, &bundle)
	}
	res, err := rt.Bootstrap(ctx, agent.BootstrapRequest{
		Purpose: intent.Purpose, Provider: intent.Route,
		Prompt: renderPrompt(prompt.Body, input), Input: input,
		CWD: cwd, SessionID: intent.Session,
	})
	if err != nil {
		return model.EffectResultInput{}, err
	}
	if err := a.verifySnapshotUnchanged(ctx, cwd, pre); err != nil {
		return model.EffectResultInput{}, err
	}
	return model.EffectResultInput{
		Kind:    model.NativeBootstrapEstablished,
		Session: res.Session,
	}, nil
}

// providerResume re-establishes an existing Provider Session (design
// 14.4, PRD 已确认：Session Resume 失败与跨 Provider 上下文交接). Native
// Resume is attempted only when the exact binding supports Resume (the
// Runtime's per-operation capability gate). An unrecoverable native
// Resume falls back: the original Session is retained as LOST, the
// immutable redacted Context Bundle is persisted, and the successor
// Session is allocated on the approved fallback binding. The executor
// reports the fallback as the typed facts — the LOST original with the
// automatic-execution failure code — never as a success claim; the
// Decision Kernel charges the approved budget from those facts (design
// 14.4 step 5).
func (a *Application) providerResume(ctx context.Context, wf model.WorkflowID, intent model.ProviderResumeIntent, cmd model.Input, rt *agent.Runtime) (model.EffectResultInput, error) {
	if rt == nil {
		return model.EffectResultInput{}, model.InvariantFault(fmt.Errorf("agent runtime is not configured for this application"))
	}
	prompt, ok := a.planningPrompt(cmd)
	if !ok {
		return model.EffectResultInput{}, model.InvalidInputFault("no embedded prompt for this planning command")
	}
	// The Provider of the CFlow Session is the Runtime's ledger fact
	// (the intent carries the CFlow Session, never a provider name); the
	// Resume request re-establishes the Session on that Provider.
	provider, providerSessionID := "", ""
	for _, fact := range rt.Sessions() {
		if fact.Session.ID == intent.Session {
			provider = fact.Provider
			providerSessionID = fact.Session.ProviderSessionID
			break
		}
	}
	if provider == "" || providerSessionID == "" {
		return model.EffectResultInput{}, model.NewFault(model.CodeSessionIndependenceViolation,
			"session is not known to the agent runtime")
	}
	cwd, err := a.planningCWD(ctx, wf)
	if err != nil {
		return model.EffectResultInput{}, err
	}
	pre, err := a.observeSnapshot(ctx, cwd)
	if err != nil {
		return model.EffectResultInput{}, err
	}
	input, err := a.sessionInput(ctx, wf, cmd)
	if err != nil {
		return model.EffectResultInput{}, err
	}
	res, err := rt.Resume(ctx, agent.ResumeRequest{
		ProviderSessionID: agent.ProviderSessionID(providerSessionID),
		Purpose:           intent.Purpose,
		Provider:          provider,
		Prompt:            renderPrompt(prompt.Body, input),
		Input:             input,
		CWD:               cwd,
		Context:           a.resumeContext(ctx, wf, intent.Session, provider),
	})
	if err != nil {
		return model.EffectResultInput{}, err
	}
	if res.Fallback != nil {
		// An unrecoverable native Resume: the original Session is LOST
		// and the successor Session is allocated on the approved fallback
		// binding. The Kernel charges the approved budget from these
		// facts and persists the successor Session with its
		// supersedes_session_id lineage in the same settle Decision
		// (design 14.4 step 5); the automatic-execution failure code is
		// the fact, never a success claim.
		lost := res.Fallback.LostSession
		if lost.Provider == "" {
			lost.Provider = provider
		}
		successor := res.Fallback.SuccessorSession
		if successor.Provider == "" {
			successor.Provider = provider
		}
		return model.EffectResultInput{
			Kind:             model.ProviderRunEnded,
			Session:          lost,
			SuccessorSession: successor,
			FailureCode:      model.CodeAgentProcessCrashed,
		}, nil
	}
	run := res.Run
	if err := a.verifySnapshotUnchanged(ctx, cwd, pre); err != nil {
		return model.EffectResultInput{}, err
	}
	return a.runOutcome(cmd, run)
}

// successorHandoff resolves the lineage facts of one Session start
// (design 14.4): when the Session is the successor of an automatic
// fallback (its ledger record carries Supersedes), it returns the LOST
// original's provider session id — the Supersedes lineage fact of the
// Start request — and the immutable redacted Context Bundle handoff
// (nil when the Session is not a successor or no bundle exists). The
// bundle lives in the evidence root, so a later dispatch pass reads the
// same persisted handoff.
func (a *Application) successorHandoff(rt *agent.Runtime, session model.SessionID) (agent.ProviderSessionID, *agent.ContextBundle) {
	if rt == nil {
		return "", nil
	}
	var lostID model.SessionID
	for _, f := range rt.Sessions() {
		if f.Session.ID != session {
			continue
		}
		lostID = f.Session.Supersedes
		break
	}
	if lostID == "" {
		return "", nil
	}
	lostProviderSessionID := ""
	for _, f := range rt.Sessions() {
		if f.Session.ID == lostID {
			lostProviderSessionID = f.Session.ProviderSessionID
			break
		}
	}
	b, ok := rt.FallbackBundle(lostID)
	if !ok {
		return "", nil
	}
	return agent.ProviderSessionID(lostProviderSessionID), &b
}

// nativeBootstrapInput is the structured input of one native discussion
// Session bootstrap (design §9.1, TUI task 12): for a switch the immutable
// redacted Context Bundle handoff of the superseded Session rides the input
// (the native counterpart of codingSessionInput.ContextBundle), so the
// successor Provider's start reads the prior discussion context. It is never
// a credential or an unredacted transcript.
type nativeBootstrapInput struct {
	Requirement   string               `json:"requirement"`
	ContextBundle *agent.ContextBundle `json:"context_bundle,omitempty"`
}

// attachBundleInput attaches the immutable redacted Context Bundle
// handoff to one Session start input (nil bundle leaves the input
// unchanged). Both the automatic fallback's coding input and the native
// discussion bootstrap input carry the bundle.
func attachBundleInput(input any, bundle *agent.ContextBundle) any {
	if bundle == nil {
		return input
	}
	switch c := input.(type) {
	case *codingSessionInput:
		c.ContextBundle = bundle
	case *nativeBootstrapInput:
		c.ContextBundle = bundle
	}
	return input
}

// resumeContext assembles the Context Bundle input of one unrecoverable
// Resume from the immutable workflow facts (design 14.4): the active
// Plan/Spec/Catalog/Workflow pins and the Provider permission boundary.
// It never copies Provider credentials or an unredacted transcript; the
// Runtime redacts the bundle before it is returned or persisted.
func (a *Application) resumeContext(ctx context.Context, wf model.WorkflowID, session model.SessionID, provider string) agent.ContextInput {
	store, err := a.artifactStore(wf)
	if err != nil {
		return agent.ContextInput{PermissionBoundary: providerTrustBoundary}
	}
	pin := func(typ model.ArtifactType) agent.ArtifactPin {
		if ref, err := store.Resolve(ctx, artifact.ResolveRequest{WorkflowID: wf, Type: typ}); err == nil {
			return agent.ArtifactPin{Type: string(typ), Revision: ref.Revision, Hash: ref.Hash}
		}
		return agent.ArtifactPin{}
	}
	return agent.ContextInput{
		Plan:               pin(model.ArtifactPlan),
		Spec:               pin(model.ArtifactSpec),
		Catalog:            pin(model.ArtifactCatalog),
		Workflow:           pin(model.ArtifactWorkflow),
		PermissionBoundary: providerTrustBoundary,
	}
}

// taskWorktreeCreate creates the Task Branch/Worktree from the recorded
// Task Base (PRD Worktree 策略, design 15.2): the branch must not exist
// and the expected HEAD is the immutable Task Base fixed at readiness, so
// the Task never silently rebases onto a different baseline.
func (a *Application) taskWorktreeCreate(ctx context.Context, wf model.WorkflowID, intent model.TaskWorktreeCreateIntent) (model.EffectResultInput, error) {
	if a.git == nil {
		return model.EffectResultInput{}, model.InvariantFault(fmt.Errorf("git seam is not configured for this application"))
	}
	path, err := a.taskWorktreePath(ctx, wf, intent.Node)
	if err != nil {
		return model.EffectResultInput{}, err
	}
	if err := a.ensureWorktreeParent(path); err != nil {
		return model.EffectResultInput{}, err
	}
	// The creation runs on a context that is not cancelled: an aborted
	// `git worktree add` removes its partial directory, which would lose
	// the Coding Worktree the interruption must preserve (PRD 已确认：
	// Ctrl+C 两阶段有限停止 step 7). The CAS-guarded creation completes;
	// the interrupted pass settles the Attempt INTERRUPTED afterwards.
	createCtx := context.WithoutCancel(ctx)
	res, err := a.git.Execute(createCtx, gitflow.CreateTask{
		Branch:   intent.Branch,
		BaseHead: intent.BaseHead,
		Path:     path,
	})
	if err != nil {
		return model.EffectResultInput{}, err
	}
	tv, ok := res.(gitflow.TaskWorktreeResult)
	if !ok {
		return model.EffectResultInput{}, model.InvariantFault(fmt.Errorf("task worktree result has an unexpected type"))
	}
	return model.EffectResultInput{
		Kind:         model.TaskWorktreeCreated,
		Attempt:      model.AttemptKey{Node: intent.Node},
		WorktreePath: tv.Worktree,
	}, nil
}

// codingProviderStart runs the coding Session of one allocated Task
// inside its Task Worktree. The Coding Agent receives only the approved
// context (the Spec, the Verification Catalog it references, and the
// Worktree facts), and its output can never set lifecycle state: the
// result carries the Runtime-observed Git facts of the Worktree, which
// the Commit gate (Task 13) judges, never the Agent's prose (design 7.3
// invariant 1, PRD Worktree 策略).
func (a *Application) codingProviderStart(ctx context.Context, wf model.WorkflowID, intent model.ProviderStartIntent, cmd model.Input, rt *agent.Runtime) (model.EffectResultInput, error) {
	// Prompts are addressed by Agent Purpose (design 14.5): the coding
	// Session always runs the TASK_IMPLEMENTATION prompt, whatever input
	// the effect loop is feeding.
	prompt, ok := a.promptForPurpose(model.PurposeImplementation)
	if !ok {
		return model.EffectResultInput{}, model.InvalidInputFault("no embedded prompt for the implementation purpose")
	}
	cwd, err := a.taskWorktreePath(ctx, wf, intent.Node)
	if err != nil {
		return model.EffectResultInput{}, err
	}
	// The RUNNING Attempt already committed; only now may the Coding
	// Session start (design 12: an in-memory queued goroutine is not an
	// in-flight Attempt).
	a.probeStep("provider:" + string(intent.Node) + ":start")
	// A pass interruption takes precedence over every Worktree judgment:
	// the Attempt settles INTERRUPTED through the chain's stop protocol,
	// never as a drift failure of an unobservable Worktree.
	if err := ctx.Err(); err != nil {
		return model.EffectResultInput{}, err
	}
	// The successor of a terminal Attempt reuses the exact Task
	// Branch/Worktree only when the Worktree's HEAD, status, and Dirty
	// Fingerprint still match the prior Attempt's end evidence (PRD 已确
	// 认：DIRTY_TASK_WORKTREE 原地 Repair: re-verify before starting repair;
	// 已确认：Ctrl+C 两阶段有限停止 step 7: re-verify before resume). A
	// mismatch fails the Attempt closed — DIRTY_WORKTREE_DRIFTED for a
	// repair successor, INTERRUPTED_WORKTREE_DRIFTED for a resumed
	// interrupted successor — and the Workflow Blocks; the Worktree is
	// never auto-fixed.
	if drift := a.reuseWorktreeDrift(ctx, wf, intent.Node); drift != "" {
		head, dirty := a.driftEndFacts(ctx, wf, intent.Node)
		return model.EffectResultInput{
			Kind:                model.AttemptEnded,
			Attempt:             a.runningAttemptKey(ctx, wf, intent.Node),
			Outcome:             model.OutcomeFailed,
			FailureCode:         model.Code(drift),
			EndHead:             head,
			EndDirtyFingerprint: dirty,
		}, nil
	}
	// The successor Session of an automatic fallback receives the LOST
	// original's immutable redacted Context Bundle in its start input
	// and re-establishes the lineage through Supersedes (design 14.4,
	// PRD 已确认：Session Resume 失败与跨 Provider 上下文交接).
	supersedes, bundle := a.successorHandoff(rt, intent.Session)
	sessionIn, err := a.sessionInput(ctx, wf, cmd)
	if err != nil {
		return model.EffectResultInput{}, err
	}
	res, err := rt.Start(ctx, agent.StartRequest{
		Purpose:  intent.Purpose,
		Provider: intent.Route,
		Prompt:   renderPrompt(prompt.Body, sessionIn),
		Input: a.providerTypedInput(ctx, rt, intent.Purpose, intent.Route,
			attachBundleInput(sessionIn, bundle)),
		CWD:        cwd,
		SessionID:  intent.Session,
		Supersedes: supersedes,
	})
	if err != nil {
		return model.EffectResultInput{}, err
	}
	// Git evidence collection: the Worktree's HEAD and dirty state after
	// the coding Session are the facts the Commit gate judges (PRD
	// Worktree 策略: coding occurs only in the Task Worktree).
	end, err := a.observeSnapshot(ctx, cwd)
	if err != nil {
		return model.EffectResultInput{}, err
	}
	out, err := a.runOutcome(cmd, res)
	if err != nil {
		return model.EffectResultInput{}, err
	}
	out.EndHead = end.Head
	out.EndDirtyFingerprint = dirtyFingerprint(end.Dirty)
	return out, nil
}

// dirtyFingerprint renders the Git-visible dirty fingerprint of one
// working tree ("" when clean).
func dirtyFingerprint(d gitflow.DirtyFingerprint) string {
	if d.Combined == "" {
		return ""
	}
	return "sha256:" + d.Combined
}

// artifactWrite persists one Artifact Revision through the immutable
// Artifact Store (design 10.2): schema validation, redaction, canonical
// serialization, atomic owner-only write, and reader verification. A
// zero intent Revision asks the executor to assign the type's next
// Revision (the aggregate does not track every type's counter).
func (a *Application) artifactWrite(ctx context.Context, wf model.WorkflowID, intent model.ArtifactWriteIntent) (model.EffectResultInput, error) {
	store, err := a.artifactStore(wf)
	if err != nil {
		return model.EffectResultInput{}, err
	}
	revision := intent.Ref.Revision
	if revision == 0 {
		latest, err := store.Resolve(ctx, artifact.ResolveRequest{WorkflowID: wf, Type: intent.Ref.Type})
		if err == nil {
			revision = latest.Revision + 1
		} else if code, ok := model.CodeOf(err); !ok || code != model.CodeInvalidInput {
			return model.EffectResultInput{}, err
		} else {
			revision = 1
		}
	}
	ref, err := store.Put(ctx, artifact.PutRequest{
		WorkflowID:    wf,
		Type:          intent.Ref.Type,
		Revision:      revision,
		SchemaVersion: "1.0.0",
		CreatedAt:     a.now().UTC().Format(time.RFC3339),
		Producer: artifact.ProducerRef{
			Purpose:   string(intent.Producer),
			SessionID: string(intent.Session),
		},
		Body: intent.Body,
	})
	if err != nil {
		return model.EffectResultInput{}, err
	}
	return model.EffectResultInput{Kind: model.ArtifactWritten, Artifact: ref, Body: intent.Body}, nil
}

// runOutcome maps one settled Runtime run onto the Kernel's Effect
// Result: the settled Session facts, the redacted artifact body the
// command's output carries, and the failure code of a failed run.
func (a *Application) runOutcome(cmd model.Input, res *agent.RunResult) (model.EffectResultInput, error) {
	session := res.Session
	if session.Provider == "" {
		session.Provider = res.Provider
	}
	body := planningBody(cmd, res)
	out := model.EffectResultInput{
		Kind:    model.ProviderRunEnded,
		Session: session,
		Body:    body,
	}
	if res.Terminal != nil && res.Terminal.Type == agent.EventFailed {
		out.FailureCode = model.Code(res.Terminal.Code)
	}
	return out, nil
}

// planningBody extracts the redacted artifact body one planning command's
// run produced: the CFlow-assembled turn body for a discussion, the
// agent's plan Markdown for plan generation, and the Checker's structured
// result for a plan check.
func planningBody(cmd model.Input, res *agent.RunResult) []byte {
	switch in := cmd.(type) {
	case model.DiscussRequirementInput:
		assistant := ""
		for _, ev := range res.Events {
			if ev.Type == agent.EventAssistantMessage && ev.Text != "" {
				if assistant != "" {
					assistant += "\n"
				}
				assistant += ev.Text
			}
		}
		if body, err := json.Marshal(map[string]any{
			"session_id": string(res.Session.ID),
			"user":       in.Text,
			"assistant":  assistant,
		}); err == nil {
			return body
		}
		return nil
	case model.GeneratePlanInput:
		if res.Terminal != nil {
			var result struct {
				PlanMarkdown string `json:"plan_markdown"`
			}
			if json.Unmarshal([]byte(res.Terminal.Result), &result) == nil && result.PlanMarkdown != "" {
				return []byte(result.PlanMarkdown)
			}
			return []byte(res.Terminal.Result)
		}
		return nil
	case model.CheckPlanInput:
		if res.Terminal != nil {
			return []byte(res.Terminal.Result)
		}
		return nil
	case model.SpecGenerationInput, model.WorkflowCompilationInput:
		// The Kernel judges the raw structured Session output (the Spec
		// document with proposals, or the restricted Patch IR).
		if res.Terminal != nil {
			return []byte(res.Terminal.Result)
		}
		return nil
	}
	return nil
}

// renderPrompt fills a prompt's <CFLOW_INPUT> block with the structured
// Session input (the approved Spec/Catalog/Worktree or the review context)
// as JSON. Without this the real provider agents receive only the prompt
// body and the literal empty input placeholder — they cannot know which
// Spec to implement, which the real Cross-Provider E2E exposed (the claude
// Task misattributed a sibling Spec). Prompts without the block are
// returned unchanged.
func renderPrompt(body string, input any) string {
	const open = "<CFLOW_INPUT>"
	const close = "</CFLOW_INPUT>"
	start := strings.Index(body, open)
	end := strings.Index(body, close)
	if start < 0 || end < 0 || end <= start {
		return body
	}
	data, err := json.Marshal(input)
	if err != nil {
		return body
	}
	return body[:start+len(open)] + "\n" + string(data) + "\n" + body[end:]
}

// sessionInput is the structured input recorded with the Prompt: the
// requirement text for a discussion turn; for plan generation and check,
// the latest discussion-turn Artifact body when one exists; for Spec
// generation, the approved Plan and the active Verification Catalog; for
// Workflow optimization, the Spec and the eligible routes.
func (a *Application) sessionInput(ctx context.Context, wf model.WorkflowID, cmd model.Input) (any, error) {
	if in, ok := cmd.(model.DiscussRequirementInput); ok {
		return struct {
			Requirement string `json:"requirement"`
		}{Requirement: in.Text}, nil
	}
	if _, ok := cmd.(model.SwitchAgentInput); ok {
		// The switch successor's native bootstrap input: the immutable
		// redacted Context Bundle of the superseded discussion is attached by
		// nativeBootstrap (the bundle content lives in the evidence root and
		// is read back through the Runtime's FallbackBundle seam), so the
		// successor Provider starts with the prior discussion context.
		return &nativeBootstrapInput{}, nil
	}
	store, err := a.artifactStore(wf)
	if err != nil {
		return nil, err
	}
	switch cmd.(type) {
	case model.SpecGenerationInput:
		plan, err := readRequiredArtifact(ctx, store, wf, model.ArtifactPlan)
		if err != nil {
			return nil, err
		}
		catalog, err := readRequiredArtifact(ctx, store, wf, model.ArtifactCatalog)
		if err != nil {
			return nil, err
		}
		return struct {
			Plan    string `json:"plan"`
			Catalog string `json:"catalog"`
		}{
			Plan:    string(plan),
			Catalog: string(catalog),
		}, nil
	case model.WorkflowCompilationInput:
		spec, err := readRequiredArtifact(ctx, store, wf, model.ArtifactSpec)
		if err != nil {
			return nil, err
		}
		return struct {
			Spec           string   `json:"spec"`
			EligibleRoutes []string `json:"eligible_routes"`
		}{
			Spec:           string(spec),
			EligibleRoutes: eligibleRouteNames(),
		}, nil
	case model.DispatchInput:
		// The coding Session receives only the approved context: the Spec
		// set, the Verification Catalog it references, and the Task
		// Worktree facts (PRD Worktree 策略; design 12).
		return a.codingSessionInput(ctx, wf, cmd.(model.DispatchInput).Node)
	}
	if _, ok := cmd.(model.GeneratePlanInput); !ok {
		if _, ok := cmd.(model.CheckPlanInput); !ok {
			return nil, nil
		}
	}
	// Plan generation reads the immutable Discussion Handoff (the native
	// discussion path) — never a terminal transcript. The legacy headless
	// discussion (no handoff) falls back to the discussion turn body for
	// backward compatibility.
	handoff, err := readOptionalArtifact(ctx, store, wf, model.ArtifactDiscussionHandoff)
	if err != nil {
		return nil, err
	}
	if handoff != nil {
		return struct {
			Requirement string `json:"requirement"`
		}{Requirement: string(handoff)}, nil
	}
	body, err := readOptionalArtifact(ctx, store, wf, model.ArtifactDiscussionTurn)
	if err != nil {
		return nil, err
	}
	if body == nil {
		return nil, nil
	}
	return struct {
		Requirement string `json:"requirement"`
	}{Requirement: string(body)}, nil
}

// readRequiredArtifact reads and validates the active Revision of one
// approval-bound Artifact. Absence and every validation failure propagate.
func readRequiredArtifact(ctx context.Context, store *artifact.Store, wf model.WorkflowID, typ model.ArtifactType) ([]byte, error) {
	ref, err := store.Resolve(ctx, artifact.ResolveRequest{WorkflowID: wf, Type: typ})
	if err != nil {
		return nil, err
	}
	body, err := store.Get(ctx, ref)
	if err != nil {
		return nil, err
	}
	if len(body) == 0 {
		return nil, model.InvariantFault(fmt.Errorf("required %s artifact body is empty", typ))
	}
	return body, nil
}

// readOptionalArtifact differs only in allowing the Store's exact not-found
// result. Corrupt or unsafe optional evidence still fails closed.
func readOptionalArtifact(ctx context.Context, store *artifact.Store, wf model.WorkflowID, typ model.ArtifactType) ([]byte, error) {
	ref, err := store.Resolve(ctx, artifact.ResolveRequest{WorkflowID: wf, Type: typ})
	if err != nil {
		if artifact.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	body, err := store.Get(ctx, ref)
	if err != nil {
		return nil, err
	}
	return body, nil
}

// eligibleRouteNames is the deterministic eligible route list for the
// Workflow Optimization Session: every enabled Provider binding.
func eligibleRouteNames() []string {
	reg, err := agent.LoadProviderRegistry()
	if err != nil {
		return nil
	}
	return reg.EnabledNames()
}

// observeSnapshot observes the Planning Snapshot's HEAD and Git-visible
// state before a non-coding Session.
func (a *Application) observeSnapshot(ctx context.Context, cwd string) (gitflow.StatusFacts, error) {
	if a.git == nil {
		return gitflow.StatusFacts{}, model.InvariantFault(fmt.Errorf("git seam is not configured for this application"))
	}
	facts, err := a.git.Observe(ctx, gitflow.GitStatus{Dir: cwd})
	if err != nil {
		return gitflow.StatusFacts{}, err
	}
	st, ok := facts.(gitflow.StatusFacts)
	if !ok {
		return gitflow.StatusFacts{}, model.InvariantFault(fmt.Errorf("git status observation has an unexpected type"))
	}
	return st, nil
}

// verifySnapshotUnchanged compares the Planning Snapshot's HEAD/Index/
// Tracked/Untracked state before and after a non-coding Session. Any
// change makes the Session's output invalid (PRD Worktree 策略:
// UNEXPECTED_AGENT_MUTATION).
func (a *Application) verifySnapshotUnchanged(ctx context.Context, cwd string, pre gitflow.StatusFacts) error {
	post, err := a.observeSnapshot(ctx, cwd)
	if err != nil {
		return err
	}
	if post.Head != pre.Head || post.Dirty != pre.Dirty {
		return model.NewFault(model.CodeUnexpectedAgentMutation,
			"the planning snapshot changed during a non-coding session; its output is invalid")
	}
	return nil
}
