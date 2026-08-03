// Package decision_test verifies the pure Decision Kernel through the
// mandatory transition cases, the encoded legal/illegal transition
// matrices, the fault policy table, and bounded-sequence property tests
// for the model invariants (design 8, PRD 状态机与持久化模型).
package decision_test

import (
	"fmt"
	"math/rand"
	"reflect"
	"slices"
	"testing"
	"time"

	"cflow.local/cflow/internal/decision"
	"cflow.local/cflow/internal/model"
)

// assertByteIdentical compares Decisions and errors structurally. The
// Kernel is a pure function of State/Input: the data bytes are identical
// for identical inputs; only the heap addresses of pointer fields (e.g. a
// freshly allocated CancelIntent) may differ between calls, which
// reflect.DeepEqual resolves by following the pointers.
func assertByteIdentical(t *testing.T, d1 model.Decision, e1 error, d2 model.Decision, e2 error) {
	t.Helper()
	if !reflect.DeepEqual(d1, d2) {
		t.Fatalf("non-deterministic decision: %#v vs %#v", d1, d2)
	}
	if !reflect.DeepEqual(e1, e2) {
		t.Fatalf("non-deterministic error: %#v vs %#v", e1, e2)
	}
}

var fixedNow = time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func assertFaultCode(t *testing.T, err error, want model.Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected fault %s, got no error", want)
	}
	code, ok := model.CodeOf(err)
	if !ok {
		t.Fatalf("expected fault %s, got error without code: %v", want, err)
	}
	if code != want {
		t.Fatalf("expected fault %s, got %s (%v)", want, code, err)
	}
}

func requireNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// requireStatus asserts the decision's final Workflow outcome (the last
// Workflow mutation wins, mirroring the Store's in-order application) is
// exactly the given Stage and Runtime Status.
func requireStatus(t *testing.T, got model.Decision, stage model.WorkflowStage, runtime model.RuntimeStatus) {
	t.Helper()
	var last *model.WorkflowMutation
	for _, m := range got.Mutations {
		if wm, ok := m.(model.WorkflowMutation); ok {
			last = &wm
		}
	}
	if last == nil {
		t.Fatalf("decision has no Workflow mutation")
	}
	if last.Stage != stage || last.Runtime != runtime {
		t.Fatalf("decision moves workflow to %s/%s, want %s/%s", last.Stage, last.Runtime, stage, runtime)
	}
}

// requireNoWorkflowMutation asserts the decision leaves the Workflow
// aggregate untouched.
func requireNoWorkflowMutation(t *testing.T, got model.Decision) {
	t.Helper()
	for _, m := range got.Mutations {
		if _, ok := m.(model.WorkflowMutation); ok {
			t.Fatalf("decision unexpectedly mutates the workflow: %+v", m)
		}
	}
}

func requireNode(t *testing.T, got model.Decision, id model.NodeID, status model.NodeStatus) {
	t.Helper()
	for _, m := range got.Mutations {
		nm, ok := m.(model.NodeStatusMutation)
		if !ok || nm.Node != id {
			continue
		}
		if nm.Status != status {
			t.Fatalf("decision moves node %s to %s, want %s", id, nm.Status, status)
		}
		return
	}
	t.Fatalf("decision has no mutation for node %s", id)
}

func requireNodeUntouched(t *testing.T, got model.Decision, id model.NodeID) {
	t.Helper()
	for _, m := range got.Mutations {
		if nm, ok := m.(model.NodeStatusMutation); ok && nm.Node == id {
			t.Fatalf("decision unexpectedly mutates node %s", id)
		}
	}
}

func requireRun(t *testing.T, got model.Decision, status model.RunStatus, gate bool) {
	t.Helper()
	for _, m := range got.Mutations {
		rm, ok := m.(model.RunMutation)
		if !ok {
			continue
		}
		if rm.Status != status || rm.DispatchGate != gate {
			t.Fatalf("decision moves run %s to %s (gate %v), want %s (gate %v)", rm.ID, rm.Status, rm.DispatchGate, status, gate)
		}
		return
	}
	t.Fatalf("decision has no Run mutation")
}

func requireEffect(t *testing.T, got model.Decision, want model.EffectIntent) {
	t.Helper()
	if got.Effect == nil {
		t.Fatalf("decision has no effect, want %T", want)
	}
	if got.Effect != want {
		t.Fatalf("decision effect = %#v, want %#v", got.Effect, want)
	}
}

func requireNoEffect(t *testing.T, got model.Decision) {
	t.Helper()
	if got.Effect != nil {
		t.Fatalf("decision has unexpected effect %#v", got.Effect)
	}
}

func hasEvent(t *testing.T, got model.Decision, kind model.EventKind) bool {
	t.Helper()
	for _, e := range got.Events {
		if e.Kind == kind {
			return true
		}
	}
	return false
}

func requireEvent(t *testing.T, got model.Decision, kind model.EventKind) {
	t.Helper()
	if !hasEvent(t, got, kind) {
		t.Fatalf("decision missing event %s (have %v)", kind, got.Events)
	}
}

// ---------------------------------------------------------------------------
// fixtures
// ---------------------------------------------------------------------------

func baseState() model.State {
	return model.State{Now: fixedNow, Version: model.AggregateVersion(1)}
}

func workflowState(stage model.WorkflowStage, rt model.RuntimeStatus) model.State {
	st := baseState()
	st.Workflow = model.Workflow{ID: "wf-1", Project: "p-1", Stage: stage, Runtime: rt,
		TargetBranch: "main", BaseCommit: "base-1", IntegrationBranch: "cflow/integration/wf-1",
		IntegrationHead: "int-1"}
	if rt == model.RuntimeCancelled {
		st.Workflow.CancelIntent = &model.CancelIntent{RequestedSeq: 1, Reason: "fixture"}
	}
	return st
}

func addNode(st *model.State, id string, kind model.NodeKind, status model.NodeStatus, budget int) {
	if st.Nodes == nil {
		st.Nodes = map[model.NodeID]*model.Node{}
	}
	st.Nodes[model.NodeID(id)] = &model.Node{ID: model.NodeID(id), Kind: kind, Status: status,
		RetryBudget: budget, Branch: "task/" + id}
}

func addNodeBranch(st *model.State, id string, kind model.NodeKind, status model.NodeStatus, budget int, branch string) {
	addNode(st, id, kind, status, budget)
	st.Nodes[model.NodeID(id)].Branch = branch
}

func addAttempt(st *model.State, node string, n int, status model.AttemptStatus) model.AttemptKey {
	if st.Attempts == nil {
		st.Attempts = map[model.AttemptKey]*model.Attempt{}
	}
	key := model.AttemptKey{Node: model.NodeID(node), Number: model.AttemptNumber(n)}
	st.Attempts[key] = &model.Attempt{Key: key, Status: status, StartHead: "base-1", StartedAt: fixedNow}
	return key
}

func addRun(st *model.State, status model.RunStatus, gate bool) model.RunID {
	id := model.RunID(fmt.Sprintf("run-%d", len(st.Runs)+1))
	st.Runs = append(st.Runs, model.Run{ID: id, Status: status, DispatchGate: gate, StartedAt: fixedNow})
	return id
}

func addProcess(st *model.State, id string, status model.ProcessStatus) {
	st.Processes = append(st.Processes, model.ProcessRecord{ID: model.ProcessID(id), Status: status, StartedAt: fixedNow})
}

func addFinding(st *model.State, id string, code model.Code, subject string) {
	st.Findings = append(st.Findings, model.Finding{ID: model.FindingID(id), Code: code, Blocking: true, Subject: subject, Text: code.String()})
}

func addQuarantine(st *model.State, branch string) {
	st.Quarantines = append(st.Quarantines, model.Quarantine{Branch: branch, Code: model.CodeCommitDuringPolicyDriftWindow, Reason: "drift window commit"})
}

func fixtureRunningAgentTask() model.State {
	st := workflowState(model.StageExecution, model.RuntimeRunning)
	addRun(&st, model.RunRunning, true)
	addNode(&st, "n-1", model.NodeAgentTask, model.NodeRunning, 2)
	addAttempt(&st, "n-1", 1, model.AttemptRunning)
	addProcess(&st, "p-1", model.ProcessStatusRunning)
	return st
}

func fixtureAttemptFailedWithBudget(budget int) model.State {
	st := workflowState(model.StageExecution, model.RuntimeRunning)
	addRun(&st, model.RunRunning, false)
	addNode(&st, "n-1", model.NodeAgentTask, model.NodeFailed, budget)
	key := addAttempt(&st, "n-1", 1, model.AttemptFailed)
	st.Attempts[key].FailureCode = model.CodeCommandFailed
	st.Attempts[key].EndedAt = fixedNow
	addFinding(&st, "f-1", model.CodeRetryExhausted, "n-1")
	return st
}

func fixtureAwaitingExecutionApproval(hash string) model.State {
	st := workflowState(model.StageWorkflowGeneration, model.RuntimePaused)
	st.Plan = &model.Plan{Revision: 1, Status: model.PlanApproved,
		Artifact: model.ArtifactRef{Workflow: "wf-1", Type: model.ArtifactPlan, Revision: 1, Hash: "plan-h"}, Hash: "plan-h"}
	st.Workflow.ExecutionFacts = &model.ExecutionFacts{
		PlanHash: "plan-h", SpecHashes: []string{"spec-1"}, CatalogHash: "cat-1",
		WorkflowHash: hash, RoutingHash: "r-1", BudgetHash: "b-1",
		CommitPolicyHash: "cp-1", Fingerprint: "fp-1",
		SpecRevision: 1, CatalogRevision: 1, WorkflowRevision: 1,
	}
	return st
}

func fixturePlanCheck(planStatus model.PlanStatus) model.State {
	st := workflowState(model.StagePlanCheck, model.RuntimePaused)
	st.Plan = &model.Plan{Revision: 1, Status: planStatus,
		Artifact: model.ArtifactRef{Workflow: "wf-1", Type: model.ArtifactPlan, Revision: 1, Hash: "plan-h"}, Hash: "plan-h"}
	return st
}

func fixtureTwoRunningAttempts() model.State {
	st := workflowState(model.StageExecution, model.RuntimeRunning)
	addRun(&st, model.RunRunning, true)
	addNode(&st, "n-1", model.NodeAgentTask, model.NodeRunning, 2)
	addNode(&st, "n-2", model.NodeVerify, model.NodeRunning, 2)
	addAttempt(&st, "n-1", 1, model.AttemptRunning)
	addAttempt(&st, "n-2", 1, model.AttemptRunning)
	return st
}

func fixtureQuiescing() model.State {
	st := workflowState(model.StageExecution, model.RuntimeRunning)
	st.Runs = append(st.Runs, model.Run{ID: "run-1", Status: model.RunQuiescing, DispatchGate: false,
		QuiesceSnapshot: []model.AttemptKey{{Node: "n-1", Number: 1}, {Node: "n-2", Number: 1}}, StartedAt: fixedNow})
	addNode(&st, "n-1", model.NodeAgentTask, model.NodeFailed, 0)
	addNode(&st, "n-2", model.NodeVerify, model.NodeRunning, 2)
	addNode(&st, "n-3", model.NodeAgentTask, model.NodeReady, 2)
	key := addAttempt(&st, "n-1", 1, model.AttemptFailed)
	st.Attempts[key].FailureCode = model.CodeCommandFailed
	st.Attempts[key].EndedAt = fixedNow
	addAttempt(&st, "n-2", 1, model.AttemptRunning)
	addFinding(&st, "f-1", model.CodeRetryExhausted, "n-1")
	return st
}

func fixtureQuiescingSettled() model.State {
	st := fixtureQuiescing()
	key := addAttempt(&st, "n-2", 1, model.AttemptFailed)
	st.Attempts[key].FailureCode = model.CodeCommandFailed
	st.Attempts[key].EndedAt = fixedNow
	st.Nodes["n-2"].Status = model.NodeFailed
	return st
}

func fixtureCompleted() model.State {
	st := workflowState(model.StageCompleted, model.RuntimeSucceeded)
	st.Workflow.IntegrationHead = "int-9"
	st.Workflow.ExecutionFacts = &model.ExecutionFacts{Fingerprint: "fp-1", CommitPolicyHash: "cp-1"}
	return st
}

func ev(kind model.EvidenceKind, hash string) model.EvidenceRef {
	return model.EvidenceRef{Kind: kind, Hash: hash, Subject: "n-1"}
}

func endAttempt(node string, n int, outcome model.AttemptOutcome, code model.Code) model.Input {
	in := model.EffectResultInput{Kind: model.AttemptEnded,
		Attempt: model.AttemptKey{Node: model.NodeID(node), Number: model.AttemptNumber(n)},
		Outcome: outcome, FailureCode: code, EndHead: "abc123", Evidence: ev(model.EvidenceCommit, "abc123")}
	if code == model.CodeDirtyTaskWorktree {
		in.EndDirtyFingerprint = "dirty-fp"
	}
	return in
}

// ---------------------------------------------------------------------------
// mandatory kernel cases (task brief Step 1)
// ---------------------------------------------------------------------------

func TestAgentCompletionCannotCompleteNode(t *testing.T) {
	state := fixtureRunningAgentTask()
	input := model.AgentEventInput{Kind: model.AgentClaimsComplete}
	_, err := decision.Decide(state, input)
	assertFaultCode(t, err, model.CodeUntrustedCompletion)
}

func TestRetryExhaustionBlocksInsteadOfWorkflowFailed(t *testing.T) {
	state := fixtureAttemptFailedWithBudget(0)
	got, err := decision.Decide(state, model.ReconcileInput{})
	requireNoError(t, err)
	requireStatus(t, got, model.StageExecution, model.RuntimeBlocked)
}

func TestApprovalRequiresExactHashes(t *testing.T) {
	state := fixtureAwaitingExecutionApproval("workflow-a")
	_, err := decision.Decide(state, model.ExecutionApprovalInput{WorkflowHash: "workflow-b"})
	assertFaultCode(t, err, model.CodeApprovalInputChanged)
}

// ---------------------------------------------------------------------------
// transition matrix encoded as data
// ---------------------------------------------------------------------------

type matrixCase struct {
	name        string
	state       model.State
	input       model.Input
	wantStage   model.WorkflowStage
	wantRuntime model.RuntimeStatus
	wantFault   model.Code
}

func TestWorkflowCommandMatrix(t *testing.T) {
	create := model.WorkflowCommandInput{Kind: model.CreateWorkflow, Workflow: "wf-1", Project: "p-1",
		TargetBranch: "main", BaseCommit: "base-1"}
	start := model.WorkflowCommandInput{Kind: model.StartWorkflow}
	pause := model.WorkflowCommandInput{Kind: model.PauseWorkflow}
	resume := model.WorkflowCommandInput{Kind: model.ResumeWorkflow}
	cancel := model.WorkflowCommandInput{Kind: model.CancelWorkflow}

	cases := []matrixCase{
		// create
		{"create from empty", model.State{}, create, model.StageRequirementDiscussion, model.RuntimePending, ""},
		{"create when workflow exists", workflowState(model.StageRequirementDiscussion, model.RuntimePending), create, "", "", model.CodeInvalidInput},
		{"create without workflow id", model.State{}, model.WorkflowCommandInput{Kind: model.CreateWorkflow, Project: "p-1"}, "", "", model.CodeInvalidInput},
		// start
		{"start from pending", workflowState(model.StageRequirementDiscussion, model.RuntimePending), start, model.StageRequirementDiscussion, model.RuntimeRunning, ""},
		{"start from running", workflowState(model.StageExecution, model.RuntimeRunning), start, "", "", model.CodeInvalidInput},
		{"start from paused", workflowState(model.StageExecution, model.RuntimePaused), start, "", "", model.CodeInvalidInput},
		{"start from cancelled", workflowState(model.StageExecution, model.RuntimeCancelled), start, "", "", model.CodeInvalidInput},
		{"start from failed", workflowState(model.StageExecution, model.RuntimeFailed), start, "", "", model.CodeInvalidInput},
		// pause
		{"pause from running", workflowState(model.StageExecution, model.RuntimeRunning), pause, model.StageExecution, model.RuntimePaused, ""},
		{"pause from paused", workflowState(model.StageExecution, model.RuntimePaused), pause, "", "", model.CodeInvalidInput},
		{"pause from blocked", workflowState(model.StageExecution, model.RuntimeBlocked), pause, "", "", model.CodeInvalidInput},
		{"pause from cancelled", workflowState(model.StageExecution, model.RuntimeCancelled), pause, "", "", model.CodeInvalidInput},
		{"pause from succeeded", workflowState(model.StageCompleted, model.RuntimeSucceeded), pause, "", "", model.CodeInvalidInput},
		// resume
		{"resume from paused", workflowState(model.StageExecution, model.RuntimePaused), resume, model.StageExecution, model.RuntimeRunning, ""},
		{"resume from blocked", workflowState(model.StageExecution, model.RuntimeBlocked), resume, model.StageExecution, model.RuntimeRunning, ""},
		{"resume from running", workflowState(model.StageExecution, model.RuntimeRunning), resume, "", "", model.CodeInvalidInput},
		{"resume from failed", workflowState(model.StageExecution, model.RuntimeFailed), resume, "", "", model.CodeInvalidInput},
		{"resume from cancelled", workflowState(model.StageExecution, model.RuntimeCancelled), resume, "", "", model.CodeInvalidInput},
		// cancel
		{"cancel from pending", workflowState(model.StageRequirementDiscussion, model.RuntimePending), cancel, model.StageRequirementDiscussion, model.RuntimeCancelled, ""},
		{"cancel from running", workflowState(model.StageExecution, model.RuntimeRunning), cancel, model.StageExecution, model.RuntimeCancelled, ""},
		{"cancel from paused", workflowState(model.StageExecution, model.RuntimePaused), cancel, model.StageExecution, model.RuntimeCancelled, ""},
		{"cancel from blocked", workflowState(model.StageExecution, model.RuntimeBlocked), cancel, model.StageExecution, model.RuntimeCancelled, ""},
		{"cancel from cancelled", workflowState(model.StageExecution, model.RuntimeCancelled), cancel, "", "", model.CodeInvalidInput},
		{"cancel from failed", workflowState(model.StageExecution, model.RuntimeFailed), cancel, "", "", model.CodeInvalidInput},
		{"cancel from succeeded", workflowState(model.StageCompleted, model.RuntimeSucceeded), cancel, "", "", model.CodeInvalidInput},
		// commands on a state without a workflow
		{"start without workflow", model.State{}, start, "", "", model.CodeInvalidInput},
		{"pause without workflow", model.State{}, pause, "", "", model.CodeInvalidInput},
	}
	runMatrix(t, cases)
}

func TestApprovalMatrix(t *testing.T) {
	planExact := model.PlanApprovalInput{PlanRef: model.ArtifactRef{Workflow: "wf-1", Type: model.ArtifactPlan, Revision: 1, Hash: "plan-h"}, Hash: "plan-h"}
	planWrong := model.PlanApprovalInput{PlanRef: planExact.PlanRef, Hash: "other-h"}
	execExact := model.ExecutionApprovalInput{WorkflowHash: "workflow-a", PlanHash: "plan-h",
		SpecHashes: []string{"spec-1"}, CatalogHash: "cat-1", RoutingHash: "r-1", BudgetHash: "b-1", CommitPolicyHash: "cp-1"}

	cases := []matrixCase{
		{"plan approval exact", fixturePlanCheck(model.PlanChecked), planExact, model.StageSpecGeneration, model.RuntimePaused, ""},
		{"plan approval wrong hash", fixturePlanCheck(model.PlanChecked), planWrong, "", "", model.CodeApprovalInputChanged},
		{"plan approval before checked", fixturePlanCheck(model.PlanDraft), planExact, "", "", model.CodeInvalidInput},
		{"plan approval without plan", workflowState(model.StagePlanCheck, model.RuntimePaused), planExact, "", "", model.CodeInvalidInput},
		{"execution approval exact", fixtureAwaitingExecutionApproval("workflow-a"), execExact, model.StageExecution, model.RuntimeRunning, ""},
		{"execution approval wrong hash", fixtureAwaitingExecutionApproval("workflow-a"), model.ExecutionApprovalInput{WorkflowHash: "workflow-b"}, "", "", model.CodeApprovalInputChanged},
		{"execution approval wrong routing", fixtureAwaitingExecutionApproval("workflow-a"), model.ExecutionApprovalInput{WorkflowHash: "workflow-a", PlanHash: "plan-h", SpecHashes: []string{"spec-1"}, CatalogHash: "cat-1", RoutingHash: "r-2", BudgetHash: "b-1", CommitPolicyHash: "cp-1"}, "", "", model.CodeApprovalInputChanged},
	}
	runMatrix(t, cases)
}

func runMatrix(t *testing.T, cases []matrixCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decision.Decide(tc.state, tc.input)
			if tc.wantFault != "" {
				assertFaultCode(t, err, tc.wantFault)
				if len(got.Mutations) != 0 || len(got.Events) != 0 || got.Effect != nil {
					t.Fatalf("fault decision must be empty, got %#v", got)
				}
				return
			}
			requireNoError(t, err)
			requireStatus(t, got, tc.wantStage, tc.wantRuntime)
		})
	}
}

// ---------------------------------------------------------------------------
// Node, Attempt and Retry semantics
// ---------------------------------------------------------------------------

func TestAttemptSuccessCompletesNode(t *testing.T) {
	state := fixtureRunningAgentTask()
	got, err := decision.Decide(state, endAttempt("n-1", 1, model.OutcomeSucceeded, ""))
	requireNoError(t, err)
	requireNode(t, got, "n-1", model.NodeSucceeded)
	requireEvent(t, got, model.EventAttemptSucceeded)
	requireEvent(t, got, model.EventNodeSucceeded)
	requireNoWorkflowMutation(t, got) // the Workflow stays RUNNING
}

func TestAttemptSuccessWithDirtyWorktreeIsFailure(t *testing.T) {
	state := fixtureRunningAgentTask()
	in := model.EffectResultInput{Kind: model.AttemptEnded,
		Attempt: model.AttemptKey{Node: "n-1", Number: 1}, Outcome: model.OutcomeSucceeded,
		EndHead: "abc123", EndDirtyFingerprint: "dirty-fp", Evidence: ev(model.EvidenceCommit, "abc123")}
	got, err := decision.Decide(state, in)
	requireNoError(t, err)
	// a dirty Worktree is a gate failure even when the process reports success
	requireNode(t, got, "n-1", model.NodeReady)
	requireEvent(t, got, model.EventAttemptFailed)
	requireEvent(t, got, model.EventNodeReady)
}

func TestAttemptSuccessWithoutCommitIsFailure(t *testing.T) {
	state := fixtureRunningAgentTask()
	in := model.EffectResultInput{Kind: model.AttemptEnded,
		Attempt: model.AttemptKey{Node: "n-1", Number: 1}, Outcome: model.OutcomeSucceeded,
		EndHead: "base-1", Evidence: ev(model.EvidenceCommit, "base-1")} // no new Commit
	got, err := decision.Decide(state, in)
	requireNoError(t, err)
	requireNode(t, got, "n-1", model.NodeReady)
	requireEvent(t, got, model.EventAttemptFailed)
}

func TestRetryCreatesSuccessorAttempt(t *testing.T) {
	state := fixtureRunningAgentTask()
	got, err := decision.Decide(state, endAttempt("n-1", 1, model.OutcomeFailed, model.CodeCommandFailed))
	requireNoError(t, err)
	requireNode(t, got, "n-1", model.NodeReady)
	requireEvent(t, got, model.EventAttemptCreated)
	for _, m := range got.Mutations {
		am, ok := m.(model.AttemptAppendMutation)
		if !ok {
			continue
		}
		if am.Attempt.Key != (model.AttemptKey{Node: "n-1", Number: 2}) {
			t.Fatalf("successor attempt = %s, want n-1#2", am.Attempt.Key)
		}
		if am.Attempt.Status != model.AttemptReady {
			t.Fatalf("successor attempt status = %s, want READY", am.Attempt.Status)
		}
		return
	}
	t.Fatal("decision created no successor attempt")
}

func TestRetryExhaustionChargesBudgetThenBlocks(t *testing.T) {
	state := fixtureRunningAgentTask() // budget 2
	// attempts 1 and 2 already failed and charged the budget
	for i := 1; i <= 2; i++ {
		state.Attempts[model.AttemptKey{Node: "n-1", Number: model.AttemptNumber(i)}] =
			&model.Attempt{Key: model.AttemptKey{Node: "n-1", Number: model.AttemptNumber(i)}, Status: model.AttemptFailed,
				FailureCode: model.CodeCommandFailed, EndedAt: fixedNow}
	}
	state.Nodes["n-1"].RetryCharged = 2
	state.Attempts[model.AttemptKey{Node: "n-1", Number: 3}] =
		&model.Attempt{Key: model.AttemptKey{Node: "n-1", Number: 3}, Status: model.AttemptRunning,
			StartHead: "base-1", StartedAt: fixedNow}
	got, err := decision.Decide(state, endAttempt("n-1", 3, model.OutcomeFailed, model.CodeCommandFailed))
	requireNoError(t, err)
	requireNode(t, got, "n-1", model.NodeFailed)
	requireStatus(t, got, model.StageExecution, model.RuntimeBlocked)
	requireEvent(t, got, model.EventFindingOpened)
	requireEvent(t, got, model.EventWorkflowBlocked)
	requireRun(t, got, model.RunBlocked, false)
}

func TestNonRetryableFailureBlocksImmediately(t *testing.T) {
	state := fixtureRunningAgentTask()
	got, err := decision.Decide(state, endAttempt("n-1", 1, model.OutcomeFailed, model.CodeScopeViolation))
	requireNoError(t, err)
	requireNode(t, got, "n-1", model.NodeFailed)
	requireStatus(t, got, model.StageExecution, model.RuntimeBlocked)
	requireRun(t, got, model.RunBlocked, false)
	requireEvent(t, got, model.EventWorkflowBlocked)
}

func TestWorkflowNeverFailsOnOrdinaryFailure(t *testing.T) {
	// retry exhaustion with zero budget must Block, never FAIL the Workflow:
	// Runtime FAILED is reserved for unrecoverable authority/invariant failure
	state := fixtureRunningAgentTask()
	state.Nodes["n-1"].RetryBudget = 0
	got, err := decision.Decide(state, endAttempt("n-1", 1, model.OutcomeFailed, model.CodeCommandFailed))
	requireNoError(t, err)
	requireStatus(t, got, model.StageExecution, model.RuntimeBlocked)
	requireNode(t, got, "n-1", model.NodeFailed)
}

// ---------------------------------------------------------------------------
// Quiescing
// ---------------------------------------------------------------------------

func TestFailureWithInFlightSiblingsQuiesces(t *testing.T) {
	state := fixtureTwoRunningAttempts()
	got, err := decision.Decide(state, endAttempt("n-1", 1, model.OutcomeFailed, model.CodeScopeViolation))
	requireNoError(t, err)
	requireNode(t, got, "n-1", model.NodeFailed)
	requireNodeUntouched(t, got, "n-2")
	requireNoWorkflowMutation(t, got) // workflow stays RUNNING while quiescing
	requireRun(t, got, model.RunQuiescing, false)
	requireEvent(t, got, model.EventWorkflowQuiesceRequested)
	// the snapshot contains exactly the persisted RUNNING attempts
	wantSnapshot := []model.AttemptKey{{Node: "n-2", Number: 1}}
	for _, m := range got.Mutations {
		if rm, ok := m.(model.RunMutation); ok {
			if !slices.Equal(rm.QuiesceSnapshot, wantSnapshot) {
				t.Fatalf("quiesce snapshot = %v, want %v", rm.QuiesceSnapshot, wantSnapshot)
			}
		}
	}
}

func TestQuiescingConvergesToBlocked(t *testing.T) {
	state := fixtureTwoRunningAttempts()
	got, err := decision.Decide(state, endAttempt("n-1", 1, model.OutcomeFailed, model.CodeScopeViolation))
	requireNoError(t, err)
	state = apply(t, state, got)
	// the in-flight sibling settles successfully
	got2, err := decision.Decide(state, endAttempt("n-2", 1, model.OutcomeSucceeded, ""))
	requireNoError(t, err)
	requireNode(t, got2, "n-2", model.NodeSucceeded)
	requireStatus(t, got2, model.StageExecution, model.RuntimeBlocked)
	requireRun(t, got2, model.RunBlocked, false)
	requireEvent(t, got2, model.EventWorkflowQuiesced)
}

func TestQuiescingSettledByReconcile(t *testing.T) {
	state := fixtureQuiescingSettled()
	got, err := decision.Decide(state, model.ReconcileInput{})
	requireNoError(t, err)
	requireStatus(t, got, model.StageExecution, model.RuntimeBlocked)
	requireRun(t, got, model.RunBlocked, false)
	requireEvent(t, got, model.EventWorkflowQuiesced)
}

func TestQuiescingNeverDispatchesReadySiblings(t *testing.T) {
	state := fixtureQuiescing() // n-2 still RUNNING, n-3 READY, gate closed
	got, err := decision.Decide(state, model.ReconcileInput{})
	requireNoError(t, err)
	requireNodeUntouched(t, got, "n-3")
	// no mutation may reopen the Dispatch Gate while quiescing
	for _, m := range got.Mutations {
		if rm, ok := m.(model.RunMutation); ok && rm.DispatchGate {
			t.Fatalf("quiescing decision reopened the dispatch gate: %+v", rm)
		}
	}
}

func TestQuiescingAttemptDoesNotStartRetry(t *testing.T) {
	// a retryable failure during quiescing projects the Node READY per the
	// normal rules but must not create a successor Attempt or charge budget
	state := fixtureQuiescing() // n-2 RUNNING with budget 2
	got, err := decision.Decide(state, endAttempt("n-2", 1, model.OutcomeFailed, model.CodeCommandFailed))
	requireNoError(t, err)
	for _, m := range got.Mutations {
		if _, ok := m.(model.AttemptAppendMutation); ok {
			t.Fatalf("quiescing decision created a successor attempt: %+v", m)
		}
	}
	requireNode(t, got, "n-2", model.NodeReady)
	for _, m := range got.Mutations {
		if nm, ok := m.(model.NodeStatusMutation); ok && nm.Node == "n-2" && nm.RetryCharged != 0 {
			t.Fatalf("quiescing retry deferred but charged budget: %+v", nm)
		}
	}
}

// ---------------------------------------------------------------------------
// Interruption and Cancellation
// ---------------------------------------------------------------------------

func TestInterruptionDoesNotChargeBudget(t *testing.T) {
	state := fixtureRunningAgentTask()
	got, err := decision.Decide(state, endAttempt("n-1", 1, model.OutcomeInterrupted, ""))
	requireNoError(t, err)
	requireNode(t, got, "n-1", model.NodeReady)
	requireEvent(t, got, model.EventAttemptInterrupted)
	requireEvent(t, got, model.EventControlledStopRequested)
	// The controlled stop opens: the Run enters STOPPING with the gate
	// closed and the managed process receives its two-phase stop Effect;
	// the Workflow converges PAUSED only after the process facts settle
	// (PRD 已确认：Ctrl+C 两阶段有限停止).
	requireRun(t, got, model.RunStopping, false)
	requireEffect(t, got, model.ManagedProcessStopIntent{Process: "p-1"})
	for _, m := range got.Mutations {
		em, ok := m.(model.AttemptEndMutation)
		if !ok || em.Key.Node != "n-1" {
			continue
		}
		if em.RetryCharged {
			t.Fatalf("interrupted attempt must never charge retry budget: %+v", em)
		}
	}
	for _, m := range got.Mutations {
		if nm, ok := m.(model.NodeStatusMutation); ok && nm.Node == "n-1" && nm.RetryCharged != 0 {
			t.Fatalf("interruption charged node budget: %+v", nm)
		}
	}
	// The process settles: the stop converges to INTERRUPTED + PAUSED in
	// the same transaction.
	state = apply(t, state, got)
	got2, err := decision.Decide(state, model.EffectResultInput{Kind: model.ProcessStopped, Process: "p-1"})
	requireNoError(t, err)
	requireStatus(t, got2, model.StageExecution, model.RuntimePaused)
	requireRun(t, got2, model.RunInterrupted, false)
}

func TestUserInterruptedCodeIsNotProviderFailure(t *testing.T) {
	state := fixtureRunningAgentTask()
	got, err := decision.Decide(state, endAttempt("n-1", 1, model.OutcomeFailed, model.CodeUserInterrupted))
	requireNoError(t, err)
	requireNode(t, got, "n-1", model.NodeReady)
	requireRun(t, got, model.RunStopping, false)
	for _, m := range got.Mutations {
		if em, ok := m.(model.AttemptEndMutation); ok && em.Key.Node == "n-1" && em.RetryCharged {
			t.Fatalf("USER_INTERRUPTED charged retry budget: %+v", em)
		}
	}
	state = apply(t, state, got)
	got2, err := decision.Decide(state, model.EffectResultInput{Kind: model.ProcessStopped, Process: "p-1"})
	requireNoError(t, err)
	requireStatus(t, got2, model.StageExecution, model.RuntimePaused)
}

func TestInterruptionDuringQuiescingBlocksNotPauses(t *testing.T) {
	state := fixtureQuiescing() // n-2 RUNNING, blocking finding f-1 present
	got, err := decision.Decide(state, endAttempt("n-2", 1, model.OutcomeInterrupted, ""))
	requireNoError(t, err)
	requireStatus(t, got, model.StageExecution, model.RuntimeBlocked)
	// The interrupt opens the controlled stop (STOPPING) and, with
	// nothing left in flight, converges the Run to INTERRUPTED in the
	// same transaction; the blocking Finding keeps the Workflow BLOCKED
	// (PRD 已确认：并行失败后的 Quiescing rule 6).
	stopping, interrupted := false, false
	for _, m := range got.Mutations {
		rm, ok := m.(model.RunMutation)
		if !ok {
			continue
		}
		if rm.Status == model.RunStopping {
			stopping = true
		}
		if rm.Status == model.RunInterrupted {
			interrupted = true
		}
	}
	if !stopping || !interrupted {
		t.Fatalf("decision must open STOPPING and converge INTERRUPTED, mutations = %+v", got.Mutations)
	}
}

func TestCancelRequiresSettledProcesses(t *testing.T) {
	state := fixtureRunningAgentTask()
	got, err := decision.Decide(state, model.WorkflowCommandInput{Kind: model.CancelWorkflow})
	requireNoError(t, err)
	requireStatus(t, got, model.StageExecution, model.RuntimeRunning) // not CANCELLED yet
	requireRun(t, got, model.RunStopping, false)
	requireEvent(t, got, model.EventWorkflowCancelRequested)
	requireEffect(t, got, model.ManagedProcessStopIntent{Process: "p-1"})
	for _, m := range got.Mutations {
		if wm, ok := m.(model.WorkflowMutation); ok && wm.CancelIntent == nil {
			t.Fatalf("cancel decision must persist the cancel intent: %+v", wm)
		}
	}
	// process settles, then the attempt is interrupted
	state = apply(t, state, got)
	got2, err := decision.Decide(state, model.EffectResultInput{Kind: model.ProcessStopped, Process: "p-1"})
	requireNoError(t, err)
	state = apply(t, state, got2)
	got3, err := decision.Decide(state, endAttempt("n-1", 1, model.OutcomeInterrupted, ""))
	requireNoError(t, err)
	requireStatus(t, got3, model.StageExecution, model.RuntimeCancelled)
	requireNode(t, got3, "n-1", model.NodeCancelled)
	requireRun(t, got3, model.RunCancelled, false)
	requireEvent(t, got3, model.EventWorkflowCancelled)
	state = apply(t, state, got3)
	// terminal: no further cancel
	_, err = decision.Decide(state, model.WorkflowCommandInput{Kind: model.CancelWorkflow})
	assertFaultCode(t, err, model.CodeInvalidInput)
}

func TestCancelCompletesImmediatelyWhenIdle(t *testing.T) {
	state := workflowState(model.StageExecution, model.RuntimeRunning)
	got, err := decision.Decide(state, model.WorkflowCommandInput{Kind: model.CancelWorkflow})
	requireNoError(t, err)
	requireStatus(t, got, model.StageExecution, model.RuntimeCancelled)
	requireEvent(t, got, model.EventWorkflowCancelRequested)
	requireEvent(t, got, model.EventWorkflowCancelled)
}

// TestRetryableFailureDuringPendingCancelDefersRetry: a retryable Attempt
// failure arriving while a Cancel intent is pending must not allocate a
// successor Attempt and must not charge the Retry Budget (design 6.1:
// cancel may not allocate a Retry).
func TestRetryableFailureDuringPendingCancelDefersRetry(t *testing.T) {
	state := fixtureRunningAgentTask() // n-1 RUNNING #1, process p-1 RUNNING
	// cancel is requested while the attempt runs; the stop effect is issued
	got, err := decision.Decide(state, model.WorkflowCommandInput{Kind: model.CancelWorkflow})
	requireNoError(t, err)
	state = apply(t, state, got)
	if state.Workflow.CancelIntent == nil || state.Workflow.Runtime != model.RuntimeRunning {
		t.Fatalf("cancel intent not pending: %+v", state.Workflow)
	}

	// the attempt fails with a retryable code before the stop lands
	got2, err := decision.Decide(state, endAttempt("n-1", 1, model.OutcomeFailed, model.CodeCommandFailed))
	requireNoError(t, err)
	for _, m := range got2.Mutations {
		if _, ok := m.(model.AttemptAppendMutation); ok {
			t.Fatalf("pending cancel must not allocate a successor attempt: %+v", m)
		}
	}
	for _, m := range got2.Mutations {
		em, ok := m.(model.AttemptEndMutation)
		if !ok || em.Key.Node != "n-1" {
			continue
		}
		if em.Status != model.AttemptFailed {
			t.Fatalf("attempt ended %s, want FAILED", em.Status)
		}
		if em.RetryCharged {
			t.Fatalf("pending cancel charged retry budget: %+v", em)
		}
	}
	for _, m := range got2.Mutations {
		if nm, ok := m.(model.NodeStatusMutation); ok && nm.Node == "n-1" && nm.RetryCharged != 0 {
			t.Fatalf("pending cancel charged the node budget: %+v", nm)
		}
	}
	requireNode(t, got2, "n-1", model.NodeReady) // deferred, never dispatched

	// the process settles; the cancel completes; no successor ever appears
	state = apply(t, state, got2)
	got3, err := decision.Decide(state, model.EffectResultInput{Kind: model.ProcessStopped, Process: "p-1"})
	requireNoError(t, err)
	state = apply(t, state, got3)
	requireStatus(t, got3, model.StageExecution, model.RuntimeCancelled)
	requireNode(t, got3, "n-1", model.NodeCancelled)
	if len(state.Attempts) != 1 {
		t.Fatalf("attempt count = %d, want exactly 1 (no successor): %+v", len(state.Attempts), state.Attempts)
	}
	if state.Attempts[model.AttemptKey{Node: "n-1", Number: 1}].Status != model.AttemptFailed {
		t.Fatalf("failed attempt mutated by cancel: %+v", state.Attempts[model.AttemptKey{Node: "n-1", Number: 1}])
	}
}

// TestRetryableFailureCompletesPendingCancelWhenSettled: when the
// retryable failure is the last running fact, the deferred decision also
// completes the pending cancel in the same transaction, cancelling any
// pre-existing READY Attempt while preserving terminal failure facts.
func TestRetryableFailureCompletesPendingCancelWhenSettled(t *testing.T) {
	state := workflowState(model.StageExecution, model.RuntimeRunning)
	addRun(&state, model.RunRunning, true)
	addNode(&state, "n-1", model.NodeAgentTask, model.NodeRunning, 2)
	addAttempt(&state, "n-1", 1, model.AttemptRunning)
	// a READY successor allocated before the cancel must not linger
	addAttempt(&state, "n-1", 2, model.AttemptReady)

	got, err := decision.Decide(state, model.WorkflowCommandInput{Kind: model.CancelWorkflow})
	requireNoError(t, err)
	state = apply(t, state, got)

	got2, err := decision.Decide(state, endAttempt("n-1", 1, model.OutcomeFailed, model.CodeCommandFailed))
	requireNoError(t, err)
	for _, m := range got2.Mutations {
		if _, ok := m.(model.AttemptAppendMutation); ok {
			t.Fatalf("pending cancel must not allocate a successor attempt: %+v", m)
		}
	}
	requireStatus(t, got2, model.StageExecution, model.RuntimeCancelled)
	state = apply(t, state, got2)
	if state.Workflow.Runtime != model.RuntimeCancelled {
		t.Fatalf("workflow = %s, want CANCELLED", state.Workflow.Runtime)
	}
	if state.Nodes["n-1"].Status != model.NodeCancelled {
		t.Fatalf("node = %s, want CANCELLED", state.Nodes["n-1"].Status)
	}
	if len(state.Attempts) != 2 {
		t.Fatalf("attempt count = %d, want 2 (no successor)", len(state.Attempts))
	}
	if got := state.Attempts[model.AttemptKey{Node: "n-1", Number: 1}].Status; got != model.AttemptFailed {
		t.Fatalf("terminal failure attempt = %s, want FAILED (immutable)", got)
	}
	if got := state.Attempts[model.AttemptKey{Node: "n-1", Number: 2}].Status; got != model.AttemptCancelled {
		t.Fatalf("pre-cancel READY attempt = %s, want CANCELLED", got)
	}
}

func TestPauseStopsManagedProcesses(t *testing.T) {
	state := fixtureRunningAgentTask()
	got, err := decision.Decide(state, model.WorkflowCommandInput{Kind: model.PauseWorkflow})
	requireNoError(t, err)
	requireStatus(t, got, model.StageExecution, model.RuntimePaused)
	requireRun(t, got, model.RunStopping, false)
	requireEvent(t, got, model.EventControlledStopRequested)
	requireEffect(t, got, model.ManagedProcessStopIntent{Process: "p-1"})
	// the attempt settles as interrupted and never charges budget; the Run
	// stays STOPPING while the managed process is still settling
	state = apply(t, state, got)
	got1, err := decision.Decide(state, endAttempt("n-1", 1, model.OutcomeInterrupted, ""))
	requireNoError(t, err)
	requireNode(t, got1, "n-1", model.NodeReady)
	for _, m := range got1.Mutations {
		if em, ok := m.(model.AttemptEndMutation); ok && em.Key.Node == "n-1" && em.RetryCharged {
			t.Fatalf("interruption charged retry budget: %+v", em)
		}
	}
	// the process settles and the stop converges the Run INTERRUPTED
	state = apply(t, state, got1)
	got2, err := decision.Decide(state, model.EffectResultInput{Kind: model.ProcessStopped, Process: "p-1"})
	requireNoError(t, err)
	requireRun(t, got2, model.RunInterrupted, false)
}

// ---------------------------------------------------------------------------
// Approvals and evidence gating
// ---------------------------------------------------------------------------

func TestPlanApprovalBindsExactRevision(t *testing.T) {
	state := fixturePlanCheck(model.PlanChecked)
	got, err := decision.Decide(state, model.PlanApprovalInput{
		PlanRef: model.ArtifactRef{Workflow: "wf-1", Type: model.ArtifactPlan, Revision: 1, Hash: "plan-h"},
		Hash:    "plan-h"})
	requireNoError(t, err)
	requireStatus(t, got, model.StageSpecGeneration, model.RuntimePaused)
	requireEvent(t, got, model.EventPlanApproved)
	for _, m := range got.Mutations {
		if am, ok := m.(model.ApprovalAppendMutation); ok && am.Approval.Kind != model.ApprovalPlan {
			t.Fatalf("approval kind = %s, want plan", am.Approval.Kind)
		}
	}
}

func TestExecutionApprovalAdvancesToExecution(t *testing.T) {
	state := fixtureAwaitingExecutionApproval("workflow-a")
	got, err := decision.Decide(state, model.ExecutionApprovalInput{WorkflowHash: "workflow-a",
		PlanHash: "plan-h", SpecHashes: []string{"spec-1"}, CatalogHash: "cat-1",
		RoutingHash: "r-1", BudgetHash: "b-1", CommitPolicyHash: "cp-1"})
	requireNoError(t, err)
	// The approval resumes the workflow into EXECUTION and requests the
	// Integration Worktree creation (PRD Worktree 策略: the Integration
	// Ref is withheld until the Execution Approval).
	requireStatus(t, got, model.StageExecution, model.RuntimeRunning)
	requireEvent(t, got, model.EventExecutionApproved)
	if got.Effect == nil {
		t.Fatal("execution approval must request the integration worktree creation")
	}
	if _, ok := got.Effect.(model.IntegrationWorktreeCreateIntent); !ok {
		t.Fatalf("execution approval effect = %T, want IntegrationWorktreeCreateIntent", got.Effect)
	}
	for _, m := range got.Mutations {
		if am, ok := m.(model.ApprovalAppendMutation); ok && am.Approval.Kind != model.ApprovalExecution {
			t.Fatalf("approval kind = %s, want execution", am.Approval.Kind)
		}
	}
}

func TestCheckerPassIsNotUserApproval(t *testing.T) {
	// a CHECKED plan is a checker conclusion; reconcile must not auto-approve
	state := fixturePlanCheck(model.PlanChecked)
	got, err := decision.Decide(state, model.ReconcileInput{})
	requireNoError(t, err)
	for _, m := range got.Mutations {
		if _, ok := m.(model.ApprovalAppendMutation); ok {
			t.Fatal("checker pass must not create an approval")
		}
		if _, ok := m.(model.PlanMutation); ok {
			t.Fatal("checker pass must not change the plan status")
		}
	}
	requireNoWorkflowMutation(t, got)
}

// ---------------------------------------------------------------------------
// Apply
// ---------------------------------------------------------------------------

func TestApplyRequiresCompletedWorkflow(t *testing.T) {
	state := fixtureRunningAgentTask()
	_, err := decision.Decide(state, model.ApplyCommandInput{Kind: model.ApplyRequest,
		TargetHead: "main-head", IntegrationHead: "int-9"})
	assertFaultCode(t, err, model.CodeInvalidInput)
}

func TestApplyRefusesQuarantinedIntegration(t *testing.T) {
	state := fixtureCompleted()
	addQuarantine(&state, "cflow/integration/wf-1")
	_, err := decision.Decide(state, model.ApplyCommandInput{Kind: model.ApplyRequest,
		TargetHead: "main-head", IntegrationHead: "int-9"})
	assertFaultCode(t, err, model.CodeCommitDuringPolicyDriftWindow)
}

func TestApplyProtocolAndTargetStability(t *testing.T) {
	state := fixtureCompleted()
	request := model.ApplyCommandInput{Kind: model.ApplyRequest, TargetHead: "main-head",
		IntegrationHead: "int-9", Preflight: model.ArtifactRef{Workflow: "wf-1", Type: model.ArtifactWorkflow, Revision: 1, Hash: "wf-h"},
		PreflightHash: "cp-1", Fingerprint: "fp-1"}
	got, err := decision.Decide(state, request)
	requireNoError(t, err)
	requireEffect(t, got, model.ApplyStagingCreateIntent{Apply: "apply-1", TargetHead: "main-head", IntegrationHead: "int-9"})
	state = apply(t, state, got)

	// staging succeeded -> awaiting exact confirmation
	got2, err := decision.Decide(state, model.EffectResultInput{Kind: model.ApplyStagingSucceeded, ApplyAttempt: "apply-1"})
	requireNoError(t, err)
	requireNoEffect(t, got2)
	state = apply(t, state, got2)

	// drifted Target HEAD -> APPROVAL_INPUT_CHANGED-family refusal, no mutation
	drifted := model.ApplyCommandInput{Kind: model.ApplyConfirm, TargetHead: "main-head-other",
		IntegrationHead: "int-9", PreflightHash: "cp-1", Fingerprint: "fp-1"}
	_, err = decision.Decide(state, drifted)
	assertFaultCode(t, err, model.CodeTargetHeadChanged)

	// drifted commit-policy fingerprint -> COMMIT_POLICY_INPUT_CHANGED
	policyDrift := model.ApplyCommandInput{Kind: model.ApplyConfirm, TargetHead: "main-head",
		IntegrationHead: "int-9", PreflightHash: "cp-1", Fingerprint: "fp-2"}
	_, err = decision.Decide(state, policyDrift)
	assertFaultCode(t, err, model.CodeCommitPolicyInputChanged)

	// exact confirmation -> fast-forward effect
	confirm := model.ApplyCommandInput{Kind: model.ApplyConfirm, TargetHead: "main-head",
		IntegrationHead: "int-9", PreflightHash: "cp-1", Fingerprint: "fp-1"}
	got3, err := decision.Decide(state, confirm)
	requireNoError(t, err)
	requireEffect(t, got3, model.ApplyFastForwardIntent{Apply: "apply-1", TargetHead: "main-head"})
	state = apply(t, state, got3)

	// fast-forward success does not alter the completed Workflow
	got4, err := decision.Decide(state, model.EffectResultInput{Kind: model.ApplyFastForwardSucceeded, ApplyAttempt: "apply-1"})
	requireNoError(t, err)
	for _, m := range got4.Mutations {
		if _, ok := m.(model.WorkflowMutation); ok {
			t.Fatalf("apply success must not alter the Workflow: %+v", m)
		}
	}
	requireEvent(t, got4, model.EventApplySucceeded)
}

// ---------------------------------------------------------------------------
// Cleanup
// ---------------------------------------------------------------------------

func cleanupItems() []model.CleanupItem {
	return []model.CleanupItem{
		{Index: 0, Kind: model.CleanupWorktree, CanonicalPath: "/home/u/cflow/worktrees/p/wf-1/task-n-1",
			Branch: "task/n-1", ExpectedHead: "abc123", Fingerprint: "fp-1", Status: model.CleanupItemPending},
		{Index: 1, Kind: model.CleanupScratch, CanonicalPath: "/home/u/cflow/scratch/p/wf-1-1",
			Status: model.CleanupItemPending},
	}
}

func TestCleanupRequiresTerminalWorkflow(t *testing.T) {
	state := fixtureRunningAgentTask()
	_, err := decision.Decide(state, model.CleanupCommandInput{Kind: model.CleanupDryRun, Items: cleanupItems()})
	assertFaultCode(t, err, model.CodeCleanupWorkflowNotTerminal)
}

func TestCleanupDryRunBuildsImmutableManifest(t *testing.T) {
	state := fixtureCompleted()
	items := cleanupItems()
	got, err := decision.Decide(state, model.CleanupCommandInput{Kind: model.CleanupDryRun, Items: items})
	requireNoError(t, err)
	requireNoEffect(t, got)
	requireEvent(t, got, model.EventCleanupAttemptCreated)
	found := false
	for _, m := range got.Mutations {
		if cam, ok := m.(model.CleanupAppendMutation); ok {
			found = true
			if cam.CleanupAttempt.Status != model.CleanupStatusAwaitingConfirmation {
				t.Fatalf("dry run attempt status = %s, want awaiting confirmation", cam.CleanupAttempt.Status)
			}
			if cam.CleanupAttempt.Manifest.Hash != model.CleanupManifestHash(items) {
				t.Fatalf("manifest hash = %s, want %s", cam.CleanupAttempt.Manifest.Hash, model.CleanupManifestHash(items))
			}
		}
	}
	if !found {
		t.Fatal("dry run created no cleanup attempt")
	}
}

func TestCleanupExecuteRequiresExactManifest(t *testing.T) {
	state := fixtureCompleted()
	items := cleanupItems()
	got, err := decision.Decide(state, model.CleanupCommandInput{Kind: model.CleanupDryRun, Items: items})
	requireNoError(t, err)
	state = apply(t, state, got)

	_, err = decision.Decide(state, model.CleanupCommandInput{Kind: model.CleanupExecute,
		Manifest: model.ArtifactRef{Workflow: "wf-1", Type: model.ArtifactCleanupManifest, Revision: 1, Hash: "other"}, Items: items})
	assertFaultCode(t, err, model.CodeCleanupFactsChanged)
}

func TestCleanupExecuteRevalidatesFacts(t *testing.T) {
	state := fixtureCompleted()
	items := cleanupItems()
	got, err := decision.Decide(state, model.CleanupCommandInput{Kind: model.CleanupDryRun, Items: items})
	requireNoError(t, err)
	state = apply(t, state, got)
	manifest := state.CleanupAttempts[0].Manifest

	// observed facts drifted from the confirmed manifest
	drifted := append([]model.CleanupItem(nil), items...)
	drifted[0].ExpectedHead = "other-head"
	_, err = decision.Decide(state, model.CleanupCommandInput{Kind: model.CleanupExecute, Manifest: manifest, Items: drifted})
	assertFaultCode(t, err, model.CodeCleanupFactsChanged)

	// dirty target
	dirty := append([]model.CleanupItem(nil), items...)
	dirty[0].Dirty = true
	_, err = decision.Decide(state, model.CleanupCommandInput{Kind: model.CleanupExecute, Manifest: manifest, Items: dirty})
	assertFaultCode(t, err, model.CodeCleanupTargetDirty)

	// active managed process
	active := fixtureCompleted()
	active.CleanupAttempts = append(active.CleanupAttempts, state.CleanupAttempts...)
	active.Processes = append(active.Processes, model.ProcessRecord{ID: "p-1", Status: model.ProcessStatusRunning})
	_, err = decision.Decide(active, model.CleanupCommandInput{Kind: model.CleanupExecute, Manifest: manifest, Items: items})
	assertFaultCode(t, err, model.CodeCleanupActiveProcess)
}

func TestCleanupExecuteRequestsItemsInOrder(t *testing.T) {
	state := fixtureCompleted()
	items := cleanupItems()
	got, err := decision.Decide(state, model.CleanupCommandInput{Kind: model.CleanupDryRun, Items: items})
	requireNoError(t, err)
	state = apply(t, state, got)
	manifest := state.CleanupAttempts[0].Manifest

	got2, err := decision.Decide(state, model.CleanupCommandInput{Kind: model.CleanupExecute, Manifest: manifest, Items: items})
	requireNoError(t, err)
	requireEvent(t, got2, model.EventCleanupItemRequested)
	requireEffect(t, got2, model.CleanupWorktreeRemoveIntent{Cleanup: "cleanup-1", Item: 0})
	for _, m := range got2.Mutations {
		if cm, ok := m.(model.CleanupItemMutation); ok && (cm.Index != 0 || cm.Status != model.CleanupItemRequested) {
			t.Fatalf("cleanup item mutation = %+v", cm)
		}
	}

	// the first item is removed; the next pending item is requested
	state = apply(t, state, got2)
	got3, err := decision.Decide(state, model.EffectResultInput{Kind: model.CleanupItemRemovedResult, CleanupAttempt: "cleanup-1", ItemIndex: 0})
	requireNoError(t, err)
	requireEffect(t, got3, model.CleanupScratchRemoveIntent{Cleanup: "cleanup-1", Item: 1})
	requireEvent(t, got3, model.EventCleanupItemCompleted)
	state = apply(t, state, got3)

	got4, err := decision.Decide(state, model.EffectResultInput{Kind: model.CleanupItemRemovedResult, CleanupAttempt: "cleanup-1", ItemIndex: 1})
	requireNoError(t, err)
	requireNoEffect(t, got4)
	requireEvent(t, got4, model.EventCleanupItemCompleted)
	found := false
	for _, m := range got4.Mutations {
		if cm, ok := m.(model.CleanupMutation); ok && cm.ID == "cleanup-1" && cm.Status == model.CleanupStatusSucceeded {
			found = true
		}
	}
	if !found {
		t.Fatalf("cleanup attempt never succeeded: %#v", got4.Mutations)
	}
}

func TestCleanupItemFailureBlocksAttemptPreservingWorkflow(t *testing.T) {
	state := fixtureCompleted()
	items := cleanupItems()
	got, err := decision.Decide(state, model.CleanupCommandInput{Kind: model.CleanupDryRun, Items: items})
	requireNoError(t, err)
	state = apply(t, state, got)
	manifest := state.CleanupAttempts[0].Manifest

	got2, err := decision.Decide(state, model.CleanupCommandInput{Kind: model.CleanupExecute, Manifest: manifest, Items: items})
	requireNoError(t, err)
	state = apply(t, state, got2)

	got3, err := decision.Decide(state, model.EffectResultInput{Kind: model.CleanupItemFailedResult,
		CleanupAttempt: "cleanup-1", ItemIndex: 0, FailureCode: model.CodeCleanupItemFailed})
	requireNoError(t, err)
	requireEvent(t, got3, model.EventCleanupItemFailed)
	found := false
	for _, m := range got3.Mutations {
		if _, ok := m.(model.WorkflowMutation); ok {
			t.Fatalf("cleanup failure must not alter the Workflow: %+v", m)
		}
		if cm, ok := m.(model.CleanupMutation); ok && cm.ID == "cleanup-1" && cm.Status == model.CleanupStatusBlocked {
			found = true
		}
	}
	if !found {
		t.Fatalf("cleanup attempt never blocked: %#v", got3.Mutations)
	}
}

// ---------------------------------------------------------------------------
// determinism
// ---------------------------------------------------------------------------

func TestDecideIsByteIdenticalForIdenticalInput(t *testing.T) {
	cases := []struct {
		state model.State
		input model.Input
	}{
		{model.State{}, model.WorkflowCommandInput{Kind: model.CreateWorkflow, Workflow: "wf-1", Project: "p-1", TargetBranch: "main", BaseCommit: "base-1"}},
		{fixtureRunningAgentTask(), endAttempt("n-1", 1, model.OutcomeFailed, model.CodeCommandFailed)},
		{fixtureRunningAgentTask(), model.AgentEventInput{Kind: model.AgentClaimsComplete}},
		{fixtureAttemptFailedWithBudget(0), model.ReconcileInput{}},
		{fixtureTwoRunningAttempts(), endAttempt("n-1", 1, model.OutcomeFailed, model.CodeScopeViolation)},
		{fixtureAwaitingExecutionApproval("workflow-a"), model.ExecutionApprovalInput{WorkflowHash: "workflow-b"}},
	}
	for i, tc := range cases {
		d1, e1 := decision.Decide(tc.state, tc.input)
		d2, e2 := decision.Decide(tc.state, tc.input)
		if !reflect.DeepEqual(d1, d2) {
			t.Errorf("case %d: decisions differ for identical input: %#v vs %#v", i, d1, d2)
		}
		if !reflect.DeepEqual(e1, e2) {
			t.Errorf("case %d: errors differ for identical input: %#v vs %#v", i, e1, e2)
		}
	}
}

// ---------------------------------------------------------------------------
// property tests (task brief Step 5): bounded sequences of Commands and
// Effect Results asserting the model invariants
// ---------------------------------------------------------------------------

func TestPropertySequenceInvariants(t *testing.T) {
	rng := rand.New(rand.NewSource(20260802))
	for iter := 0; iter < 4; iter++ {
		state := fixtureTwoRunningAttempts()
		runSequence(t, &state, rng, 150)
	}
}

// runSequence drives a bounded sequence of random valid Commands and
// Effect Results, asserting after every step: Event sequence numbers never
// decrease, terminal Attempt facts never mutate, Retry creates
// attempt_number+1, Target never changes, quarantined refs never become
// runnable, and the Decision is byte-identical for identical State/Input.
func runSequence(t *testing.T, st *model.State, rng *rand.Rand, steps int) {
	t.Helper()
	target := st.Workflow.TargetBranch
	quarantined := map[string]bool{}
	for i := 0; i < steps && !st.Workflow.Runtime.IsTerminal(); i++ {
		if i == steps/2 {
			// recovery records a quarantine on the first node's branch
			for _, n := range st.Nodes {
				if n.Branch != "" {
					addQuarantine(st, n.Branch)
					quarantined[n.Branch] = true
					break
				}
			}
		}
		in := nextInput(st, rng)
		d1, e1 := decision.Decide(*st, in)
		d2, e2 := decision.Decide(*st, in)
		assertByteIdentical(t, d1, e1, d2, e2)
		if e1 != nil {
			continue // faults cause no mutation
		}
		assertRetryNumbering(t, d1)
		*st = apply(t, *st, d1)
		if st.Workflow.TargetBranch != target {
			t.Fatalf("Target Branch changed from %s to %s", target, st.Workflow.TargetBranch)
		}
		for branch := range quarantined {
			for _, n := range st.Nodes {
				if n.Branch == branch && (n.Status == model.NodeReady || n.Status == model.NodeRunning) {
					t.Fatalf("quarantined branch %s node %s became runnable (%s)", branch, n.ID, n.Status)
				}
			}
		}
	}
}

// assertRetryNumbering checks every successor Attempt created by a
// Decision carries attempt_number+1 of the failed Attempt it replaces
// (the predecessor must be ended as FAILED by the same Decision).
func assertRetryNumbering(t *testing.T, d model.Decision) {
	t.Helper()
	ended := map[model.AttemptKey]bool{}
	for _, m := range d.Mutations {
		if em, ok := m.(model.AttemptEndMutation); ok && em.Status == model.AttemptFailed {
			ended[em.Key] = true
		}
	}
	for _, m := range d.Mutations {
		am, ok := m.(model.AttemptAppendMutation)
		if !ok {
			continue
		}
		key := am.Attempt.Key
		if key.Number < 2 {
			continue
		}
		prev := model.AttemptKey{Node: key.Node, Number: key.Number - 1}
		if !ended[prev] {
			t.Fatalf("successor %s does not follow a failed attempt %s ended in the same decision", key, prev)
		}
		if key.Number != prev.Number+1 {
			t.Fatalf("successor %s is not attempt_number+1 of %s", key, prev)
		}
	}
}

// nextInput selects a bounded random input that is meaningful for the
// current state.
func nextInput(st *model.State, rng *rand.Rand) model.Input {
	running := runningAttempts(st)
	if len(running) > 0 {
		a := running[rng.Intn(len(running))]
		switch rng.Intn(10) {
		case 0, 1, 2, 3, 4:
			return model.EffectResultInput{Kind: model.AttemptEnded, Attempt: a.Key,
				Outcome: model.OutcomeSucceeded, EndHead: "commit-" + fmt.Sprint(rng.Intn(1e6)),
				Evidence: ev(model.EvidenceCommit, "commit-1")}
		case 5, 6, 7:
			code := []model.Code{model.CodeCommandFailed, model.CodeAgentTimeout,
				model.CodeMissingImplementationCommit, model.CodeDirtyTaskWorktree}[rng.Intn(4)]
			return endAttempt(string(a.Key.Node), int(a.Key.Number), model.OutcomeFailed, code)
		case 8:
			return endAttempt(string(a.Key.Node), int(a.Key.Number), model.OutcomeFailed, model.CodeScopeViolation)
		default:
			return endAttempt(string(a.Key.Node), int(a.Key.Number), model.OutcomeInterrupted, "")
		}
	}
	if st.Workflow.Runtime == model.RuntimeRunning && anyFailedNode(st) {
		return model.ReconcileInput{}
	}
	switch rng.Intn(4) {
	case 0:
		if st.Workflow.Runtime == model.RuntimePaused || st.Workflow.Runtime == model.RuntimeBlocked {
			return model.WorkflowCommandInput{Kind: model.ResumeWorkflow}
		}
	case 1:
		if st.Workflow.Runtime == model.RuntimeRunning {
			return model.WorkflowCommandInput{Kind: model.PauseWorkflow}
		}
	case 2:
		return model.WorkflowCommandInput{Kind: model.CancelWorkflow}
	}
	return model.ReconcileInput{}
}

func runningAttempts(st *model.State) []*model.Attempt {
	var out []*model.Attempt
	for _, a := range st.Attempts {
		if a.Status == model.AttemptRunning {
			out = append(out, a)
		}
	}
	return out
}

func anyFailedNode(st *model.State) bool {
	for _, n := range st.Nodes {
		if n.Status == model.NodeFailed {
			return true
		}
	}
	return false
}

// apply applies a Decision to the state, asserting the invariants the
// applier can observe: the Workflow/Node/Run transition matrices hold, the
// Event sequence requests never decrease, and terminal Attempts are never
// reopened.
func apply(t *testing.T, st model.State, d model.Decision) model.State {
	t.Helper()
	terminal := map[model.AttemptKey]bool{}
	for k, a := range st.Attempts {
		if a.Status.IsTerminal() {
			terminal[k] = true
		}
	}
	for _, m := range d.Mutations {
		switch m := m.(type) {
		case model.WorkflowMutation:
			if st.Workflow.Stage != "" && st.Workflow.Stage != m.Stage && !st.Workflow.Stage.CanTransitionTo(m.Stage) {
				t.Fatalf("illegal stage transition %s -> %s", st.Workflow.Stage, m.Stage)
			}
			if st.Workflow.Runtime != "" && st.Workflow.Runtime != m.Runtime && !st.Workflow.Runtime.CanTransitionTo(m.Runtime) {
				t.Fatalf("illegal runtime transition %s -> %s", st.Workflow.Runtime, m.Runtime)
			}
			st.Workflow.ID = m.ID
			st.Workflow.Project = m.Project
			st.Workflow.Stage = m.Stage
			st.Workflow.Runtime = m.Runtime
			st.Workflow.TargetBranch = m.TargetBranch
			st.Workflow.BaseCommit = m.BaseCommit
			st.Workflow.IntegrationBranch = m.IntegrationBranch
			st.Workflow.IntegrationHead = m.IntegrationHead
			st.Workflow.CancelIntent = m.CancelIntent
		case model.PlanMutation:
			if st.Plan == nil {
				t.Fatalf("plan mutation without a plan")
			}
			if st.Plan.Status != m.Status && !st.Plan.Status.CanTransitionTo(m.Status) {
				t.Fatalf("illegal plan transition %s -> %s", st.Plan.Status, m.Status)
			}
			st.Plan.Status = m.Status
		case model.NodeStatusMutation:
			n := st.Nodes[m.Node]
			if n == nil {
				t.Fatalf("node %s missing", m.Node)
			}
			if n.Status != m.Status && !n.Status.CanTransitionTo(m.Status) {
				t.Fatalf("illegal node transition %s -> %s", n.Status, m.Status)
			}
			n.Status = m.Status
			n.RetryCharged = m.RetryCharged
		case model.AttemptAppendMutation:
			if _, ok := st.Attempts[m.Attempt.Key]; ok {
				t.Fatalf("attempt %s already exists", m.Attempt.Key)
			}
			a := m.Attempt
			st.Attempts[a.Key] = &a
		case model.AttemptEndMutation:
			a := st.Attempts[m.Key]
			if a == nil {
				t.Fatalf("attempt %s missing", m.Key)
			}
			if a.Status.IsTerminal() {
				t.Fatalf("terminal attempt %s reopened", m.Key)
			}
			a.Status = m.Status
			a.EndHead = m.EndHead
			a.EndDirtyFingerprint = m.EndDirtyFingerprint
			a.FailureCode = m.FailureCode
			a.Evidence = m.Evidence
			a.RetryCharged = m.RetryCharged
			a.EndedAt = m.EndedAt
		case model.FindingAppendMutation:
			st.Findings = append(st.Findings, m.Finding)
		case model.ApprovalAppendMutation:
			st.Approvals = append(st.Approvals, m.Approval)
		case model.RunAppendMutation:
			st.Runs = append(st.Runs, m.Run)
		case model.RunMutation:
			run := findRun(st, m.ID)
			if run == nil {
				t.Fatalf("run %s missing", m.ID)
			}
			if run.Status != m.Status && !run.Status.CanTransitionTo(m.Status) {
				t.Fatalf("illegal run transition %s -> %s", run.Status, m.Status)
			}
			run.Status = m.Status
			run.DispatchGate = m.DispatchGate
			run.StopReason = m.StopReason
			run.QuiesceSnapshot = m.QuiesceSnapshot
		case model.SessionAppendMutation:
			st.Sessions = append(st.Sessions, m.Session)
		case model.ProcessAppendMutation:
			st.Processes = append(st.Processes, m.Process)
		case model.ProcessEndMutation:
			p := findProcess(st, m.ID)
			if p == nil {
				t.Fatalf("process %s missing", m.ID)
			}
			if p.Status != model.ProcessStatusRunning {
				t.Fatalf("process %s already ended (%s)", m.ID, p.Status)
			}
			p.Status = m.Status
			p.ExitCode = m.ExitCode
			p.EndedAt = m.EndedAt
		case model.QuarantineAppendMutation:
			st.Quarantines = append(st.Quarantines, m.Quarantine)
		case model.ApplyAppendMutation:
			st.ApplyAttempts = append(st.ApplyAttempts, m.ApplyAttempt)
		case model.ApplyMutation:
			att := findApply(st, m.ID)
			if att == nil {
				t.Fatalf("apply attempt %s missing", m.ID)
			}
			att.Status = m.Status
			att.EndedAt = m.EndedAt
		case model.CleanupAppendMutation:
			st.CleanupAttempts = append(st.CleanupAttempts, m.CleanupAttempt)
		case model.CleanupMutation:
			att := findCleanup(st, m.ID)
			if att == nil {
				t.Fatalf("cleanup attempt %s missing", m.ID)
			}
			att.Status = m.Status
			att.EndedAt = m.EndedAt
		case model.CleanupItemMutation:
			att := findCleanup(st, m.Attempt)
			if att == nil {
				t.Fatalf("cleanup attempt %s missing", m.Attempt)
			}
			if m.Index < 0 || m.Index >= len(att.Items) {
				t.Fatalf("cleanup item index %d out of range", m.Index)
			}
			item := &att.Items[m.Index]
			if item.Status.IsTerminal() {
				t.Fatalf("terminal cleanup item %d reopened", m.Index)
			}
			item.Status = m.Status
			item.FailureCode = m.FailureCode
		}
	}
	for i, e := range d.Events {
		if e.Seq != st.NextEventSeq {
			t.Fatalf("event %d has seq %d, want %d: sequence requests must never decrease", i, e.Seq, st.NextEventSeq)
		}
		st.NextEventSeq++
	}
	for k := range terminal {
		if !st.Attempts[k].Status.IsTerminal() {
			t.Fatalf("terminal attempt %s was mutated", k)
		}
	}
	st.Version++
	return st
}

func findRun(st model.State, id model.RunID) *model.Run {
	for i := range st.Runs {
		if st.Runs[i].ID == id {
			return &st.Runs[i]
		}
	}
	return nil
}

func findProcess(st model.State, id model.ProcessID) *model.ProcessRecord {
	for i := range st.Processes {
		if st.Processes[i].ID == id {
			return &st.Processes[i]
		}
	}
	return nil
}

func findApply(st model.State, id model.ApplyAttemptID) *model.ApplyAttempt {
	for i := range st.ApplyAttempts {
		if st.ApplyAttempts[i].ID == id {
			return &st.ApplyAttempts[i]
		}
	}
	return nil
}

func findCleanup(st model.State, id model.CleanupAttemptID) *model.CleanupAttempt {
	for i := range st.CleanupAttempts {
		if st.CleanupAttempts[i].ID == id {
			return &st.CleanupAttempts[i]
		}
	}
	return nil
}

// TestPropertyCompletedNeverChangesTarget drives an Apply sequence on a
// completed Workflow: Apply success must not alter Workflow completion and
// must never touch the Target Branch.
func TestPropertyCompletedNeverChangesTarget(t *testing.T) {
	state := fixtureCompleted()
	target := state.Workflow.TargetBranch
	stage, rt := state.Workflow.Stage, state.Workflow.Runtime

	steps := []model.Input{
		model.ApplyCommandInput{Kind: model.ApplyRequest, TargetHead: "main-head",
			IntegrationHead: "int-9", Preflight: model.ArtifactRef{Workflow: "wf-1", Type: model.ArtifactWorkflow, Revision: 1, Hash: "wf-h"},
			PreflightHash: "cp-1", Fingerprint: "fp-1"},
		model.EffectResultInput{Kind: model.ApplyStagingSucceeded, ApplyAttempt: "apply-1"},
		model.ApplyCommandInput{Kind: model.ApplyConfirm, TargetHead: "main-head",
			IntegrationHead: "int-9", PreflightHash: "cp-1", Fingerprint: "fp-1"},
		model.EffectResultInput{Kind: model.ApplyFastForwardSucceeded, ApplyAttempt: "apply-1"},
	}
	for _, in := range steps {
		d, err := decision.Decide(state, in)
		requireNoError(t, err)
		state = apply(t, state, d)
	}
	if state.Workflow.Stage != stage || state.Workflow.Runtime != rt {
		t.Fatalf("apply changed completed workflow to %s/%s", state.Workflow.Stage, state.Workflow.Runtime)
	}
	if state.Workflow.TargetBranch != target {
		t.Fatalf("apply changed Target Branch to %s", state.Workflow.TargetBranch)
	}
	if len(state.ApplyAttempts) != 1 || state.ApplyAttempts[0].Status != model.ApplySucceeded {
		t.Fatalf("apply attempt = %+v", state.ApplyAttempts)
	}
}

// TestPropertyQuarantinedRefsNeverRunnable drives a sequence in which a
// Task Branch becomes quarantined: the Node must never return READY, no
// successor Attempt may be created, and Apply must refuse the
// Integration.
func TestPropertyQuarantinedRefsNeverRunnable(t *testing.T) {
	state := fixtureRunningAgentTask()
	addQuarantine(&state, "task/n-1")

	// retryable failure on a quarantined Branch must not return the Node to READY
	got, err := decision.Decide(state, endAttempt("n-1", 1, model.OutcomeFailed, model.CodeCommandFailed))
	requireNoError(t, err)
	requireNode(t, got, "n-1", model.NodeFailed)
	requireStatus(t, got, model.StageExecution, model.RuntimeBlocked)
	for _, m := range got.Mutations {
		if _, ok := m.(model.AttemptAppendMutation); ok {
			t.Fatalf("quarantined retry created a successor attempt: %+v", m)
		}
	}
	requireEvent(t, got, model.EventFindingOpened)

	// Apply cannot re-enter the quarantined delivery chain
	state2 := fixtureCompleted()
	addQuarantine(&state2, "cflow/integration/wf-1")
	_, err = decision.Decide(state2, model.ApplyCommandInput{Kind: model.ApplyRequest,
		TargetHead: "main-head", IntegrationHead: "int-9"})
	assertFaultCode(t, err, model.CodeCommitDuringPolicyDriftWindow)
}

// ---------------------------------------------------------------------------
// Task 12: serialized dispatch allocation (design 12)
// ---------------------------------------------------------------------------

// fixtureExecutionStage is a workflow at EXECUTION with an open dispatch
// gate and the Integration HEAD recorded (the Execution Approval plus the
// Integration Worktree creation completed).
func fixtureExecutionStage() model.State {
	st := workflowState(model.StageExecution, model.RuntimeRunning)
	addRun(&st, model.RunRunning, true)
	return st
}

// TestDispatchCommitsRunningAttemptBeforeEffects: one serialized
// allocation commits the RUNNING Attempt, the Task Base at readiness, and
// the Task Worktree creation Effect together; the Kernel revalidates the
// committed gate before anything commits (design 12).
func TestDispatchCommitsRunningAttemptBeforeEffects(t *testing.T) {
	state := fixtureExecutionStage()
	addNode(&state, "task-1", model.NodeAgentTask, model.NodePending, 2)
	got, err := decision.Decide(state, model.DispatchInput{Node: "task-1", Session: "s-1", Route: "fake", BaseHead: "int-1"})
	requireNoError(t, err)
	requireNode(t, got, "task-1", model.NodeRunning)
	requireEvent(t, got, model.EventNodeStarted)
	requireEvent(t, got, model.EventAttemptCreated)

	foundAttempt := false
	foundSession := false
	foundBase := false
	for _, m := range got.Mutations {
		switch mm := m.(type) {
		case model.AttemptAppendMutation:
			if mm.Attempt.Key != (model.AttemptKey{Node: "task-1", Number: 1}) ||
				mm.Attempt.Status != model.AttemptRunning ||
				mm.Attempt.Session != "s-1" ||
				mm.Attempt.StartHead != "int-1" {
				t.Fatalf("attempt append = %+v", mm.Attempt)
			}
			foundAttempt = true
		case model.SessionAppendMutation:
			if mm.Session.ID != "s-1" || mm.Session.Purpose != model.PurposeImplementation ||
				mm.Session.Status != model.SessionStarting || mm.Provider != "fake" {
				t.Fatalf("session append = %+v (provider %q)", mm.Session, mm.Provider)
			}
			foundSession = true
		case model.TaskMutation:
			if mm.Node != "task-1" || mm.BaseCommit != "int-1" {
				t.Fatalf("task mutation = %+v", mm)
			}
			foundBase = true
		}
	}
	if !foundAttempt || !foundSession || !foundBase {
		t.Fatalf("allocation missing attempt/session/base mutations: %+v", got.Mutations)
	}
	effect, ok := got.Effect.(model.TaskWorktreeCreateIntent)
	if !ok {
		t.Fatalf("allocation effect = %T, want TaskWorktreeCreateIntent", got.Effect)
	}
	if effect.Node != "task-1" || effect.BaseHead != "int-1" ||
		effect.Branch != "cflow/wf-1/task-task-1" {
		t.Fatalf("task worktree intent = %+v", effect)
	}
}

// TestDispatchGateClosureRejectsQueuedAllocation: an allocation whose gate
// closed between candidate computation and commit is refused with
// DISPATCH_GATE_CLOSED and mutates nothing (PRD 已确认：并行失败后的
// Quiescing: an in-memory queued goroutine is not an in-flight Attempt).
func TestDispatchGateClosureRejectsQueuedAllocation(t *testing.T) {
	state := fixtureExecutionStage()
	addNode(&state, "task-1", model.NodeAgentTask, model.NodePending, 2)
	// The committed gate closes (Pause, a failure closure, or a Quiesce)
	// before the queued allocation commits.
	state.Runs[0].Status = model.RunQuiescing
	state.Runs[0].DispatchGate = false
	_, err := decision.Decide(state, model.DispatchInput{Node: "task-1", Session: "s-1", Route: "fake", BaseHead: "int-1"})
	assertFaultCode(t, err, model.CodeDispatchGateClosed)

	state.Runs[0].Status = model.RunRunning
	state.Runs[0].DispatchGate = false
	_, err = decision.Decide(state, model.DispatchInput{Node: "task-1", Session: "s-1", Route: "fake", BaseHead: "int-1"})
	assertFaultCode(t, err, model.CodeDispatchGateClosed)
}

// TestDispatchRejectsNonTaskKind: only agent-task Nodes are dispatched by
// this build; Verify, Merge, Checkpoint, and FinalVerify Nodes arrive with
// their Task 13 engines and are never silently skipped.
func TestDispatchRejectsNonTaskKind(t *testing.T) {
	// Every compiled Node kind dispatches through its own decision
	// (agent-task, verify, merge, checkpoint and final-verify — the last
	// two since Task 18); the review kind is never carried by a compiled
	// workflow, so its dispatch is refused.
	state := fixtureExecutionStage()
	addNode(&state, "n-1", model.NodeReview, model.NodePending, 0)
	_, err := decision.Decide(state, model.DispatchInput{Node: "n-1", Session: "s-1", Route: "fake", BaseHead: "int-1"})
	assertFaultCode(t, err, model.CodeInvalidInput)
}

// TestDispatchRejectsRunningOrTerminalNodes: a Node that is already
// RUNNING or terminal can never be allocated again; readiness is never
// inferred from display status.
func TestDispatchRejectsRunningOrTerminalNodes(t *testing.T) {
	for _, status := range []model.NodeStatus{
		model.NodeRunning, model.NodeSucceeded, model.NodeFailed, model.NodeCancelled, model.NodeSkipped,
	} {
		state := fixtureExecutionStage()
		addNode(&state, "task-1", model.NodeAgentTask, status, 2)
		_, err := decision.Decide(state, model.DispatchInput{Node: "task-1", Session: "s-1", Route: "fake", BaseHead: "int-1"})
		assertFaultCode(t, err, model.CodeInvalidInput)
	}
}

// TestDispatchRejectsBusySession: the allocation Session identity is
// fixed by the Application before the Effect (design 6.2 rule 6); a reused
// identity is rejected.
func TestDispatchRejectsBusySession(t *testing.T) {
	state := fixtureExecutionStage()
	addNode(&state, "task-1", model.NodeAgentTask, model.NodePending, 2)
	state.Sessions = append(state.Sessions, model.Session{ID: "s-1", Purpose: model.PurposePlanning})
	_, err := decision.Decide(state, model.DispatchInput{Node: "task-1", Session: "s-1", Route: "fake", BaseHead: "int-1"})
	assertFaultCode(t, err, model.CodeInvalidInput)
}

// TestTaskWorktreeCreatedRequestsCodingSession: the Task Worktree result
// records the created worktree and requests the coding Session in it; the
// route comes from the allocated Session's Provider.
func TestTaskWorktreeCreatedRequestsCodingSession(t *testing.T) {
	state := fixtureExecutionStage()
	addNode(&state, "task-1", model.NodeAgentTask, model.NodeRunning, 2)
	key := addAttempt(&state, "task-1", 1, model.AttemptRunning)
	state.Attempts[key].Session = "s-1"
	state.Sessions = append(state.Sessions, model.Session{
		ID: "s-1", Purpose: model.PurposeImplementation, Status: model.SessionStarting, Provider: "fake",
	})
	got, err := decision.Decide(state, model.EffectResultInput{
		Kind: model.TaskWorktreeCreated, Attempt: key, WorktreePath: "/home/tasks/task-1",
	})
	requireNoError(t, err)
	found := false
	for _, m := range got.Mutations {
		if tm, ok := m.(model.TaskMutation); ok && tm.Node == "task-1" && tm.WorktreePath == "/home/tasks/task-1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("task worktree result misses the worktree path mutation: %+v", got.Mutations)
	}
	effect, ok := got.Effect.(model.ProviderStartIntent)
	if !ok {
		t.Fatalf("task worktree result effect = %T, want ProviderStartIntent", got.Effect)
	}
	if effect.Session != "s-1" || effect.Purpose != model.PurposeImplementation ||
		effect.Route != "fake" || effect.Node != "task-1" {
		t.Fatalf("coding session intent = %+v", effect)
	}
}

// TestImplementationRunEndedSettlesSessionOnly: a settled coding Session
// records its Session facts; the Coding Agent's output can never set
// state and the Attempt stays RUNNING for the Task 13 Commit gate.
func TestImplementationRunEndedSettlesSessionOnly(t *testing.T) {
	state := fixtureExecutionStage()
	addNode(&state, "task-1", model.NodeAgentTask, model.NodeRunning, 2)
	key := addAttempt(&state, "task-1", 1, model.AttemptRunning)
	state.Attempts[key].Session = "s-1"
	state.Sessions = append(state.Sessions, model.Session{
		ID: "s-1", Purpose: model.PurposeImplementation, Status: model.SessionStarting, Provider: "fake",
	})
	got, err := decision.Decide(state, model.EffectResultInput{
		Kind: model.ProviderRunEnded, Attempt: key,
		Session: model.Session{ID: "s-1", ProviderSessionID: "p-1",
			Purpose: model.PurposeImplementation, Status: model.SessionCompleted, Provider: "fake"},
	})
	requireNoError(t, err)
	if got.Effect != nil {
		t.Fatalf("settled coding session requests an effect: %+v", got.Effect)
	}
	for _, m := range got.Mutations {
		if sem, ok := m.(model.SessionEndMutation); ok && sem.ID == "s-1" && sem.Status == model.SessionCompleted {
			return
		}
	}
	t.Fatalf("coding session result misses the session settlement: %+v", got.Mutations)
}

// TestGraphInstallRecordsApprovedDagPending: the graph install records
// every approved node PENDING with its skeleton edges; a second install or
// an install outside EXECUTION is refused.
func TestGraphInstallRecordsApprovedDagPending(t *testing.T) {
	state := fixtureExecutionStage()
	got, err := decision.Decide(state, model.GraphInstallInput{Nodes: []model.InstallNode{
		{ID: "task-1", Kind: model.NodeAgentTask, Dependencies: []model.NodeID{"merge-1"}, RetryBudget: 2},
		{ID: "merge-1", Kind: model.NodeMerge},
	}})
	requireNoError(t, err)
	requireEvent(t, got, model.EventGraphInstalled)
	installed := map[model.NodeID]bool{}
	for _, m := range got.Mutations {
		nm, ok := m.(model.NodeAppendMutation)
		if !ok {
			continue
		}
		installed[nm.Node.ID] = true
		if nm.Node.Status != model.NodePending {
			t.Fatalf("installed node %s starts %s, want PENDING", nm.Node.ID, nm.Node.Status)
		}
		if nm.Node.Kind == model.NodeAgentTask && nm.Node.Branch != "cflow/wf-1/task-task-1" {
			t.Fatalf("task node branch = %q", nm.Node.Branch)
		}
		if nm.Node.Kind != model.NodeAgentTask && nm.Node.Branch != "" {
			t.Fatalf("non-task node %s carries a branch %q", nm.Node.ID, nm.Node.Branch)
		}
	}
	if !installed["task-1"] || !installed["merge-1"] {
		t.Fatalf("install missing nodes: %v", installed)
	}

	// The graph is already installed: a second install is refused.
	installedState := fixtureExecutionStage()
	addNode(&installedState, "task-1", model.NodeAgentTask, model.NodePending, 2)
	_, err = decision.Decide(installedState, model.GraphInstallInput{Nodes: []model.InstallNode{
		{ID: "task-2", Kind: model.NodeAgentTask},
	}})
	assertFaultCode(t, err, model.CodeInvalidInput)

	// An install outside EXECUTION is refused.
	planning := workflowState(model.StageWorkflowGeneration, model.RuntimeRunning)
	_, err = decision.Decide(planning, model.GraphInstallInput{Nodes: []model.InstallNode{
		{ID: "task-1", Kind: model.NodeAgentTask},
	}})
	assertFaultCode(t, err, model.CodeInvalidInput)
}

// TestGraphInstallRejectsDanglingDependencies: an install whose edges
// reference nodes outside the graph would produce an unserializable DAG
// and is refused.
func TestGraphInstallRejectsDanglingDependencies(t *testing.T) {
	state := fixtureExecutionStage()
	_, err := decision.Decide(state, model.GraphInstallInput{Nodes: []model.InstallNode{
		{ID: "task-1", Kind: model.NodeAgentTask, Dependencies: []model.NodeID{"merge-ghost"}},
	}})
	assertFaultCode(t, err, model.CodeInvalidInput)
}

// TestRetryAllocationReusesRecordedBaseWithoutWorktreeEffect (review fix
// #2): a budgeted retry (READY) reuses the recorded immutable Task Base —
// the freshly observed Integration HEAD is ignored, so the Task never
// silently rebases (PRD Worktree 策略) — and emits no Task Worktree
// creation Effect (the Worktree already exists from the first
// allocation); the coding Session starts directly inside it.
func TestRetryAllocationReusesRecordedBaseWithoutWorktreeEffect(t *testing.T) {
	state := fixtureExecutionStage()
	addNode(&state, "task-1", model.NodeAgentTask, model.NodeReady, 2)
	state.Nodes["task-1"].RetryCharged = 1
	state.Nodes["task-1"].BaseCommit = "base-1"
	got, err := decision.Decide(state, model.DispatchInput{
		Node: "task-1", Session: "s-2", Route: "fake", BaseHead: "int-9",
	})
	requireNoError(t, err)
	requireNode(t, got, "task-1", model.NodeRunning)
	requireEvent(t, got, model.EventNodeStarted)
	requireEvent(t, got, model.EventAttemptCreated)

	foundAttempt := false
	foundBase := false
	for _, m := range got.Mutations {
		switch mm := m.(type) {
		case model.AttemptAppendMutation:
			if mm.Attempt.Key != (model.AttemptKey{Node: "task-1", Number: 1}) ||
				mm.Attempt.Status != model.AttemptRunning ||
				mm.Attempt.Session != "s-2" ||
				mm.Attempt.StartHead != "base-1" {
				t.Fatalf("retry attempt append = %+v", mm.Attempt)
			}
			foundAttempt = true
		case model.SessionAppendMutation:
			if mm.Session.ID != "s-2" || mm.Session.Purpose != model.PurposeImplementation ||
				mm.Session.Status != model.SessionStarting || mm.Provider != "fake" {
				t.Fatalf("retry session append = %+v (provider %q)", mm.Session, mm.Provider)
			}
		case model.TaskMutation:
			// The recorded Base is immutable: a retry never re-records it.
			foundBase = true
		}
	}
	if !foundAttempt {
		t.Fatalf("retry allocation missing the attempt mutation: %+v", got.Mutations)
	}
	if foundBase {
		t.Fatalf("retry allocation re-recorded the task base: %+v", got.Mutations)
	}
	effect, ok := got.Effect.(model.ProviderStartIntent)
	if !ok {
		t.Fatalf("retry allocation effect = %T, want ProviderStartIntent (no worktree creation)", got.Effect)
	}
	if effect.Node != "task-1" || effect.Session != "s-2" || effect.Route != "fake" {
		t.Fatalf("retry coding intent = %+v", effect)
	}
}

// TestRetryWithoutRecordedBaseRefused: a READY allocation with no
// recorded Task Base is refused — the retry cannot rebase onto a freshly
// observed HEAD (PRD Worktree 策略: the Task never silently rebases).
func TestRetryWithoutRecordedBaseRefused(t *testing.T) {
	state := fixtureExecutionStage()
	addNode(&state, "task-1", model.NodeAgentTask, model.NodeReady, 2)
	state.Nodes["task-1"].RetryCharged = 1
	_, err := decision.Decide(state, model.DispatchInput{
		Node: "task-1", Session: "s-2", Route: "fake", BaseHead: "int-9",
	})
	assertFaultCode(t, err, model.CodeInvalidInput)
}
