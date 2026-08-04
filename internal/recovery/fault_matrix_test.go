// The Task 17 crash matrix (brief Step 6): every injected crash — during a
// Cancel, a Quiescing convergence, a controlled stop, or a Commit Policy
// Safety Stop — converges through the Recovery sweep to exactly one stable
// disposition with no duplicate external effect, and Recovery of a Stop,
// Cancel, Quiesce, or Safety Stop never reopens the Scheduler (design
// 17.3: never reopen dispatch; never start a Provider; never allocate a
// Retry).
package recovery_test

import (
	"context"
	"testing"

	"cflow.local/cflow/internal/decision"
	"cflow.local/cflow/internal/gitflow"
	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/recovery"
	"cflow.local/cflow/internal/store"
)

// sweep runs the Recovery sweep against the persisted aggregate: the
// Reconcile decisions of the crash matrix, applied through the real
// Store, one stable disposition per crash.
func (fx *recoveryFixture) sweep(t *testing.T, inputs ...model.Input) model.State {
	t.Helper()
	for _, input := range inputs {
		st, err := store.Open(context.Background(), store.OpenOptions{
			Path: fx.dbPath, Workflow: testWF, CflowVersion: "0.0.0-dev", Now: fx.now,
		})
		if err != nil {
			t.Fatalf("open store: %v", err)
		}
		view, err := st.View(context.Background(), store.StoreQuery{})
		if err != nil {
			t.Fatalf("view: %v", err)
		}
		cd, err := st.Transact(context.Background(), view.AggregateVersion, func(state model.State) (model.Decision, error) {
			return decision.Decide(state, input)
		})
		if err != nil {
			t.Fatalf("sweep decision %T: %v", input, err)
		}
		for _, ev := range cd.Decision.Events {
			if ev.Kind == model.EventRunStarted || ev.Kind == model.EventNodeStarted ||
				ev.Kind == model.EventAttemptCreated {
				t.Fatalf("recovery must never reopen dispatch or start work, event %s", ev.Kind)
			}
		}
		if cd.Decision.Effect != nil {
			t.Fatalf("recovery must never request an external effect, got %T", cd.Decision.Effect)
		}
		if err := st.Close(); err != nil {
			t.Fatalf("close store: %v", err)
		}
	}
	st, err := store.Open(context.Background(), store.OpenOptions{
		Path: fx.dbPath, Workflow: testWF, CflowVersion: "0.0.0-dev", Now: fx.now,
	})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	view, err := st.View(context.Background(), store.StoreQuery{})
	if err != nil {
		t.Fatalf("view: %v", err)
	}
	return view.State
}

// mutate applies one raw state mutation through the Store (the crash
// fixture).
func (fx *recoveryFixture) mutate(t *testing.T, mutations ...model.Mutation) {
	t.Helper()
	st, err := store.Open(context.Background(), store.OpenOptions{
		Path: fx.dbPath, Workflow: testWF, CflowVersion: "0.0.0-dev", Now: fx.now,
	})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	view, err := st.View(context.Background(), store.StoreQuery{})
	if err != nil {
		t.Fatalf("view: %v", err)
	}
	if _, err := st.Transact(context.Background(), view.AggregateVersion, func(state model.State) (model.Decision, error) {
		return model.Decision{Mutations: mutations}, nil
	}); err != nil {
		t.Fatalf("mutate: %v", err)
	}
}

// crashBase seeds the Run, the Task Node, and the first Attempt rows the
// crash fixtures mutate.
func (fx *recoveryFixture) crashBase(t *testing.T) {
	t.Helper()
	fx.mutate(t,
		model.RunAppendMutation{Run: model.Run{ID: "run-1", Status: model.RunRunning, DispatchGate: true, StartedAt: fx.now()}},
		model.NodeAppendMutation{Node: model.Node{
			ID: taskNode, Kind: model.NodeAgentTask, Status: model.NodeRunning,
			Branch: fx.taskBranch, RetryBudget: 2,
		}},
		model.AttemptAppendMutation{Attempt: model.Attempt{
			Key: model.AttemptKey{Node: taskNode, Number: 1}, Status: model.AttemptRunning,
			StartHead: fx.baseHead, StartedAt: fx.now(),
		}},
	)
}

// TestCrashDuringCancelCompletesOnlyCancellation is the mandated crash
// matrix entry: a Runtime that crashed between the persisted Cancel
// intent and the terminal Decision — the Run STOPPING, the Attempt settled
// INTERRUPTED, the processes gone. Recovery completes the cancellation
// and never reopens the Scheduler (PRD 已确认：Cancel 逻辑终止 step 4).
func TestCrashDuringCancelCompletesOnlyCancellation(t *testing.T) {
	fx := newRecoveryFixture(t)
	fx.crashBase(t)
	fx.mutate(t,
		model.WorkflowMutation{ID: testWF, Project: model.ProjectID(testProj),
			Stage: model.StageExecution, Runtime: model.RuntimeRunning,
			TargetBranch: "main", BaseCommit: fx.baseHead,
			IntegrationBranch: "cflow/" + testWF + "/integration", IntegrationHead: fx.baseHead,
			CancelIntent: &model.CancelIntent{RequestedSeq: 3, Reason: "user"}},
		model.RunMutation{ID: "run-1", Status: model.RunStopping, DispatchGate: false, StopReason: model.CodeUserInterrupted},
		model.AttemptEndMutation{Key: model.AttemptKey{Node: taskNode, Number: 1},
			Status: model.AttemptInterrupted, RetryCharged: false, EndedAt: fx.now()},
	)
	state := fx.sweep(t, model.ReconcileInput{})
	if state.Workflow.Runtime != model.RuntimeCancelled {
		t.Fatalf("runtime = %s, want CANCELLED", state.Workflow.Runtime)
	}
	if state.Workflow.CancelIntent == nil {
		t.Fatalf("the persisted cancel intent must remain for audit")
	}
	if len(state.Attempts) != 1 {
		t.Fatalf("attempts = %+v, want exactly the interrupted attempt", state.Attempts)
	}
}

// TestCrashDuringQuiesceConvergesToBlocked asserts the quiesce crash: a
// QUIESCING Run whose snapshot Attempts all settled before the crash
// converges to BLOCKED with the blocking Finding preserved — never to a
// resumable PAUSED.
func TestCrashDuringQuiesceConvergesToBlocked(t *testing.T) {
	fx := newRecoveryFixture(t)
	fx.crashBase(t)
	fx.mutate(t,
		model.NodeStatusMutation{Node: taskNode, Status: model.NodeFailed, RetryCharged: 1},
		model.AttemptEndMutation{Key: model.AttemptKey{Node: taskNode, Number: 1},
			Status: model.AttemptFailed, FailureCode: model.CodeDirtyWorktreeDrifted,
			RetryCharged: false, EndedAt: fx.now()},
		model.FindingAppendMutation{Finding: model.Finding{ID: "finding-1",
			Code: model.CodeDirtyWorktreeDrifted, Scope: model.ScopeAttempt, Subject: string(taskNode),
			Blocking: true, Text: "drifted"}},
		model.RunMutation{ID: "run-1", Status: model.RunQuiescing, DispatchGate: false},
	)
	state := fx.sweep(t, model.ReconcileInput{})
	if state.Workflow.Runtime != model.RuntimeBlocked {
		t.Fatalf("runtime = %s, want BLOCKED", state.Workflow.Runtime)
	}
	blocked := false
	for _, r := range state.Runs {
		if r.Status == model.RunBlocked {
			blocked = true
		}
	}
	if !blocked {
		t.Fatalf("runs = %+v, want a BLOCKED run", state.Runs)
	}
	if !hasBlockingFinding(state, model.CodeDirtyWorktreeDrifted) {
		t.Fatalf("the blocking finding must be preserved")
	}
}

// TestCrashDuringControlledStopConvergesPaused asserts the controlled
// stop crash: a STOPPING Run whose Attempts and processes settled before
// the crash converges to INTERRUPTED + PAUSED with no Retry charge.
func TestCrashDuringControlledStopConvergesPaused(t *testing.T) {
	fx := newRecoveryFixture(t)
	fx.crashBase(t)
	fx.mutate(t,
		model.NodeStatusMutation{Node: taskNode, Status: model.NodeReady, RetryCharged: 0},
		model.AttemptEndMutation{Key: model.AttemptKey{Node: taskNode, Number: 1},
			Status: model.AttemptInterrupted, EndHead: fx.taskHead, RetryCharged: false, EndedAt: fx.now()},
		model.RunMutation{ID: "run-1", Status: model.RunStopping, DispatchGate: false, StopReason: model.CodeUserInterrupted},
	)
	state := fx.sweep(t, model.ReconcileInput{})
	if state.Workflow.Runtime != model.RuntimePaused {
		t.Fatalf("runtime = %s, want PAUSED", state.Workflow.Runtime)
	}
	interrupted := false
	for _, r := range state.Runs {
		if r.Status == model.RunInterrupted {
			interrupted = true
		}
	}
	if !interrupted {
		t.Fatalf("runs = %+v, want an INTERRUPTED run", state.Runs)
	}
	if n := state.Nodes[taskNode]; n == nil || n.Status != model.NodeReady || n.RetryCharged != 0 {
		t.Fatalf("node = %+v, want READY without charge", n)
	}
}

// TestCrashDuringSafetyStopNeverReopensDispatch asserts the Safety Stop
// crash: the Run STOPPING with stop_reason COMMIT_POLICY_DRIFT and the
// Attempts interrupted; Recovery converges the stop to PAUSED at the
// confirmation gate and never reopens dispatch — the gate stays closed
// and no Provider ever starts.
func TestCrashDuringSafetyStopNeverReopensDispatch(t *testing.T) {
	fx := newRecoveryFixture(t)
	fx.crashBase(t)
	fx.mutate(t,
		model.NodeStatusMutation{Node: taskNode, Status: model.NodeReady, RetryCharged: 0},
		model.AttemptEndMutation{Key: model.AttemptKey{Node: taskNode, Number: 1},
			Status: model.AttemptInterrupted, EndHead: fx.taskHead, RetryCharged: false, EndedAt: fx.now()},
		model.RunMutation{ID: "run-1", Status: model.RunStopping, DispatchGate: false, StopReason: model.CodeCommitPolicyDrift},
	)
	// The sweep converges the stop; the confirmation gate then records
	// the fresh Preflight and keeps the Workflow paused.
	state := fx.sweep(t, model.ReconcileInput{})
	if state.Workflow.Runtime != model.RuntimePaused {
		t.Fatalf("runtime = %s, want PAUSED at the confirmation gate", state.Workflow.Runtime)
	}
	for _, r := range state.Runs {
		if !r.Status.IsTerminal() {
			t.Fatalf("a recovery of a safety stop must leave every run terminal, run = %+v", r)
		}
	}
	// A dispatch attempt across the recovery is refused: the gate of the
	// terminal run is closed and no new run was opened.
	st, err := store.Open(context.Background(), store.OpenOptions{
		Path: fx.dbPath, Workflow: testWF, CflowVersion: "0.0.0-dev", Now: fx.now,
	})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	view, err := st.View(context.Background(), store.StoreQuery{})
	if err != nil {
		t.Fatalf("view: %v", err)
	}
	_, err = st.Transact(context.Background(), view.AggregateVersion, func(state model.State) (model.Decision, error) {
		return decision.Decide(state, model.DispatchInput{
			Node: taskNode, Session: "session-x", Route: "fake", BaseHead: fx.baseHead,
		})
	})
	if code, ok := model.CodeOf(err); !ok || code != model.CodeDispatchGateClosed {
		t.Fatalf("dispatch after the safety-stop recovery must be refused by the closed gate, got %v", err)
	}
}

// TestCrashAfterQuarantinePreservesQuarantineEvidence asserts the
// quarantine crash: a quarantine Record whose unique audit Ref is
// missing is drift the user must act on — the evidence must never vanish
// (PRD 已确认：漂移窗口 Commit 的隔离与替代执行 step 1).
func TestCrashAfterQuarantinePreservesQuarantineEvidence(t *testing.T) {
	fx := newRecoveryFixture(t)
	fx.mutate(t,
		model.QuarantineAppendMutation{Quarantine: model.Quarantine{
			ID: "quarantine-1", AuditRef: "refs/cflow/" + testWF + "/quarantine/quarantine-1",
			Branch: fx.taskBranch, FromHead: fx.baseHead, ToHead: fx.taskHead,
			Code: model.CodeCommitDuringPolicyDriftWindow,
		}},
	)
	scope := recovery.Scope{Workflow: testWF}
	out, err := fx.engine.Reconcile(context.Background(), scope)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	// The audit ref was never created: the quarantine evidence is missing
	// and the user must act on the drift.
	if len(out.Faults) == 0 {
		t.Fatalf("a quarantine without its audit ref must block reconciliation")
	}
	// With the audit ref in place the facts reconcile cleanly.
	flow, err := gitflow.NewGitFlow(fx.sup, fx.repo.dir)
	if err != nil {
		t.Fatalf("new gitflow: %v", err)
	}
	if _, err := flow.Execute(context.Background(), gitflow.CreateAuditRef{
		Ref: "refs/cflow/" + testWF + "/quarantine/quarantine-1", Head: fx.taskHead,
	}); err != nil {
		t.Fatalf("create audit ref: %v", err)
	}
	out, err = fx.engine.Reconcile(context.Background(), scope)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(out.Faults) != 0 {
		t.Fatalf("a quarantine with its audit ref must reconcile cleanly, faults = %+v", out.Faults)
	}
}

func hasBlockingFinding(st model.State, code model.Code) bool {
	for _, f := range st.Findings {
		if f.Blocking && f.Code == code {
			return true
		}
	}
	return false
}

// TestReleaseDispositionMatrix covers the Task 21 matrix disposition rows
// not already pinned by recovery_test.go, through the real Store and Git
// facts: the Apply staging re-run (safe_to_retry), the managed process stop
// of a still-running process (blocked_drift — the row carries no OS
// identity, so the stopped settlement can never be verified and the
// crash-restarting Runtime fails closed with a user-action fault), and a
// quarantine whose audit ref is missing (a DIRTY_WORKTREE_DRIFTED blocking
// fault — the evidence must never vanish). The release matrix harness
// drives the same rows.
func TestReleaseDispositionMatrix(t *testing.T) {
	t.Run("apply staging recoverable", func(t *testing.T) {
		fx := newRecoveryFixture(t)
		fx.mutate(t, model.ApplyAppendMutation{ApplyAttempt: model.ApplyAttempt{
			ID: "apply-1", Number: 1, Status: model.ApplyStaging,
			TargetHead: fx.taskHead, IntegrationHead: fx.taskHead, StartedAt: fx.now(),
		}})
		fx.seedIntent(model.ApplyStagingCreateIntent{Apply: "apply-1"})
		out := mustReconcile(t, fx)
		requireDisposition(t, out, recovery.SafeToRetry)
	})

	t.Run("process stop of a running process fails closed", func(t *testing.T) {
		fx := newRecoveryFixture(t)
		fx.mutate(t,
			model.SessionAppendMutation{Session: model.Session{ID: "s1", Purpose: model.PurposeRepair, Status: model.SessionActive}, Provider: "fake"},
			model.ProcessAppendMutation{Process: model.ProcessRecord{ID: "rp-1", Session: "s1", Purpose: model.PurposeRepair, Status: model.ProcessStatusRunning, StartedAt: fx.now()}},
		)
		fx.seedIntent(model.ManagedProcessStopIntent{Process: "rp-1"})
		out := mustReconcile(t, fx)
		requireDisposition(t, out, recovery.BlockedDrift)
		if len(out.Faults) == 0 {
			t.Fatalf("an unverified RUNNING process row must produce a user-action fault")
		}
		for _, f := range out.Faults {
			if f.Code != model.CodeDirtyWorktreeDrifted {
				t.Fatalf("process stop fault code = %s, want DIRTY_WORKTREE_DRIFTED", f.Code)
			}
		}
	})

	t.Run("quarantine missing audit ref blocks", func(t *testing.T) {
		fx := newRecoveryFixture(t)
		fx.mutate(t, model.QuarantineAppendMutation{Quarantine: model.Quarantine{
			ID: "quarantine-1", AuditRef: "refs/cflow/" + testWF + "/quarantine/quarantine-1",
			Branch: fx.taskBranch, FromHead: fx.baseHead, ToHead: fx.taskHead,
			Code: model.CodeCommitDuringPolicyDriftWindow,
		}})
		out := mustReconcile(t, fx)
		if len(out.Faults) == 0 {
			t.Fatalf("a quarantine without its audit ref must block reconciliation")
		}
		for _, f := range out.Faults {
			if f.Code != model.CodeDirtyWorktreeDrifted {
				t.Fatalf("quarantine fault code = %s, want DIRTY_WORKTREE_DRIFTED", f.Code)
			}
		}
		// Reconcile is read-only: the quarantine evidence is never dropped.
		if st := viewState(t, fx); len(st.Quarantines) != 1 {
			t.Fatalf("the quarantine evidence was dropped: %d rows", len(st.Quarantines))
		}
	})
}

// viewState re-opens the persisted aggregate after a Reconcile (Reconcile
// never mutates anything, so the evidence must survive unchanged).
func viewState(t *testing.T, fx *recoveryFixture) model.State {
	t.Helper()
	st, err := store.Open(context.Background(), store.OpenOptions{Path: fx.dbPath, Workflow: testWF, CflowVersion: "0.0.0-dev", Now: fx.now})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	view, err := st.View(context.Background(), store.StoreQuery{})
	if err != nil {
		t.Fatalf("view: %v", err)
	}
	return view.State
}
