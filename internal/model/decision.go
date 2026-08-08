package model

import "time"

// ---------------------------------------------------------------------------
// Decision
// ---------------------------------------------------------------------------

// Decision is the pure output of the Kernel: state mutations, authoritative
// Events, and at most one next typed Effect Intent (design 6.2). A
// Decision cannot access the clock, filesystem, Git, Provider, or
// database; only the Store applies it atomically.
type Decision struct {
	Mutations []Mutation
	Events    []Event
	Effect    EffectIntent // at most one; nil when the Kernel needs no external Effect
}

// Mutation is the closed union of state changes a Decision may request.
type Mutation interface{ isMutation() }

// WorkflowMutation replaces the Workflow aggregate fields the Kernel
// owns: identity is set at creation and passed through unchanged on every
// later mutation (so a Decision can never silently change the Project,
// Target Branch, or Base Commit), while CancelIntent is replaced
// wholesale: only a Decision with an explicit intent may move a Workflow
// to CANCELLED.
type WorkflowMutation struct {
	ID                WorkflowID
	Project           ProjectID
	Stage             WorkflowStage
	Runtime           RuntimeStatus
	TargetBranch      string
	BaseCommit        string
	IntegrationBranch string
	IntegrationHead   string

	LayoutVersion             int
	WorkspacePath             string
	WorkspaceBranch           string
	CandidateWorkspaceHead    string
	VerifiedWorkspaceHead     string
	WorkspaceDirtyFingerprint string

	CancelIntent *CancelIntent
}

func (WorkflowMutation) isMutation() {}

// PlanMutation sets the current Plan Revision's review status. CHECKED
// never implies APPROVED: the user's append-only Approval is the only
// path (CONTEXT.md: Plan Approval).
type PlanMutation struct {
	Status PlanStatus
}

func (PlanMutation) isMutation() {}

// ArtifactRefMutation records the active Artifact Revision of one type
// (workflow_artifact_refs row). The Artifact body itself is immutable in
// the Artifact Store; this row is the SQLite pointer the aggregate
// hydrates from. Only ArtifactPlan has aggregate state (the current
// Plan); other declared types are recorded without an in-memory mirror.
// A ref change always pairs with the PlanMutation that re-opens the
// review (a new Revision starts DRAFT).
type ArtifactRefMutation struct {
	Type     ArtifactType
	Revision int
	Path     string
	Hash     string
}

func (ArtifactRefMutation) isMutation() {}

// PreflightRecordMutation appends one immutable Git Commit Preflight
// row (git_commit_preflights, PRD 已确认：Git Commit Identity 与 Signing
// Preflight). The evidence was observed by the Application before the
// Dry Run decision; the Kernel records the exact revision and binds the
// report Artifact reference.
type PreflightRecordMutation struct {
	Revision          int
	RepositoryContext string
	GitVersion        string
	Fingerprint       string
	IdentityJSON      string
	SigningPolicyJSON string
	ProbeStatus       string
	ArtifactPath      string
	ArtifactHash      string
}

func (PreflightRecordMutation) isMutation() {}

// SessionEndMutation settles one Session record with the Provider
// Session ID and the final status observed from the validated run
// (design 14.3: Session state is Kernel-owned).
type SessionEndMutation struct {
	ID                SessionID
	ProviderSessionID string
	Status            SessionStatus
	EndedAt           time.Time
}

func (SessionEndMutation) isMutation() {}

// NodeStatusMutation sets one Node's authoritative status and retry
// charge. Terminal Nodes are never resurrected.
type NodeStatusMutation struct {
	Node         NodeID
	Status       NodeStatus
	RetryCharged int
}

func (NodeStatusMutation) isMutation() {}

// NodeAppendMutation installs one Node of the approved Dynamic Workflow
// (the graph install, design 12). The Node starts PENDING; readiness is
// computed by the Scheduler from dependency and gate facts plus the
// persisted status, never from Task display status. The Branch is the
// deterministic Task Branch of an agent-task Node ("" for other kinds);
// the Store materializes the Task projection row from it.
type NodeAppendMutation struct {
	Node Node
}

func (NodeAppendMutation) isMutation() {}

// TaskMutation records the Task projection facts the Kernel owns: the
// immutable Task Base Commit fixed at readiness and the created Task
// Worktree path (PRD Worktree 策略). A recorded Base is never replaced:
// the Task never silently rebases onto a different baseline.
type TaskMutation struct {
	Node         NodeID
	BaseCommit   string
	WorktreePath string
}

func (TaskMutation) isMutation() {}

// AttemptAppendMutation appends a fresh Attempt record (identity
// (node, attempt_number), never reused).
type AttemptAppendMutation struct {
	Attempt Attempt
}

func (AttemptAppendMutation) isMutation() {}

// AttemptEndMutation terminalizes one running Attempt with its end facts.
// After this mutation the Attempt is immutable; the Kernel never emits it
// for a terminal Attempt.
type AttemptEndMutation struct {
	Key                 AttemptKey
	Status              AttemptStatus
	EndHead             string
	EndDirtyFingerprint string
	FailureCode         Code
	Evidence            []EvidenceRef
	RetryCharged        bool
	EndedAt             time.Time
}

func (AttemptEndMutation) isMutation() {}

// FindingAppendMutation appends one Finding.
type FindingAppendMutation struct {
	Finding Finding
}

func (FindingAppendMutation) isMutation() {}

// ApprovalAppendMutation appends one append-only user Approval.
type ApprovalAppendMutation struct {
	Approval Approval
}

func (ApprovalAppendMutation) isMutation() {}

// RunAppendMutation appends a new Run record (each start/resume creates a
// new Run).
type RunAppendMutation struct {
	Run Run
}

func (RunAppendMutation) isMutation() {}

// RunMutation sets one Run's status, dispatch gate, stop reason, and
// quiesce snapshot.
type RunMutation struct {
	ID              RunID
	Status          RunStatus
	DispatchGate    bool
	StopReason      Code
	QuiesceSnapshot []AttemptKey
}

func (RunMutation) isMutation() {}

// SessionAppendMutation appends a Session record. Provider records the
// route the Session runs on (PRD: SQLite sessions 表).
type SessionAppendMutation struct {
	Session  Session
	Provider string
}

func (SessionAppendMutation) isMutation() {}

// ProcessAppendMutation appends a managed process record.
type ProcessAppendMutation struct {
	Process ProcessRecord
}

func (ProcessAppendMutation) isMutation() {}

// ProcessEndMutation settles one running process record.
type ProcessEndMutation struct {
	ID       ProcessID
	Status   ProcessStatus
	ExitCode int
	EndedAt  time.Time
}

func (ProcessEndMutation) isMutation() {}

// QuarantineAppendMutation appends one permanent Branch quarantine.
type QuarantineAppendMutation struct {
	Quarantine Quarantine
}

func (QuarantineAppendMutation) isMutation() {}

// ApplyAppendMutation appends one Apply Attempt.
type ApplyAppendMutation struct {
	ApplyAttempt ApplyAttempt
}

func (ApplyAppendMutation) isMutation() {}

// ApplyMutation sets one Apply Attempt's status and, when the staging
// verification passed, records the verified staging head (in-memory
// rendering of the Apply Branch ref; the delivery re-observes the git
// fact).
type ApplyMutation struct {
	ID          ApplyAttemptID
	Status      ApplyStatus
	EndedAt     time.Time
	StagingHead string
}

func (ApplyMutation) isMutation() {}

// ApplyConfirmationMutation records the user's explicit Commit Policy /
// Apply Catalog confirmation facts on one Apply Attempt (PRD 约束 40-41,
// Apply Command Identity Drift): the append-only Approval row is the
// immutable record; this mutation updates the attempt's persisted
// Preflight identity and fingerprint the staging revalidation re-binds.
type ApplyConfirmationMutation struct {
	ID            ApplyAttemptID
	Preflight     ArtifactRef
	PreflightHash string
	Fingerprint   string
}

func (ApplyConfirmationMutation) isMutation() {}

// CleanupAppendMutation appends one Cleanup Attempt with its immutable
// Manifest.
type CleanupAppendMutation struct {
	CleanupAttempt CleanupAttempt
}

func (CleanupAppendMutation) isMutation() {}

// CleanupMutation sets one Cleanup Attempt's status.
type CleanupMutation struct {
	ID      CleanupAttemptID
	Status  CleanupStatus
	EndedAt time.Time
}

func (CleanupMutation) isMutation() {}

// CleanupItemMutation sets one Cleanup item's independent result.
type CleanupItemMutation struct {
	Attempt     CleanupAttemptID
	Index       int
	Status      CleanupItemStatus
	FailureCode Code
}

func (CleanupItemMutation) isMutation() {}

// ---------------------------------------------------------------------------
// Events
// ---------------------------------------------------------------------------

// EventKind is the closed set of authoritative Events the Kernel emits.
// The Store owns the authoritative Event sequence; events.jsonl is always
// an export, never the recovery stream (design 21).
type EventKind string

const (
	EventWorkflowCreated          EventKind = "WORKFLOW_CREATED"
	EventWorkflowStarted          EventKind = "WORKFLOW_STARTED"
	EventWorkflowPaused           EventKind = "WORKFLOW_PAUSED"
	EventWorkflowResumed          EventKind = "WORKFLOW_RESUMED"
	EventWorkflowBlocked          EventKind = "WORKFLOW_BLOCKED"
	EventWorkflowQuiesceRequested EventKind = "WORKFLOW_QUIESCE_REQUESTED"
	EventWorkflowQuiesced         EventKind = "WORKFLOW_QUIESCED"
	EventWorkflowCancelRequested  EventKind = "WORKFLOW_CANCEL_REQUESTED"
	EventWorkflowCancelled        EventKind = "WORKFLOW_CANCELLED"
	// EventControlledStopRequested appends the user's persisted controlled
	// stop intent (PRD 已确认：Ctrl+C 两阶段有限停止 step 1).
	EventControlledStopRequested EventKind = "CONTROLLED_STOP_REQUESTED"
	// EventCommitPolicySafetyStopRequested records the persisted Policy
	// Safety Stop intent (PRD 已确认：Commit Policy 漂移立即安全停止 step 1).
	EventCommitPolicySafetyStopRequested EventKind = "COMMIT_POLICY_SAFETY_STOP_REQUESTED"
	// EventCommitPolicyConfirmed records the append-only COMMIT_POLICY
	// Approval binding the exact new Preflight Revision/Hash/Fingerprint
	// (PRD 已确认：执行期间 Commit Policy 漂移确认 step 4).
	EventCommitPolicyConfirmed    EventKind = "COMMIT_POLICY_CONFIRMED"
	EventWorkflowFailed           EventKind = "WORKFLOW_FAILED"
	EventWorkflowSucceeded        EventKind = "WORKFLOW_SUCCEEDED"
	EventStageChanged             EventKind = "STAGE_CHANGED"
	EventPlanGenerated            EventKind = "PLAN_GENERATED"
	EventPlanCheckPassed          EventKind = "PLAN_CHECK_PASSED"
	EventPlanCheckNeedsRevision   EventKind = "PLAN_CHECK_NEEDS_REVISION"
	EventPlanCheckNeedsDiscussion EventKind = "PLAN_CHECK_NEEDS_DISCUSSION"
	EventPlanCheckRejected        EventKind = "PLAN_CHECK_REJECTED"
	EventPlanApproved             EventKind = "PLAN_APPROVED"
	EventExecutionApproved        EventKind = "EXECUTION_APPROVED"
	EventNodeReady                EventKind = "NODE_READY"
	EventNodeStarted              EventKind = "NODE_STARTED"
	EventNodeSucceeded            EventKind = "NODE_SUCCEEDED"
	EventNodeFailed               EventKind = "NODE_FAILED"
	EventNodeCancelled            EventKind = "NODE_CANCELLED"
	EventNodeSkipped              EventKind = "NODE_SKIPPED"
	EventAttemptCreated           EventKind = "ATTEMPT_CREATED"
	EventAttemptSucceeded         EventKind = "ATTEMPT_SUCCEEDED"
	EventAttemptFailed            EventKind = "ATTEMPT_FAILED"
	EventAttemptInterrupted       EventKind = "ATTEMPT_INTERRUPTED"
	EventAttemptCancelled         EventKind = "ATTEMPT_CANCELLED"
	EventFindingOpened            EventKind = "FINDING_OPENED"
	EventQuarantineRecorded       EventKind = "QUARANTINE_RECORDED"
	EventRunStarted               EventKind = "RUN_STARTED"
	EventRunQuiescing             EventKind = "RUN_QUIESCING"
	EventRunStopped               EventKind = "RUN_STOPPED"
	EventRunInterrupted           EventKind = "RUN_INTERRUPTED"
	EventRunBlocked               EventKind = "RUN_BLOCKED"
	EventRunSucceeded             EventKind = "RUN_SUCCEEDED"
	EventRunCancelled             EventKind = "RUN_CANCELLED"
	EventGraphInstalled           EventKind = "GRAPH_INSTALLED"
	EventApplyAttemptCreated      EventKind = "APPLY_ATTEMPT_CREATED"
	EventApplyRestarted           EventKind = "APPLY_RESTARTED"
	EventApplyVerified            EventKind = "APPLY_VERIFIED"
	EventApplySucceeded           EventKind = "APPLY_SUCCEEDED"
	EventApplyFailed              EventKind = "APPLY_FAILED"
	EventApplyBlocked             EventKind = "APPLY_BLOCKED"
	EventCleanupAttemptCreated    EventKind = "CLEANUP_ATTEMPT_CREATED"
	EventCleanupItemRequested     EventKind = "CLEANUP_ITEM_REQUESTED"
	EventCleanupItemCompleted     EventKind = "CLEANUP_ITEM_COMPLETED"
	EventCleanupItemFailed        EventKind = "CLEANUP_ITEM_FAILED"
)

// Valid reports whether k is a declared Event Kind.
func (k EventKind) Valid() bool {
	switch k {
	case EventWorkflowCreated, EventWorkflowStarted, EventWorkflowPaused, EventWorkflowResumed,
		EventWorkflowBlocked, EventWorkflowQuiesceRequested, EventWorkflowQuiesced,
		EventWorkflowCancelRequested, EventWorkflowCancelled, EventControlledStopRequested,
		EventCommitPolicySafetyStopRequested, EventCommitPolicyConfirmed, EventWorkflowFailed, EventWorkflowSucceeded,
		EventStageChanged, EventPlanGenerated, EventPlanCheckPassed,
		EventPlanCheckNeedsRevision, EventPlanCheckNeedsDiscussion, EventPlanCheckRejected,
		EventPlanApproved, EventExecutionApproved,
		EventNodeReady, EventNodeStarted, EventNodeSucceeded, EventNodeFailed, EventNodeCancelled, EventNodeSkipped,
		EventAttemptCreated, EventAttemptSucceeded, EventAttemptFailed, EventAttemptInterrupted, EventAttemptCancelled,
		EventFindingOpened, EventQuarantineRecorded,
		EventRunStarted, EventRunQuiescing, EventRunStopped, EventRunInterrupted, EventRunBlocked,
		EventRunSucceeded, EventRunCancelled,
		EventGraphInstalled,
		EventApplyAttemptCreated, EventApplyRestarted, EventApplyVerified, EventApplySucceeded, EventApplyFailed, EventApplyBlocked,
		EventCleanupAttemptCreated, EventCleanupItemRequested, EventCleanupItemCompleted, EventCleanupItemFailed:
		return true
	}
	return false
}

// String renders the Event Kind.
func (k EventKind) String() string { return string(k) }

// Event is one authoritative, sequentially numbered Event. Sequence
// numbers are assigned by the Kernel from State.NextEventSeq and never
// decrease across Decisions.
type Event struct {
	Seq      uint64
	Kind     EventKind
	Workflow WorkflowID
	Node     NodeID
	Attempt  AttemptKey
	Code     Code
	Text     string
	At       time.Time
}

// Effect Intents live in effects.go (design 6.3): the closed union of
// Runtime-owned operations.
