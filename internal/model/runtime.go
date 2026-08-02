package model

import (
	"fmt"
	"time"
)

// ---------------------------------------------------------------------------
// Node
// ---------------------------------------------------------------------------

// NodeKind names the closed set of schedulable operations in a Dynamic
// Workflow (CONTEXT.md: Node).
type NodeKind string

const (
	NodeAgentTask   NodeKind = "agent-task"
	NodeVerify      NodeKind = "verify"
	NodeReview      NodeKind = "review"
	NodeMerge       NodeKind = "merge"
	NodeCheckpoint  NodeKind = "checkpoint"
	NodeFinalVerify NodeKind = "final-verify"
)

// Valid reports whether k is a declared Node Kind.
func (k NodeKind) Valid() bool {
	switch k {
	case NodeAgentTask, NodeVerify, NodeReview, NodeMerge, NodeCheckpoint, NodeFinalVerify:
		return true
	}
	return false
}

// String renders the Node Kind.
func (k NodeKind) String() string { return string(k) }

// NodeStatus is the Scheduler's authoritative execution status for one
// Node. The PRD defines exactly PENDING, READY, RUNNING, SUCCEEDED,
// FAILED, CANCELLED, SKIPPED; Node has no PAUSED state (PRD 状态机与持久化
// 模型). Retry never reuses READY as RETRY_WAIT: a failed Attempt stays
// terminal and a budgeted retry returns the Node to READY with a new
// numbered Attempt.
type NodeStatus string

const (
	NodePending   NodeStatus = "PENDING"
	NodeReady     NodeStatus = "READY"
	NodeRunning   NodeStatus = "RUNNING"
	NodeSucceeded NodeStatus = "SUCCEEDED"
	NodeFailed    NodeStatus = "FAILED"
	NodeCancelled NodeStatus = "CANCELLED"
	NodeSkipped   NodeStatus = "SKIPPED"
)

// Valid reports whether s is a declared Node Status.
func (s NodeStatus) Valid() bool {
	switch s {
	case NodePending, NodeReady, NodeRunning, NodeSucceeded, NodeFailed, NodeCancelled, NodeSkipped:
		return true
	}
	return false
}

// String renders the Node Status.
func (s NodeStatus) String() string { return string(s) }

// ParseNodeStatus validates and converts a string.
func ParseNodeStatus(s string) (NodeStatus, error) {
	v := NodeStatus(s)
	if !v.Valid() {
		return "", fmt.Errorf("unknown NodeStatus %q", s)
	}
	return v, nil
}

// IsTerminal reports whether the Node cannot transition further. Terminal
// Nodes are never resurrected: repair creates a new immutable Spec and
// Workflow Revision, never a revived Node (PRD 状态机与持久化模型).
func (s NodeStatus) IsTerminal() bool {
	switch s {
	case NodeSucceeded, NodeFailed, NodeCancelled, NodeSkipped:
		return true
	}
	return false
}

// CanTransitionTo reports whether a Node Status move is legal. READY on a
// budgeted retry is the only path from RUNNING back to a schedulable
// state.
func (s NodeStatus) CanTransitionTo(next NodeStatus) bool {
	switch s {
	case NodePending:
		return next == NodeReady || next == NodeCancelled || next == NodeSkipped
	case NodeReady:
		return next == NodeRunning || next == NodeCancelled || next == NodeSkipped
	case NodeRunning:
		return next == NodeSucceeded || next == NodeFailed || next == NodeCancelled || next == NodeReady
	}
	return false
}

// Node is one schedulable operation. RetryBudget bounds automatic
// successor Attempts (CONTEXT.md: Retry Budget); the initial Attempt is
// never charged, each budgeted retry charges one.
type Node struct {
	ID           NodeID
	Kind         NodeKind
	Status       NodeStatus
	Dependencies []NodeID
	Branch       string
	RetryBudget  int
	RetryCharged int
}

// ---------------------------------------------------------------------------
// Attempt
// ---------------------------------------------------------------------------

// AttemptStatus is the lifecycle of one immutable execution record
// (CONTEXT.md: Attempt). INTERRUPTED is an immutable result distinct from
// FAILED and CANCELLED (PRD 状态机与持久化模型).
type AttemptStatus string

const (
	AttemptReady       AttemptStatus = "READY"
	AttemptRunning     AttemptStatus = "RUNNING"
	AttemptSucceeded   AttemptStatus = "SUCCEEDED"
	AttemptFailed      AttemptStatus = "FAILED"
	AttemptInterrupted AttemptStatus = "INTERRUPTED"
	AttemptCancelled   AttemptStatus = "CANCELLED"
)

// Valid reports whether s is a declared Attempt Status.
func (s AttemptStatus) Valid() bool {
	switch s {
	case AttemptReady, AttemptRunning, AttemptSucceeded, AttemptFailed, AttemptInterrupted, AttemptCancelled:
		return true
	}
	return false
}

// String renders the Attempt Status.
func (s AttemptStatus) String() string { return string(s) }

// ParseAttemptStatus validates and converts a string.
func ParseAttemptStatus(s string) (AttemptStatus, error) {
	v := AttemptStatus(s)
	if !v.Valid() {
		return "", fmt.Errorf("unknown AttemptStatus %q", s)
	}
	return v, nil
}

// IsTerminal reports whether the Attempt is immutable. Terminal Attempt
// facts are never mutated and never reopened (design 7.3, PRD 已确认).
func (s AttemptStatus) IsTerminal() bool {
	switch s {
	case AttemptSucceeded, AttemptFailed, AttemptInterrupted, AttemptCancelled:
		return true
	}
	return false
}

// Attempt is one immutable execution record for a Node, including start
// facts, end facts, evidence, and retry charge. Identity is
// (node_id, attempt_number) and is never reused; a Retry creates
// attempt_number+1 and never reopens the prior Attempt (design 7.2, 7.3).
type Attempt struct {
	Key     AttemptKey
	Session SessionID
	Status  AttemptStatus

	StartHead             string
	StartDirtyFingerprint string
	EndHead               string
	EndDirtyFingerprint   string

	FailureCode Code
	Evidence    []EvidenceRef
	// RetryCharged records whether this Attempt charged the Node Retry
	// Budget. Interrupted Attempts never charge (PRD 失败分类,
	// USER_INTERRUPTED).
	RetryCharged bool

	StartedAt time.Time
	EndedAt   time.Time
}

// ---------------------------------------------------------------------------
// Run and Session
// ---------------------------------------------------------------------------

// RunStatus is the status of one coordinated foreground execution of a
// Workflow between start or resume and its next stable stop (CONTEXT.md:
// Run). QUIESCING closes new dispatch while the persisted snapshot of
// RUNNING Attempts settles; STOPPING is terminating managed processes.
// INTERRUPTED is an immutable Run result, not FAILED or CANCELLED.
type RunStatus string

const (
	RunStarting    RunStatus = "STARTING"
	RunRunning     RunStatus = "RUNNING"
	RunQuiescing   RunStatus = "QUIESCING"
	RunStopping    RunStatus = "STOPPING"
	RunInterrupted RunStatus = "INTERRUPTED"
	RunBlocked     RunStatus = "BLOCKED"
	RunSucceeded   RunStatus = "SUCCEEDED"
	RunFailed      RunStatus = "FAILED"
	RunCancelled   RunStatus = "CANCELLED"
)

// Valid reports whether s is a declared Run Status.
func (s RunStatus) Valid() bool {
	switch s {
	case RunStarting, RunRunning, RunQuiescing, RunStopping, RunInterrupted,
		RunBlocked, RunSucceeded, RunFailed, RunCancelled:
		return true
	}
	return false
}

// String renders the Run Status.
func (s RunStatus) String() string { return string(s) }

// ParseRunStatus validates and converts a string.
func ParseRunStatus(s string) (RunStatus, error) {
	v := RunStatus(s)
	if !v.Valid() {
		return "", fmt.Errorf("unknown RunStatus %q", s)
	}
	return v, nil
}

// IsTerminal reports whether the Run record cannot transition further. A
// resume creates a new Run record; the old one stays terminal.
func (s RunStatus) IsTerminal() bool {
	switch s {
	case RunInterrupted, RunBlocked, RunSucceeded, RunFailed, RunCancelled:
		return true
	}
	return false
}

// CanTransitionTo reports whether a Run Status move is legal. QUIESCING
// converges to BLOCKED, and either state may be interrupted by the user.
func (s RunStatus) CanTransitionTo(next RunStatus) bool {
	switch s {
	case RunStarting:
		return next == RunRunning
	case RunRunning:
		return next == RunQuiescing || next == RunStopping || next == RunBlocked ||
			next == RunSucceeded || next == RunFailed || next == RunInterrupted
	case RunQuiescing:
		return next == RunBlocked || next == RunStopping || next == RunInterrupted
	case RunStopping:
		return next == RunInterrupted || next == RunCancelled
	}
	return false
}

// Run is one coordinated foreground execution. DispatchGate is the Run's
// dispatch gate: while closed, no new Node, Retry, Repair Attempt,
// Provider Session, Verification, Review, Merge, Checkpoint Agent, or
// successor DAG node may start (PRD 已确认：并行失败后的 Quiescing).
type Run struct {
	ID     RunID
	Status RunStatus
	// DispatchGate is open only while the Run may allocate new work.
	DispatchGate bool
	// StopReason records why a Stop was requested (e.g. COMMIT_POLICY_DRIFT).
	StopReason Code
	// QuiesceSnapshot fixes the persisted RUNNING Attempts the Run waits
	// for before converging to BLOCKED.
	QuiesceSnapshot []AttemptKey
	StartedAt       time.Time
	EndedAt         time.Time
}

// SessionStatus is the status of one Provider-managed conversation
// identity used for exactly one Agent Purpose and role lineage
// (CONTEXT.md: Session).
type SessionStatus string

const (
	SessionStarting    SessionStatus = "STARTING"
	SessionActive      SessionStatus = "ACTIVE"
	SessionInterrupted SessionStatus = "INTERRUPTED"
	SessionPaused      SessionStatus = "PAUSED"
	SessionCompleted   SessionStatus = "COMPLETED"
	SessionFailed      SessionStatus = "FAILED"
	SessionCancelled   SessionStatus = "CANCELLED"
	SessionLost        SessionStatus = "LOST"
)

// Valid reports whether s is a declared Session Status.
func (s SessionStatus) Valid() bool {
	switch s {
	case SessionStarting, SessionActive, SessionInterrupted, SessionPaused,
		SessionCompleted, SessionFailed, SessionCancelled, SessionLost:
		return true
	}
	return false
}

// String renders the Session Status.
func (s SessionStatus) String() string { return string(s) }

// ParseSessionStatus validates and converts a string.
func ParseSessionStatus(s string) (SessionStatus, error) {
	v := SessionStatus(s)
	if !v.Valid() {
		return "", fmt.Errorf("unknown SessionStatus %q", s)
	}
	return v, nil
}

// AgentPurpose is the constrained role assigned to a Session (CONTEXT.md:
// Agent Purpose). Planner and Checker may share a Provider but never a
// Session (design 14.4).
type AgentPurpose string

const (
	PurposePlanning          AgentPurpose = "planning"
	PurposePlanCheck         AgentPurpose = "plan-check"
	PurposeSpecGeneration    AgentPurpose = "spec-generation"
	PurposeImplementation    AgentPurpose = "implementation"
	PurposeRepair            AgentPurpose = "repair"
	PurposeReview            AgentPurpose = "review"
	PurposeFinalVerification AgentPurpose = "final-verification"
)

// Valid reports whether p is a declared Agent Purpose.
func (p AgentPurpose) Valid() bool {
	switch p {
	case PurposePlanning, PurposePlanCheck, PurposeSpecGeneration,
		PurposeImplementation, PurposeRepair, PurposeReview, PurposeFinalVerification:
		return true
	}
	return false
}

// String renders the Agent Purpose.
func (p AgentPurpose) String() string { return string(p) }

// Session is one Provider-managed conversation identity. A successor
// Session records its superseded Session for audit lineage (design 14.4).
type Session struct {
	ID                SessionID
	ProviderSessionID string
	Purpose           AgentPurpose
	Status            SessionStatus
	Supersedes        SessionID
}

// ProcessStatus is the operational status of a managed process record.
type ProcessStatus string

const (
	ProcessStatusRunning ProcessStatus = "RUNNING"
	ProcessStatusExited  ProcessStatus = "EXITED"
	ProcessStatusStopped ProcessStatus = "STOPPED"
)

// Valid reports whether s is a declared Process Status.
func (s ProcessStatus) Valid() bool {
	switch s {
	case ProcessStatusRunning, ProcessStatusExited, ProcessStatusStopped:
		return true
	}
	return false
}

// String renders the Process Status.
func (s ProcessStatus) String() string { return string(s) }

// ProcessRecord is a managed process fact owned by the Runtime. OS-level
// identity (PID plus start token) stays in the process platform adapter;
// the aggregate records lineage and settled state.
type ProcessRecord struct {
	ID        ProcessID
	Session   SessionID
	Purpose   AgentPurpose
	Status    ProcessStatus
	ExitCode  int
	StartedAt time.Time
	EndedAt   time.Time
}

// ---------------------------------------------------------------------------
// Findings, Evidence, Quarantine
// ---------------------------------------------------------------------------

// Finding is a structured, evidence-backed condition that prevents or
// constrains safe progress until the Runtime can resolve it from facts
// (CONTEXT.md: Finding).
type Finding struct {
	ID       FindingID
	Code     Code
	Scope    FaultScope
	Subject  string
	Blocking bool
	Text     string
	Evidence EvidenceRef
	Seq      uint64
}

// EvidenceKind names the persisted fact kinds the Runtime may judge
// outcomes from (CONTEXT.md: Evidence).
type EvidenceKind string

const (
	EvidenceCommit        EvidenceKind = "commit"
	EvidenceTestResult    EvidenceKind = "test-result"
	EvidenceReviewResult  EvidenceKind = "review-result"
	EvidenceProtocolEvent EvidenceKind = "protocol-event"
	EvidenceGitSnapshot   EvidenceKind = "git-snapshot"
)

// Valid reports whether k is a declared Evidence Kind.
func (k EvidenceKind) Valid() bool {
	switch k {
	case EvidenceCommit, EvidenceTestResult, EvidenceReviewResult, EvidenceProtocolEvent, EvidenceGitSnapshot:
		return true
	}
	return false
}

// String renders the Evidence Kind.
func (k EvidenceKind) String() string { return string(k) }

// EvidenceRef points at one persisted fact by kind, content hash, and the
// subject it was observed on. Faults carry the Evidence they were raised
// from (design 8.2).
type EvidenceRef struct {
	Kind    EvidenceKind
	Hash    string
	Subject string
}

// Quarantine is the permanent exclusion of a Branch or execution path
// from the trusted delivery chain while preserving its evidence
// (CONTEXT.md: Quarantine). A quarantined Branch is never repaired in
// place and can never re-enter Verify, Merge, Final Verify, or Apply.
type Quarantine struct {
	Branch   string
	FromHead string
	ToHead   string
	Code     Code
	Reason   string
	Seq      uint64
}
