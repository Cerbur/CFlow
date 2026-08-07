package decision

import (
	"fmt"
	"sort"

	"cflow.local/cflow/internal/model"
)

// decideEffectResult routes the closed Effect Result union. Effect Results
// are immutable evidence inputs to another Decision; the Kernel judges the
// facts they carry, never a success claim (design 6.2 rules 3 and 5).
func decideEffectResult(state model.State, in model.EffectResultInput) (model.Decision, error) {
	switch in.Kind {
	case model.AttemptEnded:
		return decideAttemptEnded(state, in)
	case model.ProcessStopped:
		return decideProcessStopped(state, in)
	case model.ApplyStagingSucceeded, model.ApplyStagingFailed:
		return decideApplyStagingResult(state, in)
	case model.ApplyFastForwardSucceeded, model.ApplyFastForwardFailed:
		return decideApplyResult(state, in)
	case model.CleanupItemRemovedResult, model.CleanupItemFailedResult:
		return decideCleanupResult(state, in)
	case model.PlanningWorktreeCreated:
		// The Planning Snapshot exists at the recorded Base Commit; its
		// identity facts live in workflow.yaml, not in the aggregate, so
		// the Result Decision is empty (design 15.2).
		return model.Decision{}, nil
	case model.WorkspaceWorktreeCreated:
		// The Workspace exists at the recorded Base Head on its
		// deterministic branch; the identity facts live in workflow.yaml
		// and in the aggregate's Workflow facts, so the Result Decision
		// is empty (design 8.1, Task 4).
		return model.Decision{}, nil
	case model.ProviderRunEnded:
		return decideProviderRunEnded(state, in)
	case model.ArtifactWritten:
		return decideArtifactWritten(state, in)
	case model.WorkflowCompiled:
		return decideWorkflowCompiled(state, in)
	case model.IntegrationWorktreeCreated:
		return decideIntegrationWorktreeCreated(state, in)
	case model.TaskWorktreeCreated:
		return decideTaskWorktreeCreated(state, in)
	case model.VerificationRunEnded:
		return decideVerificationRunEnded(state, in)
	case model.IntegrationMerged:
		return decideIntegrationMerged(state, in)
	case model.IntegrationMergeFailed:
		return decideIntegrationMergeFailed(state, in)
	case model.IntegrationRollbacked:
		return decideIntegrationRollbacked(state, in)
	case model.GitAuditRefCreated:
		// The append-only audit Ref exists; the aggregate records the
		// Attempt evidence through the gate result, not a separate row.
		return model.Decision{}, nil
	default:
		return model.Decision{}, model.InvalidInputFault("unsupported effect result")
	}
}

// decideAttemptEnded settles one RUNNING Attempt with its end facts. A
// terminal Attempt is never reopened; the evidence gate decides the
// outcome, so a coding Attempt that reports success while its end facts
// show a dirty Worktree or no Commit fails as DIRTY_TASK_WORKTREE or
// MISSING_IMPLEMENTATION_COMMIT (PRD 约束 31-33).
func decideAttemptEnded(state model.State, in model.EffectResultInput) (model.Decision, error) {
	attempt := state.Attempts[in.Attempt]
	if attempt == nil {
		return model.Decision{}, model.InvalidInputFault("unknown attempt " + in.Attempt.String())
	}
	if attempt.Status != model.AttemptRunning {
		return model.Decision{}, model.InvalidInputFault("attempt " + in.Attempt.String() + " is not running")
	}
	node := state.Nodes[in.Attempt.Node]
	if node == nil {
		return model.Decision{}, model.InvariantFault(fmt.Errorf("attempt %s has no node", in.Attempt))
	}
	if !in.Outcome.Valid() {
		return model.Decision{}, model.InvalidInputFault("invalid attempt outcome")
	}
	b := &builder{state: state}

	switch in.Outcome {
	case model.OutcomeInterrupted:
		return settleInterrupted(state, node, attempt, in, b)
	case model.OutcomeSucceeded:
		// The evidence gate outranks the reported outcome (design 13.3,
		// PRD 约束 31).
		if node.Kind == model.NodeAgentTask {
			if in.EndDirtyFingerprint != "" {
				return decideAttemptFailure(state, node, attempt, in, model.CodeDirtyTaskWorktree, b)
			}
			if in.EndHead == "" || in.EndHead == attempt.StartHead {
				if !repairCleanEndAccepted(state, attempt, node) {
					return decideAttemptFailure(state, node, attempt, in, model.CodeMissingImplementationCommit, b)
				}
			}
		}
		if in.Evidence == (model.EvidenceRef{}) {
			return model.Decision{}, model.InvalidInputFault("attempt success requires evidence")
		}
		settleNodeSucceeded(b, state, node, attempt, in)
		return b.decision(), nil
	case model.OutcomeFailed:
		if in.FailureCode == "" {
			return model.Decision{}, model.InvalidInputFault("failed attempt requires a failure code")
		}
		if in.FailureCode == model.CodeUserInterrupted {
			// Ctrl+C is a user interruption, never a provider failure, and
			// never charges the Retry Budget (PRD 失败分类, USER_INTERRUPTED).
			return settleInterrupted(state, node, attempt, in, b)
		}
		return decideAttemptFailure(state, node, attempt, in, in.FailureCode, b)
	}
	panic("unreachable: AttemptOutcome is closed")
}

// decideAttemptFailure applies the compiled fault policy for one failure
// Code: a retryable failure within budget returns the Node to READY with a
// successor Attempt attempt_number+1; anything else (non-retryable,
// budget exhausted, quiescing, or a quarantined Branch) keeps the failed
// Attempt terminal and blocks or quiesces the Workflow (PRD 已确认,
// design 8.2).
func decideAttemptFailure(state model.State, node *model.Node, attempt *model.Attempt, in model.EffectResultInput, code model.Code, b *builder) (model.Decision, error) {
	pol, ok := model.Policy(code)
	if !ok {
		return model.Decision{}, model.InvalidInputFault("unknown failure code " + code.String())
	}
	quarantined := branchQuarantined(state, node.Branch)
	if quarantined {
		// A quarantined Branch can never re-enter the delivery chain; the
		// retry path is closed permanently (design 7.3 invariant 10).
		code = model.CodeCommitDuringPolicyDriftWindow
		pol, _ = model.Policy(code)
	}
	quiescing := activeQuiescing(state)
	// A pending Cancel intent also closes the retry path: cancel may never
	// allocate a Retry or charge budget (design 6.1). A retryable failure
	// arriving while the controlled stop is in flight is deferred, and the
	// cancel completes once everything settles.
	cancelling := state.Workflow.CancelIntent != nil && !state.Workflow.Runtime.IsTerminal()
	retryable := pol.Retry.AllowsSuccessor && node.RetryCharged < node.RetryBudget

	if retryable && !quiescing && !cancelling && !quarantined {
		// Budgeted retry: the failed Attempt is terminal and immutable,
		// the Node returns READY, and the successor Attempt is created
		// with attempt_number+1 (design 7.3 invariants 7 and 8).
		b.mutate(model.AttemptEndMutation{
			Key:                 attempt.Key,
			Status:              model.AttemptFailed,
			EndHead:             in.EndHead,
			EndDirtyFingerprint: in.EndDirtyFingerprint,
			FailureCode:         code,
			Evidence:            evidenceOf(in),
			RetryCharged:        pol.Retry.ChargesBudget,
			EndedAt:             state.Now,
		})
		b.event(model.EventAttemptFailed, node.ID, attempt.Key, code, "attempt failed")
		charged := node.RetryCharged + 1
		b.mutate(model.NodeStatusMutation{Node: node.ID, Status: model.NodeReady, RetryCharged: charged})
		b.event(model.EventNodeReady, node.ID, attempt.Key, "", "node ready for retry")
		// An automatic fallback successor binds the Attempt to the
		// persisted successor Session (the same settle Decision wrote the
		// Session row with supersedes_session_id, design 14.4): the
		// successor's dispatch reuses that Session instead of creating a
		// fresh row without the lineage link.
		b.mutate(model.AttemptAppendMutation{Attempt: model.Attempt{
			Key:       model.AttemptKey{Node: node.ID, Number: attempt.Key.Number + 1},
			Session:   in.SuccessorSession.ID,
			Status:    model.AttemptReady,
			StartHead: in.EndHead,
			StartedAt: state.Now,
		}})
		b.event(model.EventAttemptCreated, node.ID, model.AttemptKey{Node: node.ID, Number: attempt.Key.Number + 1}, "", "successor attempt created")
		return b.decision(), nil
	}

	// Deferred during quiescing or pending cancel: the Node projects READY
	// per the normal rules but no successor starts and no budget is charged
	// (PRD 已确认：并行失败后的 Quiescing, rule 3; design 6.1). A pending
	// cancel settles through finishCancel once nothing is running.
	if retryable && (quiescing || cancelling) {
		b.mutate(model.AttemptEndMutation{
			Key:                 attempt.Key,
			Status:              model.AttemptFailed,
			EndHead:             in.EndHead,
			EndDirtyFingerprint: in.EndDirtyFingerprint,
			FailureCode:         code,
			Evidence:            evidenceOf(in),
			RetryCharged:        false,
			EndedAt:             state.Now,
		})
		b.event(model.EventAttemptFailed, node.ID, attempt.Key, code, "attempt failed; retry deferred")
		b.mutate(model.NodeStatusMutation{Node: node.ID, Status: model.NodeReady, RetryCharged: node.RetryCharged})
		b.event(model.EventNodeReady, node.ID, attempt.Key, "", "node ready; retry deferred")
		b.mutate(model.FindingAppendMutation{Finding: model.Finding{
			ID:       model.FindingID(fmt.Sprintf("finding-%d", len(state.Findings)+1)),
			Code:     code,
			Scope:    pol.Scope,
			Subject:  string(node.ID),
			Blocking: false,
			Text:     code.String(),
			Evidence: in.Evidence,
			Seq:      state.NextEventSeq,
		}})
		b.event(model.EventFindingOpened, node.ID, attempt.Key, code, "failure finding")
		settleAfterAttemptEnd(state, b, attempt.Key)
		return b.decision(), nil
	}

	// Blocking failure: the Attempt is terminal, the Node FAILED, a
	// blocking Finding is persisted, and the dispatch gate closes. If
	// other Attempts are in flight the Run quiesces with a snapshot of
	// exactly the persisted RUNNING Attempts; otherwise the Workflow
	// Blocks immediately (PRD 已确认, design 8.3). Retry exhaustion
	// blocks; it never makes the Workflow FAILED.
	findingCode := code
	if pol.Retry.AllowsSuccessor {
		findingCode = model.CodeRetryExhausted
	}
	b.mutate(model.AttemptEndMutation{
		Key:                 attempt.Key,
		Status:              model.AttemptFailed,
		EndHead:             in.EndHead,
		EndDirtyFingerprint: in.EndDirtyFingerprint,
		FailureCode:         code,
		Evidence:            evidenceOf(in),
		RetryCharged:        pol.Retry.ChargesBudget,
		EndedAt:             state.Now,
	})
	b.event(model.EventAttemptFailed, node.ID, attempt.Key, code, "attempt failed")
	b.mutate(model.NodeStatusMutation{Node: node.ID, Status: model.NodeFailed, RetryCharged: node.RetryCharged})
	b.event(model.EventNodeFailed, node.ID, attempt.Key, findingCode, "node failed")
	b.mutate(model.FindingAppendMutation{Finding: model.Finding{
		ID:       model.FindingID(fmt.Sprintf("finding-%d", len(state.Findings)+1)),
		Code:     findingCode,
		Scope:    pol.Scope,
		Subject:  string(node.ID),
		Blocking: true,
		Text:     findingCode.String(),
		Evidence: in.Evidence,
		Seq:      state.NextEventSeq,
	}})
	b.event(model.EventFindingOpened, node.ID, attempt.Key, findingCode, "blocking finding")
	closeDispatchForFailure(state, b, attempt.Key)
	return b.decision(), nil
}

// closeDispatchForFailure closes the dispatch gate and either quiesces
// (in-flight Attempts remain) or Blocks directly.
func closeDispatchForFailure(state model.State, b *builder, exclude model.AttemptKey) {
	run := activeRun(state)
	if run == nil || run.Status.IsTerminal() {
		// No Run record: the Workflow still Blocks on a blocking failure.
		b.mutate(wfMutStatus(state, model.RuntimeBlocked))
		b.event(model.EventWorkflowBlocked, "", model.AttemptKey{}, "", "workflow blocked")
		return
	}
	if hasRunningAttemptExcept(state, exclude) {
		// Quiescing: fix the exact persisted RUNNING Attempts; Ready and
		// Pending siblings never dispatch while the gate is closed. The
		// snapshot is sorted so identical State/Input always produce
		// byte-identical Decisions.
		var snapshot []model.AttemptKey
		for k, a := range state.Attempts {
			if k != exclude && a.Status == model.AttemptRunning {
				snapshot = append(snapshot, k)
			}
		}
		sortedAttemptKeys(snapshot)
		b.mutate(model.RunMutation{ID: run.ID, Status: model.RunQuiescing, DispatchGate: false, QuiesceSnapshot: snapshot})
		b.event(model.EventRunQuiescing, "", model.AttemptKey{}, "", "run quiescing")
		b.event(model.EventWorkflowQuiesceRequested, "", model.AttemptKey{}, "", "quiesce requested")
		return
	}
	b.mutate(model.RunMutation{ID: run.ID, Status: model.RunBlocked, DispatchGate: false})
	b.event(model.EventRunBlocked, "", model.AttemptKey{}, "", "run blocked")
	b.mutate(wfMutStatus(state, model.RuntimeBlocked))
	b.event(model.EventWorkflowBlocked, "", model.AttemptKey{}, "", "workflow blocked")
}

// settleInterrupted records an interrupted Attempt (never charging the
// budget) and opens the controlled stop: a persisted cancel intent
// completes cancellation (Nodes go CANCELLED, never READY); otherwise the
// Run enters STOPPING with the persisted stop intent (CONTROLLED_STOP_
// REQUESTED, or COMMIT_POLICY_SAFETY_STOP_REQUESTED with stop_reason
// COMMIT_POLICY_DRIFT for a policy safety stop), the dispatch gate closes,
// the managed processes receive their two-phase stop Effects, and once
// nothing is in flight the Run converges INTERRUPTED with the Workflow
// PAUSED — or BLOCKED when a blocking Finding or Quiescing blocker exists
// (Ctrl+C never clears the original finding; PRD 已确认：Ctrl+C 两阶段有限停
// 止 step 5).
func settleInterrupted(state model.State, node *model.Node, attempt *model.Attempt, in model.EffectResultInput, b *builder) (model.Decision, error) {
	b.mutate(model.AttemptEndMutation{
		Key:                 attempt.Key,
		Status:              model.AttemptInterrupted,
		EndHead:             in.EndHead,
		EndDirtyFingerprint: in.EndDirtyFingerprint,
		FailureCode:         in.FailureCode,
		Evidence:            evidenceOf(in),
		RetryCharged:        false,
		EndedAt:             state.Now,
	})
	b.event(model.EventAttemptInterrupted, node.ID, attempt.Key, "", "attempt interrupted")

	// A persisted Cancel intent completes through finishCancel once
	// nothing is running; the interrupted Node settles CANCELLED there,
	// never READY.
	if state.Workflow.CancelIntent != nil && !state.Workflow.Runtime.IsTerminal() {
		if !hasRunningAttemptExcept(state, attempt.Key) && !hasRunningProcess(state) {
			finishCancel(b, state, state.Workflow.CancelIntent)
		}
		return b.decision(), nil
	}
	b.mutate(model.NodeStatusMutation{Node: node.ID, Status: model.NodeReady, RetryCharged: node.RetryCharged})
	b.event(model.EventNodeReady, node.ID, attempt.Key, "", "node ready")
	run := activeRun(state)
	if run == nil || run.Status.IsTerminal() {
		return b.decision(), nil
	}
	if run.Status != model.RunStopping {
		// The first interruption of the stop: atomically persist the stop
		// intent, close the dispatch gate, and begin the two-phase stop of
		// the managed processes (PRD 已确认：Ctrl+C 两阶段有限停止 step 1;
		// 已确认：Commit Policy 漂移立即安全停止 step 1).
		reason, kind := model.CodeUserInterrupted, model.EventControlledStopRequested
		if in.FailureCode == model.CodeCommitPolicySafetyStopRequested {
			reason, kind = model.CodeCommitPolicyDrift, model.EventCommitPolicySafetyStopRequested
		}
		b.mutate(model.RunMutation{ID: run.ID, Status: model.RunStopping, DispatchGate: false, StopReason: reason})
		b.event(kind, "", model.AttemptKey{}, "", "stop requested")
		b.event(model.EventRunStopped, "", model.AttemptKey{}, "", "run stopping")
		stopRunningProcesses(b, state)
	}
	// The stop may already be complete (no processes, no other attempts).
	convergeStopping(b, state, attempt.Key, "", run.Status != model.RunStopping)
	return b.decision(), nil
}

// convergeStopping completes a controlled stop once every managed process
// and every other Attempt has settled: the Run becomes INTERRUPTED and
// the Workflow PAUSED — or BLOCKED when a blocking Finding exists (the
// Quiescing blocker or an earlier failure; Ctrl+C never clears it). A
// persisted Cancel intent converges through finishCancel instead. It
// never reopens dispatch. excludeAttempt and excludeProcess are the
// entities this same Decision settles (their pre-mutation rows still look
// RUNNING to the input State); transitioning reports that the caller's
// own Decision performed the RUN→STOPPING transition (the input State
// still shows the pre-transition Run).
func convergeStopping(b *builder, state model.State, excludeAttempt model.AttemptKey, excludeProcess model.ProcessID, transitioning bool) {
	run := activeRun(state)
	if run == nil {
		return
	}
	if !transitioning && run.Status != model.RunStopping {
		return
	}
	if hasRunningProcessExcept(state, excludeProcess) || hasRunningAttemptExcept(state, excludeAttempt) {
		return
	}
	if state.Workflow.CancelIntent != nil && !state.Workflow.Runtime.IsTerminal() {
		finishCancel(b, state, state.Workflow.CancelIntent)
		return
	}
	// The persisted stop reason rides the convergence (the store row's
	// stop_reason is replaced wholesale).
	b.mutate(model.RunMutation{ID: run.ID, Status: model.RunInterrupted, DispatchGate: false, StopReason: run.StopReason})
	b.event(model.EventRunInterrupted, "", model.AttemptKey{}, "", "run interrupted")
	if hasBlockingFinding(state) {
		b.mutate(wfMutStatus(state, model.RuntimeBlocked))
		b.event(model.EventWorkflowBlocked, "", model.AttemptKey{}, "", "workflow blocked after the stop")
		return
	}
	b.mutate(wfMutStatus(state, model.RuntimePaused))
	b.event(model.EventWorkflowPaused, "", model.AttemptKey{}, "", "workflow paused after the stop")
}

// settleAfterAttemptEnd completes the settlement protocols that a settled
// Attempt may finish: convergence of a QUIESCING Run whose snapshot
// Attempts have all settled, completion of a persisted Cancel intent, and
// convergence of a STOPPING Run whose processes and Attempts settled.
func settleAfterAttemptEnd(state model.State, b *builder, ended model.AttemptKey) {
	if !hasRunningAttemptExcept(state, ended) && !hasRunningProcess(state) {
		if state.Workflow.CancelIntent != nil && !state.Workflow.Runtime.IsTerminal() {
			finishCancel(b, state, state.Workflow.CancelIntent)
			return
		}
	}
	if run := activeRun(state); run != nil && run.Status == model.RunQuiescing && !hasRunningAttemptExcept(state, ended) {
		convergeQuiescing(b, state)
		return
	}
	convergeStopping(b, state, ended, "", false)
}

// convergeQuiescing moves a QUIESCING Run whose snapshot Attempts have all
// settled to BLOCKED in the same transaction, appending WORKFLOW_QUIESCED
// (PRD 已确认：并行失败后的 Quiescing, rule 5).
func convergeQuiescing(b *builder, state model.State) {
	run := activeRun(state)
	b.mutate(model.RunMutation{ID: run.ID, Status: model.RunBlocked, DispatchGate: false})
	b.event(model.EventRunBlocked, "", model.AttemptKey{}, "", "run blocked after quiescing")
	b.mutate(wfMutStatus(state, model.RuntimeBlocked))
	b.event(model.EventWorkflowBlocked, "", model.AttemptKey{}, "", "workflow blocked")
	b.event(model.EventWorkflowQuiesced, "", model.AttemptKey{}, "", "quiescing converged")
}

// finishCancel completes the recoverable cancellation protocol: the
// terminal CANCELLED Decision is committed only after the persisted cancel
// intent, all managed processes settled, and facts reconciled. It is
// terminal: no later command may leave it (PRD 状态机与持久化模型,
// design 17.4).
func finishCancel(b *builder, state model.State, intent *model.CancelIntent) {
	b.mutate(wfMut(state, state.Workflow.Stage, model.RuntimeCancelled, intent))
	if run := activeRun(state); run != nil && !run.Status.IsTerminal() {
		b.mutate(model.RunMutation{ID: run.ID, Status: model.RunCancelled, DispatchGate: false})
		b.event(model.EventRunCancelled, "", model.AttemptKey{}, "", "run cancelled")
	}
	for _, id := range sortedNodeIDs(state) {
		n := state.Nodes[id]
		if !n.Status.IsTerminal() {
			b.mutate(model.NodeStatusMutation{Node: n.ID, Status: model.NodeCancelled, RetryCharged: n.RetryCharged})
			b.event(model.EventNodeCancelled, n.ID, model.AttemptKey{}, "", "node cancelled")
		}
	}
	// Every non-terminal Attempt (e.g. a READY successor that was never
	// dispatched) is settled CANCELLED, so no record lingers on the
	// terminal Workflow. RUNNING Attempts are skipped: settle guarantees
	// none can be running here, except the one this same Decision is
	// ending, which the caller settles itself. Terminal Attempt facts stay
	// immutable.
	keys := make([]model.AttemptKey, 0, len(state.Attempts))
	for k := range state.Attempts {
		keys = append(keys, k)
	}
	sortedAttemptKeys(keys)
	for _, k := range keys {
		a := state.Attempts[k]
		if !a.Status.IsTerminal() && a.Status != model.AttemptRunning {
			b.mutate(model.AttemptEndMutation{Key: k, Status: model.AttemptCancelled, EndedAt: state.Now})
			b.event(model.EventAttemptCancelled, k.Node, k, "", "attempt cancelled")
		}
	}
	b.event(model.EventWorkflowCancelled, "", model.AttemptKey{}, "", "workflow cancelled")
}

// sortedAttemptKeys sorts the quiesce snapshot in place so Decisions are
// byte-identical for identical State/Input.
func sortedAttemptKeys(keys []model.AttemptKey) {
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Node != keys[j].Node {
			return keys[i].Node < keys[j].Node
		}
		return keys[i].Number < keys[j].Number
	})
}

// sortedNodeIDs returns the Node identities in deterministic order.
func sortedNodeIDs(state model.State) []model.NodeID {
	ids := make([]model.NodeID, 0, len(state.Nodes))
	for id := range state.Nodes {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// decideProcessStopped settles one managed process and continues the stop
// protocol: while a stop or cancel is in progress, the next running
// process receives its stop Effect; once nothing is running, a persisted
// cancel intent completes cancellation and a STOPPING Run converges. A
// process that survived the force-kill phase with its exact
// PID/start-token identity is the orphan fact: the Workflow Blocks with
// ORPHAN_CHILD_PROCESS and Project mutation is quarantined, or keeps the
// Cancel intent with CANCEL_PENDING_ORPHAN_PROCESS until Recovery
// completes it (PRD 已确认：Ctrl+C 两阶段有限停止 step 9; Cancel step 4).
func decideProcessStopped(state model.State, in model.EffectResultInput) (model.Decision, error) {
	p := findProcess(state, in.Process)
	if p == nil {
		return model.Decision{}, model.InvalidInputFault("unknown process " + string(in.Process))
	}
	if p.Status != model.ProcessStatusRunning {
		return model.Decision{}, model.InvalidInputFault("process " + string(in.Process) + " is not running")
	}
	b := &builder{state: state}
	b.mutate(model.ProcessEndMutation{ID: p.ID, Status: model.ProcessStatusStopped, EndedAt: state.Now})
	if in.Orphan {
		settleOrphanProcess(state, b, in.Process)
		return b.decision(), nil
	}
	if state.Workflow.CancelIntent != nil || activeRunStopping(state) {
		stopRunningProcesses(b, state, in.Process)
	}
	// The process this Decision settles is excluded: its pre-mutation row
	// still looks RUNNING to the input State.
	convergeStopping(b, state, model.AttemptKey{}, in.Process, false)
	return b.decision(), nil
}

// settleOrphanProcess records the orphan fact of a force-killed process
// that is still alive with its exact identity: the Workflow Blocks and
// Project mutation is quarantined; a persisted Cancel keeps its intent so
// Recovery can complete the cancellation once the process facts settle.
func settleOrphanProcess(state model.State, b *builder, process model.ProcessID) {
	code := model.CodeOrphanChildProcess
	scope := model.ScopeRun
	if state.Workflow.CancelIntent != nil && !state.Workflow.Runtime.IsTerminal() {
		code = model.CodeCancelPendingOrphanProcess
		scope = model.ScopeWorkflow
	}
	b.mutate(model.FindingAppendMutation{Finding: model.Finding{
		ID:       model.FindingID(fmt.Sprintf("finding-%d", len(state.Findings)+1)),
		Code:     code,
		Scope:    scope,
		Subject:  string(process),
		Blocking: true,
		Text:     code.String(),
		Seq:      state.NextEventSeq,
	}})
	b.event(model.EventFindingOpened, "", model.AttemptKey{}, code, "orphan process finding")
	if run := activeRun(state); run != nil && !run.Status.IsTerminal() {
		b.mutate(model.RunMutation{ID: run.ID, Status: model.RunInterrupted, DispatchGate: false})
		b.event(model.EventRunInterrupted, "", model.AttemptKey{}, "", "run interrupted by an orphan process")
	}
	b.mutate(wfMutStatus(state, model.RuntimeBlocked))
	b.event(model.EventWorkflowBlocked, "", model.AttemptKey{}, "", "workflow blocked by an orphan process")
}

func activeRunStopping(state model.State) bool {
	if run := activeRun(state); run != nil {
		return run.Status == model.RunStopping
	}
	return false
}

// decideReconcile is the Kernel's Recovery sweep: complete a persisted
// cancel intent once everything is settled, converge a QUIESCING Run whose
// snapshot Attempts have all settled, converge a STOPPING Run whose
// processes and Attempts settled (Recovery of a Stop or a Safety Stop can
// never reopen dispatch), and Block a RUNNING Workflow that carries a
// FAILED Node with no in-flight Attempts. It never reopens dispatch and
// never resurrects failed Nodes or terminal Attempts (PRD 状态机与持久化模
// 型, design 17).
func decideReconcile(state model.State, in model.ReconcileInput) (model.Decision, error) {
	b := &builder{state: state}

	if state.Workflow.CancelIntent != nil && !state.Workflow.Runtime.IsTerminal() &&
		!hasRunningAttempt(state) && !hasRunningProcess(state) {
		finishCancel(b, state, state.Workflow.CancelIntent)
		return b.decision(), nil
	}
	if run := activeRun(state); run != nil && run.Status == model.RunQuiescing && !hasRunningAttempt(state) {
		convergeQuiescing(b, state)
		return b.decision(), nil
	}
	convergeStopping(b, state, model.AttemptKey{}, "", false)
	if state.Workflow.Runtime == model.RuntimeRunning && anyFailedNode(state) && !hasRunningAttempt(state) {
		b.mutate(wfMutStatus(state, model.RuntimeBlocked))
		b.event(model.EventWorkflowBlocked, "", model.AttemptKey{}, "", "workflow blocked")
		if run := activeRun(state); run != nil && !run.Status.IsTerminal() {
			b.mutate(model.RunMutation{ID: run.ID, Status: model.RunBlocked, DispatchGate: false})
			b.event(model.EventRunBlocked, "", model.AttemptKey{}, "", "run blocked")
		}
	}
	return b.decision(), nil
}

// decideAgentEvent handles validated Agent Events. Agent output can never
// write authoritative lifecycle state: an Agent-declared completion is
// rejected with UNTRUSTED_COMPLETION (design 7.3 invariant 1, 14.3).
func decideAgentEvent(state model.State, in model.AgentEventInput) (model.Decision, error) {
	switch in.Kind {
	case model.AgentClaimsComplete:
		return model.Decision{}, model.NewFault(model.CodeUntrustedCompletion,
			"agent output cannot complete a Node; completion requires Runtime-judged evidence")
	default:
		return model.Decision{}, model.InvalidInputFault("unsupported agent event")
	}
}

// repairCleanEndAccepted reports whether a repair Attempt may end at the
// HEAD it started from: the repair Session removed the residuals and the
// prior Attempt already produced a legal implementation Commit beyond the
// Task Base, so no empty Commit is required (PRD 已确认：DIRTY_TASK_WORKTREE
// 原地 Repair). Repair never fabricates success: an empty head or a
// missing legal prior Commit is still rejected with
// MISSING_IMPLEMENTATION_COMMIT.
func repairCleanEndAccepted(state model.State, attempt *model.Attempt, node *model.Node) bool {
	if attempt.StartHead == "" {
		return false
	}
	repair := false
	for i := range state.Sessions {
		if state.Sessions[i].ID == attempt.Session && state.Sessions[i].Purpose == model.PurposeRepair {
			repair = true
			break
		}
	}
	if !repair {
		return false
	}
	prior := priorAttemptEndHead(state, node.ID, attempt.Key.Number)
	return prior != "" && prior != node.BaseCommit
}

// priorAttemptEndHead is the end HEAD of the highest-numbered terminal
// Attempt below number ("" when none).
func priorAttemptEndHead(state model.State, node model.NodeID, number model.AttemptNumber) string {
	var best model.AttemptNumber
	var end string
	for k, a := range state.Attempts {
		if k.Node != node || k.Number >= number || !a.Status.IsTerminal() {
			continue
		}
		if a.EndHead != "" && (end == "" || k.Number > best) {
			best = k.Number
			end = a.EndHead
		}
	}
	return end
}

// highestTerminalAttempt returns the highest-numbered terminal Attempt of
// one Node (nil when none).
func highestTerminalAttempt(state model.State, node model.NodeID) *model.Attempt {
	var best *model.Attempt
	for k, a := range state.Attempts {
		if k.Node != node || !a.Status.IsTerminal() {
			continue
		}
		if best == nil || k.Number > best.Key.Number {
			best = a
		}
	}
	return best
}
