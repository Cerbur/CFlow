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

func (ListQuery) isQuery()      {}
func (StatusQuery) isQuery()    {}
func (InspectQuery) isQuery()   {}
func (LogsQuery) isQuery()      {}
func (DiscoveryQuery) isQuery() {}
func (PlanQuery) isQuery()      {}

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

func (ListView) isView()      {}
func (StatusView) isView()    {}
func (InspectView) isView()   {}
func (LogsView) isView()      {}
func (DiscoveryView) isView() {}
func (PlanView) isView()      {}

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

func (CreateWorkflowCommand) isCommand()     {}
func (DiscussRequirementCommand) isCommand() {}
func (GeneratePlanCommand) isCommand()       {}
func (CheckPlanCommand) isCommand()          {}
func (ApprovePlanCommand) isCommand()        {}
func (StartWorkflowCommand) isCommand()      {}
func (PauseWorkflowCommand) isCommand()      {}
func (ResumeWorkflowCommand) isCommand()     {}
func (CancelWorkflowCommand) isCommand()     {}
func (DryRunCommand) isCommand()             {}

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
