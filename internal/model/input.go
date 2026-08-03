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

// DiscussRequirementInput is one user turn of the requirement discussion
// (PRD 需求讨论交互). The CFlow Session identity is allocated by the
// Application and fixed before the Effect (design 6.2 rule 6); the Kernel
// derives the superseded Session from the aggregate, so every turn joins
// one discussion Session lineage.
type DiscussRequirementInput struct {
	Text     string
	Provider string
	Session  SessionID
}

func (DiscussRequirementInput) isInput() {}

// GeneratePlanInput is the /finish transition (PRD Plan 生成): the
// planner produces a new immutable Plan Revision from the requirement
// discussion lineage. The Plan body arrives through the Provider run
// Result; the Kernel validates it against the PRD's required sections.
type GeneratePlanInput struct {
	Provider string
	Session  SessionID
}

func (GeneratePlanInput) isInput() {}

// CheckPlanInput starts an independent plan-check Session (PRD Plan Check
// 交互). The checker is always a fresh Session with the plan-check
// purpose; it can never be the Planner's Session (design 14.4).
type CheckPlanInput struct {
	Provider string
	Session  SessionID
}

func (CheckPlanInput) isInput() {}

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

// SpecGenerationInput starts one Spec Generation Session (PRD Agent 角色:
// SPEC_GENERATION 将 Plan 拆成 Specs). CatalogRef is the immutable
// Verification Catalog Revision the Runtime assembled and wrote before
// the Session may reference any command id (PRD 已确认：Workflow-local
// Verification Command Catalog step 1: discovery only produces
// Candidates; CFlow assembles the immutable Catalog Revision).
type SpecGenerationInput struct {
	Provider   string
	Session    SessionID
	CatalogRef ArtifactRef
}

func (SpecGenerationInput) isInput() {}

// WorkflowCompilationInput starts one Workflow Optimization Session
// (PRD Agent 角色: WORKFLOW_OPTIMIZATION 对确定性骨架提出受限调度补丁).
// The Session output is the restricted Patch IR the Compiler validates
// against the deterministic skeleton.
type WorkflowCompilationInput struct {
	Provider string
	Session  SessionID
}

func (WorkflowCompilationInput) isInput() {}

// ExecutionDryRunInput carries the freshly observed Git Commit Preflight
// evidence and the resolved routing/budget policy references into the
// Execution Dry Run gate (Task 16, design 20.1). The Workflow pauses at
// WORKFLOW_GENERATION only after the Dry Run records a successful
// Preflight Revision and the complete execution input set.
type ExecutionDryRunInput struct {
	Preflight PreflightFacts
	// RoutingRef and BudgetRef are the immutable routing-policy and
	// budget-policy Revisions the Dry Run resolved and wrote; the gate
	// records their active references so the Execution Approval preview
	// binds their hashes.
	RoutingRef ArtifactRef
	BudgetRef  ArtifactRef
}

func (ExecutionDryRunInput) isInput() {}

// PreflightFacts is the normalized Commit Preflight evidence the Dry Run
// gate records (PRD 已确认：Git Commit Identity 与 Signing Preflight). It
// never carries private keys, passphrases, or credential-helper output.
type PreflightFacts struct {
	EvidenceHash      string // artifact sha256 of the preflight report
	GitVersion        string
	RepositoryContext string
	Fingerprint       string
	IdentityJSON      string
	SigningPolicyJSON string
	ProbeStatus       string
	ProbeRequired     bool
	ProbeSuccess      bool
	ArtifactPath      string
}

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
	// ProviderRunEnded settles one Provider run with its Session facts and
	// the redacted artifact body it produced (planning lifecycle).
	ProviderRunEnded EffectResultKind = "provider-run-ended"
	// ArtifactWritten reports the immutable reference of a persisted
	// Artifact Revision, echoing the written body for the Result Decision.
	ArtifactWritten EffectResultKind = "artifact-written"
	// PlanningWorktreeCreated reports that the Planning Snapshot Worktree
	// exists at the recorded Base Commit.
	PlanningWorktreeCreated EffectResultKind = "planning-worktree-created"
	// WorkflowCompiled reports the canonical Dynamic Workflow body the
	// Compiler produced from the approved Specs, Catalog, and Patch IR.
	WorkflowCompiled EffectResultKind = "workflow-compiled"
	// IntegrationWorktreeCreated reports the Integration Branch/Worktree
	// created at the recorded Base Commit after the Execution Approval.
	IntegrationWorktreeCreated EffectResultKind = "integration-worktree-created"
	// TaskWorktreeCreated reports the Task Branch/Worktree created from the
	// recorded Task Base (PRD Worktree 策略; Task 12).
	TaskWorktreeCreated EffectResultKind = "task-worktree-created"
	// VerificationRunEnded reports one deterministic Verification run with
	// its Evidence Manifest and classification (design 16.2; Task 13).
	VerificationRunEnded EffectResultKind = "verification-run-ended"
	// IntegrationMerged reports one serial --no-ff Integration merge with
	// the Merge Commit evidence (design 15.5; Task 13).
	IntegrationMerged EffectResultKind = "integration-merged"
	// IntegrationMergeFailed reports a failed Integration merge (text
	// conflict or a post-merge check failure) with the typed reason; the
	// Kernel requests the recorded Integration Rollback (design 15.5).
	IntegrationMergeFailed EffectResultKind = "integration-merge-failed"
	// IntegrationRollbacked reports that the managed Integration Worktree
	// was restored to the recorded pre-merge HEAD.
	IntegrationRollbacked EffectResultKind = "integration-rollbacked"
	// GitAuditRefCreated reports one created append-only audit Ref.
	GitAuditRefCreated EffectResultKind = "git-audit-ref-created"
)

// Valid reports whether k is a declared Effect Result Kind.
func (k EffectResultKind) Valid() bool {
	switch k {
	case AttemptEnded, ProcessStopped, ApplyStagingSucceeded,
		ApplyFastForwardSucceeded, ApplyFastForwardFailed,
		CleanupItemRemovedResult, CleanupItemFailedResult,
		ProviderRunEnded, ArtifactWritten, PlanningWorktreeCreated,
		WorkflowCompiled, IntegrationWorktreeCreated, TaskWorktreeCreated,
		VerificationRunEnded, IntegrationMerged, IntegrationMergeFailed,
		IntegrationRollbacked, GitAuditRefCreated:
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

	// Session is the settled Session fact of one Provider run (design
	// 14.3): the identity, the Provider Session ID, the purpose, and the
	// terminal status.
	Session Session
	// SuccessorSession is the automatic fallback successor Session of an
	// unrecoverable Resume (design 14.4): the successor carries
	// supersedes_session_id pointing at the LOST original and the
	// fallback Provider. The Kernel persists it in the same settle
	// Decision and binds the successor Attempt to it, so the lineage is
	// durable — never only in the pass's Runtime ledger.
	SuccessorSession Session
	// Artifact is the immutable reference produced by an ArtifactWrite
	// (design 10.1).
	Artifact ArtifactRef
	// Body is the redacted artifact body one Provider run produced (the
	// requirement turn, plan, or check output) or the written content an
	// ArtifactWrite echoes to its Result Decision.
	Body []byte
	// CatalogRef is the immutable reference of a new Verification Catalog
	// Revision the Runtime assembled from the Session's validated
	// proposals (zero when no proposal was accepted).
	CatalogRef ArtifactRef
	// RejectedOps lists the Compiler's inert Patch operations (skipped
	// without replacing the deterministic skeleton); the Kernel records
	// one Compile Finding per op, visible in Dry Run.
	RejectedOps []string
	// AppliedOps lists the Patch operations the Compiler applied to the
	// deterministic skeleton (route pins, concurrency caps, budget
	// tightenings). The Kernel records one non-blocking Finding per op so
	// the applied patch is durable and visible at the Execution Approval
	// gate (design 11: applied operations are compile evidence).
	AppliedOps []string
	// IntegrationHead is the HEAD of the created Integration Worktree
	// (the recorded Base Commit at approval).
	IntegrationHead string
	// WorktreePath is the created Task Worktree path of a
	// TaskWorktreeCreated result (PRD Worktree 策略; Task 12).
	WorktreePath string

	// Verification result facts (Task 13, design 16.2): Passed carries
	// the classification, Manifest the immutable Evidence Manifest, and
	// ManifestHash its self-hash.
	Passed       bool
	Manifest     []byte
	ManifestHash string
	// PreMergeHead is the recorded Integration HEAD before a failed merge
	// (the Rollback target, design 15.5).
	PreMergeHead string
	// Reason is the typed failure reason of an IntegrationMergeFailed
	// result ("conflict" or "post-merge-check").
	Reason string
	// EvidenceRefs is the full immutable evidence list an Attempt's end
	// records (the chain evidence: test-result, review-result, commit,
	// git snapshot); Evidence stays the primary reference.
	EvidenceRefs []EvidenceRef
	// Orphan reports that the stopped process was still alive with the
	// exact PID/start-token identity after the force-kill phase of the
	// controlled stop (PRD 已确认：Ctrl+C 两阶段有限停止 step 9): the
	// Workflow Blocks with ORPHAN_CHILD_PROCESS (or keeps a Cancel intent
	// with CANCEL_PENDING_ORPHAN_PROCESS) and Project mutation is
	// quarantined.
	Orphan bool
}

func (EffectResultInput) isInput() {}

// ReconcileInput triggers the Recovery sweep of the Kernel: it completes a
// persisted Cancel intent once everything is settled, converges a
// QUIESCING Run to BLOCKED, converges a STOPPING Run whose processes and
// Attempts have all settled, and blocks a Workflow that carries a FAILED
// Node with no in-flight Attempts. It never reopens dispatch.
type ReconcileInput struct{}

func (ReconcileInput) isInput() {}

// PolicyDriftSettleInput carries the post-Safety-Stop facts the Runtime
// observed after the two-phase stop settled (PRD 已确认：Commit Policy 漂移
// 立即安全停止 steps 6-7): the freshly observed Commit Preflight (nil when
// the new Preflight failed) and the scanned drift-window Commit facts of
// every active Task/Integration Worktree (Stop-request HEAD to final
// HEAD). With no window Commit the Kernel records the exact new Preflight
// and pauses the Workflow for the COMMIT_POLICY confirmation; with window
// Commits it records one immutable Branch Quarantine per window Commit,
// marks the contaminated Node FAILED, Blocks the Workflow, and requests
// the unique audit Refs.
type PolicyDriftSettleInput struct {
	// Preflight is the freshly observed Commit Preflight evidence (nil
	// when the new Preflight failed: the Workflow stays Blocked with
	// GIT_IDENTITY_NOT_CONFIGURED or GIT_SIGNING_PREFLIGHT_FAILED).
	Preflight *PreflightFacts
	// WindowCommits are the per-Branch Commit ranges created inside the
	// drift window.
	WindowCommits []WindowCommit
}

func (PolicyDriftSettleInput) isInput() {}

// WindowCommit is one scanned Branch Commit range of the drift window:
// the HEAD fixed at the Safety Stop request and the final HEAD after the
// stop, with the Branch and the owning Node when the Branch is a Task
// Branch.
type WindowCommit struct {
	Branch   string
	FromHead string
	ToHead   string
	Node     NodeID
}

// CommitPolicyApprovalInput is the append-only user decision binding the
// exact new Commit Policy Preflight Revision, hash, and fingerprint after
// a drift Safety Stop (PRD 已确认：执行期间 Commit Policy 漂移确认 step 4).
// Any reference change since the recorded Preflight is
// COMMIT_POLICY_INPUT_CHANGED with no mutation.
type CommitPolicyApprovalInput struct {
	PreflightRevision int
	PreflightHash     string
	Fingerprint       string
}

func (CommitPolicyApprovalInput) isInput() {}

// ReplacementApprovalInput is the user's unified Replacement Execution
// Approval (PRD 已确认：Replacement Execution Approval 吸收 Policy 确认):
// one append-only EXECUTION Approval binding the Quarantine Set, the
// superseded Execution Approval, every successor execution Artifact and
// policy hash, the current Preflight, and the fixed Reconciliation
// Manifest Revision/Hash. It absorbs the Commit Policy confirmation for
// the exact bound fingerprint, so no duplicate COMMIT_POLICY Approval is
// required.
// ReplacementPreviewInput carries the successor execution of a
// drift-window quarantine into the Kernel (PRD 已确认：漂移窗口 Commit 的隔
// 离与替代执行): the successor Spec/Workflow references, the current
// routing and budget references, the fresh Commit Preflight, the
// Quarantine ID set, the superseded Execution Approval, and the fixed
// Reconciliation Manifest Revision/Hash. The Kernel records the
// successor references and keeps the Workflow at the unified Replacement
// Execution Approval gate; the successor references are what the
// Replacement Execution Approval compares-and-swaps against.
type ReplacementPreviewInput struct {
	PlanHash             string
	SpecHashes           []string
	SpecRevision         int
	CatalogHash          string
	WorkflowHash         string
	WorkflowRevision     int
	RoutingHash          string
	BudgetHash           string
	Preflight            PreflightFacts
	QuarantineIDs        []string
	SupersededApprovalID string
	ManifestRevision     int
	ManifestHash         string
}

func (ReplacementPreviewInput) isInput() {}

type ReplacementApprovalInput struct {
	// The exact successor execution references (compare-and-swap against
	// the active ExecutionFacts and the newly recorded Preflight).
	PlanHash          string
	SpecHashes        []string
	CatalogHash       string
	WorkflowHash      string
	RoutingHash       string
	BudgetHash        string
	PreflightRevision int
	PreflightHash     string
	Fingerprint       string
	// QuarantineIDs is the exact Quarantine ID set the approval contains.
	QuarantineIDs []string
	// SupersededApprovalID is the Execution Approval the replacement
	// supersedes (approval-<n>).
	SupersededApprovalID string
	// ManifestRevision and ManifestHash fix the persisted Reconciliation
	// Manifest the approval binds.
	ManifestRevision int
	ManifestHash     string
}

func (ReplacementApprovalInput) isInput() {}

// ManifestActionKind is one per-Node action of the Reconciliation
// Manifest (design 15.6, PRD 已确认：未污染兄弟 Task 增量恢复).
type ManifestActionKind string

const (
	// ManifestReuseSucceeded: a SUCCEEDED Node whose evidence is intact
	// and whose definition/dependencies match; never re-executed.
	ManifestReuseSucceeded ManifestActionKind = "reuse_succeeded"
	// ManifestResumeInterrupted: an interrupted Node whose facts still
	// match; resumes with a successor Attempt on the same Branch/Worktree.
	ManifestResumeInterrupted ManifestActionKind = "resume_interrupted"
	// ManifestReplaceContaminated: the Node owns a quarantined Branch or
	// failed within the drift window; replaced on a new Branch/Worktree.
	ManifestReplaceContaminated ManifestActionKind = "replace_contaminated"
	// ManifestRerunVerification: a Verify Node of a replaced Task must
	// re-run against the new path.
	ManifestRerunVerification ManifestActionKind = "rerun_verification"
)

// Valid reports whether k is a declared Manifest action.
func (k ManifestActionKind) Valid() bool {
	switch k {
	case ManifestReuseSucceeded, ManifestResumeInterrupted, ManifestReplaceContaminated, ManifestRerunVerification:
		return true
	}
	return false
}

// String renders the Manifest action.
func (k ManifestActionKind) String() string { return string(k) }

// ManifestAction is one Node's classification with its reason.
type ManifestAction struct {
	Node   NodeID
	Action ManifestActionKind
	Reason string
}

// ReconciliationManifest is the immutable per-Workflow classification of
// every Node after a Policy Safety Stop (design 15.6): the Runtime
// recomputes it from Git, Attempt, Session, and evidence facts — never
// from an Agent claim. The Replacement Execution Approval binds its
// Revision and Hash; Recovery recomputes and compares.
type ReconciliationManifest struct {
	Revision int
	Hash     string
	Actions  []ManifestAction
}

// GraphInstallInput installs the execution graph of the approved compiled
// Dynamic Workflow (design 12). The Node definitions arrive from the
// approved Workflow Artifact (the Runtime parses it; the Kernel records
// the DAG PENDING). Dependencies are the deterministic skeleton edges; the
// Scheduler computes readiness from them plus the persisted Node status,
// never from Task display status.
type GraphInstallInput struct {
	Nodes []InstallNode
	// Replacement installs the successor graph of an approved Replacement
	// Execution Approval: existing Node identities whose kind and
	// dependencies are unchanged stay (the persisted state is reused),
	// only the new Nodes are appended (PRD 已确认：未污染兄弟 Task 增量恢复
	// step 3).
	Replacement bool
}

func (GraphInstallInput) isInput() {}

// InstallNode is one Node of the approved Dynamic Workflow: its kind, its
// skeleton dependencies, and the approved Retry Budget.
type InstallNode struct {
	ID           NodeID
	Kind         NodeKind
	Dependencies []NodeID
	RetryBudget  int
}

// DispatchInput is one serialized allocation (design 12): the Application
// computed the eligible Node with the pure Scheduler, and the Kernel
// revalidates the readiness facts against the committed aggregate in the
// same transaction that commits the RUNNING Attempt. A queued goroutine is
// never an in-flight Attempt: only the committed row is (PRD 已确认：并行
// 失败后的 Quiescing). Session is the Application-allocated Session
// identity, Route the approved route of the Task's Spec, and BaseHead the
// current verified Integration HEAD observed by the Runtime at readiness
// (the immutable Task Base the Task Branch and Worktree are created from,
// PRD Worktree 策略); all three are fixed before any Effect (design 6.2
// rule 6).
type DispatchInput struct {
	Node     NodeID
	Session  SessionID
	Route    string
	BaseHead string
	// Process is the Application-allocated managed Process identity of the
	// Node's chain (design 13.3): the Kernel records the ProcessRecord as
	// RUNNING with the chain's Session, so the controlled stop ledger
	// covers every active Provider run.
	Process ProcessID
}

func (DispatchInput) isInput() {}

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
