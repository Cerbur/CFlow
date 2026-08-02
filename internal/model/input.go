package model

// Input is the closed union of inputs the Decision Kernel accepts: user
// Commands, Effect Results, Agent Events, and the Reconcile sweep. The
// unexported marker keeps the union closed to this package (design 6.2).
type Input interface{ isInput() }

// WorkflowCommandKind is a user Workflow mutation command (design 6.1).
type WorkflowCommandKind string

const (
	CreateWorkflow WorkflowCommandKind = "create"
	StartWorkflow  WorkflowCommandKind = "start"
	PauseWorkflow  WorkflowCommandKind = "pause"
	ResumeWorkflow WorkflowCommandKind = "resume"
	CancelWorkflow WorkflowCommandKind = "cancel"
)

// Valid reports whether k is a declared Workflow Command Kind.
func (k WorkflowCommandKind) Valid() bool {
	switch k {
	case CreateWorkflow, StartWorkflow, PauseWorkflow, ResumeWorkflow, CancelWorkflow:
		return true
	}
	return false
}

// String renders the Command Kind.
func (k WorkflowCommandKind) String() string { return string(k) }

// WorkflowCommandInput is a user Workflow mutation Command. CreateWorkflow
// carries the opaque Workflow identity the Application generated; the
// Kernel derives every later identity deterministically from the
// aggregate (design 6.2 rule 6).
type WorkflowCommandInput struct {
	Kind WorkflowCommandKind

	Workflow     WorkflowID
	Project      ProjectID
	TargetBranch string
	BaseCommit   string
	Reason       string
}

func (WorkflowCommandInput) isInput() {}

// PlanApprovalInput is the append-only user decision accepting one exact
// checked Plan Revision and hash (CONTEXT.md: Plan Approval). The hash is
// part of the Plan Artifact identity; a mismatched input is an
// APPROVAL_INPUT_CHANGED fault and never a generalised consent.
type PlanApprovalInput struct {
	PlanRef ArtifactRef
	Hash    string
}

func (PlanApprovalInput) isInput() {}

// ExecutionApprovalInput is the append-only user decision accepting one
// exact set of execution Artifacts, routing, budgets, and commit-policy
// facts (CONTEXT.md: Execution Approval). Every hash must match the
// active ExecutionFacts exactly.
type ExecutionApprovalInput struct {
	PlanHash         string
	SpecHashes       []string
	CatalogHash      string
	WorkflowHash     string
	RoutingHash      string
	BudgetHash       string
	CommitPolicyHash string
}

func (ExecutionApprovalInput) isInput() {}

// AgentEventKind names the closed set of validated Agent Events the
// Runtime may deliver to the Kernel. Agent output can never write
// authoritative lifecycle state: an Agent-declared completion is an
// UNTRUSTED_COMPLETION fault (design 7.3 invariant 1).
type AgentEventKind string

const (
	AgentClaimsComplete AgentEventKind = "claims-complete"
)

// Valid reports whether k is a declared Agent Event Kind.
func (k AgentEventKind) Valid() bool {
	return k == AgentClaimsComplete
}

// String renders the Agent Event Kind.
func (k AgentEventKind) String() string { return string(k) }

// AgentEventInput is one validated Agent Event.
type AgentEventInput struct {
	Kind    AgentEventKind
	Session SessionID
	Node    NodeID
}

func (AgentEventInput) isInput() {}

// EffectResultKind names the closed set of Effect Results: immutable
// evidence inputs to another Decision (design 6.2 rule 3).
type EffectResultKind string

const (
	AttemptEnded              EffectResultKind = "attempt-ended"
	ProcessStopped            EffectResultKind = "process-stopped"
	ApplyStagingSucceeded     EffectResultKind = "apply-staging-succeeded"
	ApplyFastForwardSucceeded EffectResultKind = "apply-fast-forward-succeeded"
	ApplyFastForwardFailed    EffectResultKind = "apply-fast-forward-failed"
	CleanupItemRemovedResult  EffectResultKind = "cleanup-item-removed"
	CleanupItemFailedResult   EffectResultKind = "cleanup-item-failed"
)

// Valid reports whether k is a declared Effect Result Kind.
func (k EffectResultKind) Valid() bool {
	switch k {
	case AttemptEnded, ProcessStopped, ApplyStagingSucceeded,
		ApplyFastForwardSucceeded, ApplyFastForwardFailed,
		CleanupItemRemovedResult, CleanupItemFailedResult:
		return true
	}
	return false
}

// String renders the Effect Result Kind.
func (k EffectResultKind) String() string { return string(k) }

// AttemptOutcome is the end result of one Attempt as reported by settled
// process and Git facts. It is never inferred from an Agent message or a
// zero exit code (design 6.2 rule 5, 14.3).
type AttemptOutcome string

const (
	OutcomeSucceeded   AttemptOutcome = "SUCCEEDED"
	OutcomeFailed      AttemptOutcome = "FAILED"
	OutcomeInterrupted AttemptOutcome = "INTERRUPTED"
)

// Valid reports whether o is a declared Attempt Outcome.
func (o AttemptOutcome) Valid() bool {
	switch o {
	case OutcomeSucceeded, OutcomeFailed, OutcomeInterrupted:
		return true
	}
	return false
}

// String renders the Attempt Outcome.
func (o AttemptOutcome) String() string { return string(o) }

// EffectResultInput is one Effect Result carrying immutable end facts.
// The Kernel validates the facts, not the claim: for a coding Attempt,
// success requires a new Commit in a clean Worktree, so the end facts are
// the authority.
type EffectResultInput struct {
	Kind EffectResultKind

	Attempt             AttemptKey
	Outcome             AttemptOutcome
	FailureCode         Code
	EndHead             string
	EndDirtyFingerprint string
	Evidence            EvidenceRef

	Process        ProcessID
	ApplyAttempt   ApplyAttemptID
	CleanupAttempt CleanupAttemptID
	ItemIndex      int
}

func (EffectResultInput) isInput() {}

// ReconcileInput triggers the Recovery sweep of the Kernel: it completes a
// persisted Cancel intent once everything is settled, converges a
// QUIESCING Run to BLOCKED, and blocks a Workflow that carries a FAILED
// Node with no in-flight Attempts. It never reopens dispatch.
type ReconcileInput struct{}

func (ReconcileInput) isInput() {}

// ApplyCommandKind is the user Apply mutation (design 6.1).
type ApplyCommandKind string

const (
	ApplyRequest ApplyCommandKind = "request"
	ApplyConfirm ApplyCommandKind = "confirm"
)

// Valid reports whether k is a declared Apply Command Kind.
func (k ApplyCommandKind) Valid() bool {
	return k == ApplyRequest || k == ApplyConfirm
}

// String renders the Apply Command Kind.
func (k ApplyCommandKind) String() string { return string(k) }

// ApplyCommandInput is the user Apply interaction. The request fixes the
// Target HEAD and Integration HEAD the Apply Attempt must revalidate; the
// confirmation re-binds them together with the exact Preflight hash and
// fingerprint, so a drifted fact invalidates the confirmation.
type ApplyCommandInput struct {
	Kind ApplyCommandKind

	TargetHead      string
	IntegrationHead string
	Preflight       ArtifactRef
	PreflightHash   string
	Fingerprint     string
}

func (ApplyCommandInput) isInput() {}

// CleanupCommandKind is the user Cleanup mutation (design 6.1).
type CleanupCommandKind string

const (
	CleanupDryRun  CleanupCommandKind = "dry-run"
	CleanupExecute CleanupCommandKind = "execute"
)

// Valid reports whether k is a declared Cleanup Command Kind.
func (k CleanupCommandKind) Valid() bool {
	return k == CleanupDryRun || k == CleanupExecute
}

// String renders the Cleanup Command Kind.
func (k CleanupCommandKind) String() string { return string(k) }

// CleanupCommandInput is the user Cleanup interaction. DryRun carries the
// freshly observed candidate targets; Execute carries the immutable
// Manifest the confirmation binds plus freshly re-observed facts the
// Kernel revalidates against that exact Manifest.
type CleanupCommandInput struct {
	Kind CleanupCommandKind

	Manifest ArtifactRef
	Items    []CleanupItem
}

func (CleanupCommandInput) isInput() {}
