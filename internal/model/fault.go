package model

import (
	"errors"
	"fmt"
)

// Code is the stable identity of one failure class. The canonical naming
// style is Code<Name> (design), mapping every PRD UPPER_SNAKE code from
// the PRD 失败分类 table and the design's code list one-to-one. This
// Fault table is the single source of truth: every Code declares its
// Category, Scope, retry charge, dispatch closure, safety-stop scope, and
// successor permission (design 8.2).
type Code string

const (
	// CodeInvalidInput is the constructor default for requests that cannot
	// be interpreted safely (design 8.2, Invalid Input).
	CodeInvalidInput Code = "INVALID_INPUT"
	// CodeStateInvariantViolation is the constructor default for State
	// validation failures: authoritative facts cannot be reconciled and
	// the Workflow is Failed or the Project quarantined (design 8.2).
	CodeStateInvariantViolation Code = "STATE_INVARIANT_VIOLATION"

	// CodeUntrustedCompletion: Agent-declared completion can never
	// complete a Node; agent output cannot write authoritative lifecycle
	// state (design 7.3 invariant 1).
	CodeUntrustedCompletion Code = "UNTRUSTED_COMPLETION"

	// Retryable coding/verification Attempt failures (PRD 失败分类, 是).
	CodeAgentProcessCrashed         Code = "AGENT_PROCESS_CRASHED"
	CodeAgentTimeout                Code = "AGENT_TIMEOUT"
	CodeCommandFailed               Code = "COMMAND_FAILED"
	CodeProviderError               Code = "PROVIDER_ERROR"
	CodeSemanticReviewFailed        Code = "SEMANTIC_REVIEW_FAILED"
	CodeMergeConflict               Code = "MERGE_CONFLICT"
	CodeMissingImplementationCommit Code = "MISSING_IMPLEMENTATION_COMMIT"
	CodeDirtyTaskWorktree           Code = "DIRTY_TASK_WORKTREE"
	CodeDirtyWorktreeDrifted        Code = "DIRTY_WORKTREE_DRIFTED"
	CodeTaskHistoryRewritten        Code = "TASK_HISTORY_REWRITTEN"
	CodeEvidenceSubjectChanged      Code = "EVIDENCE_SUBJECT_CHANGED"

	// User interruption and Provider protocol failures
	// (PRD 失败分类, 否，且不扣失败重试预算).
	CodeUserInterrupted                Code = "USER_INTERRUPTED"
	CodeProviderProtocolUnsupported    Code = "PROVIDER_PROTOCOL_UNSUPPORTED"
	CodeProviderBindingChanged         Code = "PROVIDER_PROTOCOL_BINDING_CHANGED"
	CodeProviderProtocolViolation      Code = "PROVIDER_PROTOCOL_VIOLATION"
	CodeProviderSessionIDMissing       Code = "PROVIDER_SESSION_ID_MISSING"
	CodeProviderAuthenticationRequired Code = "PROVIDER_AUTHENTICATION_REQUIRED"

	// Controlled-stop facts (PRD 已确认：Ctrl+C 两阶段有限停止): an orphan
	// that survived the force-kill phase quarantines Project mutation and
	// Blocks the Workflow; a resume whose Worktree drifted from the
	// interruption Checkpoint blocks with INTERRUPTED_WORKTREE_DRIFTED; a
	// Cancel whose process facts cannot settle keeps its intent and Blocks
	// with CANCEL_PENDING_ORPHAN_PROCESS until Recovery completes it.
	CodeOrphanChildProcess         Code = "ORPHAN_CHILD_PROCESS"
	CodeInterruptedWorktreeDrifted Code = "INTERRUPTED_WORKTREE_DRIFTED"
	CodeCancelPendingOrphanProcess Code = "CANCEL_PENDING_ORPHAN_PROCESS"

	// Local environment and data-safety failures (PRD 失败分类).
	CodeInsecureCFLOWHomePermissions Code = "INSECURE_CFLOW_HOME_PERMISSIONS"
	CodeSensitiveDataRedactionFailed Code = "SENSITIVE_DATA_REDACTION_FAILED"
	CodeDatabaseSchemaTooNew         Code = "DATABASE_SCHEMA_TOO_NEW"
	CodeDatabaseMigrationPathMissing Code = "DATABASE_MIGRATION_PATH_MISSING"
	CodeMigrationChecksumMismatch    Code = "MIGRATION_CHECKSUM_MISMATCH"
	CodeDatabaseMigrationFailed      Code = "DATABASE_MIGRATION_FAILED"
	CodeDatabaseMigrationIncomplete  Code = "DATABASE_MIGRATION_INCOMPLETE"
	CodeArtifactSchemaUnsupported    Code = "ARTIFACT_SCHEMA_UNSUPPORTED"

	// Gate and policy failures (PRD 失败分类, 否).
	CodeScopeViolation                   Code = "SCOPE_VIOLATION"
	CodeIntegrationVerificationFailed    Code = "INTEGRATION_VERIFICATION_FAILED"
	CodeMissingCredentials               Code = "MISSING_CREDENTIALS"
	CodePlanDrift                        Code = "PLAN_DRIFT"
	CodeBudgetExceeded                   Code = "BUDGET_EXCEEDED"
	CodeRetryExhausted                   Code = "RETRY_EXHAUSTED"
	CodeCommitDuringPolicyDriftWindow    Code = "COMMIT_DURING_POLICY_DRIFT_WINDOW"
	CodeReplacementReconciliationChanged Code = "REPLACEMENT_RECONCILIATION_CHANGED"

	// Commit Policy drift and Git preflight failures (PRD 约束 39-41).
	CodeCommitPolicyConfirmationRequired Code = "COMMIT_POLICY_CONFIRMATION_REQUIRED"
	CodeCommitPolicySafetyStopRequested  Code = "COMMIT_POLICY_SAFETY_STOP_REQUESTED"
	CodeCommitPolicyInputChanged         Code = "COMMIT_POLICY_INPUT_CHANGED"
	CodeCommitPolicyMismatch             Code = "COMMIT_POLICY_MISMATCH"
	CodeGitIdentityNotConfigured         Code = "GIT_IDENTITY_NOT_CONFIGURED"
	CodeGitSigningPreflightFailed        Code = "GIT_SIGNING_PREFLIGHT_FAILED"
	// CodeCommitPolicyDrift is the Run stop_reason of a Policy Safety Stop
	// (PRD 已确认：Commit Policy 漂移立即安全停止 step 1: stop_reason =
	// COMMIT_POLICY_DRIFT). It is recorded on the Run, never charged.
	CodeCommitPolicyDrift Code = "COMMIT_POLICY_DRIFT"

	// Apply, Approval, and Cleanup failures. The protected Apply
	// (PRD 已确认：显式受保护 Apply) gates the user workspace and the
	// delivery with typed codes: a dirty workspace is APPLY_TARGET_DIRTY,
	// a wrong attached Branch or a detached HEAD is
	// APPLY_TARGET_BRANCH_CHANGED, and a Target Drift that changed the
	// Wrapper/Manifest/Executable identity is COMMAND_IDENTITY_CHANGED
	// (Target unchanged; only an explicit APPLY_CATALOG approval may
	// continue).
	CodeTargetHeadChanged          Code = "TARGET_HEAD_DRIFTED"
	CodeApplyTargetDirty           Code = "APPLY_TARGET_DIRTY"
	CodeApplyTargetBranchChanged   Code = "APPLY_TARGET_BRANCH_CHANGED"
	CodeCommandIdentityChanged     Code = "COMMAND_IDENTITY_CHANGED"
	CodeApprovalInputChanged       Code = "APPROVAL_INPUT_CHANGED"
	CodeCleanupWorkflowNotTerminal Code = "CLEANUP_WORKFLOW_NOT_TERMINAL"
	CodeCleanupActiveProcess       Code = "CLEANUP_ACTIVE_PROCESS"
	CodeCleanupTargetDirty         Code = "CLEANUP_TARGET_DIRTY"
	CodeCleanupFactsChanged        Code = "CLEANUP_FACT_MISMATCH"
	CodeCleanupItemFailed          Code = "CLEANUP_ITEM_FAILED"
	// CodeCleanupActiveApply: an Apply Attempt is in flight (staging,
	// awaiting the explicit delivery, or running); the Cleanup execution
	// re-confirms no active Apply before removing anything (design 17.4).
	CodeCleanupActiveApply Code = "CLEANUP_ACTIVE_APPLY"
	// CodeCleanupQuarantined: the Project's mutation is quarantined (a
	// persisted Branch Quarantine or a Project-mutation Blocking Finding);
	// the Cleanup execution re-confirms no Project Mutation Quarantine.
	CodeCleanupQuarantined Code = "CLEANUP_QUARANTINED"

	// Design codes (compiler, session independence, snapshot isolation).
	CodeWorkflowPatchForbidden       Code = "WORKFLOW_PATCH_FORBIDDEN"
	CodeWorkflowPatchApplied         Code = "WORKFLOW_PATCH_APPLIED"
	CodeSchemaInvalid                Code = "SCHEMA_INVALID"
	CodeSessionIndependenceViolation Code = "SESSION_INDEPENDENCE_VIOLATION"
	// CodeUnexpectedAgentMutation: a non-coding Session changed the
	// Planning Snapshot's HEAD or Git-visible state; its output is
	// invalid and can never enter an Artifact or Approval (PRD Worktree
	// 策略, 约束 33).
	CodeUnexpectedAgentMutation Code = "UNEXPECTED_AGENT_MUTATION"
	// CodeDispatchGateClosed: the Run Dispatch Gate is closed (Pause,
	// Quiesce, Cancel, or Safety Stop); an allocation request crossing the
	// committed closure is refused without mutation (design 12, PRD 已确
	// 认：并行失败后的 Quiescing).
	CodeDispatchGateClosed Code = "DISPATCH_GATE_CLOSED"
	// CodeNotYetAvailable: a requested protocol whose full semantics land
	// with a later task (the protected Apply, the Cleanup execute) is
	// reported with this stable finding, never with a fabricated result
	// (Task 18; PRD 必须提供的 CLI).
	CodeNotYetAvailable Code = "NOT_YET_AVAILABLE"
)

// String renders the Code.
func (c Code) String() string { return string(c) }

// Codes returns every declared Code in declaration order. The compiled
// fault-policy table must cover each of them (TestFaultTableComplete).
func Codes() []Code {
	return []Code{
		CodeInvalidInput,
		CodeStateInvariantViolation,
		CodeUntrustedCompletion,
		CodeAgentProcessCrashed,
		CodeAgentTimeout,
		CodeCommandFailed,
		CodeProviderError,
		CodeSemanticReviewFailed,
		CodeMergeConflict,
		CodeMissingImplementationCommit,
		CodeDirtyTaskWorktree,
		CodeDirtyWorktreeDrifted,
		CodeTaskHistoryRewritten,
		CodeEvidenceSubjectChanged,
		CodeUserInterrupted,
		CodeProviderProtocolUnsupported,
		CodeProviderBindingChanged,
		CodeProviderProtocolViolation,
		CodeProviderSessionIDMissing,
		CodeProviderAuthenticationRequired,
		CodeOrphanChildProcess,
		CodeInterruptedWorktreeDrifted,
		CodeCancelPendingOrphanProcess,
		CodeInsecureCFLOWHomePermissions,
		CodeSensitiveDataRedactionFailed,
		CodeDatabaseSchemaTooNew,
		CodeDatabaseMigrationPathMissing,
		CodeMigrationChecksumMismatch,
		CodeDatabaseMigrationFailed,
		CodeDatabaseMigrationIncomplete,
		CodeArtifactSchemaUnsupported,
		CodeScopeViolation,
		CodeIntegrationVerificationFailed,
		CodeMissingCredentials,
		CodePlanDrift,
		CodeBudgetExceeded,
		CodeRetryExhausted,
		CodeCommitDuringPolicyDriftWindow,
		CodeReplacementReconciliationChanged,
		CodeCommitPolicyConfirmationRequired,
		CodeCommitPolicySafetyStopRequested,
		CodeCommitPolicyInputChanged,
		CodeCommitPolicyMismatch,
		CodeGitIdentityNotConfigured,
		CodeGitSigningPreflightFailed,
		CodeCommitPolicyDrift,
		CodeTargetHeadChanged,
		CodeApplyTargetDirty,
		CodeApplyTargetBranchChanged,
		CodeCommandIdentityChanged,
		CodeApprovalInputChanged,
		CodeCleanupWorkflowNotTerminal,
		CodeCleanupActiveProcess,
		CodeCleanupTargetDirty,
		CodeCleanupFactsChanged,
		CodeCleanupItemFailed,
		CodeCleanupActiveApply,
		CodeCleanupQuarantined,
		CodeWorkflowPatchForbidden,
		CodeWorkflowPatchApplied,
		CodeSchemaInvalid,
		CodeSessionIndependenceViolation,
		CodeUnexpectedAgentMutation,
		CodeDispatchGateClosed,
		CodeNotYetAvailable,
	}
}

// ---------------------------------------------------------------------------
// Fault categories and scopes (design 8.2)
// ---------------------------------------------------------------------------

// FaultCategory is the disposition class of a failure.
type FaultCategory string

const (
	// CatInvalidInput: the user request cannot be interpreted safely; no
	// mutation; CLI error.
	CatInvalidInput FaultCategory = "INVALID_INPUT"
	// CatRetryableAttemptFailure: an approved automatic successor is
	// allowed; the Attempt is terminal, the Node returns READY, and the
	// budget is charged.
	CatRetryableAttemptFailure FaultCategory = "RETRYABLE_ATTEMPT_FAILURE"
	// CatUserActionRequired: facts are safe but automatic progress is
	// forbidden; a Finding plus Paused or Blocked.
	CatUserActionRequired FaultCategory = "USER_ACTION_REQUIRED"
	// CatSafetyStop: active work must be stopped before facts can be
	// trusted; close dispatch, controlled stop, reconcile.
	CatSafetyStop FaultCategory = "SAFETY_STOP"
	// CatInvariantFailure: authoritative facts cannot be reconciled;
	// Workflow Failed or Project Quarantine.
	CatInvariantFailure FaultCategory = "INVARIANT_FAILURE"
)

// Valid reports whether c is a declared Fault Category.
func (c FaultCategory) Valid() bool {
	switch c {
	case CatInvalidInput, CatRetryableAttemptFailure, CatUserActionRequired, CatSafetyStop, CatInvariantFailure:
		return true
	}
	return false
}

// String renders the Fault Category.
func (c FaultCategory) String() string { return string(c) }

// FaultScope names the aggregate a failure applies to.
type FaultScope string

const (
	ScopeInput            FaultScope = "INPUT"
	ScopeProject          FaultScope = "PROJECT"
	ScopeWorkflow         FaultScope = "WORKFLOW"
	ScopeWorkflowRevision FaultScope = "WORKFLOW_REVISION"
	ScopeRun              FaultScope = "RUN"
	ScopeNode             FaultScope = "NODE"
	ScopeAttempt          FaultScope = "ATTEMPT"
	ScopeSession          FaultScope = "SESSION"
	ScopePlan             FaultScope = "PLAN"
	ScopeArtifact         FaultScope = "ARTIFACT"
	ScopeApproval         FaultScope = "APPROVAL"
	ScopeBranch           FaultScope = "BRANCH"
	ScopeApply            FaultScope = "APPLY"
	ScopeCleanup          FaultScope = "CLEANUP"
	ScopeDatabase         FaultScope = "DATABASE"
)

// Valid reports whether s is a declared Fault Scope.
func (s FaultScope) Valid() bool {
	switch s {
	case ScopeInput, ScopeProject, ScopeWorkflow, ScopeWorkflowRevision, ScopeRun,
		ScopeNode, ScopeAttempt, ScopeSession, ScopePlan, ScopeArtifact,
		ScopeApproval, ScopeBranch, ScopeApply, ScopeCleanup, ScopeDatabase:
		return true
	}
	return false
}

// String renders the Fault Scope.
func (s FaultScope) String() string { return string(s) }

// StopScope is the safety-stop scope a fault demands: stop nothing, stop
// the affected attempt/process, or stop all active attempts through the
// controlled-stop protocol.
type StopScope string

const (
	StopNone     StopScope = "NONE"
	StopAffected StopScope = "AFFECTED"
	StopAll      StopScope = "ALL"
)

// Valid reports whether s is a declared Stop Scope.
func (s StopScope) Valid() bool {
	switch s {
	case StopNone, StopAffected, StopAll:
		return true
	}
	return false
}

// String renders the Stop Scope.
func (s StopScope) String() string { return string(s) }

// RetryDisposition declares whether a failure charges the Node Retry
// Budget and whether an automatic successor Attempt is permitted.
// Interrupted Attempts never charge (PRD 失败分类, USER_INTERRUPTED).
type RetryDisposition struct {
	ChargesBudget   bool
	AllowsSuccessor bool
}

// Fault is the stable, normalized failure value (design 8.2). Every Fault
// carries the compiled policy for its Code, the Evidence it was raised
// from, and a SafeText for humans.
type Fault struct {
	Code     Code
	Category FaultCategory
	Scope    FaultScope
	Retry    RetryDisposition
	Evidence EvidenceRef
	SafeText string
}

// Error renders the Fault.
func (f *Fault) Error() string {
	return fmt.Sprintf("%s: %s", f.Code, f.SafeText)
}

// FaultPolicy is one row of the compiled fault-policy table: every Code
// declares whether it charges Retry Budget, closes the Dispatch Gate,
// stops other Attempts (safety-stop scope), and permits automatic
// successor creation (design 8.2).
type FaultPolicy struct {
	Code          Code
	Category      FaultCategory
	Scope         FaultScope
	Retry         RetryDisposition
	CloseDispatch bool
	StopScope     StopScope
}

// p is the compact row constructor for the compiled table.
func p(code Code, cat FaultCategory, scope FaultScope, charge, closeGate bool, stop StopScope, successor bool) FaultPolicy {
	return FaultPolicy{
		Code:          code,
		Category:      cat,
		Scope:         scope,
		Retry:         RetryDisposition{ChargesBudget: charge, AllowsSuccessor: successor},
		CloseDispatch: closeGate,
		StopScope:     stop,
	}
}

// faultPolicies is the compiled fault-policy table: the single source of
// truth for failure disposition. It is code, never read from Agent output
// or mutable configuration (design 8.2).
var faultPolicies = []FaultPolicy{
	// Invalid input and invariant defaults.
	p(CodeInvalidInput, CatInvalidInput, ScopeInput, false, false, StopNone, false),
	p(CodeStateInvariantViolation, CatInvariantFailure, ScopeWorkflow, false, true, StopAll, false),
	p(CodeUntrustedCompletion, CatInvalidInput, ScopeAttempt, false, false, StopAffected, false),

	// Retryable Attempt failures: charged, successor allowed, dispatch
	// stays open (PRD 失败分类, 是).
	p(CodeAgentProcessCrashed, CatRetryableAttemptFailure, ScopeAttempt, true, false, StopNone, true),
	p(CodeAgentTimeout, CatRetryableAttemptFailure, ScopeAttempt, true, false, StopAffected, true),
	p(CodeCommandFailed, CatRetryableAttemptFailure, ScopeAttempt, true, false, StopNone, true),
	p(CodeProviderError, CatRetryableAttemptFailure, ScopeAttempt, true, false, StopNone, true),
	p(CodeSemanticReviewFailed, CatRetryableAttemptFailure, ScopeAttempt, true, false, StopNone, true),
	p(CodeMergeConflict, CatRetryableAttemptFailure, ScopeAttempt, true, false, StopNone, true),
	p(CodeMissingImplementationCommit, CatRetryableAttemptFailure, ScopeAttempt, true, false, StopNone, true),
	p(CodeDirtyTaskWorktree, CatRetryableAttemptFailure, ScopeAttempt, true, false, StopNone, true),
	p(CodeEvidenceSubjectChanged, CatRetryableAttemptFailure, ScopeAttempt, true, false, StopAffected, true),

	// Non-retryable attempt failures that never charge: recovery facts
	// drifted or history was rewritten (PRD 约束 31-33).
	p(CodeDirtyWorktreeDrifted, CatUserActionRequired, ScopeAttempt, false, true, StopAffected, false),
	p(CodeTaskHistoryRewritten, CatUserActionRequired, ScopeAttempt, false, true, StopAffected, false),

	// User interruption: no retry charge; the run stops and the Workflow
	// pauses (or blocks when a blocking Finding is present).
	p(CodeUserInterrupted, CatUserActionRequired, ScopeRun, false, true, StopAll, false),
	// Orphan and drift facts of the controlled stop (PRD 已确认：Ctrl+C 两
	// 阶段有限停止): an orphaned process that survived the force-kill phase
	// quarantines Project mutation and Blocks; a resume whose Worktree
	// drifted from the interruption Checkpoint never reuses the path and
	// Blocks; a Cancel whose processes cannot settle keeps its intent and
	// Blocks until Recovery completes the cancellation.
	p(CodeOrphanChildProcess, CatUserActionRequired, ScopeRun, false, true, StopNone, false),
	p(CodeInterruptedWorktreeDrifted, CatUserActionRequired, ScopeAttempt, false, true, StopAffected, false),
	p(CodeCancelPendingOrphanProcess, CatUserActionRequired, ScopeWorkflow, false, true, StopNone, false),

	// Provider protocol failures: never charged, dispatch closed
	// (PRD 失败分类, 否，且不扣失败重试预算).
	p(CodeProviderProtocolUnsupported, CatUserActionRequired, ScopeWorkflow, false, true, StopAffected, false),
	p(CodeProviderBindingChanged, CatSafetyStop, ScopeWorkflow, false, true, StopAll, false),
	p(CodeProviderProtocolViolation, CatSafetyStop, ScopeSession, false, true, StopAffected, false),
	p(CodeProviderSessionIDMissing, CatUserActionRequired, ScopeSession, false, true, StopAffected, false),
	p(CodeProviderAuthenticationRequired, CatUserActionRequired, ScopeSession, false, true, StopAffected, false),

	// Local environment and data-safety failures.
	p(CodeInsecureCFLOWHomePermissions, CatSafetyStop, ScopeProject, false, true, StopAll, false),
	p(CodeSensitiveDataRedactionFailed, CatSafetyStop, ScopeSession, false, true, StopAffected, false),
	p(CodeDatabaseSchemaTooNew, CatUserActionRequired, ScopeDatabase, false, true, StopAll, false),
	p(CodeDatabaseMigrationPathMissing, CatUserActionRequired, ScopeDatabase, false, true, StopAll, false),
	p(CodeMigrationChecksumMismatch, CatInvariantFailure, ScopeDatabase, false, true, StopAll, false),
	p(CodeDatabaseMigrationFailed, CatInvariantFailure, ScopeDatabase, false, true, StopAll, false),
	p(CodeDatabaseMigrationIncomplete, CatInvariantFailure, ScopeDatabase, false, true, StopAll, false),
	p(CodeArtifactSchemaUnsupported, CatUserActionRequired, ScopeArtifact, false, true, StopAffected, false),

	// Gate and policy failures: never auto-retried (PRD 失败分类, 否).
	p(CodeScopeViolation, CatUserActionRequired, ScopeAttempt, false, true, StopAffected, false),
	p(CodeIntegrationVerificationFailed, CatUserActionRequired, ScopeWorkflow, false, true, StopAffected, false),
	p(CodeMissingCredentials, CatUserActionRequired, ScopeWorkflow, false, true, StopAffected, false),
	p(CodePlanDrift, CatUserActionRequired, ScopePlan, false, true, StopAffected, false),
	p(CodeBudgetExceeded, CatUserActionRequired, ScopeWorkflow, false, true, StopAffected, false),
	p(CodeRetryExhausted, CatUserActionRequired, ScopeNode, false, true, StopNone, false),
	p(CodeCommitDuringPolicyDriftWindow, CatUserActionRequired, ScopeBranch, false, true, StopNone, false),
	p(CodeReplacementReconciliationChanged, CatUserActionRequired, ScopeWorkflow, false, true, StopAffected, false),

	// Commit Policy drift and Git preflight failures (PRD 约束 39-41).
	p(CodeCommitPolicyConfirmationRequired, CatUserActionRequired, ScopeWorkflow, false, true, StopNone, false),
	p(CodeCommitPolicySafetyStopRequested, CatSafetyStop, ScopeRun, false, true, StopAll, false),
	p(CodeCommitPolicyInputChanged, CatInvalidInput, ScopeApproval, false, false, StopNone, false),
	p(CodeCommitPolicyMismatch, CatUserActionRequired, ScopeAttempt, false, true, StopAffected, false),
	p(CodeCommitPolicyDrift, CatSafetyStop, ScopeRun, false, true, StopAll, false),
	p(CodeGitIdentityNotConfigured, CatUserActionRequired, ScopeWorkflow, false, true, StopAffected, false),
	p(CodeGitSigningPreflightFailed, CatUserActionRequired, ScopeWorkflow, false, true, StopAffected, false),

	// Apply, Approval, and Cleanup.
	p(CodeTargetHeadChanged, CatUserActionRequired, ScopeApply, false, true, StopAffected, false),
	p(CodeApplyTargetDirty, CatUserActionRequired, ScopeApply, false, true, StopAffected, false),
	p(CodeApplyTargetBranchChanged, CatUserActionRequired, ScopeApply, false, true, StopAffected, false),
	p(CodeCommandIdentityChanged, CatUserActionRequired, ScopeApply, false, true, StopAffected, false),
	p(CodeApprovalInputChanged, CatInvalidInput, ScopeApproval, false, false, StopNone, false),
	p(CodeCleanupWorkflowNotTerminal, CatUserActionRequired, ScopeCleanup, false, false, StopNone, false),
	p(CodeCleanupActiveProcess, CatUserActionRequired, ScopeCleanup, false, false, StopNone, false),
	p(CodeCleanupTargetDirty, CatUserActionRequired, ScopeCleanup, false, false, StopNone, false),
	p(CodeCleanupFactsChanged, CatUserActionRequired, ScopeCleanup, false, false, StopNone, false),
	p(CodeCleanupItemFailed, CatUserActionRequired, ScopeCleanup, false, false, StopNone, false),
	p(CodeCleanupActiveApply, CatUserActionRequired, ScopeCleanup, false, false, StopNone, false),
	p(CodeCleanupQuarantined, CatUserActionRequired, ScopeCleanup, false, false, StopNone, false),

	// Design codes.
	p(CodeWorkflowPatchForbidden, CatInvalidInput, ScopeWorkflowRevision, false, false, StopNone, false),
	p(CodeWorkflowPatchApplied, CatInvalidInput, ScopeWorkflowRevision, false, false, StopNone, false),
	p(CodeSchemaInvalid, CatInvalidInput, ScopeArtifact, false, false, StopNone, false),
	p(CodeSessionIndependenceViolation, CatInvariantFailure, ScopeSession, false, true, StopAll, false),
	p(CodeUnexpectedAgentMutation, CatSafetyStop, ScopeSession, false, true, StopAll, false),
	p(CodeDispatchGateClosed, CatInvalidInput, ScopeRun, false, false, StopNone, false),
	p(CodeNotYetAvailable, CatUserActionRequired, ScopeWorkflow, false, false, StopNone, false),
}

// Policy returns the compiled disposition for one Code. Every declared
// Code has exactly one policy.
func Policy(code Code) (FaultPolicy, bool) {
	for _, pol := range faultPolicies {
		if pol.Code == code {
			return pol, true
		}
	}
	return FaultPolicy{}, false
}

// NewFault constructs a Fault from the compiled policy for its Code.
func NewFault(code Code, safeText string) *Fault {
	pol, _ := Policy(code)
	return &Fault{Code: code, Category: pol.Category, Scope: pol.Scope, Retry: pol.Retry, SafeText: safeText}
}

// NewFaultWithEvidence constructs a Fault and attaches the Evidence it was
// raised from.
func NewFaultWithEvidence(code Code, safeText string, evidence EvidenceRef) *Fault {
	f := NewFault(code, safeText)
	f.Evidence = evidence
	return f
}

// InvalidInputFault is the constructor for requests that cannot be
// interpreted safely: no mutation, CLI error (design 8.2).
func InvalidInputFault(safeText string) error {
	return NewFault(CodeInvalidInput, safeText)
}

// InvariantFault is the constructor for authoritative facts that cannot
// be reconciled: Workflow Failed or Project Quarantine.
func InvariantFault(err error) error {
	text := "state invariant violation"
	if err != nil {
		text = err.Error()
	}
	return NewFault(CodeStateInvariantViolation, text)
}

// CodeOf extracts the Code from any error chain. It reports false when
// the error carries no Fault.
func CodeOf(err error) (Code, bool) {
	var f *Fault
	if errors.As(err, &f) {
		return f.Code, true
	}
	return "", false
}
