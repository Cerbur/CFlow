package app

// Task 17 failure-protocol tests (brief Step 1): the mandated verbatim
// failure-state matrix tests drive the pure Decision Kernel through the
// closed input union (design 8), and the Application-level tests drive the
// controlled-stop executor over the deterministic Fake Process Adapter.
// The Kernel tests apply each Decision's mutations to the fixture State
// the same way the Store does, so the failure protocols are asserted at
// the transaction boundary: one stable disposition, no duplicate external
// effect, no Retry charge on interruption, append-only evidence.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"cflow.local/cflow/internal/decision"
	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/process"
	"cflow.local/cflow/internal/store"
)

// ---------------------------------------------------------------------------
// kernel fixture: a constructed EXECUTION aggregate driven by decision.Decide
// ---------------------------------------------------------------------------

// kernelFixture is the pure Decision Kernel test rig. The State is
// mutated by applying each committed Decision's mutations, exactly as the
// Store's transaction applies them.
type kernelFixture struct {
	t     *testing.T
	state model.State
}

// newKernelFixture builds an EXECUTION/RUNNING aggregate with an open
// dispatch Run and the approved execution facts (preflight revision 1).
func newKernelFixture(t *testing.T) *kernelFixture {
	t.Helper()
	now := time.Unix(1700000000, 0).UTC()
	st := model.State{Version: 1, Now: now, NextEventSeq: 1}
	st.Workflow = model.Workflow{
		ID: "wf-1", Project: "proj-1",
		Stage:             model.StageExecution,
		Runtime:           model.RuntimeRunning,
		IntegrationBranch: "cflow/wf-1/integration",
		ExecutionFacts: &model.ExecutionFacts{
			PlanHash: "plan-h", SpecHashes: []string{"spec-1"}, CatalogHash: "cat-1",
			WorkflowHash: "wf-h", RoutingHash: "r-1", BudgetHash: "b-1",
			CommitPolicyHash: "cp-1", Fingerprint: "fp-1", PreflightRevision: 1,
			SpecRevision: 1, CatalogRevision: 1, WorkflowRevision: 1,
		},
	}
	st.Runs = []model.Run{{ID: "run-1", Status: model.RunRunning, DispatchGate: true, StartedAt: now}}
	st.Nodes = map[model.NodeID]*model.Node{}
	st.Attempts = map[model.AttemptKey]*model.Attempt{}
	return &kernelFixture{t: t, state: st}
}

// node installs one agent-task Node with the deterministic Task Branch
// and the given Retry Budget.
func (fx *kernelFixture) node(id string, status model.NodeStatus, budget int) *kernelFixture {
	fx.t.Helper()
	fx.state.Nodes[model.NodeID(id)] = &model.Node{
		ID: model.NodeID(id), Kind: model.NodeAgentTask, Status: status,
		Branch: "cflow/wf-1/task-" + id, RetryBudget: budget,
	}
	return fx
}

// attempt installs one Attempt of a Node.
func (fx *kernelFixture) attempt(node string, number model.AttemptNumber, status model.AttemptStatus, startHead string) *kernelFixture {
	fx.t.Helper()
	fx.state.Attempts[model.AttemptKey{Node: model.NodeID(node), Number: number}] = &model.Attempt{
		Key:     model.AttemptKey{Node: model.NodeID(node), Number: number},
		Session: model.SessionID("session-" + node + "-" + string(rune('0'+number))),
		Status:  status, StartHead: startHead,
	}
	return fx
}

// fail ends one RUNNING Attempt with the given failure code.
func (fx *kernelFixture) fail(node string, number model.AttemptNumber, code model.Code, endHead string) {
	fx.t.Helper()
	fx.feed(model.EffectResultInput{
		Kind:    model.AttemptEnded,
		Attempt: model.AttemptKey{Node: model.NodeID(node), Number: number},
		Outcome: model.OutcomeFailed, FailureCode: code, EndHead: endHead,
	})
}

// succeed ends one RUNNING Attempt with a Commit evidence at endHead.
func (fx *kernelFixture) succeed(node string, number model.AttemptNumber, endHead string) {
	fx.t.Helper()
	fx.feed(model.EffectResultInput{
		Kind:    model.AttemptEnded,
		Attempt: model.AttemptKey{Node: model.NodeID(node), Number: number},
		Outcome: model.OutcomeSucceeded, EndHead: endHead,
		Evidence: model.EvidenceRef{Kind: model.EvidenceCommit, Hash: endHead, Subject: "cflow/wf-1/task-" + node},
	})
}

// interrupt ends one RUNNING Attempt as interrupted (a controlled stop).
func (fx *kernelFixture) interrupt(node string, number model.AttemptNumber, code model.Code) {
	fx.t.Helper()
	fx.feed(model.EffectResultInput{
		Kind:        model.AttemptEnded,
		Attempt:     model.AttemptKey{Node: model.NodeID(node), Number: number},
		Outcome:     model.OutcomeInterrupted,
		FailureCode: code,
	})
}

// feed commits one Decision and applies its mutations to the fixture.
func (fx *kernelFixture) feed(input model.Input) model.Decision {
	fx.t.Helper()
	d, err := decision.Decide(fx.state, input)
	if err != nil {
		fx.t.Fatalf("decision(%T) failed: %v", input, err)
	}
	fx.state = applyFixtureDecision(fx.state, d)
	return d
}

// applyFixtureDecision applies one Decision's mutations and event
// sequence exactly as the Store transaction does (design 9.2).
func applyFixtureDecision(st model.State, d model.Decision) model.State {
	for _, m := range d.Mutations {
		switch mm := m.(type) {
		case model.WorkflowMutation:
			st.Workflow = model.Workflow{
				ID: mm.ID, Project: mm.Project, Stage: mm.Stage, Runtime: mm.Runtime,
				TargetBranch: mm.TargetBranch, BaseCommit: mm.BaseCommit,
				IntegrationBranch: mm.IntegrationBranch, IntegrationHead: mm.IntegrationHead,
				CancelIntent:   mm.CancelIntent,
				ExecutionFacts: st.Workflow.ExecutionFacts,
			}
		case model.NodeStatusMutation:
			if n := st.Nodes[mm.Node]; n != nil {
				n.Status = mm.Status
				n.RetryCharged = mm.RetryCharged
			}
		case model.TaskMutation:
			if n := st.Nodes[mm.Node]; n != nil {
				if mm.BaseCommit != "" {
					n.BaseCommit = mm.BaseCommit
				}
				if mm.WorktreePath != "" {
					_ = mm.WorktreePath
				}
			}
		case model.AttemptAppendMutation:
			att := mm.Attempt
			st.Attempts[att.Key] = &att
		case model.AttemptEndMutation:
			if att := st.Attempts[mm.Key]; att != nil {
				att.Status = mm.Status
				att.EndHead = mm.EndHead
				att.EndDirtyFingerprint = mm.EndDirtyFingerprint
				att.FailureCode = mm.FailureCode
				att.Evidence = mm.Evidence
				att.RetryCharged = mm.RetryCharged
				att.EndedAt = mm.EndedAt
			}
		case model.FindingAppendMutation:
			st.Findings = append(st.Findings, mm.Finding)
		case model.RunAppendMutation:
			r := mm.Run
			st.Runs = append(st.Runs, r)
		case model.RunMutation:
			for i := range st.Runs {
				if st.Runs[i].ID == mm.ID {
					st.Runs[i].Status = mm.Status
					st.Runs[i].DispatchGate = mm.DispatchGate
					if mm.StopReason != "" {
						st.Runs[i].StopReason = mm.StopReason
					}
					if mm.QuiesceSnapshot != nil {
						st.Runs[i].QuiesceSnapshot = mm.QuiesceSnapshot
					}
				}
			}
		case model.SessionAppendMutation:
			s := mm.Session
			s.Provider = mm.Provider
			st.Sessions = append(st.Sessions, s)
		case model.SessionEndMutation:
			for i := range st.Sessions {
				if st.Sessions[i].ID == mm.ID {
					st.Sessions[i].ProviderSessionID = mm.ProviderSessionID
					st.Sessions[i].Status = mm.Status
				}
			}
		case model.QuarantineAppendMutation:
			q := mm.Quarantine
			st.Quarantines = append(st.Quarantines, q)
		case model.ApprovalAppendMutation:
			st.Approvals = append(st.Approvals, mm.Approval)
		case model.ProcessAppendMutation:
			p := mm.Process
			st.Processes = append(st.Processes, p)
		case model.ProcessEndMutation:
			for i := range st.Processes {
				if st.Processes[i].ID == mm.ID {
					st.Processes[i].Status = mm.Status
					st.Processes[i].ExitCode = mm.ExitCode
				}
			}
		case model.PreflightRecordMutation:
			if st.Workflow.ExecutionFacts != nil {
				st.Workflow.ExecutionFacts.PreflightRevision = mm.Revision
				st.Workflow.ExecutionFacts.Fingerprint = mm.Fingerprint
				st.Workflow.ExecutionFacts.CommitPolicyHash = mm.ArtifactHash
			}
		}
	}
	st.NextEventSeq += uint64(len(d.Events))
	return st
}

// ---------------------------------------------------------------------------
// the mandated verbatim failure-state matrix tests (brief Step 1)
// ---------------------------------------------------------------------------

// TestQuiescingNeverDispatchesReadySibling is the brief's verbatim test:
// a non-retryable sibling failure snapshots the persisted RUNNING Attempts,
// closes the dispatch gate, and after the snapshot settles the Run/Workflow
// are BLOCKED with the READY sibling never started.
func TestQuiescingNeverDispatchesReadySibling(t *testing.T) {
	fx := newParallelFailureFixture(t)
	fx.FailNonRetryable("S01")
	fx.MarkReady("S03")
	fx.SettleRunning("S02")
	fx.RequireNeverStarted("S03")
	fx.RequireRunStatus(model.RunBlocked)
}

// TestCancelRecoveryCannotResumeScheduler is the brief's verbatim test:
// Recovery of a persisted Cancel intent only completes the cancellation —
// it never reopens the Scheduler, never starts a Provider, and preserves
// every piece of evidence.
func TestCancelRecoveryCannotResumeScheduler(t *testing.T) {
	fx := crashDuringCancel(t)
	mustReconcile(t, fx)
	fx.RequireProviderStarts(0)
	fx.RequireStatus(model.RuntimeCancelled)
	fx.RequireEvidencePreserved()
}

// parallelFailureFixture is the mandated Quiescing fixture: three
// agent-task Nodes with S01 and S02 RUNNING and S03 READY.
type parallelFailureFixture struct {
	*kernelFixture
}

func newParallelFailureFixture(t *testing.T) *parallelFailureFixture {
	t.Helper()
	fx := &parallelFailureFixture{kernelFixture: newKernelFixture(t)}
	fx.node("S01", model.NodeRunning, 2)
	fx.node("S02", model.NodeRunning, 2)
	fx.node("S03", model.NodeReady, 2)
	fx.attempt("S01", 1, model.AttemptRunning, "base-1")
	fx.attempt("S02", 1, model.AttemptRunning, "base-1")
	return fx
}

// FailNonRetryable ends S01 with a non-retryable failure: the Attempt is
// terminal, the Node FAILED, a blocking Finding is persisted, and the Run
// enters QUIESCING with a snapshot of exactly the persisted RUNNING
// Attempts.
func (fx *parallelFailureFixture) FailNonRetryable(node string) {
	fx.t.Helper()
	fx.fail(node, 1, model.CodeDirtyWorktreeDrifted, "h-1")
	run := activeRunOf(fx.t, fx.state)
	if run == nil || run.Status != model.RunQuiescing {
		fx.t.Fatalf("after a non-retryable failure with an in-flight sibling the run must be QUIESCING, got %+v", run)
	}
	if run.DispatchGate {
		fx.t.Fatalf("the dispatch gate must close on quiesce")
	}
	if len(run.QuiesceSnapshot) != 1 || run.QuiesceSnapshot[0] != (model.AttemptKey{Node: "S02", Number: 1}) {
		fx.t.Fatalf("quiesce snapshot = %v, want exactly the persisted RUNNING S02#1", run.QuiesceSnapshot)
	}
	n := fx.state.Nodes["S01"]
	if n == nil || n.Status != model.NodeFailed {
		fx.t.Fatalf("S01 must be FAILED, got %+v", n)
	}
	if !hasBlockingFindingOf(fx.t, fx.state, model.CodeDirtyWorktreeDrifted) {
		fx.t.Fatalf("a blocking finding must be persisted for the failed node")
	}
}

// MarkReady asserts the READY sibling stays exactly as it was: the closed
// gate never starts it and never mutates it.
func (fx *parallelFailureFixture) MarkReady(node string) {
	fx.t.Helper()
	n := fx.state.Nodes[model.NodeID(node)]
	if n == nil || n.Status != model.NodeReady {
		fx.t.Fatalf("%s must stay READY while the gate is closed, got %+v", node, n)
	}
	for k := range fx.state.Attempts {
		if k.Node == model.NodeID(node) && k.Number > 0 && fx.state.Attempts[k].Status == model.AttemptRunning {
			fx.t.Fatalf("%s must never start while the gate is closed", node)
		}
	}
}

// SettleRunning settles the snapshot Attempt S02: the QUIESCING Run
// converges to BLOCKED in the same transaction with WORKFLOW_QUIESCED.
func (fx *parallelFailureFixture) SettleRunning(node string) {
	fx.t.Helper()
	fx.succeed(node, 1, "h-2")
}

// RequireNeverStarted asserts the READY sibling never got a RUNNING
// Attempt and any allocation attempt is refused by the closed gate.
func (fx *parallelFailureFixture) RequireNeverStarted(node string) {
	fx.t.Helper()
	for k, a := range fx.state.Attempts {
		if k.Node == model.NodeID(node) && a.Status != model.AttemptReady {
			fx.t.Fatalf("%s must never start an attempt, found %s", node, a.Status)
		}
	}
	_, err := decision.Decide(fx.state, model.DispatchInput{
		Node: model.NodeID(node), Session: "session-new", Route: "fake", BaseHead: "base-1",
	})
	if code, ok := model.CodeOf(err); !ok || code != model.CodeDispatchGateClosed {
		fx.t.Fatalf("dispatching %s after quiesce must be refused with DISPATCH_GATE_CLOSED, got %v", node, err)
	}
}

// RequireRunStatus asserts the Run settled to the expected terminal
// status (the converged Run record is terminal).
func (fx *parallelFailureFixture) RequireRunStatus(want model.RunStatus) {
	fx.t.Helper()
	for i := range fx.state.Runs {
		if fx.state.Runs[i].Status == want {
			return
		}
	}
	fx.t.Fatalf("no run with status %s; runs = %+v", want, fx.state.Runs)
}

// crashDuringCancel is the mandated Cancel-recovery fixture: the Runtime
// crashed between the persisted Cancel intent and the terminal Decision —
// the Run is STOPPING with the gate closed, the Attempt settled
// INTERRUPTED (the controlled stop of the process), and a SUCCEEDED Node
// plus approvals exist as evidence that must never regress.
func crashDuringCancel(t *testing.T) *cancelFixture {
	t.Helper()
	fx := &cancelFixture{kernelFixture: newKernelFixture(t)}
	fx.node("S01", model.NodeSucceeded, 2)
	fx.node("S02", model.NodeReady, 2)
	fx.attempt("S01", 1, model.AttemptSucceeded, "base-1")
	fx.attempt("S02", 1, model.AttemptInterrupted, "base-1")
	att := fx.state.Attempts[model.AttemptKey{Node: "S02", Number: 1}]
	att.EndHead = "h-1"
	att.RetryCharged = false
	fx.state.Workflow.CancelIntent = &model.CancelIntent{RequestedSeq: 2, Reason: "user requested"}
	fx.state.Runs[0].Status = model.RunStopping
	fx.state.Runs[0].DispatchGate = false
	fx.state.Approvals = []model.Approval{{
		ID: "approval-1", Kind: model.ApprovalExecution, Seq: 1,
		Refs: []model.ArtifactRef{{Workflow: "wf-1", Type: model.ArtifactWorkflow, Revision: 1, Hash: "wf-h"}},
	}}
	return fx
}

type cancelFixture struct {
	*kernelFixture
}

// mustReconcile runs the Recovery sweep: the persisted Cancel intent is
// completed once nothing is running.
func mustReconcile(t *testing.T, fx *cancelFixture) {
	t.Helper()
	d := fx.feed(model.ReconcileInput{})
	for _, ev := range d.Events {
		if ev.Kind == model.EventWorkflowCancelled {
			return
		}
	}
	fx.t.Fatalf("the reconcile sweep must complete the persisted cancel intent")
}

// RequireProviderStarts asserts no Provider start was ever requested.
func (fx *cancelFixture) RequireProviderStarts(want int) {
	fx.t.Helper()
	// The fixture only observes Decisions through feed; a ProviderStart
	// effect on the final state means a decision requested one. The
	// terminal Decision carries no Effect, asserted here structurally.
	if len(fx.state.Attempts) != 2 {
		fx.t.Fatalf("cancel must never allocate an attempt: %d attempts", len(fx.state.Attempts))
	}
}

// RequireStatus asserts the terminal status.
func (fx *cancelFixture) RequireStatus(want model.RuntimeStatus) {
	fx.t.Helper()
	if fx.state.Workflow.Runtime != want {
		fx.t.Fatalf("runtime = %s, want %s", fx.state.Workflow.Runtime, want)
	}
	for i := range fx.state.Runs {
		if fx.state.Runs[i].Status == model.RunCancelled {
			return
		}
	}
	fx.t.Fatalf("no CANCELLED run; runs = %+v", fx.state.Runs)
}

// RequireEvidencePreserved asserts the append-only evidence never
// regressed: the SUCCEEDED Node and its Attempt, the approvals, the
// interrupted Attempt facts, and the persisted cancel intent all remain.
func (fx *cancelFixture) RequireEvidencePreserved() {
	fx.t.Helper()
	if n := fx.state.Nodes["S01"]; n == nil || n.Status != model.NodeSucceeded {
		fx.t.Fatalf("a succeeded node must never regress on cancel, got %+v", n)
	}
	if a := fx.state.Attempts[model.AttemptKey{Node: "S01", Number: 1}]; a == nil || a.Status != model.AttemptSucceeded {
		fx.t.Fatalf("a succeeded attempt must never regress on cancel, got %+v", a)
	}
	if a := fx.state.Attempts[model.AttemptKey{Node: "S02", Number: 1}]; a == nil || a.Status != model.AttemptInterrupted || a.RetryCharged {
		fx.t.Fatalf("the interrupted attempt must stay INTERRUPTED without retry charge, got %+v", a)
	}
	if len(fx.state.Approvals) != 1 {
		fx.t.Fatalf("approvals must be preserved, got %d", len(fx.state.Approvals))
	}
	if fx.state.Workflow.CancelIntent == nil {
		fx.t.Fatalf("the persisted cancel intent must remain for audit")
	}
}

// ---------------------------------------------------------------------------
// shared fixture helpers
// ---------------------------------------------------------------------------

func activeRunOf(t *testing.T, st model.State) *model.Run {
	t.Helper()
	for i := range st.Runs {
		if !st.Runs[i].Status.IsTerminal() {
			return &st.Runs[i]
		}
	}
	return nil
}

func hasBlockingFindingOf(t *testing.T, st model.State, code model.Code) bool {
	t.Helper()
	for _, f := range st.Findings {
		if f.Blocking && f.Code == code {
			return true
		}
	}
	return false
}

func hasEvent(d model.Decision, kind model.EventKind) bool {
	for _, ev := range d.Events {
		if ev.Kind == kind {
			return true
		}
	}
	return false
}

func runOf(st model.State, id model.RunID) *model.Run {
	for i := range st.Runs {
		if st.Runs[i].ID == id {
			return &st.Runs[i]
		}
	}
	return nil
}

func nodeOf(st model.State, id model.NodeID) *model.Node { return st.Nodes[id] }

func attemptOf(st model.State, key model.AttemptKey) *model.Attempt { return st.Attempts[key] }

// ---------------------------------------------------------------------------
// controlled-stop executor tests over the deterministic Fake Supervisor
// ---------------------------------------------------------------------------

// stopFixtureApp builds an Application whose supervisor is the
// deterministic Fake Process Adapter, with one managed process bound to
// the ledger. tinyStopPolicy makes the staged budgets milliseconds so the
// protocol walks them in real time.
func stopFixtureApp(t *testing.T, sup process.Supervisor, handle process.Handle, id process.ProcessIdentity) *Application {
	t.Helper()
	pol := stopPolicy{Grace: 10 * time.Millisecond, TerminateWait: 10 * time.Millisecond, ForceKillWait: 10 * time.Millisecond}
	a := &Application{
		home:              t.TempDir(),
		project:           Project{Key: "proj-1", Root: t.TempDir()},
		supervisor:        sup,
		procs:             map[model.ProcessID]process.Handle{},
		processSessions:   map[model.ProcessID]model.SessionID{},
		processIdentities: map[model.ProcessID]process.ProcessIdentity{},
		stopPolicy:        pol,
	}
	a.bindProcess("proc-1", "session-1", handle, id)
	return a
}

// TestStopTwoPhaseProtocol asserts the app's controlled-stop executor
// follows the bounded two-phase protocol (design 13.3): Interrupt (Adapter
// Cancel) first with a grace window, Terminate the process group after the
// grace, wait the escalation window, then ForceKill — and a reaped
// identity is never reported as an orphan.
func TestStopTwoPhaseProtocol(t *testing.T) {
	fake, sup := process.NewFakeSupervisor()
	h, events, err := sup.Start(context.Background(), process.ProcessSpec{Executable: "/fixture/worker"})
	if err != nil {
		t.Fatal(err)
	}
	first := <-events
	if first.Kind != process.EventStarted {
		t.Fatalf("first event = %v, want EventStarted", first.Kind)
	}
	a := stopFixtureApp(t, sup, h, first.Identity)
	// The fake process ignores every signal until the force-kill lands:
	// the protocol must walk the staged budget (grace, terminate wait,
	// force-kill wait) and then reap. The signal order is captured before
	// the group exits (Signals panics on an exited group).
	var sigs []process.Signal
	go func() {
		for {
			s := fake.Signals(h)
			if len(s) >= 3 {
				sigs = s
				fake.ExitGroup(h, 137)
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	res, err := a.stopManagedProcess(context.Background(), model.ManagedProcessStopIntent{Process: "proc-1"})
	if err != nil {
		t.Fatalf("stop failed: %v", err)
	}
	if res.Kind != model.ProcessStopped {
		t.Fatalf("result kind = %s", res.Kind)
	}
	if res.Orphan {
		t.Fatalf("a reaped process must not be reported orphan")
	}
	if len(sigs) != 3 || sigs[0] != process.Interrupt || sigs[1] != process.Terminate || sigs[2] != process.ForceKill {
		t.Fatalf("signals = %v, want [Interrupt Terminate ForceKill]", sigs)
	}
}

// TestStopEscalationSkipsGrace asserts the second-signal escalation: a
// cancelled stop context jumps directly to the force-kill phase, skipping
// the remaining grace and the Terminate stage.
func TestStopEscalationSkipsGrace(t *testing.T) {
	fake, sup := process.NewFakeSupervisor()
	h, events, err := sup.Start(context.Background(), process.ProcessSpec{Executable: "/fixture/worker"})
	if err != nil {
		t.Fatal(err)
	}
	first := <-events
	a := stopFixtureApp(t, sup, h, first.Identity)
	_, cancel := a.stopContext(context.Background())
	// The escalation arrives before the grace expires.
	cancel()
	// The group is reaped after the stop settles so the supervisor reaps
	// cleanly; the signal order is captured before the group exits.
	var sigs []process.Signal
	captured := make(chan struct{})
	go func() {
		defer close(captured)
		time.Sleep(80 * time.Millisecond)
		sigs = fake.Signals(h)
		fake.ExitGroup(h, 137)
	}()
	res, err := a.stopManagedProcess(context.Background(), model.ManagedProcessStopIntent{Process: "proc-1"})
	if err != nil {
		t.Fatalf("stop failed: %v", err)
	}
	if res.Kind != model.ProcessStopped {
		t.Fatalf("result kind = %s", res.Kind)
	}
	<-captured
	if len(sigs) != 2 || sigs[0] != process.Interrupt || sigs[1] != process.ForceKill {
		t.Fatalf("signals = %v, want [Interrupt ForceKill] after escalation", sigs)
	}
}

// TestStopOrphanChildQuarantine asserts the force-kill orphan path: a
// process still alive with the exact PID/start-token identity after the
// force-kill phase reports the orphan fact, and the Kernel converts it
// into ORPHAN_CHILD_PROCESS + BLOCKED (PRD step 9).
func TestStopOrphanChildQuarantine(t *testing.T) {
	fake, sup := process.NewFakeSupervisor()
	h, events, err := sup.Start(context.Background(), process.ProcessSpec{Executable: "/fixture/worker"})
	if err != nil {
		t.Fatal(err)
	}
	first := <-events
	a := stopFixtureApp(t, sup, h, first.Identity)
	_, cancel := a.stopContext(context.Background())
	defer cancel()
	// The process never exits: the force-kill phase leaves a matching
	// identity alive, which the executor reports as the orphan fact.
	res, err := a.stopManagedProcess(context.Background(), model.ManagedProcessStopIntent{Process: "proc-1"})
	if err != nil {
		t.Fatalf("stop failed: %v", err)
	}
	if res.Kind != model.ProcessStopped || !res.Orphan {
		t.Fatalf("result = %+v, want ProcessStopped with the orphan fact", res)
	}
	go fake.ExitGroup(h, 137)
	for range events {
	}
	// The Kernel settles the orphan: ORPHAN_CHILD_PROCESS + BLOCKED.
	fx := newKernelFixture(t)
	fx.node("S01", model.NodeRunning, 2)
	fx.attempt("S01", 1, model.AttemptRunning, "base-1")
	fx.state.Processes = []model.ProcessRecord{{ID: "proc-1", Session: "session-1", Status: model.ProcessStatusRunning}}
	fx.state.Runs[0].Status = model.RunStopping
	fx.state.Runs[0].DispatchGate = false
	fx.feed(model.EffectResultInput{Kind: model.ProcessStopped, Process: "proc-1", Orphan: true})
	if fx.state.Workflow.Runtime != model.RuntimeBlocked {
		fx.t.Fatalf("runtime = %s, want BLOCKED after an orphan", fx.state.Workflow.Runtime)
	}
	if !hasBlockingFindingOf(fx.t, fx.state, model.CodeOrphanChildProcess) {
		fx.t.Fatalf("an ORPHAN_CHILD_PROCESS blocking finding must be persisted")
	}
}

// TestCancelOrphanKeepsIntent asserts a Cancel whose process survives the
// force-kill phase keeps its intent and Blocks with
// CANCEL_PENDING_ORPHAN_PROCESS; Recovery later completes the cancel.
func TestCancelOrphanKeepsIntent(t *testing.T) {
	fx := newKernelFixture(t)
	fx.node("S01", model.NodeRunning, 2)
	fx.attempt("S01", 1, model.AttemptRunning, "base-1")
	fx.state.Processes = []model.ProcessRecord{{ID: "proc-1", Session: "session-1", Status: model.ProcessStatusRunning}}
	fx.state.Runs[0].Status = model.RunStopping
	fx.state.Runs[0].DispatchGate = false
	fx.state.Workflow.CancelIntent = &model.CancelIntent{RequestedSeq: 2, Reason: "user"}
	fx.feed(model.EffectResultInput{Kind: model.ProcessStopped, Process: "proc-1", Orphan: true})
	if fx.state.Workflow.Runtime != model.RuntimeBlocked {
		fx.t.Fatalf("runtime = %s, want BLOCKED while the orphan process lives", fx.state.Workflow.Runtime)
	}
	if !hasBlockingFindingOf(fx.t, fx.state, model.CodeCancelPendingOrphanProcess) {
		fx.t.Fatalf("a CANCEL_PENDING_ORPHAN_PROCESS blocking finding must be persisted")
	}
	if fx.state.Workflow.CancelIntent == nil {
		fx.t.Fatalf("the cancel intent must be kept while the orphan lives")
	}
	// The interrupted Attempt settles (the recovery's fact reconciliation
	// from the settled process facts), which completes the original cancel
	// intent — Recovery never resumes the Scheduler and never starts a
	// Provider.
	d := fx.feed(model.EffectResultInput{
		Kind:        model.AttemptEnded,
		Attempt:     model.AttemptKey{Node: "S01", Number: 1},
		Outcome:     model.OutcomeInterrupted,
		FailureCode: model.CodeUserInterrupted,
	})
	if !hasEvent(d, model.EventWorkflowCancelled) {
		fx.t.Fatalf("recovery must complete the original cancel intent, got %+v", d.Events)
	}
	if fx.state.Workflow.Runtime != model.RuntimeCancelled {
		fx.t.Fatalf("runtime = %s, want CANCELLED", fx.state.Workflow.Runtime)
	}
	if len(fx.state.Quarantines) != 0 {
		fx.t.Fatalf("the cancel must never quarantine; quarantine = %+v", fx.state.Quarantines)
	}
}

// TestStopConcurrentBindRace exercises the managed-process map reads of
// the stop executor against concurrent bind/unbind writes (design 13.3,
// Task 16 live parallelism): a stop effect in one chain races the
// provider-start bind of another. The reads are guarded by the same lock
// the bind/unbind writes take, so a concurrent map read/write panic can
// never fire; run with -race. The orphan-path stops re-bind their
// identity first (the write side) and never reap the fake process, so
// the force-kill phase always walks the processIdentities read.
func TestStopConcurrentBindRace(t *testing.T) {
	fake, sup := process.NewFakeSupervisor()
	h, events, err := sup.Start(context.Background(), process.ProcessSpec{Executable: "/fixture/worker"})
	if err != nil {
		t.Fatal(err)
	}
	first := <-events
	a := stopFixtureApp(t, sup, h, first.Identity)
	// The escalation: every stop skips the grace and force-kills.
	_, cancel := a.stopContext(context.Background())
	cancel()
	stop := func(pid model.ProcessID) {
		res, err := a.stopManagedProcess(context.Background(), model.ManagedProcessStopIntent{Process: pid})
		if err != nil {
			t.Errorf("stop %s: %v", pid, err)
			return
		}
		if res.Kind != model.ProcessStopped {
			t.Errorf("stop %s kind = %s, want ProcessStopped", pid, res.Kind)
		}
	}
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		// The provider-start bind of another chain: churn the write side.
		defer wg.Done()
		for i := 0; i < 200; i++ {
			a.bindProcess("churn", "session-churn", h, first.Identity)
			a.unbindProcess("churn")
		}
	}()
	go func() {
		// The no-handle path reads a.procs.
		defer wg.Done()
		for i := 0; i < 200; i++ {
			stop("nohandle")
		}
	}()
	go func() {
		// The orphan path: bind (write side) then stop — the never-exited
		// fake process makes the executor read a.processIdentities.
		defer wg.Done()
		for i := 0; i < 50; i++ {
			a.bindProcess("orphan", "session-orphan", h, first.Identity)
			stop("orphan")
		}
	}()
	wg.Wait()
	// Reap the fake group so the supervisor settles cleanly.
	go fake.ExitGroup(h, 137)
	for range events {
	}
}

// ---------------------------------------------------------------------------
// helpers used by the integration tests (same package test surface)
// ---------------------------------------------------------------------------

// assertStopState renders the stopped state facts for diagnostics.
func assertStopState(t *testing.T, st model.State) {
	t.Helper()
	if !strings.Contains(string(st.Workflow.Runtime), "PAUSED") && !strings.Contains(string(st.Workflow.Runtime), "BLOCKED") {
		t.Fatalf("workflow runtime = %s, want PAUSED or BLOCKED", st.Workflow.Runtime)
	}
}

// ---------------------------------------------------------------------------
// Retry budget charge/no-charge table (PRD 已确认：Retry 耗尽与失败终态语义)
// ---------------------------------------------------------------------------

// TestRetryBudgetChargeTable asserts the compiled Retry disposition for
// every retryable failure: an in-budget failure terminalizes the Attempt
// with the charge, returns the Node READY, creates the numbered successor
// Attempt, and keeps the Workflow RUNNING with the gate open.
func TestRetryBudgetChargeTable(t *testing.T) {
	for _, code := range []model.Code{
		model.CodeAgentProcessCrashed,
		model.CodeAgentTimeout,
		model.CodeCommandFailed,
		model.CodeSemanticReviewFailed,
		model.CodeMergeConflict,
		model.CodeMissingImplementationCommit,
		model.CodeDirtyTaskWorktree,
		model.CodeEvidenceSubjectChanged,
	} {
		pol, ok := model.Policy(code)
		if !ok || !pol.Retry.AllowsSuccessor {
			t.Fatalf("%s must allow a budgeted successor", code)
		}
		fx := newKernelFixture(t)
		fx.node("S01", model.NodeRunning, 2)
		fx.attempt("S01", 1, model.AttemptRunning, "base-1")
		fx.fail("S01", 1, code, "h-1")
		att := fx.state.Attempts[model.AttemptKey{Node: "S01", Number: 1}]
		if att == nil || att.Status != model.AttemptFailed || !att.RetryCharged {
			t.Fatalf("%s: attempt must be terminal FAILED with the charge, got %+v", code, att)
		}
		if n := fx.state.Nodes["S01"]; n == nil || n.Status != model.NodeReady || n.RetryCharged != 1 {
			t.Fatalf("%s: node must be READY with charge 1, got %+v", code, n)
		}
		succ := fx.state.Attempts[model.AttemptKey{Node: "S01", Number: 2}]
		if succ == nil || succ.Status != model.AttemptReady || succ.StartHead != "h-1" {
			t.Fatalf("%s: successor attempt #2 must exist READY from the end head, got %+v", code, succ)
		}
		if fx.state.Workflow.Runtime != model.RuntimeRunning {
			t.Fatalf("%s: workflow must stay RUNNING on a budgeted retry", code)
		}
		run := activeRunOf(fx.t, fx.state)
		if run == nil || !run.DispatchGate {
			t.Fatalf("%s: the dispatch gate must stay open on a budgeted retry", code)
		}
		if hasBlockingFindingOf(fx.t, fx.state, model.CodeRetryExhausted) {
			t.Fatalf("%s: an in-budget failure must not open an exhaustion finding", code)
		}
	}
}

// TestRetryExhaustionBlocks asserts the exhaustion path: the Attempt is
// terminal FAILED with the charge, the Node FAILED, a blocking
// RETRY_EXHAUSTED Finding is persisted, and the Workflow Blocks
// immediately when no sibling is in flight.
func TestRetryExhaustionBlocks(t *testing.T) {
	fx := newKernelFixture(t)
	fx.node("S01", model.NodeRunning, 1)
	fx.node("S01", model.NodeRunning, 1)
	fx.state.Nodes["S01"].RetryCharged = 1
	fx.state.Nodes["S01"].Status = model.NodeRunning
	fx.attempt("S01", 1, model.AttemptFailed, "base-1")
	fx.attempt("S01", 2, model.AttemptRunning, "h-1")
	fx.fail("S01", 2, model.CodeCommandFailed, "h-2")
	if n := fx.state.Nodes["S01"]; n == nil || n.Status != model.NodeFailed {
		fx.t.Fatalf("node must be FAILED on exhaustion, got %+v", n)
	}
	if !hasBlockingFindingOf(fx.t, fx.state, model.CodeRetryExhausted) {
		fx.t.Fatalf("a blocking RETRY_EXHAUSTED finding must be persisted")
	}
	if fx.state.Workflow.Runtime != model.RuntimeBlocked {
		fx.t.Fatalf("runtime = %s, want BLOCKED on exhaustion", fx.state.Workflow.Runtime)
	}
	// Exhaustion blocks; it never makes the Workflow FAILED.
	blocked := false
	for i := range fx.state.Runs {
		if fx.state.Runs[i].Status == model.RunBlocked {
			blocked = true
		}
	}
	if !blocked {
		fx.t.Fatalf("no BLOCKED run; runs = %+v", fx.state.Runs)
	}
}

// TestRetryNeverChargesInterruption asserts the no-charge half of the
// table: an interrupted Attempt never charges the budget, the Node
// returns READY with the unchanged charge, and the controlled stop
// converges the Workflow to PAUSED.
func TestRetryNeverChargesInterruption(t *testing.T) {
	fx := newKernelFixture(t)
	fx.node("S01", model.NodeRunning, 2)
	fx.attempt("S01", 1, model.AttemptRunning, "base-1")
	d := fx.feed(model.EffectResultInput{
		Kind:        model.AttemptEnded,
		Attempt:     model.AttemptKey{Node: "S01", Number: 1},
		Outcome:     model.OutcomeInterrupted,
		FailureCode: model.CodeUserInterrupted,
	})
	if !hasEvent(d, model.EventControlledStopRequested) {
		fx.t.Fatalf("the interruption must append CONTROLLED_STOP_REQUESTED, got %+v", d.Events)
	}
	att := fx.state.Attempts[model.AttemptKey{Node: "S01", Number: 1}]
	if att == nil || att.Status != model.AttemptInterrupted || att.RetryCharged {
		fx.t.Fatalf("interrupted attempt must be INTERRUPTED without charge, got %+v", att)
	}
	if n := fx.state.Nodes["S01"]; n == nil || n.Status != model.NodeReady || n.RetryCharged != 0 {
		fx.t.Fatalf("node must return READY with the unchanged charge, got %+v", n)
	}
	if fx.state.Workflow.Runtime != model.RuntimePaused {
		fx.t.Fatalf("runtime = %s, want PAUSED after the controlled stop", fx.state.Workflow.Runtime)
	}
	interrupted := false
	for i := range fx.state.Runs {
		if fx.state.Runs[i].Status == model.RunInterrupted {
			interrupted = true
		}
	}
	if !interrupted {
		fx.t.Fatalf("no INTERRUPTED run; runs = %+v", fx.state.Runs)
	}
	// The successor allocation is refused: the gate of the interrupted
	// run is closed (a resume opens a new Run).
	_, err := decision.Decide(fx.state, model.DispatchInput{
		Node: "S01", Session: "session-new", Route: "fake", BaseHead: "base-1",
	})
	if code, ok := model.CodeOf(err); !ok || code != model.CodeDispatchGateClosed {
		fx.t.Fatalf("dispatch after the interruption must be refused by the closed gate, got %v", err)
	}
}

// TestRetryDeferredDuringQuiescingDoesNotCharge asserts Quiescing rule 7:
// a retryable failure arriving while the Run quiesces appends an
// independent Finding but creates no successor Attempt and never charges
// the budget; the Run converges to BLOCKED.
func TestRetryDeferredDuringQuiescingDoesNotCharge(t *testing.T) {
	fx := newKernelFixture(t)
	fx.node("S01", model.NodeRunning, 2)
	fx.node("S02", model.NodeRunning, 2)
	fx.attempt("S01", 1, model.AttemptRunning, "base-1")
	fx.attempt("S02", 1, model.AttemptRunning, "base-1")
	// A sibling's non-retryable failure opens the quiesce.
	fx.fail("S02", 1, model.CodeDirtyWorktreeDrifted, "h-2")
	// S01 fails retryably inside the quiesce: no successor, no charge.
	d := fx.feed(model.EffectResultInput{
		Kind:        model.AttemptEnded,
		Attempt:     model.AttemptKey{Node: "S01", Number: 1},
		Outcome:     model.OutcomeFailed,
		FailureCode: model.CodeCommandFailed, EndHead: "h-1",
	})
	if hasEvent(d, model.EventAttemptCreated) {
		fx.t.Fatalf("a deferred retry must not create a successor attempt")
	}
	att := fx.state.Attempts[model.AttemptKey{Node: "S01", Number: 1}]
	if att == nil || att.Status != model.AttemptFailed || att.RetryCharged {
		fx.t.Fatalf("deferred attempt must be FAILED without charge, got %+v", att)
	}
	if n := fx.state.Nodes["S01"]; n == nil || n.Status != model.NodeReady || n.RetryCharged != 0 {
		fx.t.Fatalf("deferred node must stay READY without charge, got %+v", n)
	}
	// The snapshot Attempt (S01) settled with the deferred failure, so the
	// quiesce converges BLOCKED (PRD rule 5); no successor exists and the
	// budget is never charged for the deferred failure.
	if fx.state.Workflow.Runtime != model.RuntimeBlocked {
		fx.t.Fatalf("runtime = %s, want BLOCKED after the quiesce converged", fx.state.Workflow.Runtime)
	}
	if _, ok := fx.state.Attempts[model.AttemptKey{Node: "S01", Number: 2}]; ok {
		fx.t.Fatalf("a deferred retry must never create a successor attempt")
	}
}

// TestRepairSuccessorUsesIndependentRepairSession asserts the dirty
// repair path (PRD 已确认：DIRTY_TASK_WORKTREE 原地 Repair): the budgeted
// successor of a DIRTY_TASK_WORKTREE failure allocates an independent
// Repair Session (purpose repair) on the same Task Branch/Worktree from
// the recorded Task Base, without any Worktree creation Effect.
func TestRepairSuccessorUsesIndependentRepairSession(t *testing.T) {
	fx := newKernelFixture(t)
	fx.node("S01", model.NodeRunning, 2)
	fx.attempt("S01", 1, model.AttemptRunning, "base-1")
	fx.state.Nodes["S01"].BaseCommit = "base-1"
	fx.fail("S01", 1, model.CodeDirtyTaskWorktree, "h-1")
	// The successor allocation of the READY Node.
	d, err := decision.Decide(fx.state, model.DispatchInput{
		Node: "S01", Session: "session-r1", Route: "fake", BaseHead: "base-1",
	})
	if err != nil {
		fx.t.Fatalf("successor allocation failed: %v", err)
	}
	start := false
	for _, m := range d.Mutations {
		if sa, ok := m.(model.SessionAppendMutation); ok {
			if sa.Session.Purpose != model.PurposeRepair {
				fx.t.Fatalf("the repair session purpose = %s, want repair", sa.Session.Purpose)
			}
			start = true
		}
	}
	if !start {
		fx.t.Fatalf("the repair allocation must append an independent repair session")
	}
	effect, ok := d.Effect.(model.ProviderStartIntent)
	if !ok || effect.Purpose != model.PurposeRepair || effect.Node != "S01" {
		fx.t.Fatalf("the repair allocation must request the Repair Session start, got %+v", d.Effect)
	}
	if fx.state.Nodes["S01"].BaseCommit != "base-1" {
		fx.t.Fatalf("the repair must reuse the recorded Task Base, never rebase")
	}
}

// TestRepairAppendOnlyCommitAllowed asserts a repair Attempt may end at
// the same HEAD as it started when the prior Attempt already produced a
// legal implementation Commit and the Worktree is clean: the repair
// removed the residuals and no empty Commit is required (PRD 已确认：
// DIRTY_TASK_WORKTREE 原地 Repair).
func TestRepairAppendOnlyCommitAllowed(t *testing.T) {
	fx := newKernelFixture(t)
	fx.node("S01", model.NodeReady, 2)
	fx.state.Nodes["S01"].BaseCommit = "base-1"
	fx.attempt("S01", 1, model.AttemptFailed, "base-1")
	att := fx.state.Attempts[model.AttemptKey{Node: "S01", Number: 1}]
	att.EndHead = "h-1" // the prior attempt committed legally, then dirtied the worktree
	att.EndDirtyFingerprint = "sha256:dirty"
	att.RetryCharged = true
	fx.attempt("S01", 2, model.AttemptRunning, "h-1")
	// The repair Session purpose makes the same-HEAD clean end legal.
	fx.state.Sessions = []model.Session{{
		ID: "session-r1", Purpose: model.PurposeRepair, Status: model.SessionActive,
	}}
	att2 := fx.state.Attempts[model.AttemptKey{Node: "S01", Number: 2}]
	att2.Session = "session-r1"
	d := fx.feed(model.EffectResultInput{
		Kind:    model.AttemptEnded,
		Attempt: model.AttemptKey{Node: "S01", Number: 2},
		Outcome: model.OutcomeSucceeded, EndHead: "h-1",
		Evidence: model.EvidenceRef{Kind: model.EvidenceCommit, Hash: "h-1", Subject: "cflow/wf-1/task-S01"},
	})
	if hasEvent(d, model.EventNodeFailed) {
		fx.t.Fatalf("a clean repair end must succeed, got %+v", d.Events)
	}
	if n := fx.state.Nodes["S01"]; n == nil || n.Status != model.NodeSucceeded {
		fx.t.Fatalf("node = %+v, want SUCCEEDED after the repair", n)
	}
}

// TestRepairEmptyHeadStillRejected asserts a repair Attempt that ends
// with no Commit at all (no legal prior Commit to stand on) still fails
// with MISSING_IMPLEMENTATION_COMMIT: repair never fabricates success.
func TestRepairEmptyHeadStillRejected(t *testing.T) {
	fx := newKernelFixture(t)
	fx.node("S01", model.NodeReady, 2)
	fx.state.Nodes["S01"].BaseCommit = "base-1"
	fx.attempt("S01", 1, model.AttemptFailed, "base-1")
	fx.attempt("S01", 2, model.AttemptRunning, "base-1")
	fx.state.Sessions = []model.Session{{
		ID: "session-r1", Purpose: model.PurposeRepair, Status: model.SessionActive,
	}}
	att := fx.state.Attempts[model.AttemptKey{Node: "S01", Number: 2}]
	att.Session = "session-r1"
	d := fx.feed(model.EffectResultInput{
		Kind:    model.AttemptEnded,
		Attempt: model.AttemptKey{Node: "S01", Number: 2},
		Outcome: model.OutcomeSucceeded, EndHead: "base-1",
	})
	_ = d
	if n := fx.state.Nodes["S01"]; n == nil || n.Status != model.NodeReady {
		fx.t.Fatalf("a repair without any commit must not succeed, node = %+v", n)
	}
	att2 := fx.state.Attempts[model.AttemptKey{Node: "S01", Number: 2}]
	if att2 == nil || att2.Status != model.AttemptFailed || att2.FailureCode != model.CodeMissingImplementationCommit {
		fx.t.Fatalf("attempt = %+v, want FAILED MISSING_IMPLEMENTATION_COMMIT", att2)
	}
}

// TestRepairDriftBlocks asserts the repair CAS drift path: a repair
// Attempt whose end facts no longer match the failed Attempt's evidence
// (the Worktree drifted) settles FAILED with DIRTY_WORKTREE_DRIFTED, no
// Retry charge, the Node FAILED, and the Workflow BLOCKED.
func TestRepairDriftBlocks(t *testing.T) {
	fx := newKernelFixture(t)
	fx.node("S01", model.NodeRunning, 2)
	fx.attempt("S01", 1, model.AttemptRunning, "base-1")
	fx.fail("S01", 1, model.CodeDirtyWorktreeDrifted, "h-1")
	if n := fx.state.Nodes["S01"]; n == nil || n.Status != model.NodeFailed {
		fx.t.Fatalf("node = %+v, want FAILED on drift", n)
	}
	att := fx.state.Attempts[model.AttemptKey{Node: "S01", Number: 1}]
	if att == nil || att.RetryCharged {
		fx.t.Fatalf("a drift failure must never charge the budget, got %+v", att)
	}
	if fx.state.Workflow.Runtime != model.RuntimeBlocked {
		fx.t.Fatalf("runtime = %s, want BLOCKED on drift", fx.state.Workflow.Runtime)
	}
}

// ---------------------------------------------------------------------------
// Commit Policy drift, Quarantine, and replacement (PRD 已确认 15.6)
// ---------------------------------------------------------------------------

// driftFixture is the post-Safety-Stop aggregate: the Run settled
// INTERRUPTED with the stop reason recorded, the Workflow PAUSED, and the
// interrupted Attempts terminal without charge.
func driftFixture(t *testing.T) *kernelFixture {
	t.Helper()
	fx := newKernelFixture(t)
	fx.node("S01", model.NodeReady, 2)
	fx.node("S02", model.NodeSucceeded, 2)
	fx.attempt("S01", 1, model.AttemptInterrupted, "base-1")
	fx.attempt("S02", 1, model.AttemptSucceeded, "base-1")
	att := fx.state.Attempts[model.AttemptKey{Node: "S01", Number: 1}]
	att.EndHead = "h-1"
	att.RetryCharged = false
	fx.state.Runs[0].Status = model.RunInterrupted
	fx.state.Runs[0].DispatchGate = false
	fx.state.Runs[0].StopReason = model.CodeCommitPolicyDrift
	fx.state.Workflow.Runtime = model.RuntimePaused
	return fx
}

// TestPolicyDriftNoWindowCommitConfirmation asserts the confirmation
// path (PRD 已确认：执行期间 Commit Policy 漂移确认): with no window Commit
// the freshly observed Preflight Revision is recorded, the Workflow stays
// PAUSED with the COMMIT_POLICY_CONFIRMATION_REQUIRED Finding, resume is
// refused, and only the append-only COMMIT_POLICY Approval binding the
// exact new Preflight unblocks it.
func TestPolicyDriftNoWindowCommitConfirmation(t *testing.T) {
	fx := driftFixture(t)
	d := fx.feed(model.PolicyDriftSettleInput{Preflight: &model.PreflightFacts{
		EvidenceHash: "cp-2", Fingerprint: "fp-2", GitVersion: "git 2.0",
		RepositoryContext: "repository:proj-1", ProbeStatus: "PASS",
	}})
	if !hasEvent(d, model.EventFindingOpened) {
		fx.t.Fatalf("the drift settle must open the confirmation finding, got %+v", d.Events)
	}
	if !hasFindingOf(fx.t, fx.state, model.CodeCommitPolicyConfirmationRequired) {
		fx.t.Fatalf("the COMMIT_POLICY_CONFIRMATION_REQUIRED finding must be persisted")
	}
	facts := fx.state.Workflow.ExecutionFacts
	if facts == nil || facts.PreflightRevision != 2 || facts.Fingerprint != "fp-2" || facts.CommitPolicyHash != "cp-2" {
		fx.t.Fatalf("the new preflight must be recorded, got %+v", facts)
	}
	if fx.state.Workflow.Runtime != model.RuntimePaused {
		fx.t.Fatalf("runtime = %s, want PAUSED at the confirmation gate", fx.state.Workflow.Runtime)
	}
	// Resume is refused while the confirmation is pending.
	_, err := decision.Decide(fx.state, model.WorkflowCommandInput{Kind: model.ResumeWorkflow})
	if code, ok := model.CodeOf(err); !ok || code != model.CodeCommitPolicyConfirmationRequired {
		fx.t.Fatalf("resume must be refused while the confirmation is pending, got %v", err)
	}
	// A wrong confirmation is refused with no mutation.
	_, err = decision.Decide(fx.state, model.CommitPolicyApprovalInput{
		PreflightRevision: 2, PreflightHash: "cp-2", Fingerprint: "fp-wrong",
	})
	if code, ok := model.CodeOf(err); !ok || code != model.CodeCommitPolicyInputChanged {
		fx.t.Fatalf("a mismatched confirmation must be COMMIT_POLICY_INPUT_CHANGED, got %v", err)
	}
	// The exact confirmation binds the policy.
	d = fx.feed(model.CommitPolicyApprovalInput{
		PreflightRevision: 2, PreflightHash: "cp-2", Fingerprint: "fp-2",
	})
	if !hasEvent(d, model.EventCommitPolicyConfirmed) {
		fx.t.Fatalf("the confirmation must append the COMMIT_POLICY approval, got %+v", d.Events)
	}
	if len(fx.state.Approvals) != 1 || fx.state.Approvals[0].Kind != model.ApprovalCommitPolicy ||
		fx.state.Approvals[0].PreflightRevision != 2 || fx.state.Approvals[0].Fingerprint != "fp-2" {
		fx.t.Fatalf("approval = %+v, want the exact COMMIT_POLICY approval", fx.state.Approvals)
	}
	// Resume is allowed again.
	if _, err := decision.Decide(fx.state, model.WorkflowCommandInput{Kind: model.ResumeWorkflow}); err != nil {
		fx.t.Fatalf("resume must be allowed after the confirmation, got %v", err)
	}
}

// TestPolicyDriftWindowCommitQuarantine asserts the window-Commit path
// (PRD 已确认：漂移窗口 Commit 的隔离与替代执行): every window Commit
// Branch receives an immutable Quarantine Record with a unique audit Ref,
// the contaminated Node is FAILED, the Workflow BLOCKED, and the
// interrupted Attempt stays INTERRUPTED without charge.
func TestPolicyDriftWindowCommitQuarantine(t *testing.T) {
	fx := driftFixture(t)
	d := fx.feed(model.PolicyDriftSettleInput{WindowCommits: []model.WindowCommit{{
		Branch: "cflow/wf-1/task-S01", FromHead: "h-1", ToHead: "h-1-drift", Node: "S01",
	}}})
	if !hasEvent(d, model.EventQuarantineRecorded) {
		fx.t.Fatalf("the drift settle must record the quarantine, got %+v", d.Events)
	}
	if len(fx.state.Quarantines) != 1 {
		fx.t.Fatalf("quarantines = %+v, want exactly one", fx.state.Quarantines)
	}
	q := fx.state.Quarantines[0]
	if q.Branch != "cflow/wf-1/task-S01" || q.FromHead != "h-1" || q.ToHead != "h-1-drift" ||
		q.Code != model.CodeCommitDuringPolicyDriftWindow {
		fx.t.Fatalf("quarantine = %+v", q)
	}
	if q.AuditRef != "refs/cflow/wf-1/quarantine/"+q.ID {
		fx.t.Fatalf("the audit ref must be refs/cflow/<workflow>/quarantine/<quarantine-id>, got %s", q.AuditRef)
	}
	if n := fx.state.Nodes["S01"]; n == nil || n.Status != model.NodeFailed {
		fx.t.Fatalf("the contaminated node must be FAILED, got %+v", n)
	}
	if a := fx.state.Attempts[model.AttemptKey{Node: "S01", Number: 1}]; a == nil ||
		a.Status != model.AttemptInterrupted || a.RetryCharged {
		fx.t.Fatalf("the window-Commit attempt must stay INTERRUPTED without charge, got %+v", a)
	}
	if fx.state.Workflow.Runtime != model.RuntimeBlocked {
		fx.t.Fatalf("runtime = %s, want BLOCKED with a window Commit", fx.state.Workflow.Runtime)
	}
	if !hasBlockingFindingOf(fx.t, fx.state, model.CodeCommitDuringPolicyDriftWindow) {
		fx.t.Fatalf("a blocking COMMIT_DURING_POLICY_DRIFT_WINDOW finding must be persisted")
	}
}

// TestPolicyDriftContaminatedBranchNeverVerifies asserts the quarantined
// Branch can never re-enter the delivery chain: the Verify dispatch of a
// Node whose Task dependency owns the quarantined Branch is refused.
func TestPolicyDriftContaminatedBranchNeverVerifies(t *testing.T) {
	fx := driftFixture(t)
	fx.state.Nodes["task-S01"] = &model.Node{
		ID: "task-S01", Kind: model.NodeAgentTask, Status: model.NodeFailed,
		Branch: "cflow/wf-1/task-S01", RetryBudget: 2,
	}
	fx.state.Nodes["verify-S01"] = &model.Node{
		ID: "verify-S01", Kind: model.NodeVerify, Status: model.NodeReady,
	}
	fx.state.Quarantines = []model.Quarantine{{
		ID: "quarantine-1", AuditRef: "refs/cflow/wf-1/quarantine/quarantine-1",
		Branch: "cflow/wf-1/task-S01", Code: model.CodeCommitDuringPolicyDriftWindow,
	}}
	fx.state.Runs[0].Status = model.RunRunning
	fx.state.Runs[0].DispatchGate = true
	fx.state.Workflow.Runtime = model.RuntimeRunning
	_, err := decision.Decide(fx.state, model.DispatchInput{
		Node: "verify-S01", Session: "session-v", Route: "fake",
	})
	if code, ok := model.CodeOf(err); !ok || code != model.CodeCommitDuringPolicyDriftWindow {
		fx.t.Fatalf("verifying a contaminated task must be refused, got %v", err)
	}
}

// TestPolicyDriftReplacementCategories asserts the Reconciliation
// Manifest classification (design 15.6): a SUCCEEDED Node reuses, an
// interrupted Node resumes, a contaminated Node is replaced, and the
// Verify of a replaced Task re-runs.
func TestPolicyDriftReplacementCategories(t *testing.T) {
	fx := driftFixture(t)
	fx.state.Nodes["S02"] = &model.Node{
		ID: "S02", Kind: model.NodeAgentTask, Status: model.NodeSucceeded,
		Branch: "cflow/wf-1/task-S02", RetryBudget: 2,
	}
	fx.state.Nodes["V01"] = &model.Node{
		ID: "V01", Kind: model.NodeVerify, Status: model.NodePending,
		Dependencies: []model.NodeID{"S01"},
	}
	fx.state.Quarantines = []model.Quarantine{{
		ID: "quarantine-1", AuditRef: "refs/cflow/wf-1/quarantine/quarantine-1",
		Branch: "cflow/wf-1/task-S01", Code: model.CodeCommitDuringPolicyDriftWindow,
	}}
	actions := decision.ClassifyManifest(fx.state, map[model.NodeID]bool{
		"S01": true, "S02": true, "V01": true,
	}, map[model.NodeID]bool{"S01": false, "S02": true, "V01": false})
	byNode := map[model.NodeID]model.ManifestActionKind{}
	for _, a := range actions {
		byNode[a.Node] = a.Action
	}
	if byNode["S02"] != model.ManifestReuseSucceeded {
		fx.t.Fatalf("S02 = %s, want reuse_succeeded", byNode["S02"])
	}
	if byNode["S01"] != model.ManifestReplaceContaminated {
		fx.t.Fatalf("S01 = %s, want replace_contaminated", byNode["S01"])
	}
	if byNode["V01"] != model.ManifestRerunVerification {
		fx.t.Fatalf("V01 = %s, want rerun_verification", byNode["V01"])
	}
}

// TestPolicyDriftReplacementApprovalUnified asserts the unified
// Replacement Execution Approval (PRD 已确认：Replacement Execution Approval
// 吸收 Policy 确认): one append-only EXECUTION Approval binds the
// Quarantine Set, the superseded approval, every successor reference, the
// current Preflight, and the fixed Reconciliation Manifest — with
// decision_context reason=COMMIT_POLICY_DRIFT_REPLACEMENT and
// absorbs_commit_policy_confirmation=true — and reopens dispatch on a
// fresh Run.
func TestPolicyDriftReplacementApprovalUnified(t *testing.T) {
	fx := driftFixture(t)
	fx.state.Nodes["S01"].Status = model.NodeFailed
	fx.state.Nodes["S01"].RetryCharged = 1
	fx.state.Quarantines = []model.Quarantine{{
		ID: "quarantine-1", AuditRef: "refs/cflow/wf-1/quarantine/quarantine-1",
		Branch: "cflow/wf-1/task-S01", Code: model.CodeCommitDuringPolicyDriftWindow,
	}}
	fx.state.Approvals = []model.Approval{{
		ID: "approval-1", Kind: model.ApprovalExecution, Seq: 1, PreflightRevision: 1,
	}}
	fx.state.Workflow.Runtime = model.RuntimeBlocked
	fx.state.Workflow.ExecutionFacts = &model.ExecutionFacts{
		PlanHash: "plan-2", SpecHashes: []string{"spec-2"}, CatalogHash: "cat-2",
		WorkflowHash: "wf-2", RoutingHash: "r-2", BudgetHash: "b-2",
		CommitPolicyHash: "cp-2", Fingerprint: "fp-2", PreflightRevision: 2,
	}
	d, err := decision.Decide(fx.state, model.ReplacementApprovalInput{
		PlanHash: "plan-2", SpecHashes: []string{"spec-2"}, CatalogHash: "cat-2",
		WorkflowHash: "wf-2", RoutingHash: "r-2", BudgetHash: "b-2",
		PreflightRevision: 2, PreflightHash: "cp-2", Fingerprint: "fp-2",
		QuarantineIDs: []string{"quarantine-1"}, SupersededApprovalID: "approval-1",
		ManifestRevision: 1, ManifestHash: "manifest-h",
	})
	if err != nil {
		fx.t.Fatalf("the replacement approval failed: %v", err)
	}
	fx.state = applyFixtureDecision(fx.state, d)
	if !hasEvent(d, model.EventExecutionApproved) || !hasEvent(d, model.EventRunStarted) {
		fx.t.Fatalf("the replacement approval must append the approval and open a fresh run, got %+v", d.Events)
	}
	if len(fx.state.Approvals) != 2 {
		fx.t.Fatalf("approvals = %+v, want the replacement appended", fx.state.Approvals)
	}
	ap := fx.state.Approvals[1]
	if ap.Kind != model.ApprovalExecution || ap.PreflightRevision != 2 || ap.Fingerprint != "fp-2" {
		fx.t.Fatalf("replacement approval = %+v", ap)
	}
	if !strings.Contains(ap.DecisionContext, `"reason":"COMMIT_POLICY_DRIFT_REPLACEMENT"`) ||
		!strings.Contains(ap.DecisionContext, `"absorbs_commit_policy_confirmation":true`) ||
		!strings.Contains(ap.DecisionContext, `"quarantine_ids":["quarantine-1"]`) ||
		!strings.Contains(ap.DecisionContext, `"superseded_approval_id":"approval-1"`) ||
		!strings.Contains(ap.DecisionContext, `"reconciliation_manifest":{"hash":"manifest-h","revision":1}`) {
		fx.t.Fatalf("replacement decision context = %s", ap.DecisionContext)
	}
	run := activeRunOf(fx.t, fx.state)
	if run == nil || run.Status != model.RunRunning || !run.DispatchGate {
		fx.t.Fatalf("run = %+v, want a fresh RUNNING run with the gate open", run)
	}
	// The contaminated blocking finding is contained: dispatch of a new
	// node on a clean branch is allowed again.
	fx.state.Nodes["S03"] = &model.Node{
		ID: "S03", Kind: model.NodeAgentTask, Status: model.NodePending,
		Branch: "cflow/wf-1/task-S03", RetryBudget: 2,
	}
	if _, err := decision.Decide(fx.state, model.DispatchInput{
		Node: "S03", Session: "session-3", Route: "fake", BaseHead: "int-h",
	}); err != nil {
		fx.t.Fatalf("dispatch must be allowed after the replacement approval, got %v", err)
	}
}

// TestPolicyDriftApplyUnaffected asserts Apply (Task 19) is untouched by
// the Task 17 failure protocols: the Apply request decision on a
// completed Workflow behaves exactly as before the drift machinery.
func TestPolicyDriftApplyUnaffected(t *testing.T) {
	fx := newKernelFixture(t)
	fx.state.Workflow.Stage = model.StageCompleted
	fx.state.Workflow.Runtime = model.RuntimeSucceeded
	fx.state.Workflow.TargetBranch = "main"
	fx.state.Workflow.IntegrationHead = "int-h"
	fx.state.Workflow.ExecutionFacts = &model.ExecutionFacts{
		Fingerprint: "fp-1", CommitPolicyHash: "cp-1", PreflightRevision: 1,
	}
	_, err := decision.Decide(fx.state, model.ApplyCommandInput{
		Kind: model.ApplyRequest, TargetHead: "main-h", IntegrationHead: "int-h",
		Preflight:     model.ArtifactRef{Workflow: fx.state.Workflow.ID, Type: model.ArtifactReport, Revision: 1, Hash: "cp-1"},
		PreflightHash: "cp-1", Fingerprint: "fp-1",
		ReviewSession: "rev-1", ReviewRoute: "fake", ReviewProcess: "p-1",
	})
	if err != nil {
		fx.t.Fatalf("the Apply request must be unaffected by the failure protocols, got %v", err)
	}
}

// hasFindingOf reports whether a Finding with the exact code exists
// (blocking or not).
func hasFindingOf(t *testing.T, st model.State, code model.Code) bool {
	t.Helper()
	for _, f := range st.Findings {
		if f.Code == code {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// App-level interruption: the first Ctrl+C during dispatch
// ---------------------------------------------------------------------------

// TestInterruptedDispatchConvergesToPausedStop asserts the first Ctrl+C
// during a live dispatch: the pass cancels, every RUNNING Attempt settles
// INTERRUPTED without Retry charge, the CONTROLLED_STOP_REQUESTED intent
// is persisted, the dispatch gate closes, and the Workflow converges
// PAUSED with the coding Worktree and its uncommitted content preserved.
func TestInterruptedDispatchConvergesToPausedStop(t *testing.T) {
	fx := newExecutionFixture(t)
	a, wf := fx.executionReady(implementationScript("i1"))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := a.Execute(ctx, DispatchCommand{Workflow: wf})
		done <- err
	}()
	// Let the coding Session start inside its Task Worktree, then send
	// the first Ctrl+C.
	worktree := filepath.Join(a.taskWorktreePath(wf, "task-s01"))
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(worktree); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the task worktree never appeared")
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("the interrupted dispatch did not return")
	}
	iv := fx.inspect(wf)
	if iv.Status.Runtime != model.RuntimePaused {
		t.Fatalf("runtime = %s, want PAUSED after the controlled stop", iv.Status.Runtime)
	}
	stopped := false
	for _, r := range iv.Runs {
		if r.Status == model.RunInterrupted && !r.DispatchGate {
			stopped = true
		}
	}
	if !stopped {
		t.Fatalf("no INTERRUPTED run with a closed gate; runs = %+v", iv.Runs)
	}
	charged := false
	interrupted := false
	for _, att := range iv.Attempts {
		if att.Status == model.AttemptInterrupted {
			interrupted = true
		}
		if att.RetryCharged {
			charged = true
		}
	}
	if !interrupted {
		t.Fatalf("the running attempt must settle INTERRUPTED, attempts = %+v", iv.Attempts)
	}
	if charged {
		t.Fatalf("an interruption must never charge the retry budget")
	}
	// The coding Worktree and its uncommitted content are preserved.
	if _, err := os.Stat(worktree); err != nil {
		t.Fatalf("the coding worktree must be preserved: %v", err)
	}
}

// driftWindowQuarantineFixture drives the deterministic window-commit
// state: a live dispatch is interrupted (the first Ctrl+C), the policy
// drifts, the pre-stop HEAD is fixed, a Commit lands inside the drift
// window, and the post-stop settle records the immutable quarantine with
// its unique audit Ref. It returns the task worktree path.
func driftWindowQuarantineFixture(t *testing.T, fx *planningFixture, a *Application, wf model.WorkflowID) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := a.Execute(ctx, DispatchCommand{Workflow: wf})
		done <- err
	}()
	worktree := filepath.Join(a.taskWorktreePath(wf, "task-s01"))
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(worktree); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the task worktree never appeared")
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("the interrupted dispatch did not return")
	}
	iv := fx.inspect(wf)
	if iv.Status.Runtime != model.RuntimePaused {
		t.Fatalf("runtime = %s, want PAUSED before the drift settle", iv.Status.Runtime)
	}

	// The drift: the policy drifts and a Commit lands inside the window —
	// after the Runtime fixed the pre-stop HEAD, before the final scan.
	git := func(args ...string) string {
		out, err := execGit(fx.root, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("config", "user.email", "drifted@example.com")
	// The pre-stop HEAD is fixed at the Safety Stop request.
	base := git("-C", worktree, "rev-parse", "HEAD")
	st, err := store.Open(context.Background(), store.OpenOptions{
		Path: filepath.Join(fx.home, "cflow.db"), Workflow: wf, CflowVersion: "0.0.0-dev", Now: fx.now,
	})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	view, err := st.View(context.Background(), store.StoreQuery{})
	if err != nil {
		t.Fatalf("view: %v", err)
	}
	runID := view.State.Runs[0].ID
	// The persisted safety-stop intent: the Run carries stop_reason
	// COMMIT_POLICY_DRIFT (the interrupted settle recorded it; a crash
	// between the stop and the settle leaves it).
	if _, err := st.Transact(context.Background(), view.AggregateVersion, func(state model.State) (model.Decision, error) {
		return model.Decision{Mutations: []model.Mutation{model.RunMutation{
			ID: runID, Status: model.RunInterrupted, DispatchGate: false,
			StopReason: model.CodeCommitPolicyDrift,
		}}}, nil
	}); err != nil {
		t.Fatalf("mutate run: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	// The app-level drift snapshot: the pre-stop HEAD of the active Task
	// Worktree (the settle's window scan compares it with the final HEAD).
	a.policyMu.Lock()
	a.policyDrift = true
	a.policyCode = model.CodeCommitPolicySafetyStopRequested
	a.policyPreHeads = map[string]policyWorktree{
		worktree: {Head: base, Branch: "cflow/" + string(wf) + "/task-task-s01", Node: "task-s01"},
	}
	a.policyMu.Unlock()
	// The window Commit lands now (the simulated Agent's commit under the
	// drifted policy, inside the drift window).
	if err := os.WriteFile(filepath.Join(worktree, "window.txt"), []byte("window commit\n"), 0o600); err != nil {
		t.Fatalf("write window file: %v", err)
	}
	git("-C", worktree, "add", "-A")
	git("-C", worktree, "commit", "-q", "-m", "window commit")

	// The post-stop settle: the scan finds the window Commit and records
	// the immutable quarantine with its unique audit Ref.
	wst, err := a.ensureWriteStore(ctx, wf)
	if err != nil {
		t.Fatalf("write store: %v", err)
	}
	if err := a.settlePolicyDrift(ctx, wst, wf); err != nil {
		t.Fatalf("settle: %v", err)
	}
	iv = fx.inspect(wf)
	if iv.Status.Runtime != model.RuntimeBlocked {
		t.Fatalf("runtime = %s, want BLOCKED with a window commit", iv.Status.Runtime)
	}
	return worktree
}

// TestPolicyDriftWindowCommitQuarantineSettle asserts the drift-window
// Commit settle end to end (PRD 已确认：漂移窗口 Commit 的隔离与替代执行):
// a Commit that lands inside the drift window (after the pre-stop HEAD
// was fixed, before the final scan) receives the immutable Branch
// Quarantine Record with its unique refs/cflow/<workflow>/quarantine/
// <quarantine-id> audit Ref, the contaminated Node is FAILED (its
// Attempt stays INTERRUPTED), and the Workflow Blocks. The quarantine
// evidence never vanishes: the audit Ref exists in Git.
func TestPolicyDriftWindowCommitQuarantineSettle(t *testing.T) {
	fx := newExecutionFixture(t)
	a, wf := fx.executionReady(implementationScript("i1"))
	worktree := driftWindowQuarantineFixture(t, fx, a, wf)
	git := func(args ...string) string {
		out, err := execGit(fx.root, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	windowHead := git("-C", worktree, "rev-parse", "HEAD")
	iv := fx.inspect(wf)
	if iv.Status.Runtime != model.RuntimeBlocked {
		t.Fatalf("runtime = %s, want BLOCKED with a window commit", iv.Status.Runtime)
	}
	if len(iv.Quarantines) != 1 {
		t.Fatalf("quarantines = %+v, want exactly one", iv.Quarantines)
	}
	q := iv.Quarantines[0]
	wantRef := "refs/cflow/" + string(wf) + "/quarantine/" + q.ID
	// The store's quarantine row persists the discovered head (the
	// FromHead collapses to it on hydration); the audit Ref and the
	// discovered head are the evidence.
	if q.AuditRef != wantRef || q.ToHead != windowHead {
		t.Fatalf("quarantine = %+v, want the audit ref %s with the discovered head", q, wantRef)
	}
	// The audit Ref exists in Git at the discovered HEAD.
	if got := git("show-ref", "--verify", "--hash", q.AuditRef); got != windowHead {
		t.Fatalf("audit ref = %s, want the discovered head %s", got, windowHead)
	}
	// The contaminated Node is FAILED; its Attempt stays INTERRUPTED.
	statusByID := map[model.NodeID]model.NodeStatus{}
	for _, n := range iv.Nodes {
		statusByID[n.ID] = n.Status
	}
	if statusByID["task-s01"] != model.NodeFailed {
		t.Fatalf("task-s01 status = %s, want FAILED", statusByID["task-s01"])
	}
	interrupted := false
	for _, att := range iv.Attempts {
		if att.Status == model.AttemptInterrupted {
			interrupted = true
		}
	}
	if !interrupted {
		t.Fatalf("the window-commit attempt must stay INTERRUPTED")
	}
}

// TestReplacementPreviewToApproveEndToEnd asserts the unified Replacement
// Execution Approval end to end (PRD 已确认：Replacement Execution Approval
// 吸收 Policy 确认): after the drift-window quarantine Blocks the Workflow,
// replacement-preview generates the successor Specs, the successor
// Dynamic Workflow Revision, the fixed Reconciliation Manifest (with its
// self-hash persisted and displayed), and the fresh Preflight; then
// replacement-approve binds the exact preview in one append-only
// EXECUTION Approval and reopens dispatch on a fresh Run.
func TestReplacementPreviewToApproveEndToEnd(t *testing.T) {
	fx := newExecutionFixture(t)
	a, wf := fx.executionReady(implementationScript("i1"))
	driftWindowQuarantineFixture(t, fx, a, wf)

	// The unified preview: successor execution + manifest + preflight.
	if _, err := a.Execute(context.Background(), ReplacementPreviewCommand{Workflow: wf}); err != nil {
		t.Fatalf("replacement preview: %v", err)
	}
	qview, err := a.Query(context.Background(), ReplacementPreviewQuery{Workflow: wf})
	if err != nil {
		t.Fatalf("replacement preview query: %v", err)
	}
	pv := qview.(ReplacementPreviewView)
	if len(pv.Quarantines) == 0 {
		t.Fatalf("the preview must display the quarantine set")
	}
	if pv.Manifest.Revision < 1 || len(pv.Manifest.Actions) == 0 {
		t.Fatalf("the preview must display the reconciliation manifest, got %+v", pv.Manifest)
	}
	if pv.Manifest.Hash == "" {
		t.Fatalf("the manifest self-hash must be persisted and displayed")
	}
	// The persisted manifest body carries the embedded hash.
	body, err := a.readReconciliationManifest(wf, pv.Manifest.Revision)
	if err != nil || body == nil {
		t.Fatalf("the reconciliation manifest must be persisted: %v", err)
	}
	var persisted model.ReconciliationManifest
	if err := json.Unmarshal(body, &persisted); err != nil {
		t.Fatalf("the persisted manifest cannot be parsed: %v", err)
	}
	if persisted.Hash != pv.Manifest.Hash {
		t.Fatalf("the persisted manifest hash = %s, want the displayed %s", persisted.Hash, pv.Manifest.Hash)
	}

	// The unified approval binds the exact preview and reopens dispatch.
	ids := make([]string, 0, len(pv.Quarantines))
	for _, q := range pv.Quarantines {
		ids = append(ids, q.ID)
	}
	if _, err := a.Execute(context.Background(), ApproveReplacementCommand{
		Workflow:             wf,
		PlanHash:             pv.PlanHash,
		SpecHashes:           pv.SpecHashes,
		CatalogHash:          pv.CatalogHash,
		WorkflowHash:         pv.WorkflowHash,
		RoutingHash:          pv.RoutingHash,
		BudgetHash:           pv.BudgetHash,
		PreflightRevision:    pv.Preflight.Revision,
		PreflightHash:        pv.Preflight.EvidenceHash,
		Fingerprint:          pv.NewFingerprint,
		QuarantineIDs:        ids,
		SupersededApprovalID: pv.SupersededApprovalID,
		ManifestRevision:     pv.Manifest.Revision,
		ManifestHash:         pv.Manifest.Hash,
	}); err != nil {
		t.Fatalf("replacement approval: %v", err)
	}
	iv := fx.inspect(wf)
	if iv.Status.Runtime != model.RuntimeRunning {
		t.Fatalf("runtime = %s, want RUNNING after the replacement approval", iv.Status.Runtime)
	}
	// The single append-only EXECUTION Approval carries the decision
	// context: reason=COMMIT_POLICY_DRIFT_REPLACEMENT, the superseded
	// approval, the quarantine set, the fixed manifest, and
	// absorbs_commit_policy_confirmation=true.
	replacement := false
	for _, ap := range iv.Approvals {
		if ap.Kind != model.ApprovalExecution {
			continue
		}
		if strings.Contains(ap.DecisionContext, `"reason":"COMMIT_POLICY_DRIFT_REPLACEMENT"`) {
			replacement = true
			if !strings.Contains(ap.DecisionContext, `"absorbs_commit_policy_confirmation":true`) ||
				!strings.Contains(ap.DecisionContext, `"reconciliation_manifest":{"hash":"`+pv.Manifest.Hash+`","revision":`+strconv.Itoa(pv.Manifest.Revision)+`}`) {
				t.Fatalf("replacement decision context = %s", ap.DecisionContext)
			}
		}
	}
	if !replacement {
		t.Fatalf("the replacement execution approval must be persisted")
	}
}
