// Package model_test verifies the canonical domain model: the legal and
// illegal transition matrices for every authoritative state machine, the
// Stage/Runtime pair rules, terminal sets, closed enums, the compiled
// fault-policy table, and State validation (PRD 状态机与持久化模型,
// design 7 and 8).
package model

import (
	"slices"
	"testing"
	"time"
)

var fixedTestNow = time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)

// allStatuses enumerates every value of each status enum used by the
// matrix tests.
var (
	allStages  = []WorkflowStage{StageRequirementDiscussion, StagePlanGeneration, StagePlanCheck, StageSpecGeneration, StageWorkflowGeneration, StageExecution, StageFinalVerification, StageCompleted}
	allRuntime = []RuntimeStatus{RuntimePending, RuntimeRunning, RuntimePaused, RuntimeBlocked, RuntimeFailed, RuntimeSucceeded, RuntimeCancelled}
	allNodes   = []NodeStatus{NodePending, NodeReady, NodeRunning, NodeSucceeded, NodeFailed, NodeCancelled, NodeSkipped}
	allPlans   = []PlanStatus{PlanDraft, PlanChecking, PlanChecked, PlanApproved, PlanStale, PlanRejected}
	allRuns    = []RunStatus{RunStarting, RunRunning, RunQuiescing, RunStopping, RunInterrupted, RunBlocked, RunSucceeded, RunFailed, RunCancelled}
	allSession = []SessionStatus{SessionStarting, SessionActive, SessionInterrupted, SessionPaused, SessionCompleted, SessionFailed, SessionCancelled, SessionLost}
	allAttempt = []AttemptStatus{AttemptReady, AttemptRunning, AttemptSucceeded, AttemptFailed, AttemptInterrupted, AttemptCancelled}
)

// assertMatrix verifies that can reports exactly the transitions listed in
// legal for every ordered pair of statuses. Every pair not listed is an
// illegal transition, so the table encodes both the legal and illegal
// matrices as data.
func assertMatrix[E ~string](t *testing.T, all []E, legal map[E][]E, can func(E, E) bool) {
	t.Helper()
	for _, from := range all {
		for _, to := range all {
			want := slices.Contains(legal[from], to)
			if got := can(from, to); got != want {
				t.Errorf("%s -> %s: can = %v, want %v", from, to, got, want)
			}
		}
	}
}

func TestWorkflowStageMatrix(t *testing.T) {
	assertMatrix(t, allStages, map[WorkflowStage][]WorkflowStage{
		StageRequirementDiscussion: {StagePlanGeneration},
		StagePlanGeneration:        {StagePlanCheck},
		StagePlanCheck:             {StageRequirementDiscussion, StagePlanGeneration, StageSpecGeneration},
		StageSpecGeneration:        {StageWorkflowGeneration},
		StageWorkflowGeneration:    {StageExecution},
		StageExecution:             {StageFinalVerification},
		StageFinalVerification:     {StageExecution, StageCompleted},
		StageCompleted:             nil,
	}, WorkflowStage.CanTransitionTo)
}

func TestRuntimeStatusMatrix(t *testing.T) {
	assertMatrix(t, allRuntime, map[RuntimeStatus][]RuntimeStatus{
		RuntimePending:   {RuntimeRunning, RuntimeCancelled, RuntimeFailed},
		RuntimeRunning:   {RuntimePaused, RuntimeBlocked, RuntimeSucceeded, RuntimeFailed, RuntimeCancelled},
		RuntimePaused:    {RuntimeRunning, RuntimeCancelled, RuntimeFailed},
		RuntimeBlocked:   {RuntimeRunning, RuntimeCancelled, RuntimeFailed},
		RuntimeSucceeded: nil,
		RuntimeFailed:    nil,
		RuntimeCancelled: nil,
	}, RuntimeStatus.CanTransitionTo)
}

func TestNodeStatusMatrix(t *testing.T) {
	assertMatrix(t, allNodes, map[NodeStatus][]NodeStatus{
		NodePending:   {NodeReady, NodeCancelled, NodeSkipped},
		NodeReady:     {NodeRunning, NodeCancelled, NodeSkipped},
		NodeRunning:   {NodeSucceeded, NodeFailed, NodeCancelled, NodeReady},
		NodeSucceeded: nil,
		NodeFailed:    nil,
		NodeCancelled: nil,
		NodeSkipped:   nil,
	}, NodeStatus.CanTransitionTo)
}

func TestPlanStatusMatrix(t *testing.T) {
	assertMatrix(t, allPlans, map[PlanStatus][]PlanStatus{
		PlanDraft:    {PlanChecking},
		PlanChecking: {PlanDraft, PlanChecked, PlanRejected},
		PlanChecked:  {PlanApproved, PlanStale},
		PlanApproved: {PlanStale},
		PlanStale:    {PlanDraft},
		PlanRejected: {PlanDraft},
	}, PlanStatus.CanTransitionTo)
}

func TestRunStatusMatrix(t *testing.T) {
	assertMatrix(t, allRuns, map[RunStatus][]RunStatus{
		RunStarting:    {RunRunning},
		RunRunning:     {RunQuiescing, RunStopping, RunBlocked, RunSucceeded, RunFailed, RunInterrupted},
		RunQuiescing:   {RunBlocked, RunStopping, RunInterrupted},
		RunStopping:    {RunInterrupted, RunCancelled},
		RunInterrupted: nil,
		RunBlocked:     nil,
		RunSucceeded:   nil,
		RunFailed:      nil,
		RunCancelled:   nil,
	}, RunStatus.CanTransitionTo)
}

// TestStageRuntimePairs encodes the PRD typical-combination table: a
// COMPLETED Workflow must be SUCCEEDED, SUCCEEDED is reserved for the
// COMPLETED Stage, PENDING exists only before the first Start, and every
// non-completed Stage may carry CANCELLED, FAILED, BLOCKED, PAUSED or
// RUNNING (PRD 状态机与持久化模型, "典型组合").
func TestStageRuntimePairs(t *testing.T) {
	legal := map[WorkflowStage][]RuntimeStatus{
		StageRequirementDiscussion: {RuntimePending, RuntimeRunning, RuntimePaused, RuntimeBlocked, RuntimeFailed, RuntimeCancelled},
		StagePlanGeneration:        {RuntimeRunning, RuntimePaused, RuntimeBlocked, RuntimeFailed, RuntimeCancelled},
		StagePlanCheck:             {RuntimeRunning, RuntimePaused, RuntimeBlocked, RuntimeFailed, RuntimeCancelled},
		StageSpecGeneration:        {RuntimeRunning, RuntimePaused, RuntimeBlocked, RuntimeFailed, RuntimeCancelled},
		StageWorkflowGeneration:    {RuntimeRunning, RuntimePaused, RuntimeBlocked, RuntimeFailed, RuntimeCancelled},
		StageExecution:             {RuntimeRunning, RuntimePaused, RuntimeBlocked, RuntimeFailed, RuntimeCancelled},
		StageFinalVerification:     {RuntimeRunning, RuntimePaused, RuntimeBlocked, RuntimeFailed, RuntimeCancelled},
		StageCompleted:             {RuntimeSucceeded},
	}
	for _, s := range allStages {
		for _, r := range allRuntime {
			want := slices.Contains(legal[s], r)
			if got := s.ValidWithRuntime(r); got != want {
				t.Errorf("%s with %s: valid = %v, want %v", s, r, got, want)
			}
		}
	}
}

func TestTerminalSets(t *testing.T) {
	cases := []struct {
		name string
		got  map[string]bool
	}{
		{"RuntimeStatus", map[string]bool{
			"PENDING": RuntimePending.IsTerminal(), "RUNNING": RuntimeRunning.IsTerminal(),
			"PAUSED": RuntimePaused.IsTerminal(), "BLOCKED": RuntimeBlocked.IsTerminal(),
			"FAILED": RuntimeFailed.IsTerminal(), "SUCCEEDED": RuntimeSucceeded.IsTerminal(),
			"CANCELLED": RuntimeCancelled.IsTerminal(),
		}},
		{"NodeStatus", map[string]bool{
			"PENDING": NodePending.IsTerminal(), "READY": NodeReady.IsTerminal(),
			"RUNNING": NodeRunning.IsTerminal(), "SUCCEEDED": NodeSucceeded.IsTerminal(),
			"FAILED": NodeFailed.IsTerminal(), "CANCELLED": NodeCancelled.IsTerminal(),
			"SKIPPED": NodeSkipped.IsTerminal(),
		}},
		{"AttemptStatus", map[string]bool{
			"READY": AttemptReady.IsTerminal(), "RUNNING": AttemptRunning.IsTerminal(),
			"SUCCEEDED": AttemptSucceeded.IsTerminal(), "FAILED": AttemptFailed.IsTerminal(),
			"INTERRUPTED": AttemptInterrupted.IsTerminal(), "CANCELLED": AttemptCancelled.IsTerminal(),
		}},
		{"RunStatus", map[string]bool{
			"STARTING": RunStarting.IsTerminal(), "RUNNING": RunRunning.IsTerminal(),
			"QUIESCING": RunQuiescing.IsTerminal(), "STOPPING": RunStopping.IsTerminal(),
			"INTERRUPTED": RunInterrupted.IsTerminal(), "BLOCKED": RunBlocked.IsTerminal(),
			"SUCCEEDED": RunSucceeded.IsTerminal(), "FAILED": RunFailed.IsTerminal(),
			"CANCELLED": RunCancelled.IsTerminal(),
		}},
	}
	wants := map[string]map[string]bool{
		"RuntimeStatus": {
			"PENDING": false, "RUNNING": false, "PAUSED": false, "BLOCKED": false,
			"FAILED": true, "SUCCEEDED": true, "CANCELLED": true,
		},
		"NodeStatus": {
			"PENDING": false, "READY": false, "RUNNING": false,
			"SUCCEEDED": true, "FAILED": true, "CANCELLED": true, "SKIPPED": true,
		},
		"AttemptStatus": {
			"READY": false, "RUNNING": false,
			"SUCCEEDED": true, "FAILED": true, "INTERRUPTED": true, "CANCELLED": true,
		},
		"RunStatus": {
			"STARTING": false, "RUNNING": false, "QUIESCING": false, "STOPPING": false,
			"INTERRUPTED": true, "BLOCKED": true, "SUCCEEDED": true, "FAILED": true, "CANCELLED": true,
		},
	}
	for _, tc := range cases {
		for k, got := range tc.got {
			if got != wants[tc.name][k] {
				t.Errorf("%s %s: IsTerminal = %v, want %v", tc.name, k, got, wants[tc.name][k])
			}
		}
	}
}

// assertEnum verifies that every declared constant of an enum is Valid and
// round-trips through its validating constructor, that the zero value is
// invalid, and that unknown strings are rejected.
func assertEnum[E ~string](t *testing.T, name string, all []E, valid func(E) bool, parse func(string) (E, error)) {
	t.Helper()
	for _, v := range all {
		if !valid(v) {
			t.Errorf("%s %q must be valid", name, v)
		}
		got, err := parse(string(v))
		if err != nil {
			t.Errorf("%s %q must parse: %v", name, v, err)
		} else if got != v {
			t.Errorf("%s %q parsed to %q", name, v, got)
		}
	}
	if valid(E("")) {
		t.Errorf("%s zero value must be invalid", name)
	}
	if _, err := parse("NOT_A_STATUS"); err == nil {
		t.Errorf("%s must reject unknown values", name)
	}
}

func TestClosedEnums(t *testing.T) {
	assertEnum(t, "WorkflowStage", allStages, WorkflowStage.Valid, ParseWorkflowStage)
	assertEnum(t, "RuntimeStatus", allRuntime, RuntimeStatus.Valid, ParseRuntimeStatus)
	assertEnum(t, "PlanStatus", allPlans, PlanStatus.Valid, ParsePlanStatus)
	assertEnum(t, "NodeStatus", allNodes, NodeStatus.Valid, ParseNodeStatus)
	assertEnum(t, "SessionStatus", allSession, SessionStatus.Valid, ParseSessionStatus)
	assertEnum(t, "RunStatus", allRuns, RunStatus.Valid, ParseRunStatus)
	assertEnum(t, "AttemptStatus", allAttempt, AttemptStatus.Valid, ParseAttemptStatus)
}

func TestOpaqueIDs(t *testing.T) {
	if !WorkflowID("wf-1").Valid() {
		t.Error("non-empty WorkflowID must be valid")
	}
	if WorkflowID("").Valid() {
		t.Error("empty WorkflowID must be invalid")
	}
	for name, id := range map[string]interface{ Valid() bool }{
		"ProjectID":        ProjectID("p-1"),
		"WorkflowID":       WorkflowID("wf-1"),
		"NodeID":           NodeID("n-1"),
		"SessionID":        SessionID("s-1"),
		"RunID":            RunID("run-1"),
		"ProcessID":        ProcessID("p-1"),
		"FindingID":        FindingID("f-1"),
		"ApprovalID":       ApprovalID("a-1"),
		"ApplyAttemptID":   ApplyAttemptID("apply-1"),
		"CleanupAttemptID": CleanupAttemptID("cleanup-1"),
	} {
		if !id.Valid() {
			t.Errorf("%s must be valid", name)
		}
	}
	if key := (AttemptKey{Node: NodeID("n-1"), Number: 1}).String(); key != "n-1#1" {
		t.Errorf("AttemptKey.String() = %q, want n-1#1", key)
	}
}

// TestFaultTableComplete is the compiled fault-policy table's
// self-consistency check: every declared Code has exactly one policy, the
// policy echoes its Code, and its Category and Scope are closed values.
func TestFaultTableComplete(t *testing.T) {
	seen := map[Code]bool{}
	for _, c := range Codes() {
		if seen[c] {
			t.Errorf("Code %s declared twice", c)
		}
		seen[c] = true
		pol, ok := Policy(c)
		if !ok {
			t.Errorf("no compiled policy for %s", c)
			continue
		}
		if pol.Code != c {
			t.Errorf("policy for %s echoes code %s", c, pol.Code)
		}
		if !pol.Category.Valid() {
			t.Errorf("policy for %s has invalid category %s", c, pol.Category)
		}
		if !pol.Scope.Valid() {
			t.Errorf("policy for %s has invalid scope %s", c, pol.Scope)
		}
		if !pol.Retry.AllowsSuccessor && pol.Retry.ChargesBudget {
			t.Errorf("policy for %s charges budget without successor permission", c)
		}
	}
}

// TestFaultDispositions pins the PRD 失败分类 dispositions for the codes
// the kernel enforces directly.
func TestFaultDispositions(t *testing.T) {
	cases := []struct {
		code                         Code
		cat                          FaultCategory
		scope                        FaultScope
		charge, closeGate, successor bool
		stop                         StopScope
	}{
		{CodeAgentTimeout, CatRetryableAttemptFailure, ScopeAttempt, true, false, true, StopAffected},
		{CodeCommandFailed, CatRetryableAttemptFailure, ScopeAttempt, true, false, true, StopNone},
		{CodeDirtyTaskWorktree, CatRetryableAttemptFailure, ScopeAttempt, true, false, true, StopNone},
		{CodeMissingImplementationCommit, CatRetryableAttemptFailure, ScopeAttempt, true, false, true, StopNone},
		{CodeMergeConflict, CatRetryableAttemptFailure, ScopeAttempt, true, false, true, StopNone},
		{CodeEvidenceSubjectChanged, CatRetryableAttemptFailure, ScopeAttempt, true, false, true, StopAffected},
		{CodeUserInterrupted, CatUserActionRequired, ScopeRun, false, true, false, StopAll},
		{CodeRetryExhausted, CatUserActionRequired, ScopeNode, false, true, false, StopNone},
		{CodeApprovalInputChanged, CatInvalidInput, ScopeApproval, false, false, false, StopNone},
		{CodeCommitPolicyInputChanged, CatInvalidInput, ScopeApproval, false, false, false, StopNone},
		{CodeCommitPolicySafetyStopRequested, CatSafetyStop, ScopeRun, false, true, false, StopAll},
		{CodeProviderProtocolViolation, CatSafetyStop, ScopeSession, false, true, false, StopAffected},
		{CodeProviderBindingChanged, CatSafetyStop, ScopeWorkflow, false, true, false, StopAll},
		{CodeSessionIndependenceViolation, CatInvariantFailure, ScopeSession, false, true, false, StopAll},
		{CodeStateInvariantViolation, CatInvariantFailure, ScopeWorkflow, false, true, false, StopAll},
		{CodeTargetHeadChanged, CatUserActionRequired, ScopeApply, false, true, false, StopAffected},
		{CodeCleanupFactsChanged, CatUserActionRequired, ScopeCleanup, false, false, false, StopNone},
		{CodeUntrustedCompletion, CatInvalidInput, ScopeAttempt, false, false, false, StopAffected},
	}
	for _, tc := range cases {
		pol, ok := Policy(tc.code)
		if !ok {
			t.Errorf("no policy for %s", tc.code)
			continue
		}
		if pol.Category != tc.cat || pol.Scope != tc.scope ||
			pol.Retry.ChargesBudget != tc.charge || pol.CloseDispatch != tc.closeGate ||
			pol.Retry.AllowsSuccessor != tc.successor || pol.StopScope != tc.stop {
			t.Errorf("policy for %s = %+v, want cat=%s scope=%s charge=%v close=%v successor=%v stop=%s",
				tc.code, pol, tc.cat, tc.scope, tc.charge, tc.closeGate, tc.successor, tc.stop)
		}
	}
}

// TestNewFaultSnapshotsPolicy verifies that a constructed Fault carries the
// compiled policy and that InvalidInputFault and InvariantFault use the
// canonical constructor codes.
func TestNewFaultSnapshotsPolicy(t *testing.T) {
	f := NewFault(CodeRetryExhausted, "retry budget exhausted")
	if f.Code != CodeRetryExhausted || f.Category != CatUserActionRequired || f.SafeText != "retry budget exhausted" {
		t.Errorf("NewFault = %+v", f)
	}
	if got, _ := CodeOf(InvalidInputFault("bad")); got != CodeInvalidInput {
		t.Errorf("InvalidInputFault code = %s", got)
	}
	if got, _ := CodeOf(InvariantFault(nil)); got != CodeStateInvariantViolation {
		t.Errorf("InvariantFault code = %s", got)
	}
	if code, _ := CodeOf(nil); code != "" {
		t.Error("CodeOf must report no code for nil errors")
	}
	if code, _ := CodeOf(errInvalid{}); code != "" {
		t.Error("CodeOf must report no code for non-Fault errors")
	}
}

type errInvalid struct{}

func (errInvalid) Error() string { return "not a fault" }

func TestValidateState(t *testing.T) {
	valid := baseValidState()
	if err := ValidateState(valid); err != nil {
		t.Fatalf("valid state rejected: %v", err)
	}
	if err := ValidateState(State{}); err != nil {
		t.Fatalf("empty state rejected: %v", err)
	}

	cases := []struct {
		name string
		mut  func(*State)
	}{
		{"completed without succeeded", func(s *State) { s.Workflow.Stage = StageCompleted; s.Workflow.Runtime = RuntimePaused }},
		{"succeeded outside completed", func(s *State) { s.Workflow.Stage = StageExecution; s.Workflow.Runtime = RuntimeSucceeded }},
		{"pending after start", func(s *State) { s.Workflow.Stage = StagePlanCheck; s.Workflow.Runtime = RuntimePending }},
		{"cancelled without cancel intent", func(s *State) { s.Workflow.Runtime = RuntimeCancelled }},
		{"attempt without node", func(s *State) {
			s.Attempts[AttemptKey{Node: "n-x", Number: 1}] = &Attempt{
				Key: AttemptKey{Node: "n-x", Number: 1}, Status: AttemptRunning}
		}},
	}
	for _, tc := range cases {
		st := baseValidState()
		tc.mut(&st)
		if err := ValidateState(st); err == nil {
			t.Errorf("%s: state accepted, want invariant violation", tc.name)
		}
	}
}

// baseValidState is a minimal aggregate State that passes ValidateState:
// an EXECUTION Workflow with one terminal-free Node and its Attempt.
func baseValidState() State {
	st := State{Now: fixedTestNow, Version: 1}
	st.Workflow = Workflow{ID: "wf-1", Project: "p-1", Stage: StageExecution, Runtime: RuntimeRunning,
		TargetBranch: "main", BaseCommit: "base-1", IntegrationHead: "int-1"}
	st.Nodes = map[NodeID]*Node{
		"n-1": {ID: "n-1", Kind: NodeAgentTask, Status: NodeRunning, RetryBudget: 2, Branch: "task/n-1"},
	}
	st.Attempts = map[AttemptKey]*Attempt{
		{Node: "n-1", Number: 1}: {Key: AttemptKey{Node: "n-1", Number: 1}, Status: AttemptRunning, StartHead: "base-1"},
	}
	st.Runs = []Run{{ID: "run-1", Status: RunRunning, DispatchGate: true}}
	return st
}
