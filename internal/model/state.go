package model

import (
	"fmt"
	"time"
)

// ---------------------------------------------------------------------------
// Workflow Stage and Runtime Status (PRD 状态机与持久化模型)
// ---------------------------------------------------------------------------

// WorkflowStage is the product phase currently occupied by a Workflow
// (CONTEXT.md: Stage). Values are fixed by the PRD.
type WorkflowStage string

const (
	StageRequirementDiscussion WorkflowStage = "REQUIREMENT_DISCUSSION"
	StagePlanGeneration        WorkflowStage = "PLAN_GENERATION"
	StagePlanCheck             WorkflowStage = "PLAN_CHECK"
	StageSpecGeneration        WorkflowStage = "SPEC_GENERATION"
	StageWorkflowGeneration    WorkflowStage = "WORKFLOW_GENERATION"
	StageExecution             WorkflowStage = "EXECUTION"
	StageFinalVerification     WorkflowStage = "FINAL_VERIFICATION"
	StageCompleted             WorkflowStage = "COMPLETED"
)

// Valid reports whether s is a declared Stage.
func (s WorkflowStage) Valid() bool {
	switch s {
	case StageRequirementDiscussion, StagePlanGeneration, StagePlanCheck,
		StageSpecGeneration, StageWorkflowGeneration, StageExecution,
		StageFinalVerification, StageCompleted:
		return true
	}
	return false
}

// String renders the Stage.
func (s WorkflowStage) String() string { return string(s) }

// ParseWorkflowStage validates and converts a string.
func ParseWorkflowStage(s string) (WorkflowStage, error) {
	v := WorkflowStage(s)
	if !v.Valid() {
		return "", fmt.Errorf("unknown WorkflowStage %q", s)
	}
	return v, nil
}

// CanTransitionTo reports whether a Stage move is legal (PRD main state
// transition graph).
func (s WorkflowStage) CanTransitionTo(next WorkflowStage) bool {
	switch s {
	case StageRequirementDiscussion:
		return next == StagePlanGeneration
	case StagePlanGeneration:
		return next == StagePlanCheck
	case StagePlanCheck:
		return next == StageRequirementDiscussion || next == StagePlanGeneration || next == StageSpecGeneration
	case StageSpecGeneration:
		// The user-driven adjustment loop (PRD 历史工作流交互: 调整需求或
		// Plan) re-plans after an Approval; the main-line graph has no
		// backward edge, so the adjustment is this explicit loop edge.
		return next == StageWorkflowGeneration || next == StagePlanGeneration
	case StageWorkflowGeneration:
		return next == StageExecution
	case StageExecution:
		return next == StageFinalVerification
	case StageFinalVerification:
		return next == StageExecution || next == StageCompleted
	}
	return false
}

// RuntimeStatus is the operational condition of a Workflow within its
// Stage (CONTEXT.md: Runtime Status). Values are fixed by the PRD.
type RuntimeStatus string

const (
	RuntimePending   RuntimeStatus = "PENDING"
	RuntimeRunning   RuntimeStatus = "RUNNING"
	RuntimePaused    RuntimeStatus = "PAUSED"
	RuntimeBlocked   RuntimeStatus = "BLOCKED"
	RuntimeFailed    RuntimeStatus = "FAILED"
	RuntimeSucceeded RuntimeStatus = "SUCCEEDED"
	RuntimeCancelled RuntimeStatus = "CANCELLED"
)

// Valid reports whether r is a declared Runtime Status.
func (r RuntimeStatus) Valid() bool {
	switch r {
	case RuntimePending, RuntimeRunning, RuntimePaused, RuntimeBlocked,
		RuntimeFailed, RuntimeSucceeded, RuntimeCancelled:
		return true
	}
	return false
}

// String renders the Runtime Status.
func (r RuntimeStatus) String() string { return string(r) }

// ParseRuntimeStatus validates and converts a string.
func ParseRuntimeStatus(s string) (RuntimeStatus, error) {
	v := RuntimeStatus(s)
	if !v.Valid() {
		return "", fmt.Errorf("unknown RuntimeStatus %q", s)
	}
	return v, nil
}

// IsTerminal reports whether the Workflow cannot transition further.
// FAILED, CANCELLED and COMPLETED/SUCCEEDED are terminal; a terminal
// Workflow cannot be resumed directly (PRD 状态机与持久化模型).
func (r RuntimeStatus) IsTerminal() bool {
	switch r {
	case RuntimeSucceeded, RuntimeFailed, RuntimeCancelled:
		return true
	}
	return false
}

// CanTransitionTo reports whether a Runtime Status move is legal under
// ordinary control and the special terminal paths (PRD 状态机与持久化模型).
// CANCELLED additionally requires a persisted cancel intent, which the
// Kernel enforces in every decision that uses this matrix.
func (r RuntimeStatus) CanTransitionTo(next RuntimeStatus) bool {
	switch r {
	case RuntimePending:
		return next == RuntimeRunning || next == RuntimeCancelled || next == RuntimeFailed
	case RuntimeRunning:
		return next == RuntimePaused || next == RuntimeBlocked || next == RuntimeSucceeded ||
			next == RuntimeFailed || next == RuntimeCancelled
	case RuntimePaused:
		return next == RuntimeRunning || next == RuntimeCancelled || next == RuntimeFailed
	case RuntimeBlocked:
		return next == RuntimeRunning || next == RuntimeCancelled || next == RuntimeFailed
	}
	return false
}

// ValidWithRuntime reports whether a Stage and a Runtime Status form a
// legal pair following the PRD typical-combination table: COMPLETED only
// with SUCCEEDED, SUCCEEDED only with COMPLETED, PENDING only before the
// first Start, and every other status legal on any non-completed Stage.
func (s WorkflowStage) ValidWithRuntime(r RuntimeStatus) bool {
	if s == StageCompleted {
		return r == RuntimeSucceeded
	}
	if r == RuntimeSucceeded {
		return false
	}
	if r == RuntimePending {
		return s == StageRequirementDiscussion
	}
	return r.Valid()
}

// ---------------------------------------------------------------------------
// Plan Status (PRD Plan 状态)
// ---------------------------------------------------------------------------

// PlanStatus is the review status of a Plan Artifact Revision. CHECKED is
// an independent Checker conclusion; APPROVED is the user's append-only
// decision binding one exact Plan Revision and hash (CONTEXT.md: Plan
// Approval). The two must never be conflated.
type PlanStatus string

const (
	PlanDraft    PlanStatus = "DRAFT"
	PlanChecking PlanStatus = "CHECKING"
	PlanChecked  PlanStatus = "CHECKED"
	PlanApproved PlanStatus = "APPROVED"
	PlanStale    PlanStatus = "STALE"
	PlanRejected PlanStatus = "REJECTED"
)

// Valid reports whether p is a declared Plan Status.
func (p PlanStatus) Valid() bool {
	switch p {
	case PlanDraft, PlanChecking, PlanChecked, PlanApproved, PlanStale, PlanRejected:
		return true
	}
	return false
}

// String renders the Plan Status.
func (p PlanStatus) String() string { return string(p) }

// ParsePlanStatus validates and converts a string.
func ParsePlanStatus(s string) (PlanStatus, error) {
	v := PlanStatus(s)
	if !v.Valid() {
		return "", fmt.Errorf("unknown PlanStatus %q", s)
	}
	return v, nil
}

// CanTransitionTo reports whether a Plan Status move is legal (PRD Plan
// 状态约束). REJECTED can only produce a new Plan Revision that starts as
// DRAFT; the old Revision stays REJECTED.
func (p PlanStatus) CanTransitionTo(next PlanStatus) bool {
	switch p {
	case PlanDraft:
		return next == PlanChecking
	case PlanChecking:
		return next == PlanDraft || next == PlanChecked || next == PlanRejected
	case PlanChecked:
		return next == PlanApproved || next == PlanStale
	case PlanApproved:
		return next == PlanStale
	case PlanStale:
		return next == PlanDraft
	case PlanRejected:
		return next == PlanDraft
	}
	return false
}

// ---------------------------------------------------------------------------
// Aggregate State
// ---------------------------------------------------------------------------

// AggregateVersion is the optimistic-concurrency version of the Workflow
// aggregate. The Store rejects any Decision applied at a stale version.
type AggregateVersion uint64

// State is the current aggregate data loaded for one Workflow (design 7.1,
// 8.1). It is pure data: the Kernel decides from it and produces
// mutations, Events, and at most one Effect Intent. Timestamps are
// injected through Now so the Kernel stays deterministic.
type State struct {
	Version AggregateVersion
	Now     time.Time

	Workflow Workflow
	Plan     *Plan

	Nodes     map[NodeID]*Node
	Attempts  map[AttemptKey]*Attempt
	Approvals []Approval
	Sessions  []Session
	Runs      []Run
	Findings  []Finding

	Processes       []ProcessRecord
	Quarantines     []Quarantine
	ApplyAttempts   []ApplyAttempt
	CleanupAttempts []CleanupAttempt

	// NextEventSeq is the sequence number the next authoritative Event
	// will receive. The Kernel assigns Event sequence numbers from it;
	// the Store owns the authoritative Event log (design 9).
	NextEventSeq uint64
}

// ValidateState checks the aggregate invariants the Kernel relies on. It
// returns an error the Kernel converts into a Workflow FAILED-producing
// Invariant Failure (design 8.2). The zero State is valid: it is a
// Workflow that has not been created yet.
func ValidateState(st State) error {
	if st.Workflow.ID == "" {
		if st.Workflow.Stage != "" || st.Workflow.Runtime != "" {
			return fmt.Errorf("workflow absent but stage/runtime set")
		}
		return nil
	}
	if !st.Workflow.Stage.Valid() {
		return fmt.Errorf("invalid WorkflowStage %q", st.Workflow.Stage)
	}
	if !st.Workflow.Runtime.Valid() {
		return fmt.Errorf("invalid RuntimeStatus %q", st.Workflow.Runtime)
	}
	if !st.Workflow.Stage.ValidWithRuntime(st.Workflow.Runtime) {
		return fmt.Errorf("illegal Stage/Runtime pair %s/%s", st.Workflow.Stage, st.Workflow.Runtime)
	}
	if st.Workflow.Runtime == RuntimeCancelled && st.Workflow.CancelIntent == nil {
		return fmt.Errorf("CANCELLED workflow has no persisted cancel intent")
	}
	for _, n := range st.Nodes {
		if n == nil || !n.Status.Valid() {
			return fmt.Errorf("invalid Node status")
		}
		if n.RetryCharged < 0 || n.RetryCharged > n.RetryBudget {
			return fmt.Errorf("node %s retry charge %d outside budget %d", n.ID, n.RetryCharged, n.RetryBudget)
		}
	}
	for key, a := range st.Attempts {
		if a == nil {
			return fmt.Errorf("nil attempt %s", key)
		}
		if a.Key != key {
			return fmt.Errorf("attempt %s has mismatched key %s", key, a.Key)
		}
		if key.Number < 1 {
			return fmt.Errorf("attempt %s has non-positive number", key)
		}
		if _, ok := st.Nodes[key.Node]; !ok {
			return fmt.Errorf("attempt %s has no Node", key)
		}
		if !a.Status.Valid() {
			return fmt.Errorf("attempt %s has invalid status", key)
		}
	}
	for _, r := range st.Runs {
		if !r.Status.Valid() {
			return fmt.Errorf("invalid Run status %q", r.Status)
		}
	}
	for _, q := range st.Quarantines {
		if q.Branch == "" {
			return fmt.Errorf("quarantine with empty branch")
		}
	}
	return nil
}

// Workflow is the durable lifecycle of one user requirement (CONTEXT.md:
// Workflow). Its identity is stable across Workflow Revisions; the Target
// Branch is recorded at creation and only ever modified by a later
// explicit protected Apply (design 7.3).
type Workflow struct {
	ID      WorkflowID
	Project ProjectID
	Name    string

	Stage   WorkflowStage
	Runtime RuntimeStatus

	// TargetBranch is the user branch recorded when the Workflow is
	// created. Completed Workflows never change it.
	TargetBranch string
	// BaseCommit fixes the Git base the Workflow was created on.
	BaseCommit string
	// IntegrationBranch is the CFlow-owned branch that serially
	// accumulates verified Task Commit histories for one Workflow
	// (CONTEXT.md: Integration Branch). Legacy compatibility read field:
	// no new Workflow writes it (design 7).
	IntegrationBranch string
	// IntegrationHead is the current HEAD of the CFlow-owned Integration
	// Branch, advanced only by verified serial merges. Legacy read field:
	// Task 6 stops writing it for new Workflows; existing rows keep it.
	IntegrationHead string

	// LayoutVersion is the layout the Workflow's managed directories bind
	// to: 1 is the legacy integration layout, 2 is the aggregated
	// workspace layout (design 7). The default for migrated legacy rows
	// is 1; a create that predates workspace wiring persists 1, and the
	// aggregated create path (Task 4) persists 2.
	LayoutVersion int
	// WorkspacePath is the canonical aggregated workspace root of a
	// Layout Version 2 Workflow: <home>/projects/<key>/<workflow-id>/workspace
	// (design 8.1).
	WorkspacePath string
	// WorkspaceBranch is the CFlow-owned branch of the workspace
	// Worktree (design 8.2).
	WorkspaceBranch string
	// CandidateWorkspaceHead is the workspace HEAD recorded when the
	// Execution Approval was granted (design 8.4); the adopted head must
	// equal it before further scheduling.
	CandidateWorkspaceHead string
	// VerifiedWorkspaceHead is the workspace HEAD verified after the
	// workspace was adopted (design 8.4).
	VerifiedWorkspaceHead string
	// WorkspaceDirtyFingerprint is the observed dirty state of the
	// workspace when the facts were written; a later dispatch must
	// reconcile against it (design 8.4).
	WorkspaceDirtyFingerprint string

	// ExecutionFacts are the immutable policy references an Execution
	// Approval binds by exact hash (design 20.1).
	ExecutionFacts *ExecutionFacts

	// CancelIntent is the persisted WORKFLOW_CANCEL_REQUESTED intent.
	// CANCELLED requires it, all managed processes settled, and facts
	// reconciled (PRD 状态机与持久化模型).
	CancelIntent *CancelIntent
}

// CancelIntent is the persisted user cancel request.
type CancelIntent struct {
	RequestedSeq uint64
	Reason       string
}

// Plan is the current Plan Artifact Revision under review.
type Plan struct {
	Revision int
	Status   PlanStatus
	Artifact ArtifactRef
	Hash     string
}

// ---------------------------------------------------------------------------
// Scheduler inputs (stable interface ledger, design 12)
// ---------------------------------------------------------------------------

// GraphSnapshot is the pure input to Scheduler.Next: the current Node
// graph plus whether the Run Dispatch Gate is open.
type GraphSnapshot struct {
	Nodes            []GraphNode
	DispatchGateOpen bool
}

// GraphNode is one schedulable Node view.
type GraphNode struct {
	ID           NodeID
	Kind         NodeKind
	Status       NodeStatus
	Dependencies []NodeID
	Branch       string
}

// DispatchPolicy carries the approved scheduling bounds.
type DispatchPolicy struct {
	MaxConcurrency     int
	DefaultRetryBudget int
	Budgets            map[NodeKind]int
}

// DispatchDecision is the Scheduler's pure output: eligible Nodes plus why
// every other Node is not eligible.
type DispatchDecision struct {
	Eligible []NodeID
	Reasons  map[NodeID]string
}

// CommittedDecision is what the Store returns after atomically applying a
// Decision and its Events to the aggregate (stable interface ledger).
type CommittedDecision struct {
	Decision   Decision
	Version    AggregateVersion
	EventRange EventRange
}

// EventRange is the half-open Event sequence range [From, To) a Decision
// consumed.
type EventRange struct {
	From uint64
	To   uint64
}
