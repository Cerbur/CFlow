package app

// Foreground Drive tests (TUI task 13): DriveOnce performs one safe
// forward step and returns the typed outcome; a fresh workflow drives to
// a user decision (the plan approval gate), and a completed workflow is
// terminal.

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"cflow.local/cflow/internal/model"
)

// TestDriveOnceFreshWorkflowNeedsUser: a fresh workflow at the
// requirement-discussion stage cannot auto-dispatch; the driver reports
// a user decision is needed (discussion), never a fabricated step.
func TestDriveOnceFreshWorkflowNeedsUser(t *testing.T) {
	fx := newPlanningFixture(t)
	wf, err := fx.create("drive-demo", false)
	if err != nil {
		t.Fatal(err)
	}
	a := fx.app()
	out, err := a.DriveOnce(context.Background(), wf)
	if err != nil {
		t.Fatal(err)
	}
	if out.Kind != DriveNeedsUser {
		t.Fatalf("drive outcome = %+v, want a user decision", out)
	}
	if out.Reason != string(model.StageRequirementDiscussion) {
		t.Fatalf("reason = %q, want the requirement discussion stage", out.Reason)
	}
}

// TestDriveOnceUnknownWorkflowFails: an unknown workflow fails closed.
func TestDriveOnceUnknownWorkflowFails(t *testing.T) {
	fx := newPlanningFixture(t)
	a := fx.app()
	if _, err := a.DriveOnce(context.Background(), "missing"); err == nil {
		t.Fatal("DriveOnce on an unknown workflow succeeded")
	}
}

// TestDriveOnceReconcilesBlockedWorkflow: a workflow with a blocking
// finding reports the user decision.
func TestDriveOnceBlockedWorkflowNeedsUser(t *testing.T) {
	fx := newPlanningFixture(t)
	wf, err := fx.create("drive-blocked", false)
	if err != nil {
		t.Fatal(err)
	}
	a := fx.app()
	// Block the workflow through a blocking finding (raw SQL update:
	// the aggregate blocks on a blocking finding even without events).
	db, err := sql.Open("sqlite", filepath.Join(fx.home, "cflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE workflows SET runtime_status = 'BLOCKED' WHERE id = ?`, string(wf)); err != nil {
		t.Fatal(err)
	}
	out, err := a.DriveOnce(context.Background(), wf)
	if err != nil {
		t.Fatal(err)
	}
	if out.Kind != DriveNeedsUser {
		t.Fatalf("blocked drive outcome = %+v, want needs-user", out)
	}
}
