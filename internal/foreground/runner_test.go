package foreground_test

// Foreground Runner tests (TUI task 13): the Runner drives one workflow
// through the typed outcomes until a stop reason, waits on Waiting
// outcomes, streams committed events, and never busy-loops.

import (
	"context"
	"testing"

	"cflow.local/cflow/internal/app"
	"cflow.local/cflow/internal/foreground"
	"cflow.local/cflow/internal/model"
)

// fakeDriver replays a scripted outcome sequence.
type fakeDriver struct {
	outcomes []app.DriveOutcome
	calls    int
}

func (d *fakeDriver) DriveOnce(ctx context.Context, wf model.WorkflowID) (app.DriveOutcome, error) {
	if d.calls >= len(d.outcomes) {
		return app.DriveOutcome{Kind: app.DriveNoSafeProgress}, nil
	}
	out := d.outcomes[d.calls]
	d.calls++
	return out, nil
}

// TestRunnerDrivesUntilTerminal is the TUI task 13 failure test: the
// Runner drives progressed steps until the terminal outcome and reports
// the terminal stop reason.
func TestRunnerDrivesUntilTerminal(t *testing.T) {
	runner := foreground.Runner{Driver: &fakeDriver{outcomes: []app.DriveOutcome{
		{Kind: app.DriveProgressed, Outcome: app.Outcome{Workflow: "wf-1"}},
		{Kind: app.DriveProgressed, Outcome: app.Outcome{Workflow: "wf-1"}},
		{Kind: app.DriveTerminal, Outcome: app.Outcome{Workflow: "wf-1"}},
	}}}
	result, err := runner.Run(context.Background(), "wf-1")
	if err != nil {
		t.Fatal(err)
	}
	if result.Reason != foreground.StopTerminal {
		t.Fatalf("%+v %v", result, err)
	}
}

// TestRunnerStopsOnUserDecision: a needs-user outcome stops the Runner
// with the decision reason.
func TestRunnerStopsOnUserDecision(t *testing.T) {
	runner := foreground.Runner{Driver: &fakeDriver{outcomes: []app.DriveOutcome{
		{Kind: app.DriveNeedsUser, Reason: "blocked"},
	}}}
	result, err := runner.Run(context.Background(), "wf-1")
	if err != nil {
		t.Fatal(err)
	}
	if result.Reason != foreground.StopNeedsUser {
		t.Fatalf("reason = %s, want needs-user", result.Reason)
	}
}

// TestRunnerWaitsOnWaitingOutcomes: a waiting outcome's channel gates the
// next step; the runner does not busy-loop.
func TestRunnerWaitsOnWaitingOutcomes(t *testing.T) {
	wait := make(chan struct{})
	driver := &fakeDriver{outcomes: []app.DriveOutcome{
		{Kind: app.DriveWaiting, Wait: wait},
		{Kind: app.DriveTerminal},
	}}
	runner := foreground.Runner{Driver: driver}
	done := make(chan foreground.Result, 1)
	go func() {
		res, _ := runner.Run(context.Background(), "wf-1")
		done <- res
	}()
	select {
	case res := <-done:
		t.Fatalf("runner finished before the wait released: %+v", res)
	default:
	}
	close(wait)
	res := <-done
	if res.Reason != foreground.StopTerminal {
		t.Fatalf("reason = %s, want terminal", res.Reason)
	}
}

// TestRunnerNoSafeProgressStops: no safe progress stops the runner
// without an infinite loop.
func TestRunnerNoSafeProgressStops(t *testing.T) {
	runner := foreground.Runner{Driver: &fakeDriver{outcomes: []app.DriveOutcome{
		{Kind: app.DriveNoSafeProgress},
	}}}
	result, err := runner.Run(context.Background(), "wf-1")
	if err != nil {
		t.Fatal(err)
	}
	if result.Reason != foreground.StopNoSafeProgress {
		t.Fatalf("reason = %s, want no-safe-progress", result.Reason)
	}
}

// TestRunnerCancelPauses: a cancelled context stops the Runner with the
// cancelled reason (the workflow is paused by the caller's controlled
// stop, never left running).
func TestRunnerCancelPauses(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	wait := make(chan struct{})
	driver := &fakeDriver{outcomes: []app.DriveOutcome{
		{Kind: app.DriveWaiting, Wait: wait},
	}}
	runner := foreground.Runner{Driver: driver}
	done := make(chan foreground.Result, 1)
	go func() {
		res, _ := runner.Run(ctx, "wf-1")
		done <- res
	}()
	cancel()
	select {
	case res := <-done:
		if res.Reason != foreground.StopCancelled {
			t.Fatalf("reason = %s, want cancelled", res.Reason)
		}
	case <-wait:
		t.Fatal("runner did not stop on cancel")
	}
}

// TestRunnerStreamsEvents: committed events are streamed through the
// OnEvent callback.
func TestRunnerStreamsEvents(t *testing.T) {
	ev := model.Event{Seq: 1, Kind: model.EventRunStarted, Workflow: "wf-1"}
	driver := &fakeDriver{outcomes: []app.DriveOutcome{
		{Kind: app.DriveProgressed, Outcome: app.Outcome{Workflow: "wf-1", Events: []model.Event{ev}}},
		{Kind: app.DriveTerminal},
	}}
	got := []model.Event{}
	runner := foreground.Runner{Driver: driver, OnEvent: func(e model.Event) {
		got = append(got, e)
	}}
	if _, err := runner.Run(context.Background(), "wf-1"); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Kind != model.EventRunStarted {
		t.Fatalf("events = %+v", got)
	}
}
