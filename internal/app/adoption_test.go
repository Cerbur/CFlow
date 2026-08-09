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

// TestAdoptWorkspaceOutOfScopeDirtyBlocks covers the out-of-scope native
// uncommitted case (Task 4, design 8.4 step 3): the managed adoption
// Session commits the dirty change, the re-frozen Change Set then exposes
// the out-of-scope path to the Scope gate, and the gate Blocks the Workflow
// while preserving the Workspace, the Change Set, and the Target Branch.
func TestAdoptWorkspaceOutOfScopeDirtyBlocks(t *testing.T) {
	fx := newExecutionFixture(t)
	wf, err := fx.create("add divide", false)
	if err != nil {
		t.Fatal(err)
	}
	pv, _ := freezeAndDriveToGate(t, fx, wf)
	approveExecution(t, fx, wf, pv)

	// A native session leaves an out-of-scope uncommitted file in the
	// Workspace after the freeze.
	ws := fx.workspacePath(wf)
	if err := os.WriteFile(filepath.Join(ws, "wip.txt"), []byte("wip"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := fx.app(adoptionCommitScript()).Execute(context.Background(),
		AdoptWorkspaceCommand{Workflow: wf}); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	st := fx.status(wf)
	if st.VerifiedWorkspaceHead != "" {
		t.Fatalf("verified head was set despite the scope violation: %+v", st)
	}
	if st.Runtime != model.RuntimeBlocked {
		t.Fatalf("out-of-scope adoption left the workflow %s, want BLOCKED", st.Runtime)
	}
	if !pathExists(filepath.Join(ws, "wip.txt")) {
		t.Fatal("the workspace drift was discarded by the failed adoption")
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

// adoptionCommitScript is the deterministic managed adoption Session output
// (Task 4, design 8.4 step 2): the adoption/coding Session runs inside the
// Workspace and creates the real implementation Commit that organizes the
// dirty native changes (`commits` makes the Fake Provider run git add -A and
// git commit in its working directory — CFlow itself never does).
func adoptionCommitScript() string {
	return `{"fixture":"fake-run","script_version":1,"provider":"fake","dialect":"cflow.dialect.fake.v1","purpose":"adoption","session_id":"ad1","exit_code":0,"resume":"ok","commits":["adopt native changes"]}
{"type":"session_started","session_id":"ad1","at_ms":0}
{"type":"assistant_message","session_id":"ad1","text":"Organizing and committing the native workspace changes.","at_ms":10}
{"type":"session_finished","session_id":"ad1","result":{"summary":"adopted"},"at_ms":20}`
}

// adoptionNoopScript is the deterministic managed adoption Session output
// that settles WITHOUT creating any Commit: the Workspace stays dirty at the
// same HEAD, so the adoption evidence (no new commit, dirty fingerprint)
// must Block the Workflow.
func adoptionNoopScript() string {
	return `{"fixture":"fake-run","script_version":1,"provider":"fake","dialect":"cflow.dialect.fake.v1","purpose":"adoption","session_id":"ad2","exit_code":0,"resume":"ok"}
{"type":"session_started","session_id":"ad2","at_ms":0}
{"type":"assistant_message","session_id":"ad2","text":"Nothing to commit.","at_ms":10}
{"type":"session_finished","session_id":"ad2","result":{"summary":"no commit"},"at_ms":20}`
}

// freezeAndDriveDirtyToGate drives the execution lifecycle for a Workspace
// whose native session left an uncommitted change BEFORE the freeze: the
// frozen Change Set captures the dirty candidate facts, the Execution
// Approval binds the frozen hash, and the Workspace stays dirty at the
// adoption gate.
func freezeAndDriveDirtyToGate(t *testing.T, fx *planningFixture, wf model.WorkflowID) string {
	t.Helper()
	out, err := fx.app(discussionScript("d1", "division by zero must error")).Execute(context.Background(),
		DiscussRequirementCommand{Workflow: wf, Text: "division by zero must error", Provider: "fake"})
	if err != nil {
		t.Fatalf("discuss: %v", err)
	}
	ws := fx.workspacePath(wf)
	if err := os.MkdirAll(filepath.Join(ws, "src", "divide"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "src", "divide", "divide.go"),
		[]byte("package divide\n\n// Divide returns a/b.\nfunc Divide(a, b int) (int, error) {\n\treturn a / b, nil\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	freeze, err := fx.app().Execute(context.Background(),
		FreezeDiscussionCommand{Workflow: wf, Session: out.SessionID})
	if err != nil {
		t.Fatalf("freeze: %v", err)
	}
	if freeze.ChangeSet == nil || !freeze.ChangeSet.Dirty {
		t.Fatalf("freeze captured no dirty candidate: %+v", freeze.ChangeSet)
	}
	changeSetHash := freeze.ChangeSet.Ref.Hash
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
	approveExecution(t, fx, wf, pv)
	return changeSetHash
}

// TestAdoptWorkspaceAdoptsDirtyNativeChanges is the Task 4 adoption PASS
// test (design 8.4 step 2): an uncommitted native Workspace is not rejected;
// the Adoption flow starts a managed adoption/coding Session that organizes
// and commits the native changes, then the gate chain (Change Set
// re-observation, Commit Policy, Clean/Scope, Catalog, independent Review)
// runs against the NEW candidate Head, and verified_workspace_head advances
// to the exact post-adoption HEAD.
func TestAdoptWorkspaceAdoptsDirtyNativeChanges(t *testing.T) {
	fx := newExecutionFixture(t)
	wf, err := fx.create("add divide", false)
	if err != nil {
		t.Fatal(err)
	}
	freezeAndDriveDirtyToGate(t, fx, wf)
	ws := fx.workspacePath(wf)
	preHead := gitOut(t, ws, "rev-parse", "HEAD")

	// Dispatch still waits: the candidate Head is unverified.
	_, err = fx.app().Execute(context.Background(), DispatchCommand{Workflow: wf})
	requireFaultCode(t, err, model.CodeWorkspaceAdoptionRequired)

	// The managed adoption Session commits the native changes; the gate
	// chain passes and the verified head advances to the post-adoption HEAD.
	if _, err := fx.app(adoptionCommitScript(), reviewPassScript()).Execute(context.Background(),
		AdoptWorkspaceCommand{Workflow: wf}); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	postHead := gitOut(t, ws, "rev-parse", "HEAD")
	if postHead == preHead {
		t.Fatal("the adoption session did not advance the workspace head")
	}
	st := fx.status(wf)
	if st.VerifiedWorkspaceHead != postHead {
		t.Fatalf("verified workspace head = %q, want the post-adoption head %q", st.VerifiedWorkspaceHead, postHead)
	}
	if st.CandidateWorkspaceHead != postHead {
		t.Fatalf("candidate workspace head = %q, want %q", st.CandidateWorkspaceHead, postHead)
	}
	// The verified workspace fingerprint is the clean-state fingerprint at
	// the adopted Head (the existing convention records the deterministic
	// clean fingerprint, never an empty string).
	if st.WorkspaceDirtyFingerprint == "" {
		t.Fatalf("the adopted workspace recorded no dirty fingerprint")
	}
	if !pathExists(filepath.Join(ws, "src", "divide", "divide.go")) {
		t.Fatal("the native change was lost by the adoption")
	}
	if out := gitOut(t, ws, "status", "--porcelain"); out != "" {
		t.Fatalf("the workspace is not clean after the adoption:\n%s", out)
	}

	// Dispatch now schedules from the verified workspace head.
	a := fx.app(implementationScript("i1"))
	fx.probe = &callProbe{}
	a.probe = fx.probe
	if _, err := a.Execute(context.Background(), DispatchCommand{Workflow: wf}); err != nil {
		t.Fatalf("dispatch after adoption: %v", err)
	}
}

// TestAdoptWorkspaceAdoptionFailureBlocks covers the adoption-session
// failure case (Task 4, design 8.4 step 7): the managed adoption Session
// settles without creating any Commit, so the evidence (a new Commit must
// exist, the Workspace must be clean, the candidate HEAD must advance)
// Blocks the Workflow and preserves the Workspace, the Change Set, and the
// Target Branch.
func TestAdoptWorkspaceAdoptionFailureBlocks(t *testing.T) {
	fx := newExecutionFixture(t)
	wf, err := fx.create("add divide", false)
	if err != nil {
		t.Fatal(err)
	}
	freezeAndDriveDirtyToGate(t, fx, wf)
	ws := fx.workspacePath(wf)
	preHead := gitOut(t, ws, "rev-parse", "HEAD")

	if _, err := fx.app(adoptionNoopScript()).Execute(context.Background(),
		AdoptWorkspaceCommand{Workflow: wf}); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if out := gitOut(t, ws, "rev-parse", "HEAD"); out != preHead {
		t.Fatalf("the workspace head moved despite the failed adoption: %s -> %s", preHead, out)
	}
	st := fx.status(wf)
	if st.VerifiedWorkspaceHead != "" {
		t.Fatalf("verified head was set despite the failed adoption: %+v", st)
	}
	if st.Runtime != model.RuntimeBlocked {
		t.Fatalf("failed adoption left the workflow %s, want BLOCKED", st.Runtime)
	}
	blocked := false
	for _, f := range st.Findings {
		if f.Blocking {
			blocked = true
		}
	}
	if !blocked {
		t.Fatalf("failed adoption left no blocking finding: %+v", st.Findings)
	}
	if !pathExists(filepath.Join(ws, "src", "divide", "divide.go")) {
		t.Fatal("the workspace was not preserved by the failed adoption")
	}
	if out := gitOut(t, ws, "status", "--porcelain"); out == "" {
		t.Fatal("the native change was discarded by the failed adoption")
	}
}
