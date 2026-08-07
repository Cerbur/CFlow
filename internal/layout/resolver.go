// Package layout implements the aggregated workflow directory resolver
// (TUI workflow design §7): every CFlow-managed path under
// <home>/projects/<project-key>/<workflow-id>/ is constructed here and
// nowhere else. Code, schemas, CLI text, and tests must use these typed
// entry points instead of composing paths themselves.
package layout

import (
	"fmt"
	"path/filepath"
	"strings"

	"cflow.local/cflow/internal/model"
)

// Resolver builds aggregated workflow paths under one CFLOW_HOME for one
// project. Home must be absolute and owner-only managed (the caller
// validates it through the Security Guard); ProjectKey and WorkflowID must
// be safe single path components.
type Resolver struct {
	Home       string
	ProjectKey string
}

// Validate checks that Home is absolute and clean and that ProjectKey is a
// safe single path component. It fails closed on traversal, separators, or
// empty values.
func (r Resolver) Validate() error {
	if r.Home == "" || !filepath.IsAbs(r.Home) || filepath.Clean(r.Home) != r.Home {
		return model.InvalidInputFault("layout home must be an absolute clean path")
	}
	if !validComponent(r.ProjectKey) {
		return model.InvalidInputFault("project key is not a safe managed path component")
	}
	return nil
}

// ValidateWorkflowID rejects an ID that is not a safe single path
// component (empty, `.`, `..`, or carrying separators or NUL).
func (r Resolver) ValidateWorkflowID(wf model.WorkflowID) error {
	if !validComponent(string(wf)) {
		return model.InvalidInputFault("workflow id is not a safe managed path component")
	}
	return nil
}

// WorkflowRoot returns the aggregated root of one workflow:
// <home>/projects/<project-key>/<workflow-id>.
func (r Resolver) WorkflowRoot(wf model.WorkflowID) string {
	return filepath.Join(r.Home, "projects", r.ProjectKey, string(wf))
}

// Workspace returns the unique long-lived code mainline of one workflow
// (TUI workflow design §8): <root>/workspace.
func (r Resolver) Workspace(wf model.WorkflowID) string {
	return filepath.Join(r.WorkflowRoot(wf), "workspace")
}

// Task returns the temporary parallel task worktree root of one task
// (design §8.5): <root>/tmp/tasks/<node-id>.
func (r Resolver) Task(wf model.WorkflowID, node model.NodeID) string {
	return filepath.Join(r.WorkflowRoot(wf), "tmp", "tasks", string(node))
}

// Apply returns the staging worktree root of the n-th apply attempt
// (design §13.1): <root>/tmp/apply-<n>.
func (r Resolver) Apply(wf model.WorkflowID, n int) string {
	return filepath.Join(r.WorkflowRoot(wf), "tmp", fmt.Sprintf("apply-%d", n))
}

// DiscussionDir is the non-code home of discussion turns and handoffs.
func (r Resolver) DiscussionDir(wf model.WorkflowID) string {
	return filepath.Join(r.WorkflowRoot(wf), "discussion")
}

// PlansDir is the non-code home of Plan artifacts.
func (r Resolver) PlansDir(wf model.WorkflowID) string {
	return filepath.Join(r.WorkflowRoot(wf), "plans")
}

// SpecsDir is the non-code home of Spec artifacts.
func (r Resolver) SpecsDir(wf model.WorkflowID) string {
	return filepath.Join(r.WorkflowRoot(wf), "specs")
}

// WorkflowsDir is the non-code home of Dynamic Workflow / DAG artifacts.
func (r Resolver) WorkflowsDir(wf model.WorkflowID) string {
	return filepath.Join(r.WorkflowRoot(wf), "workflows")
}

// ReviewsDir is the non-code home of review evidence.
func (r Resolver) ReviewsDir(wf model.WorkflowID) string {
	return filepath.Join(r.WorkflowRoot(wf), "reviews")
}

// SessionsDir is the non-code home of session lineage records.
func (r Resolver) SessionsDir(wf model.WorkflowID) string {
	return filepath.Join(r.WorkflowRoot(wf), "sessions")
}

// EvidenceDir is the non-code home of evidence manifests.
func (r Resolver) EvidenceDir(wf model.WorkflowID) string {
	return filepath.Join(r.WorkflowRoot(wf), "evidence")
}

// LogsDir is the non-code home of workflow logs.
func (r Resolver) LogsDir(wf model.WorkflowID) string {
	return filepath.Join(r.WorkflowRoot(wf), "logs")
}

// ReportsDir is the non-code home of workflow reports.
func (r Resolver) ReportsDir(wf model.WorkflowID) string {
	return filepath.Join(r.WorkflowRoot(wf), "reports")
}

// StateDir is the home of rebuildable projections, manifests, and
// recovery-aid state. It never forms a second authority beside SQLite
// (design §7 rule 2).
func (r Resolver) StateDir(wf model.WorkflowID) string {
	return filepath.Join(r.WorkflowRoot(wf), "state")
}

// validComponent reports whether s is a safe single path component:
// non-empty, not `.`/`..`, and free of separators and NUL.
func validComponent(s string) bool {
	return s != "" && s != "." && s != ".." && !strings.ContainsAny(s, "/\\\x00")
}
