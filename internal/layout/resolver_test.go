// Package layout defines the aggregated workflow directory resolver
// (TUI workflow design §7): the single construction point for every
// CFlow-managed path under <home>/projects/<project-key>/<workflow-id>/.
// No other package may compose these paths itself.
package layout_test

import (
	"path/filepath"
	"testing"

	"cflow.local/cflow/internal/layout"
	"cflow.local/cflow/internal/model"
)

func TestResolverAggregatesWorkflowFiles(t *testing.T) {
	r := layout.Resolver{Home: "/home/u/.cflow", ProjectKey: "project-a"}
	wf := model.WorkflowID("wf-1")
	want := "/home/u/.cflow/projects/project-a/wf-1"
	if got := r.WorkflowRoot(wf); got != want {
		t.Fatalf("root=%q want=%q", got, want)
	}
	if got := r.Workspace(wf); got != want+"/workspace" {
		t.Fatalf("workspace=%q", got)
	}
	if got := r.Task(wf, "S01"); got != want+"/tmp/tasks/S01" {
		t.Fatalf("task=%q", got)
	}
	if got := r.Apply(wf, 2); got != want+"/tmp/apply-2" {
		t.Fatalf("apply=%q", got)
	}
}

func TestResolverAggregatedTypeDirs(t *testing.T) {
	r := layout.Resolver{Home: "/home/u/.cflow", ProjectKey: "project-a"}
	wf := model.WorkflowID("wf-1")
	want := "/home/u/.cflow/projects/project-a/wf-1"
	for dir, name := range map[string]string{
		r.DiscussionDir(wf): "discussion",
		r.PlansDir(wf):      "plans",
		r.SpecsDir(wf):      "specs",
		r.WorkflowsDir(wf):  "workflows",
		r.ReviewsDir(wf):    "reviews",
		r.SessionsDir(wf):   "sessions",
		r.EvidenceDir(wf):   "evidence",
		r.LogsDir(wf):       "logs",
		r.ReportsDir(wf):    "reports",
		r.StateDir(wf):      "state",
	} {
		if got := filepath.Join(want, name); dir != got {
			t.Fatalf("dir=%q want=%q", dir, got)
		}
	}
}

func TestResolverRejectsUnsafeIdentifiers(t *testing.T) {
	if err := (layout.Resolver{}).Validate(); err == nil {
		t.Fatal("expected rejection of empty resolver")
	}
	if err := (layout.Resolver{Home: "/home/u/.cflow"}).Validate(); err == nil {
		t.Fatal("expected rejection of empty project key")
	}
	for _, proj := range []string{"a/b", "../escape", "..", ".", "a\x00b"} {
		r := layout.Resolver{Home: "/home/u/.cflow", ProjectKey: proj}
		if err := r.Validate(); err == nil {
			t.Fatalf("expected rejection of unsafe project key %q", proj)
		}
	}
	r := layout.Resolver{Home: "/home/u/.cflow", ProjectKey: "project-a"}
	for _, wf := range []model.WorkflowID{"", "a/b", "..", ".", "wf\x00id"} {
		if err := r.ValidateWorkflowID(wf); err == nil {
			t.Fatalf("expected rejection of unsafe workflow id %q", wf)
		}
	}
	if err := r.ValidateWorkflowID("wf-1"); err != nil {
		t.Fatalf("valid workflow id rejected: %v", err)
	}
}

func TestResolverRejectsRelativeOrUnsafeHome(t *testing.T) {
	for _, home := range []string{"", "relative/.cflow", "..", "a\x00b"} {
		r := layout.Resolver{Home: home, ProjectKey: "p"}
		if err := r.Validate(); err == nil {
			t.Fatalf("expected rejection of unsafe home %q", home)
		}
	}
}
