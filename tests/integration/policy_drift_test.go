// Package integration: the Commit Policy Safety Stop end to end (Task 17,
// PRD 已确认：Commit Policy 漂移立即安全停止 / 漂移窗口 Commit 的隔离与替代
// 执行). The fixture drives the real pipeline: while a coding Session is
// active, the monitor re-observes the fingerprint probe-less (the
// injected period) and a drifted Git configuration triggers the Safety
// Stop — the gate closes, every Attempt settles INTERRUPTED without
// charge, and the post-stop scan either pauses the Workflow at the
// COMMIT_POLICY_CONFIRMATION_REQUIRED gate (no window Commit) or
// quarantines the window-Commit Branch with its unique audit Ref and
// Blocks the Workflow (window Commit).
package integration

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"cflow.local/cflow/internal/agent"
	"cflow.local/cflow/internal/agent/fake"
	"cflow.local/cflow/internal/app"
	"cflow.local/cflow/internal/gitflow"
	"cflow.local/cflow/internal/model"
	"cflow.local/cflow/internal/security"
)

// appWithPolicy builds the fixture Application with an injected Commit
// Policy monitor period and stop policy (the parallel fixture's app
// mirror).
func (fx *parallelFixture) appWithPolicy(interval time.Duration, scripts ...string) *app.Application {
	fx.t.Helper()
	reg, err := agent.LoadProviderRegistry()
	if err != nil {
		fx.t.Fatalf("provider registry: %v", err)
	}
	prompts, err := agent.LoadPromptRegistry()
	if err != nil {
		fx.t.Fatalf("prompt registry: %v", err)
	}
	ad := fake.New(reg)
	for _, s := range scripts {
		if err := ad.LoadScript([]byte(s)); err != nil {
			fx.t.Fatalf("load fake script: %v", err)
		}
	}
	flow, err := gitflow.NewGitFlow(fx.sup, fx.repo.Root)
	if err != nil {
		fx.t.Fatalf("new gitflow: %v", err)
	}
	a, err := app.New(app.Options{
		Home:               fx.home,
		Project:            app.ProjectFor(fx.repo.Root),
		CflowVersion:       "0.0.0-dev",
		Now:                fx.now,
		IDs:                fx.ids,
		Supervisor:         fx.sup,
		GitFlow:            flow,
		Prompts:            prompts,
		PolicyPollInterval: interval,
		Agent: agent.RuntimeOptions{
			Registry:    reg,
			Redaction:   security.Registry{},
			Adapters:    map[string]agent.Adapter{"fake": ad},
			EvidenceDir: filepath.Join(fx.home, "evidence"),
		},
	})
	if err != nil {
		fx.t.Fatalf("new application: %v", err)
	}
	return a
}

// driftPolicy changes the effective Commit Policy fingerprint of the
// fixture repository (the monitor observes it probe-less).
func (fx *parallelFixture) driftPolicy(t *testing.T) {
	t.Helper()
	fx.repo.git("config", "user.email", "drifted@example.com")
}

// TestPolicyDriftNoWindowCommitConfirmation drives the monitor: a drifted
// Commit Policy while a coding Session runs triggers the Safety Stop, the
// post-stop scan finds no window Commit (the fixture script writes
// without committing), and the Workflow pauses at the
// COMMIT_POLICY_CONFIRMATION_REQUIRED gate — resume is refused until the
// append-only COMMIT_POLICY Approval binds the exact new Preflight (PRD
// 已确认：执行期间 Commit Policy 漂移确认).
func TestPolicyDriftNoWindowCommitConfirmation(t *testing.T) {
	fx := newParallelFixture(t)
	wf, _ := fx.driveToExecutionApproval(t)
	fx.driftPolicy(t)
	// The session blocks deterministically at the declared stop boundary
	// (the Fake's stop_after) until the Safety Stop cancels it: the
	// monitor's recompute always observes the drift while the Session is
	// live.
	a := fx.appWithPolicy(5*time.Millisecond, stopAfterImplementationScript("i1", 2))
	if _, err := a.Execute(context.Background(), app.DispatchCommand{Workflow: wf}); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	iv := fx.Inspect(wf)
	if iv.Status.Runtime != model.RuntimePaused {
		t.Fatalf("runtime = %s, want PAUSED at the confirmation gate", iv.Status.Runtime)
	}
	finding := false
	for _, f := range iv.Status.Findings {
		if f.Code == model.CodeCommitPolicyConfirmationRequired {
			finding = true
		}
	}
	if !finding {
		t.Fatalf("the COMMIT_POLICY_CONFIRMATION_REQUIRED finding must be persisted")
	}
	charged := false
	for _, att := range iv.Attempts {
		if att.RetryCharged {
			charged = true
		}
	}
	if charged {
		t.Fatalf("a policy safety stop must never charge the retry budget")
	}
	// Resume is refused while the confirmation is pending.
	_, err := a.Execute(context.Background(), app.ResumeWorkflowCommand{Workflow: wf})
	if code, ok := model.CodeOf(err); !ok || code != model.CodeCommitPolicyConfirmationRequired {
		t.Fatalf("resume before the confirmation must be refused, got %v", err)
	}
	// The confirmation gate shows the exact new Preflight; the append-only
	// COMMIT_POLICY Approval binds it and unblocks resume.
	qview, err := a.Query(context.Background(), app.PolicyConfirmationQuery{Workflow: wf})
	if err != nil {
		t.Fatalf("confirmation query: %v", err)
	}
	pv := qview.(app.PolicyConfirmationView)
	if !pv.Pending || pv.PreflightRevision < 1 || pv.Fingerprint == "" || pv.PreflightHash == "" {
		t.Fatalf("confirmation view = %+v, want the pending gate", pv)
	}
	if _, err := a.Execute(context.Background(), app.CommitPolicyConfirmCommand{
		Workflow: wf, PreflightRevision: pv.PreflightRevision,
		PreflightHash: pv.PreflightHash, Fingerprint: pv.Fingerprint,
	}); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if _, err := a.Execute(context.Background(), app.ResumeWorkflowCommand{Workflow: wf}); err != nil {
		t.Fatalf("resume after the confirmation: %v", err)
	}
	iv = fx.Inspect(wf)
	if iv.Status.Runtime != model.RuntimeRunning {
		t.Fatalf("runtime = %s, want RUNNING after the confirmation and resume", iv.Status.Runtime)
	}
}

// stopAfterImplementationScript is the implementation fixture with the
// Fake's deterministic stop boundary (stop_after): the run blocks at the
// declared frame boundary until the Safety Stop cancels its context, so
// the monitor always observes the drift while the Session is live.
func stopAfterImplementationScript(sessionID string, stopAfter int) string {
	return fmt.Sprintf(`{"fixture":"fake-run","script_version":1,"provider":"fake","dialect":"cflow.dialect.fake.v1","purpose":"implementation","session_id":%s,"exit_code":0,"resume":"ok","stop_after":%d,"writes":[{"path":"src/calc/divide.go","content":"package calc\n\n// Divide returns a/b.\nfunc Divide(a, b int) (int, error) {\n\treturn 0, nil\n}\n"}]}
{"type":"session_started","session_id":%s,"at_ms":0}
{"type":"assistant_message","session_id":%s,"text":"Implemented the calculator task.","at_ms":10}
{"type":"session_finished","session_id":%s,"result":{"summary":"implemented"},"at_ms":20}`,
		strconv.Quote(sessionID), stopAfter, strconv.Quote(sessionID), strconv.Quote(sessionID), strconv.Quote(sessionID))
}

// TestPolicyDriftRedispatchAfterSettleDoesNotFalselyQuarantine asserts
// the settle/monitor lifecycle: after the confirmation gate settled and
// the exact new policy was confirmed and resumed, a second dispatch pass
// must run normally — the first pass's monitor goroutines must die before
// the post-stop settle (never re-arming the drift snapshot with post-stop
// pre-heads), so the successor Session's legal Commits are never falsely
// quarantined and no duplicate COMMIT_POLICY gate re-opens.
func TestPolicyDriftRedispatchAfterSettleDoesNotFalselyQuarantine(t *testing.T) {
	fx := newParallelFixture(t)
	wf, _ := fx.driveToExecutionApproval(t)
	fx.driftPolicy(t)
	// Pass 1: the monitor detects the drift (the Session blocks at the
	// stop boundary) and the Safety Stop settles the confirmation gate.
	a := fx.appWithPolicy(5*time.Millisecond, stopAfterImplementationScript("i1", 2))
	if _, err := a.Execute(context.Background(), app.DispatchCommand{Workflow: wf}); err != nil {
		t.Fatalf("dispatch 1: %v", err)
	}
	iv := fx.Inspect(wf)
	if iv.Status.Runtime != model.RuntimePaused {
		t.Fatalf("runtime = %s, want PAUSED at the confirmation gate", iv.Status.Runtime)
	}
	// The exact new policy is confirmed and the Workflow resumes.
	qview, err := a.Query(context.Background(), app.PolicyConfirmationQuery{Workflow: wf})
	if err != nil {
		t.Fatalf("confirmation query: %v", err)
	}
	pv := qview.(app.PolicyConfirmationView)
	if !pv.Pending {
		t.Fatalf("no pending confirmation after the safety stop")
	}
	if _, err := a.Execute(context.Background(), app.CommitPolicyConfirmCommand{
		Workflow: wf, PreflightRevision: pv.PreflightRevision,
		PreflightHash: pv.PreflightHash, Fingerprint: pv.Fingerprint,
	}); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if _, err := a.Execute(context.Background(), app.ResumeWorkflowCommand{Workflow: wf}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	// Pass 2: the successor Session commits legally (the committing
	// fixture). The stale-monitor bug would settle with pre-stop pre-heads
	// and falsely quarantine the successor's own Commit range.
	a2 := fx.appWithPolicy(5*time.Millisecond, committingImplementationScript("i1"))
	if _, err := a2.Execute(context.Background(), app.DispatchCommand{Workflow: wf}); err != nil {
		t.Fatalf("dispatch 2: %v", err)
	}
	iv = fx.Inspect(wf)
	if len(iv.Quarantines) != 0 {
		t.Fatalf("no quarantine may be fabricated for a legitimate successor session, got %+v", iv.Quarantines)
	}
	for _, f := range iv.Status.Findings {
		if f.Blocking && f.Code == model.CodeCommitDuringPolicyDriftWindow {
			t.Fatalf("no drift-window finding may be fabricated for a legitimate successor session")
		}
	}
	if iv.Status.Runtime == model.RuntimeBlocked {
		t.Fatalf("the workflow must not be blocked by a stale settle")
	}
}

// TestResumeAfterInterruptWorktreeDrift asserts the resume re-verification
// (PRD 已确认：Ctrl+C 两阶段有限停止 step 7): after the interruption the
// coding Worktree is preserved, and a resume whose Worktree drifted from
// the interruption Checkpoint blocks with INTERRUPTED_WORKTREE_DRIFTED —
// the successor never reuses a drifted path and never charges the budget.
func TestResumeAfterInterruptWorktreeDrift(t *testing.T) {
	fx := newParallelFixture(t)
	wf, _ := fx.driveToExecutionApproval(t)
	a := fx.app(stopAfterImplementationScript("i1", 2))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := a.Execute(ctx, app.DispatchCommand{Workflow: wf})
		done <- err
	}()
	worktree := filepath.Join(fx.home, "projects", app.ProjectFor(fx.repo.Root).Key, string(wf), "tmp", "tasks", "task-s01")
	deadline := time.Now().Add(15 * time.Second)
	for {
		iv := fx.Inspect(wf)
		running := false
		for _, att := range iv.Attempts {
			if att.Status == model.AttemptRunning {
				running = true
			}
		}
		ready := execGitAt(worktree, "rev-parse", "--verify", "HEAD") == nil
		if running && ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the coding attempt never started in its worktree")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatalf("the interrupted dispatch did not return")
	}
	iv := fx.Inspect(wf)
	if iv.Status.Runtime != model.RuntimePaused {
		t.Fatalf("runtime = %s, want PAUSED after the controlled stop", iv.Status.Runtime)
	}
	// The interrupted Attempt recorded its Worktree end facts (the resume
	// re-verification compares against them).
	interrupted := false
	for _, att := range iv.Attempts {
		if att.Status == model.AttemptInterrupted && att.EndHead != "" {
			interrupted = true
		}
	}
	if !interrupted {
		t.Fatalf("the interrupted attempt must carry its end facts, attempts = %+v", iv.Attempts)
	}
	// The user's Worktree drifted after the interruption (a Commit landed
	// inside the preserved coding Worktree).
	fx.repo.gitAt(worktree, "commit", "--allow-empty", "-q", "-m", "user drift after the interruption")
	// Resume and dispatch: the successor's reuse check sees the drift and
	// blocks with INTERRUPTED_WORKTREE_DRIFTED. The second dispatch runs
	// on a fresh Application with a completing fixture (the stop-boundary
	// fixture of the interrupted pass would block a successor Session
	// forever).
	if _, err := a.Execute(context.Background(), app.ResumeWorkflowCommand{Workflow: wf}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	a2 := fx.app(implementationScript("i1"))
	if _, err := a2.Execute(context.Background(), app.DispatchCommand{Workflow: wf}); err != nil {
		t.Fatalf("dispatch after the drift: %v", err)
	}
	iv = fx.Inspect(wf)
	if iv.Status.Runtime != model.RuntimeBlocked {
		t.Fatalf("runtime = %s, want BLOCKED on the worktree drift", iv.Status.Runtime)
	}
	drifted := false
	for _, f := range iv.Status.Findings {
		if f.Blocking && f.Code == model.CodeInterruptedWorktreeDrifted {
			drifted = true
		}
	}
	if !drifted {
		t.Fatalf("a blocking INTERRUPTED_WORKTREE_DRIFTED finding must be persisted")
	}
	charged := false
	for _, att := range iv.Attempts {
		if att.RetryCharged {
			charged = true
		}
	}
	if charged {
		t.Fatalf("a worktree-drift failure must never charge the retry budget")
	}
}

// committingImplementationScript is the implementation fixture with
// declared Commits: the simulated Agent writes inside each task's
// approved scope (the parallel fixture's add/sub/mul scopes) and commits
// the written files at the terminal frame.
func committingImplementationScript(sessionID string) string {
	return fmt.Sprintf(`{"fixture":"fake-run","script_version":1,"provider":"fake","dialect":"cflow.dialect.fake.v1","purpose":"implementation","session_id":%s,"exit_code":0,"resume":"ok","tasks":{"task-s01":{"writes":[{"path":"src/calc/add/add.ts","content":"// Add returns the sum of two numbers.\nexport function add(a: number, b: number): number {\n  return a + b;\n}\n"}],"commit":"implement add"},"task-s02":{"writes":[{"path":"src/calc/sub/sub.ts","content":"// Sub returns the difference of two numbers.\nexport function sub(a: number, b: number): number {\n  return a - b;\n}\n"}],"commit":"implement sub"},"task-s03":{"writes":[{"path":"src/calc/mul/mul.ts","content":"// Mul returns the product of two numbers.\nexport function mul(a: number, b: number): number {\n  return a * b;\n}\n"}],"commit":"implement mul"}}}
{"type":"session_started","session_id":%s,"at_ms":0}
{"type":"assistant_message","session_id":%s,"text":"Implemented the calculator task.","at_ms":10}
{"type":"session_finished","session_id":%s,"result":{"summary":"implemented"},"at_ms":20}`,
		strconv.Quote(sessionID), strconv.Quote(sessionID), strconv.Quote(sessionID), strconv.Quote(sessionID))
}
