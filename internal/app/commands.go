package app

// The closed Query/Command/View/Outcome unions (design 5): typed command
// data only, no stringly typed registry and no extension hook.

import (
	"context"
	"path/filepath"
	"time"

	"cflow.local/cflow/internal/agent"
	"cflow.local/cflow/internal/gitflow"
	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/observe"
	"cflow.local/cflow/internal/process"
	"cflow.local/cflow/internal/security"
)

// Options configures one Application.
type Options struct {
	// Home is the CFLOW_HOME root (design 20.1).
	Home string
	// Project is the local project identity the Application serves.
	Project Project
	// CflowVersion is recorded in the store's migration bookkeeping.
	CflowVersion string
	// Redaction is the embedded redaction rule registry every export path
	// uses (design 19.2); a zero registry passes text through.
	Redaction security.Registry
	// Supervisor owns managed process lifecycle (design 13).
	Supervisor process.Supervisor
	// Recoverer is the Recovery-before-mutation hook (design 17). nil uses
	// the Task 7 default (home posture and schema compatibility).
	Recoverer Recoverer
	// Now is the injected clock for store bookkeeping.
	Now func() time.Time
	// IDs is the injected opaque ID source (design 22.2).
	IDs model.IDSource
	// GitFlow is the Git seam (design 15): project discovery and the
	// Worktree primitives. nil fails every Git-dependent command closed.
	GitFlow *gitflow.GitFlow
	// Prompts is the immutable Prompt Registry (design 14.5). nil loads
	// the embedded registry on demand.
	Prompts *agent.PromptRegistry
	// Agent is the Agent Runtime configuration the Application constructs
	// per command execution (design 14): the Provider Registry, the
	// redaction policy, the evidence root, and the Provider Adapters. A
	// zero Registry disables Provider effects (fail closed).
	Agent agent.RuntimeOptions
	// StopPolicy overrides the staged budgets of the two-phase controlled
	// stop (design 13.3). nil uses the PRD 10s + 2s + 2s budget; tests
	// inject tiny values.
	StopPolicy *StopPolicy
	// PolicyPollInterval is the Commit Policy monitor's recompute period
	// (PRD 已确认：Commit Policy 漂移立即安全停止 step 5: no slower than once
	// per second while a commit-capable managed process is active). 0 uses
	// the one-second default; tests inject tiny values.
	PolicyPollInterval time.Duration
}

// Project is the local project identity one Application serves: the
// canonical repository root and the stable derived key used for lock
// names, directories, and database rows (design 7.2).
type Project struct {
	Key  string
	Root string
}

// ProjectFor derives the deterministic project identity from the
// repository root with the PRD 启动与项目识别 rules: the readable slug of
// the canonical path plus a short content hash (gitflow.ProjectKey). Full
// project discovery (git-root resolution) happens through the GitFlow
// seam; this constructor canonicalizes the given root.
func ProjectFor(root string) Project {
	clean := filepath.Clean(root)
	return Project{Key: gitflow.ProjectKey(clean), Root: clean}
}

// ---------------------------------------------------------------------------
// Query union (design 5): closed read projections
// ---------------------------------------------------------------------------

// Query is the closed union of read projections.
type Query interface{ isQuery() }

// ListQuery enumerates the project's workflows.
type ListQuery struct{}

// StatusQuery summarizes one workflow, or the project's single workflow
// when empty.
type StatusQuery struct{ Workflow model.WorkflowID }

// InspectQuery renders the full aggregate of one workflow.
type InspectQuery struct{ Workflow model.WorkflowID }

// LogsQuery returns the redacted authoritative Event window.
type LogsQuery struct {
	Workflow model.WorkflowID
	From     uint64
	Limit    int
}

// DiscoveryQuery observes the project's Git facts (PRD 启动与项目识别):
// the canonical root, the attached local branch, HEAD, the dirty
// classification, and the Project Key. It is a git-only projection; no
// database lock is taken.
type DiscoveryQuery struct{}

// PlanQuery projects the active Plan Revision's review state.
type PlanQuery struct{ Workflow model.WorkflowID }

// ExecutionPreviewQuery projects the full Execution Approval preview
// (PRD 已确认：两个用户批准门 step 2): the exact Plan/Spec/Catalog/
// Workflow Revisions and hashes, routes, budgets, the Commit Preflight
// fingerprint, the trust boundary, the Worktree plan, the parallel
// groups, and the command identities.
type ExecutionPreviewQuery struct{ Workflow model.WorkflowID }

// PolicyConfirmationQuery projects the pending Commit Policy
// confirmation gate after a drift Safety Stop (PRD 已确认：执行期间 Commit
// Policy 漂移确认 step 3): the exact new Preflight Revision/Hash/
// Fingerprint and the old/new normalized diff, rendered side by side.
type PolicyConfirmationQuery struct{ Workflow model.WorkflowID }

// CancelSummaryQuery projects the cancel confirmation summary (PRD 已确
// 认：Cancel 逻辑终止 step 1): the Workflow ID, current Stage, active
// Sessions and Nodes, every managed Worktree/Branch, the dirty state,
// the unmerged Commits, and the preserved paths.
type CancelSummaryQuery struct{ Workflow model.WorkflowID }

// ReplacementPreviewQuery projects the unified Replacement Execution
// Approval gate (PRD 已确认：Replacement Execution Approval 吸收 Policy 确
// 认 step 1): the Quarantine Findings/Branches/Audit Refs, the old and
// new execution Revisions, the Replacement baseline, the routing/budget
// references, the old/new Commit Policy diff, the current Preflight, and
// the fixed Reconciliation Manifest with its per-Node categories.
type ReplacementPreviewQuery struct{ Workflow model.WorkflowID }

// ReportQuery projects the immutable Final Execution Report read model
// (Task 18, design 21, PRD 最终报告示例): a read over approved hashes,
// Git facts, Sessions, Attempts, Findings, verification manifests,
// migration compatibility, security posture, permissions, and Apply
// state. Report generation never changes Workflow state. Build is the
// binary identity the report's Runtime section records.
type ReportQuery struct {
	Workflow model.WorkflowID
	Build    observe.BuildInfo
}

func (ListQuery) isQuery()               {}
func (StatusQuery) isQuery()             {}
func (InspectQuery) isQuery()            {}
func (LogsQuery) isQuery()               {}
func (DiscoveryQuery) isQuery()          {}
func (PlanQuery) isQuery()               {}
func (ExecutionPreviewQuery) isQuery()   {}
func (PolicyConfirmationQuery) isQuery() {}
func (CancelSummaryQuery) isQuery()      {}
func (ReplacementPreviewQuery) isQuery() {}
func (ReportQuery) isQuery()             {}

// ---------------------------------------------------------------------------
// View union: projection results
// ---------------------------------------------------------------------------

// View is the closed union of read projection results. Views carry only
// bounded, redacted fields and immutable references (design 21).
type View interface{ isView() }

// WorkflowSummary is one list row.
type WorkflowSummary struct {
	ID           model.WorkflowID
	Stage        model.WorkflowStage
	Runtime      model.RuntimeStatus
	TargetBranch string
	BaseCommit   string
}

// ListView is the list projection.
type ListView struct{ Workflows []WorkflowSummary }

// StatusView is the status projection.
type StatusView struct {
	Workflow          model.WorkflowID
	Stage             model.WorkflowStage
	Runtime           model.RuntimeStatus
	TargetBranch      string
	BaseCommit        string
	IntegrationBranch string
	IntegrationHead   string
	Findings          []model.Finding
	Run               *model.Run
	Processes         []model.ProcessRecord
	// PlanStatus is the active Plan Revision's review status ("" when no
	// Plan exists). PlanApproved is true only for the user's append-only
	// Approval; a checker pass never sets it.
	PlanStatus   model.PlanStatus
	PlanApproved bool
	PlanRevision int
	PlanHash     string
}

// DiscoveryView is the project discovery projection (PRD 启动与项目识别).
type DiscoveryView struct {
	Root             string
	Branch           string
	Head             string
	Unborn           bool
	Detached         bool
	Dirty            bool
	DirtyFingerprint string
	ProjectKey       string
	StagedCount      int
	UnstagedCount    int
	UntrackedCount   int
}

// PlanView is the active Plan Revision's review projection.
type PlanView struct {
	Workflow   model.WorkflowID
	Stage      model.WorkflowStage
	Runtime    model.RuntimeStatus
	PlanStatus model.PlanStatus
	Revision   int
	Hash       string
	Approved   bool
}

// ExecutionPreviewView is the Execution Approval preview projection.
type ExecutionPreviewView struct {
	Workflow model.WorkflowID
	Stage    model.WorkflowStage
	Runtime  model.RuntimeStatus

	Plan             *model.ArtifactRef
	Spec             *model.ArtifactRef
	Catalog          *model.ArtifactRef
	WorkflowArtifact *model.ArtifactRef
	Preflight        *PreflightPreview

	// The exact hashes the Execution Approval binds (the Dry Run's
	// compare-and-swap inputs).
	PlanHash         string
	SpecHashes       []string
	CatalogHash      string
	WorkflowHash     string
	RoutingHash      string
	BudgetHash       string
	CommitPolicyHash string

	Routes            []RoutePreview
	Budgets           []BudgetPreview
	TotalAgentRuns    int
	TotalRetries      int
	ParallelGroups    [][]string
	Locks             []LockPreview
	CommandIdentities []CommandIdentity
	WorktreePlan      []string
	TrustBoundary     string
	Findings          []model.Finding
}

// PreflightPreview is the Commit Preflight summary of the Dry Run.
type PreflightPreview struct {
	Revision     int
	EvidenceHash string
	GitVersion   string
	Fingerprint  string
	ProbeStatus  string
}

// PolicyConfirmationView is the pending Commit Policy confirmation gate:
// the exact new Preflight Revision/Hash/Fingerprint plus the old
// fingerprint the drift moved away from.
type PolicyConfirmationView struct {
	Workflow          model.WorkflowID
	Stage             model.WorkflowStage
	Runtime           model.RuntimeStatus
	PreflightRevision int
	PreflightHash     string
	Fingerprint       string
	OldFingerprint    string
	Pending           bool
}

// CancelSummaryView is the cancel confirmation summary: the Workflow ID,
// Stage, active Sessions and Nodes, every managed Worktree/Branch with
// its dirty state and unmerged Commits, and the preserved paths.
type CancelSummaryView struct {
	Workflow        model.WorkflowID
	Stage           model.WorkflowStage
	Runtime         model.RuntimeStatus
	ActiveNodes     []model.NodeID
	ActiveSessions  []model.SessionID
	Worktrees       []CancelWorktree
	Preserved       []string
	UnmergedCommits int
}

// CancelWorktree is one managed Worktree of the cancel summary.
type CancelWorktree struct {
	Path     string
	Branch   string
	Dirty    bool
	Unmerged bool
}

// ReportView is the rendered Final Execution Report: the read model plus
// its redacted Markdown rendering (PRD 最终报告示例).
type ReportView struct {
	Report   observe.Report
	Markdown string
}

// ReplacementPreviewView is the unified Replacement Execution Approval
// gate: the Quarantine set with its audit Refs, the old and new
// execution Revisions, the Replacement baseline, the routing/budget
// references, the old/new Commit Policy diff, the current Preflight, and
// the fixed Reconciliation Manifest with its per-Node categories.
type ReplacementPreviewView struct {
	Workflow model.WorkflowID
	Stage    model.WorkflowStage
	Runtime  model.RuntimeStatus

	Quarantines  []model.Quarantine
	OldRevision  int
	NewRevision  int
	BaselineHead string

	RoutingHash string
	BudgetHash  string

	OldFingerprint string
	NewFingerprint string
	Preflight      *PreflightPreview

	Manifest             model.ReconciliationManifest
	SupersededApprovalID string
	SpecHashes           []string
	PlanHash             string
	CatalogHash          string
	WorkflowHash         string
}

// RoutePreview is one task's approved route.
type RoutePreview struct {
	NodeID   string
	Provider string
	Model    string
}

// BudgetPreview is one task's approved budgets.
type BudgetPreview struct {
	NodeID         string
	TimeoutSeconds int
	MaxRetry       int
	Budget         float64
}

// LockPreview is one injected Resource Lock assignment.
type LockPreview struct {
	NodeID string
	Lock   string
}

// CommandIdentity is one Catalog entry's pinned identity.
type CommandIdentity struct {
	CommandID  string
	Executable string
	SHA256     string
	Purpose    string
}

// InspectView is the full aggregate projection.
type InspectView struct {
	Status          StatusView
	Plan            *model.Plan
	Nodes           []model.Node
	Attempts        []model.Attempt
	Approvals       []model.Approval
	Sessions        []model.Session
	Runs            []model.Run
	Quarantines     []model.Quarantine
	ApplyAttempts   []model.ApplyAttempt
	CleanupAttempts []model.CleanupAttempt
	PendingEffects  []string
}

// LogsView is the Event window projection.
type LogsView struct {
	Events       []model.Event
	NextEventSeq uint64
}

func (ListView) isView()               {}
func (StatusView) isView()             {}
func (InspectView) isView()            {}
func (LogsView) isView()               {}
func (DiscoveryView) isView()          {}
func (PlanView) isView()               {}
func (ExecutionPreviewView) isView()   {}
func (PolicyConfirmationView) isView() {}
func (CancelSummaryView) isView()      {}
func (ReplacementPreviewView) isView() {}
func (ReportView) isView()             {}

// ---------------------------------------------------------------------------
// Command union (design 5, 6.1): closed mutations
// ---------------------------------------------------------------------------

// Command is the closed union of mutation commands.
type Command interface{ isCommand() }

// CreateWorkflowCommand establishes a workflow for the project's Git
// repository. The Application observes the canonical root, the attached
// local Target Branch, and HEAD through GitFlow (PRD 启动与项目识别) and
// refuses creation on a non-Git root, an unborn repository, or a
// Detached HEAD. ConfirmDirty confirms the user's dirty workspace is
// isolated: it never enters the Planning Snapshot or any managed
// Worktree. The opaque workflow identity is generated by the Application
// (design 6.2 rule 6).
type CreateWorkflowCommand struct {
	Name         string
	Provider     string
	ConfirmDirty bool
}

// DiscussRequirementCommand submits one requirement turn (PRD 需求讨论
// 交互). Text is the user's message; Provider names the discussion
// Agent route.
type DiscussRequirementCommand struct {
	Workflow model.WorkflowID
	Text     string
	Provider string
}

// GeneratePlanCommand is the /finish transition: the planner produces a
// new immutable Plan Revision from the requirement discussion lineage
// (PRD Plan 生成).
type GeneratePlanCommand struct {
	Workflow model.WorkflowID
	Provider string
}

// CheckPlanCommand runs an independent plan-check Session over the
// active DRAFT Plan Revision (PRD Plan Check 交互). The Checker is never
// the Planner's Session.
type CheckPlanCommand struct {
	Workflow model.WorkflowID
	Provider string
}

// ApprovePlanCommand is the user's append-only decision binding one
// exact checked Plan Revision and hash (PRD 已确认：两个用户批准门). Any
// mismatch with the active Plan is APPROVAL_INPUT_CHANGED with no
// mutation.
type ApprovePlanCommand struct {
	Workflow model.WorkflowID
	Revision int
	Hash     string
}

// StartWorkflowCommand starts the first Run.
type StartWorkflowCommand struct{ Workflow model.WorkflowID }

// PauseWorkflowCommand applies the controlled-stop pause: dispatch closes
// and every managed process is stopped (design 13.3).
type PauseWorkflowCommand struct{ Workflow model.WorkflowID }

// ResumeWorkflowCommand reopens dispatch with a new Run record.
type ResumeWorkflowCommand struct{ Workflow model.WorkflowID }

// CancelWorkflowCommand persists the cancel intent and completes the
// recoverable cancellation protocol (design 17.4).
type CancelWorkflowCommand struct {
	Workflow model.WorkflowID
	Reason   string
}

// DryRunCommand produces the immutable Cleanup Dry Run Manifest (design
// 17.4). Items carry the freshly observed candidate facts.
type DryRunCommand struct {
	Workflow model.WorkflowID
	Items    []model.CleanupItem
}

// GenerateSpecsCommand runs the Spec Generation Session (PRD Agent 角色:
// SPEC_GENERATION 将 Plan 拆成 Specs). The Runtime first discovers the
// Verification Catalog candidates from the fixed Base Commit and writes
// the immutable Catalog Revision; the Session output is judged by the
// Kernel and written as the Spec Revision.
type GenerateSpecsCommand struct {
	Workflow model.WorkflowID
	Provider string
}

// CompileWorkflowCommand runs the Workflow Optimization Session (PRD
// Agent 角色: WORKFLOW_OPTIMIZATION): the independent scheduling Agent
// proposes a restricted Patch IR, the Compiler validates it against the
// deterministic skeleton, and the canonical Dynamic Workflow Revision is
// written.
type CompileWorkflowCommand struct {
	Workflow model.WorkflowID
	Provider string
}

// ExecutionDryRunCommand runs the Git Commit Preflight, records the
// immutable Preflight Revision, and pauses the Workflow at the Execution
// Approval gate (PRD 已确认：两个用户批准门 step 2).
type ExecutionDryRunCommand struct {
	Workflow model.WorkflowID
}

// ApproveExecutionCommand is the user's append-only Execution Approval
// binding one exact set of execution Artifact hashes, routing, budgets,
// and the Commit Preflight hash. Any reference change since the
// displayed preview is APPROVAL_INPUT_CHANGED with no mutation; only a
// match requests the Integration Worktree creation.
type ApproveExecutionCommand struct {
	Workflow         model.WorkflowID
	PlanHash         string
	SpecHashes       []string
	CatalogHash      string
	WorkflowHash     string
	RoutingHash      string
	BudgetHash       string
	CommitPolicyHash string
}

// DispatchCommand runs one allocation pass of the approved execution
// (design 12): the pure Scheduler computes the eligible Nodes from the
// persisted graph and the Dispatch Gate; every RUNNING Attempt commits
// before its Effects (Task Worktree creation, coding Session) submit, and
// the Kernel revalidates the gate in the same transaction so no start can
// cross a committed closure (Pause, Quiesce, Cancel, Safety Stop).
type DispatchCommand struct {
	Workflow model.WorkflowID
}

// ReconcileCommand runs the Recovery sweep of the Kernel: it completes a
// persisted Cancel intent, converges a QUIESCING or STOPPING Run whose
// Attempts and processes settled, and Blocks a Workflow that carries a
// FAILED Node with nothing in flight. It never reopens dispatch (design
// 17: Recovery of a Stop, Cancel, Quiesce, or Safety Stop only finishes
// the persisted protocol).
type ReconcileCommand struct {
	Workflow model.WorkflowID
}

// CommitPolicyConfirmCommand is the user's append-only COMMIT_POLICY
// Approval binding the exact new Preflight Revision, hash, and
// fingerprint after a drift Safety Stop (PRD 已确认：执行期间 Commit Policy
// 漂移确认 step 4). The CLI shows the old/new diff before asking.
type CommitPolicyConfirmCommand struct {
	Workflow          model.WorkflowID
	PreflightRevision int
	PreflightHash     string
	Fingerprint       string
}

// ReplacementPreviewCommand generates the successor execution of a
// drift-window quarantine (PRD 已确认：漂移窗口 Commit 的隔离与替代执行): the
// Repair Specs (new Spec IDs with replaces_task_id), the successor
// Dynamic Workflow Revision, the fixed Reconciliation Manifest, and the
// fresh Commit Preflight. It pauses the Workflow at the unified
// Replacement Execution Approval gate; nothing dispatches until the user
// approves the exact preview.
type ReplacementPreviewCommand struct {
	Workflow model.WorkflowID
}

// ApproveReplacementCommand is the user's unified Replacement Execution
// Approval (PRD 已确认：Replacement Execution Approval 吸收 Policy 确认):
// one append-only EXECUTION Approval binding the Quarantine Set, the
// superseded approval, every successor reference, the current Preflight,
// and the fixed Reconciliation Manifest Revision/Hash.
type ApproveReplacementCommand struct {
	Workflow model.WorkflowID

	PlanHash             string
	SpecHashes           []string
	CatalogHash          string
	WorkflowHash         string
	RoutingHash          string
	BudgetHash           string
	PreflightRevision    int
	PreflightHash        string
	Fingerprint          string
	QuarantineIDs        []string
	SupersededApprovalID string
	ManifestRevision     int
	ManifestHash         string
}

// CompleteWorkflowCommand records the Workflow's completion (Task 18,
// PRD 最终验收): the Kernel validates the exact Integration Commit
// evidence (every Node SUCCEEDED, no Blocking Finding, the Integration
// HEAD still the head the independent Final Reviewer verified) and
// records COMPLETED without changing the Target Branch. The Application
// then writes the immutable Final Report Artifact (a rebuildable read
// model; report generation never changes Workflow state).
type CompleteWorkflowCommand struct {
	Workflow model.WorkflowID
}

// RetryCommand drives one dispatch pass for a workflow whose named Task
// carries a READY successor Attempt (PRD 必须提供的 CLI: `cflow retry
// <task-id>`). The command validates the Task exists before any dispatch
// and otherwise runs the ordinary dispatch pass.
type RetryCommand struct {
	Workflow model.WorkflowID
	Node     model.NodeID
}

func (CreateWorkflowCommand) isCommand()      {}
func (DiscussRequirementCommand) isCommand()  {}
func (GeneratePlanCommand) isCommand()        {}
func (CheckPlanCommand) isCommand()           {}
func (ApprovePlanCommand) isCommand()         {}
func (StartWorkflowCommand) isCommand()       {}
func (PauseWorkflowCommand) isCommand()       {}
func (ResumeWorkflowCommand) isCommand()      {}
func (CancelWorkflowCommand) isCommand()      {}
func (DryRunCommand) isCommand()              {}
func (GenerateSpecsCommand) isCommand()       {}
func (CompileWorkflowCommand) isCommand()     {}
func (ExecutionDryRunCommand) isCommand()     {}
func (ApproveExecutionCommand) isCommand()    {}
func (DispatchCommand) isCommand()            {}
func (ReconcileCommand) isCommand()           {}
func (CommitPolicyConfirmCommand) isCommand() {}
func (ReplacementPreviewCommand) isCommand()  {}
func (ApproveReplacementCommand) isCommand()  {}
func (CompleteWorkflowCommand) isCommand()    {}
func (RetryCommand) isCommand()               {}

// ---------------------------------------------------------------------------
// Outcome
// ---------------------------------------------------------------------------

// Outcome is the renderable result of one executed Command: the
// committed authoritative Events and Findings, the resulting lifecycle
// facts, and the immutable references the command produced (design 20).
type Outcome struct {
	Workflow   model.WorkflowID
	Stage      model.WorkflowStage
	Runtime    model.RuntimeStatus
	Restricted bool // the restricted safety path was used (design 6.1)
	Events     []model.Event
	Findings   []model.Finding
	Cleanup    *model.CleanupAttempt
	// SessionID is the Session identity a planning command created (""
	// for commands without one).
	SessionID model.SessionID
	// ExportErr reports a failed events.jsonl export. The export is a
	// rebuildable audit file, never the recovery stream (design 21); the
	// mutation itself is unaffected.
	ExportErr error
}

// ---------------------------------------------------------------------------
// Recovery hook seam (design 17)
// ---------------------------------------------------------------------------

// Recoverer is the Recovery-before-mutation hook. The Application calls
// Reconcile before every mutation, under the shared DB Schema Lock. Task
// 13 delivers the full Recovery Engine; the Task 7 implementation checks
// home security posture and store/schema compatibility.
type Recoverer interface {
	Reconcile(ctx context.Context) error
}
