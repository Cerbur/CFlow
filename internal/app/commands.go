package app

// The closed Query/Command/View/Outcome unions (design 5): typed command
// data only, no stringly typed registry and no extension hook.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"
	"time"

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
}

// Project is the local project identity one Application serves: the
// canonical repository root and the stable derived key used for lock
// names, directories, and database rows (design 7.2).
type Project struct {
	Key  string
	Root string
}

// ProjectFor derives the deterministic project identity from the
// repository root: a readable slug of the canonical path plus a short
// content hash (PRD 全局目录结构). Full project discovery (git-root
// resolution and approved slugs) arrives with Task 8; this constructor is
// its Task 7 stand-in.
func ProjectFor(root string) Project {
	clean := filepath.Clean(root)
	slug := strings.TrimPrefix(clean, string(filepath.Separator))
	slug = strings.ReplaceAll(slug, string(filepath.Separator), "-")
	sum := sha256.Sum256([]byte(clean))
	return Project{Key: slug + "--" + hex.EncodeToString(sum[:4]), Root: clean}
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

func (ListQuery) isQuery()    {}
func (StatusQuery) isQuery()  {}
func (InspectQuery) isQuery() {}
func (LogsQuery) isQuery()    {}

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

func (ListView) isView()    {}
func (StatusView) isView()  {}
func (InspectView) isView() {}
func (LogsView) isView()    {}

// ---------------------------------------------------------------------------
// Command union (design 5, 6.1): closed mutations
// ---------------------------------------------------------------------------

// Command is the closed union of mutation commands.
type Command interface{ isCommand() }

// CreateWorkflowCommand establishes a workflow with the user branch and
// base commit fixed at creation. The opaque workflow identity is
// generated by the Application (design 6.2 rule 6).
type CreateWorkflowCommand struct {
	TargetBranch string
	BaseCommit   string
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

func (CreateWorkflowCommand) isCommand() {}
func (StartWorkflowCommand) isCommand()  {}
func (PauseWorkflowCommand) isCommand()  {}
func (ResumeWorkflowCommand) isCommand() {}
func (CancelWorkflowCommand) isCommand() {}
func (DryRunCommand) isCommand()         {}

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
