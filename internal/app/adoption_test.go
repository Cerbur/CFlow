package app

// Workspace Adoption Gate Application tests (TUI task 6, design 8.4): an
// Execution Approval bound to a frozen Change Set may not schedule normal
// Tasks until the Workspace was adopted; the adoption re-verifies the
// Change Set, Commit Policy, Identity/Signing, Clean/Scope, Catalog
// Verification, and an independent Review, and only then advances
// verified_workspace_head to the exact candidate Head. Failures preserve
// the Workspace, the Change Set, and the Target Branch.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"cflow.local/cflow/internal/model"
)

// freezeAndDriveToGate freezes the candidate Change Set of one workflow
// after a discussion turn, drives the execution lifecycle to the paused
// Execution Approval gate, and returns the preview bound to the frozen
// Change Set.
func freezeAndDriveToGate(t *testing.T, fx *planningFixture, wf model.WorkflowID) (ExecutionPreviewView, string) {
	t.Helper()
	out, err := fx.app(discussionScript("d1", "division by zero must error")).Execute(context.Background(),
		DiscussRequirementCommand{Workflow: wf, Text: "division by zero must error", Provider: "fake"})
	if err != nil {
		t.Fatalf("discuss: %v", err)
	}
	freeze, err := fx.app().Execute(context.Background(),
		FreezeDiscussionCommand{Workflow: wf, Session: out.SessionID})
	if err != nil {
		t.Fatalf("freeze: %v", err)
	}
	changeSetHash := freeze.ChangeSet.Ref.Hash
	if changeSetHash == "" {
		t.Fatal("freeze outcome carried no change set hash")
	}
	fx.discussSeq++
	if _, err := fx.app(planScript("p1", validPlan())).Execute(context.Background(),
		GeneratePlanCommand{Workflow: wf, Provider: "fake"}); err != nil {
		t.Fatal(err)
	}
	fx.checkSeq++
	if _, err := fx.app(checkScript("c1", "pass")).Execute(context.Background(),
		CheckPlanCommand{Workflow: wf, Provider: "fake"}); err != nil {
		t.Fatal(err)
	}
	approveCheckedPlan(t, fx, wf)
	pv := driveToExecutionGate(t, fx, wf)
	if pv.ChangeSetHash != changeSetHash {
		t.Fatalf("preview change set hash = %q, want the frozen %q", pv.ChangeSetHash, changeSetHash)
	}
	return pv, changeSetHash
}

// requireNoTaskWorktrees asserts no temporary Task worktree exists for the
// workflow (the adoption gate created nothing).
func requireNoTaskWorktrees(t *testing.T, fx *planningFixture, wf model.WorkflowID) {
	t.Helper()
	dir := filepath.Join(fx.home, "projects", ProjectFor(fx.root).Key, string(wf), "tmp", "tasks")
	if pathExists(dir) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read task dir: %v", err)
		}
		if len(entries) > 0 {
			t.Fatalf("task worktrees were created before the adoption: %v", entries)
		}
	}
}

// TestDispatchWaitsForWorkspaceAdoption is the TUI task 6 failure test: an
// Execution Approval bound to a frozen Change Set opens no Task scheduling
// until the Workspace was adopted (design 8.4: any automatic Task must be
// created from verified_workspace_head).
func TestDispatchWaitsForWorkspaceAdoption(t *testing.T) {
	fx := newExecutionFixture(t)
	wf, err := fx.create("add divide", false)
	if err != nil {
		t.Fatal(err)
	}
	pv, _ := freezeAndDriveToGate(t, fx, wf)
	approveExecution(t, fx, wf, pv)

	_, err = fx.app().Execute(context.Background(), DispatchCommand{Workflow: wf})
	requireFaultCode(t, err, model.CodeWorkspaceAdoptionRequired)
	requireNoTaskWorktrees(t, fx, wf)
}

// TestAdoptWorkspaceAdvancesVerifiedHead drives the adoption PASS path:
// after the independent Adoption Review PASSes, verified_workspace_head
// advances to the exact candidate Head and dispatch may schedule Tasks.
func TestAdoptWorkspaceAdvancesVerifiedHead(t *testing.T) {
	fx := newExecutionFixture(t)
	wf, err := fx.create("add divide", false)
	if err != nil {
		t.Fatal(err)
	}
	pv, _ := freezeAndDriveToGate(t, fx, wf)
	approveExecution(t, fx, wf, pv)

	if _, err := fx.app().Execute(context.Background(), DispatchCommand{Workflow: wf}); err == nil {
		t.Fatal("dispatch without adoption succeeded")
	}

	// The Adoption Review PASS advances the verified head.
	ws := fx.workspacePath(wf)
	candidate := gitOut(t, ws, "rev-parse", "HEAD")
	if _, err := fx.app(reviewPassScript()).Execute(context.Background(),
		AdoptWorkspaceCommand{Workflow: wf}); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	st := fx.status(wf)
	if st.VerifiedWorkspaceHead != candidate {
		t.Fatalf("verified workspace head = %q, want the candidate %q", st.VerifiedWorkspaceHead, candidate)
	}
	if st.CandidateWorkspaceHead != candidate {
		t.Fatalf("candidate workspace head = %q, want %q", st.CandidateWorkspaceHead, candidate)
	}
	if st.WorkspaceDirtyFingerprint == "" {
		t.Fatal("the adopted workspace recorded no dirty fingerprint")
	}

	// Dispatch now schedules: the Task Base is the verified workspace head.
	a := fx.app(implementationScript("i1"))
	fx.probe = &callProbe{}
	a.probe = fx.probe
	if _, err := a.Execute(context.Background(), DispatchCommand{Workflow: wf}); err != nil {
		t.Fatalf("dispatch after adoption: %v", err)
	}
}

// TestAdoptWorkspaceRejectsDirtyWorkspace covers the native-session
// uncommitted case: an uncommitted Workspace can never be adopted; the
// failure preserves the Workspace and the Target Branch.
func TestAdoptWorkspaceRejectsDirtyWorkspace(t *testing.T) {
	fx := newExecutionFixture(t)
	wf, err := fx.create("add divide", false)
	if err != nil {
		t.Fatal(err)
	}
	pv, _ := freezeAndDriveToGate(t, fx, wf)
	approveExecution(t, fx, wf, pv)

	// The native session left an uncommitted file in the Workspace after
	// the freeze (the approval-bound Change Set no longer matches).
	ws := fx.workspacePath(wf)
	if err := os.WriteFile(filepath.Join(ws, "wip.txt"), []byte("wip"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err = fx.app(reviewPassScript()).Execute(context.Background(),
		AdoptWorkspaceCommand{Workflow: wf})
	requireFaultCode(t, err, model.CodeEvidenceSubjectChanged)

	// The Workspace is preserved (the drift is still there), the verified
	// head is empty, and the Target Branch never moved.
	if !pathExists(filepath.Join(ws, "wip.txt")) {
		t.Fatal("the workspace drift was discarded by the failed adoption")
	}
	if st := fx.status(wf); st.VerifiedWorkspaceHead != "" {
		t.Fatalf("verified head was set despite the drift: %+v", st)
	}
	requireFaultCode(t, mustDispatchAdopt(t, fx, wf), model.CodeWorkspaceAdoptionRequired)
}

// TestAdoptWorkspaceRejectsOutOfScopeChange covers the out-of-scope case:
// a candidate Change Set outside the approved Spec write scope can never
// be adopted; the failure preserves the Workspace and the Target Branch.
func TestAdoptWorkspaceRejectsOutOfScopeChange(t *testing.T) {
	fx := newExecutionFixture(t)
	wf, err := fx.create("add divide", false)
	if err != nil {
		t.Fatal(err)
	}
	out, err := fx.app(discussionScript("d1", "division by zero must error")).Execute(context.Background(),
		DiscussRequirementCommand{Workflow: wf, Text: "division by zero must error", Provider: "fake"})
	if err != nil {
		t.Fatal(err)
	}
	// The native session commits an out-of-scope file before the freeze.
	ws := fx.workspacePath(wf)
	evil := filepath.Join(ws, "etc", "config.txt")
	if err := os.MkdirAll(filepath.Dir(evil), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(evil, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitAt(t, ws, "add", "etc/config.txt")
	gitAt(t, ws, "commit", "-q", "-m", "out of scope")
	freeze, err := fx.app().Execute(context.Background(),
		FreezeDiscussionCommand{Workflow: wf, Session: out.SessionID})
	if err != nil {
		t.Fatalf("freeze: %v", err)
	}
	if freeze.ChangeSet == nil {
		t.Fatal("freeze outcome carried no change set")
	}
	fx.discussSeq++
	if _, err := fx.app(planScript("p1", validPlan())).Execute(context.Background(),
		GeneratePlanCommand{Workflow: wf, Provider: "fake"}); err != nil {
		t.Fatal(err)
	}
	fx.checkSeq++
	if _, err := fx.app(checkScript("c1", "pass")).Execute(context.Background(),
		CheckPlanCommand{Workflow: wf, Provider: "fake"}); err != nil {
		t.Fatal(err)
	}
	approveCheckedPlan(t, fx, wf)
	pv := driveToExecutionGate(t, fx, wf)
	approveExecution(t, fx, wf, pv)

	_, err = fx.app(reviewPassScript()).Execute(context.Background(),
		AdoptWorkspaceCommand{Workflow: wf})
	requireFaultCode(t, err, model.CodeScopeViolation)
	if st := fx.status(wf); st.VerifiedWorkspaceHead != "" {
		t.Fatalf("verified head was set despite the scope violation: %+v", st)
	}
}

// reviewFailScript is the deterministic TASK_REVIEW Session output: a
// structured FAIL verdict.
func reviewFailScript() string {
	return `{"fixture":"fake-run","script_version":1,"provider":"fake","dialect":"cflow.dialect.fake.v1","purpose":"review","session_id":"r2","exit_code":0,"resume":"ok"}
{"type":"session_started","session_id":"r2","at_ms":0}
{"type":"assistant_message","session_id":"r2","text":"Reviewed the change set.","at_ms":10}
{"type":"session_finished","session_id":"r2","result":{"decision":"FAIL","report":"FAIL\n\nFindings:\n- unacceptable\n"},"at_ms":20}`
}

// TestAdoptWorkspaceReviewRejectBlocks covers the Review Reject case: a
// FAIL Adoption Review Blocks the Workflow and preserves the Workspace,
// the Change Set, and the Target Branch.
func TestAdoptWorkspaceReviewRejectBlocks(t *testing.T) {
	fx := newExecutionFixture(t)
	wf, err := fx.create("add divide", false)
	if err != nil {
		t.Fatal(err)
	}
	pv, _ := freezeAndDriveToGate(t, fx, wf)
	approveExecution(t, fx, wf, pv)

	if _, err := fx.app(reviewFailScript()).Execute(context.Background(), AdoptWorkspaceCommand{Workflow: wf}); err != nil {
		t.Fatalf("adopt with failing review: %v", err)
	}
	// The FAIL verdict Blocks the Workflow with a blocking finding.
	st := fx.status(wf)
	if st.VerifiedWorkspaceHead != "" {
		t.Fatalf("verified head was set despite the rejected review: %+v", st)
	}
	blocked := false
	for _, f := range st.Findings {
		if f.Blocking {
			blocked = true
		}
	}
	if !blocked {
		t.Fatalf("rejected adoption left no blocking finding: %+v", st.Findings)
	}
	if st.Runtime != model.RuntimeBlocked {
		t.Fatalf("rejected adoption left the workflow %s, want BLOCKED", st.Runtime)
	}
}

// TestAdoptWorkspaceRejectsPostApprovalDrift covers the post-approval
// drift case: a Workspace that moved after the Execution Approval (beyond
// the frozen Change Set) can never be adopted; the failure preserves the
// Workspace and the Target Branch.
func TestAdoptWorkspaceRejectsPostApprovalDrift(t *testing.T) {
	fx := newExecutionFixture(t)
	wf, err := fx.create("add divide", false)
	if err != nil {
		t.Fatal(err)
	}
	pv, _ := freezeAndDriveToGate(t, fx, wf)
	approveExecution(t, fx, wf, pv)

	// The native session commits after the approval: the frozen Change
	// Set's candidate facts no longer match.
	ws := fx.workspacePath(wf)
	if err := os.WriteFile(filepath.Join(ws, "late.txt"), []byte("late"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitAt(t, ws, "add", "late.txt")
	gitAt(t, ws, "commit", "-q", "-m", "post approval")

	_, err = fx.app(reviewPassScript()).Execute(context.Background(),
		AdoptWorkspaceCommand{Workflow: wf})
	requireFaultCode(t, err, model.CodeEvidenceSubjectChanged)
	if st := fx.status(wf); st.VerifiedWorkspaceHead != "" {
		t.Fatalf("verified head was set despite the post-approval drift: %+v", st)
	}
}

// mustDispatchAdopt runs one DispatchCommand and returns its error.
func mustDispatchAdopt(t *testing.T, fx *planningFixture, wf model.WorkflowID) error {
	t.Helper()
	_, err := fx.app().Execute(context.Background(), DispatchCommand{Workflow: wf})
	return err
}
