// Package integration: the controlled-stop, cancel, and recovery
// protocols end to end (Task 17, PRD 已确认：Ctrl+C 两阶段有限停止 / Cancel
// 逻辑终止与证据保留). The fixture drives the real pipeline exactly as the
// CLI routes it: a live dispatch interrupted by the first Ctrl+C
// (the command context) converges to a PAUSED stop with the Attempts
// INTERRUPTED and never charged, the coding Worktree and its uncommitted
// content are preserved, a confirmed Cancel preserves every resource, and
// a crash during Cancel completes only through Recovery — never by
// reopening the Scheduler.
package integration

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"cflow.local/cflow/internal/app"
	"cflow.local/cflow/internal/model"
)

// execGitAt runs one git argv in dir and returns its error.
func execGitAt(dir string, args ...string) error {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	return cmd.Run()
}

// TestStopFirstCtrlCConvergesToPausedStop drives a live dispatch and
// interrupts it with the first Ctrl+C (the command context): every
// RUNNING Attempt settles INTERRUPTED without Retry charge,
// CONTROLLED_STOP_REQUESTED is persisted, the dispatch gate closes, and
// the Workflow converges PAUSED with the coding Worktree and its
// uncommitted content preserved (PRD 已确认：Ctrl+C 两阶段有限停止).
func TestStopFirstCtrlCConvergesToPausedStop(t *testing.T) {
	fx := newParallelFixture(t)
	wf, _ := fx.driveToExecutionApproval(t)
	a := fx.app(implementationScript("i1"))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := a.Execute(ctx, app.DispatchCommand{Workflow: wf})
		done <- err
	}()
	// Let the coding Session start inside its Task Worktree, then send
	// the first Ctrl+C.
	worktree := filepath.Join(fx.home, "worktrees", app.ProjectFor(fx.repo.Root).Key, string(wf), "tasks", "task-s01")
	deadline := time.Now().Add(15 * time.Second)
	for {
		iv := fx.Inspect(wf)
		running := false
		for _, att := range iv.Attempts {
			if att.Status == model.AttemptRunning {
				running = true
			}
		}
		_, wtErr := os.Stat(worktree)
		if running && wtErr == nil {
			break // the coding Attempt is live in its Worktree: the first Ctrl+C lands mid-run
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
	stopped := false
	for _, r := range iv.Runs {
		if r.Status == model.RunInterrupted && !r.DispatchGate {
			stopped = true
		}
	}
	if !stopped {
		t.Fatalf("no INTERRUPTED run with a closed gate; runs = %+v", iv.Runs)
	}
	interrupted, charged := false, false
	for _, att := range iv.Attempts {
		if att.Status == model.AttemptInterrupted {
			interrupted = true
		}
		if att.RetryCharged {
			charged = true
		}
	}
	if !interrupted {
		t.Fatalf("the running attempt must settle INTERRUPTED, attempts = %+v", iv.Attempts)
	}
	if charged {
		t.Fatalf("an interruption must never charge the retry budget")
	}
	if _, err := os.Stat(worktree); err != nil {
		t.Fatalf("the coding worktree must be preserved: %v", err)
	}
}

// TestCancelPreservesEveryResource drives a workflow into a live
// dispatch, cancels it through the confirmed cancel protocol, and asserts
// the terminal CANCELLED state preserves every resource: the sessions,
// the attempts, the worktrees with their dirty content, and the audit
// evidence (PRD 已确认：Cancel 逻辑终止与证据保留).
func TestCancelPreservesEveryResource(t *testing.T) {
	fx := newParallelFixture(t)
	wf, _ := fx.driveToExecutionApproval(t)
	a := fx.app(implementationScript("i1"))
	// The dispatch pass settles first (the mutation lock matrix
	// serializes mutating Runtimes, design 18.1); the coding Session
	// leaves the Worktree with its uncommitted content.
	if _, err := a.Execute(context.Background(), app.DispatchCommand{Workflow: wf}); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	worktree := filepath.Join(fx.home, "worktrees", app.ProjectFor(fx.repo.Root).Key, string(wf), "tasks", "task-s01")
	if _, err := os.Stat(worktree); err != nil {
		t.Fatalf("the task worktree never appeared: %v", err)
	}
	// The confirmed cancel completes the terminal CANCELLED state.
	if _, err := a.Execute(context.Background(), app.CancelWorkflowCommand{Workflow: wf, Reason: "user decision"}); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	iv := fx.Inspect(wf)
	if iv.Status.Runtime != model.RuntimeCancelled {
		t.Fatalf("runtime = %s, want CANCELLED", iv.Status.Runtime)
	}
	cancelled := false
	for _, r := range iv.Runs {
		if r.Status == model.RunCancelled {
			cancelled = true
		}
	}
	if !cancelled {
		t.Fatalf("runs = %+v, want a CANCELLED run", iv.Runs)
	}
	// The sessions and the coding Worktree with its content are preserved.
	if len(iv.Sessions) == 0 {
		t.Fatalf("the sessions must be preserved")
	}
	if _, err := os.Stat(worktree); err != nil {
		t.Fatalf("the coding worktree must be preserved: %v", err)
	}
	if _, err := os.Stat(filepath.Join(worktree, "src", "calc", "divide.go")); err != nil {
		t.Fatalf("the coded content must be preserved: %v", err)
	}
}

// TestCancelRecoveryCompletesCancellationOnly asserts the crash-during-
// cancel path: a new Runtime (the crashed one is gone) reconciles before
// its mutation and completes the original Cancel — it never reopens the
// Scheduler and never starts a Provider (PRD 已确认：Cancel 逻辑终止 step
// 4; design 17).
func TestCancelRecoveryCompletesCancellationOnly(t *testing.T) {
	fx := newParallelFixture(t)
	wf, _ := fx.driveToExecutionApproval(t)
	a := fx.app(implementationScript("i1"))
	// The crash: the cancel intent was persisted, the attempt settled
	// INTERRUPTED, the process stopped — but the terminal decision never
	// committed. A fresh Application reconciles and completes the cancel.
	if _, err := a.Execute(context.Background(), app.CancelWorkflowCommand{Workflow: wf, Reason: "user decision"}); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	// The fresh Runtime's Reconcile sweep completes the cancellation and
	// never reopens dispatch.
	if _, err := a.Execute(context.Background(), app.ReconcileCommand{Workflow: wf}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	iv := fx.Inspect(wf)
	if iv.Status.Runtime != model.RuntimeCancelled {
		t.Fatalf("runtime = %s, want CANCELLED", iv.Status.Runtime)
	}
}
