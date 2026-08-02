package process_test

// Deterministic Supervisor lifecycle tests (design 13, 22.1): the Fake
// Process Adapter's virtual time drives process-group signaling, exit
// facts, output bounds, and identity facts. No real processes are used
// here; the OS seam is covered by internal/platform/process_unix_test.go.

import (
	"bytes"
	"context"
	"runtime"
	"strings"
	"testing"
	"time"

	"cflow.local/cflow/internal/process"
)

func requireNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// drain reads every event until the supervisor closes the channel. It is
// safe to call after Wait returns, because Wait returns only after the
// events channel has been drained and closed.
func drain(t *testing.T, events process.Events) []process.Event {
	t.Helper()
	var got []process.Event
	for ev := range events {
		got = append(got, ev)
	}
	return got
}

// TestSupervisorSignalsExactProcessGroup is the brief Step 1 verbatim
// lifecycle test: a process group that receives Terminate then exits with
// 143 reports exactly that exit code through Wait.
func TestSupervisorSignalsExactProcessGroup(t *testing.T) {
	fake, supervisor := process.NewFakeSupervisor()
	h, _, err := supervisor.Start(context.Background(), process.ProcessSpec{Executable: "/fixture/worker", Args: []string{"run"}})
	requireNoError(t, err)
	requireNoError(t, supervisor.Signal(context.Background(), h, process.Terminate))
	fake.ExitGroup(h, 143)
	exit, err := supervisor.Wait(context.Background(), h)
	requireNoError(t, err)
	if exit.Code != 143 {
		t.Fatalf("exit=%d", exit.Code)
	}
}

// TestSupervisorSignalDeliveredToProcessGroup asserts the supervisor
// signals the exact process group, not merely the process: every signal
// is recorded on the group owned by the handle.
func TestSupervisorSignalDeliveredToProcessGroup(t *testing.T) {
	fake, supervisor := process.NewFakeSupervisor()
	h, _, err := supervisor.Start(context.Background(), process.ProcessSpec{Executable: "/fixture/worker"})
	requireNoError(t, err)
	requireNoError(t, supervisor.Signal(context.Background(), h, process.Terminate))
	requireNoError(t, supervisor.Signal(context.Background(), h, process.Interrupt))
	requireNoError(t, supervisor.Signal(context.Background(), h, process.ForceKill))
	if got := fake.Signals(h); len(got) != 3 || got[0] != process.Terminate || got[1] != process.Interrupt || got[2] != process.ForceKill {
		t.Fatalf("group signals=%v, want [Terminate Interrupt ForceKill]", got)
	}
	fake.ExitGroup(h, 137)
	exit, err := supervisor.Wait(context.Background(), h)
	requireNoError(t, err)
	if exit.Code != 137 {
		t.Fatalf("exit=%d", exit.Code)
	}
}

// TestSupervisorFramesAndEOF asserts the framed output pipeline: lines
// arrive as bounded frames in order, the final unterminated line is
// flushed at EOF, and each stream ends with an EOF fact distinct from
// process exit. The first event carries the process identity.
func TestSupervisorFramesAndEOF(t *testing.T) {
	fake, supervisor := process.NewFakeSupervisor()
	h, events, err := supervisor.Start(context.Background(), process.ProcessSpec{
		Executable:     "/fixture/worker",
		MaxFrameBytes:  4096,
		MaxOutputBytes: 1 << 20,
	})
	requireNoError(t, err)
	fake.EmitOutput(h, process.Stdout, []byte("alpha\nbeta\ngamma"))
	fake.EmitEOF(h, process.Stdout)
	fake.EmitOutput(h, process.Stderr, []byte("warn\n"))
	fake.ExitGroup(h, 0)
	exit, err := supervisor.Wait(context.Background(), h)
	requireNoError(t, err)
	if exit.Code != 0 || exit.Fact != process.FactProcessExit {
		t.Fatalf("exit=%+v, want clean process exit", exit)
	}
	got := drain(t, events)

	if got[0].Kind != process.EventStarted {
		t.Fatalf("first event kind=%v, want EventStarted", got[0].Kind)
	}
	if got[0].Identity.PID <= 0 || got[0].Identity.StartToken == 0 {
		t.Fatalf("identity=%+v, want live PID and start token", got[0].Identity)
	}
	if got[0].GroupID != got[0].Identity.PID {
		t.Fatalf("group=%d, want isolated group equal to PID %d", got[0].GroupID, got[0].Identity.PID)
	}

	// Frame order within each stream is deterministic; the two stream
	// pumps interleave only across streams.
	var out, errStream []string
	for _, ev := range got[1:] {
		switch ev.Kind {
		case process.EventFrameOut:
			out = append(out, "frame:"+string(ev.Frame))
		case process.EventEOFOut:
			out = append(out, "eof")
		case process.EventFrameErr:
			errStream = append(errStream, "frame:"+string(ev.Frame))
		case process.EventEOFErr:
			errStream = append(errStream, "eof")
		}
	}
	wantOut := []string{"frame:alpha", "frame:beta", "frame:gamma", "eof"}
	wantErr := []string{"frame:warn", "eof"}
	if strings.Join(out, ",") != strings.Join(wantOut, ",") {
		t.Fatalf("stdout=%v, want %v", out, wantOut)
	}
	if strings.Join(errStream, ",") != strings.Join(wantErr, ",") {
		t.Fatalf("stderr=%v, want %v", errStream, wantErr)
	}
}

// TestSupervisorOutputOverflow asserts the total byte budget: once the
// budget is exhausted the stream emits one overflow fact carrying the
// total dropped byte count and never delivers another frame.
func TestSupervisorOutputOverflow(t *testing.T) {
	fake, supervisor := process.NewFakeSupervisor()
	h, events, err := supervisor.Start(context.Background(), process.ProcessSpec{
		Executable:     "/fixture/worker",
		MaxFrameBytes:  8,
		MaxOutputBytes: 16,
	})
	requireNoError(t, err)
	fake.EmitOutput(h, process.Stdout, []byte("aaaaaaaaaa\nbbbbbbbbbb\ncccccccccc\n"))
	fake.ExitGroup(h, 0)
	_, err = supervisor.Wait(context.Background(), h)
	requireNoError(t, err)
	got := drain(t, events)

	var frames []string
	var overflow process.Event
	var eof bool
	for _, ev := range got {
		switch ev.Kind {
		case process.EventFrameOut:
			frames = append(frames, string(ev.Frame))
		case process.EventOverflowOut:
			overflow = ev
		case process.EventEOFOut:
			eof = true
		}
	}
	// Budget 16: "aaaaaaaa" (8) and "aa" (2) fit; everything after is dropped.
	wantFrames := []string{"aaaaaaaa", "aa"}
	if len(frames) != 2 || frames[0] != wantFrames[0] || frames[1] != wantFrames[1] {
		t.Fatalf("frames=%v, want %v", frames, wantFrames)
	}
	// Stream is 33 bytes: 10 emitted, 3 line delimiters consumed,
	// 20 dropped ("bbbbbbbb" + "bb" + "cccccccc" + "cc").
	if overflow.Kind != process.EventOverflowOut || overflow.Count != 20 {
		t.Fatalf("overflow=%+v, want EventOverflowOut with 20 dropped bytes", overflow)
	}
	if !eof {
		t.Fatal("missing EOFOut after overflow")
	}
}

// TestSupervisorFramesAreBounded asserts no delivered frame ever exceeds
// the spec's frame bound, even for a line far longer than the bound.
func TestSupervisorFramesAreBounded(t *testing.T) {
	fake, supervisor := process.NewFakeSupervisor()
	h, events, err := supervisor.Start(context.Background(), process.ProcessSpec{
		Executable:     "/fixture/worker",
		MaxFrameBytes:  16,
		MaxOutputBytes: 1 << 20,
	})
	requireNoError(t, err)
	fake.EmitOutput(h, process.Stdout, []byte("0123456789abcdefghijklmnopqrstuvwxyz\n"))
	fake.ExitGroup(h, 0)
	_, err = supervisor.Wait(context.Background(), h)
	requireNoError(t, err)
	var got int
	for _, ev := range drain(t, events) {
		switch ev.Kind {
		case process.EventFrameOut, process.EventFrameErr:
			if len(ev.Frame) > 16 {
				t.Fatalf("frame of %d bytes exceeds bound", len(ev.Frame))
			}
			got++
		}
	}
	if got != 3 { // 16 + 16 + 4
		t.Fatalf("frame count=%d, want 3", got)
	}
}

// TestSupervisorPropagatesMalformedFrame asserts the supervisor is a
// transparent pipe: malformed bytes (here a NUL) are delivered verbatim.
// The owning Adapter validates and fails closed; the supervisor never
// filters or redacts output.
func TestSupervisorPropagatesMalformedFrame(t *testing.T) {
	fake, supervisor := process.NewFakeSupervisor()
	h, events, err := supervisor.Start(context.Background(), process.ProcessSpec{Executable: "/fixture/worker"})
	requireNoError(t, err)
	fake.EmitOutput(h, process.Stdout, []byte("bad\x00frame\n"))
	fake.ExitGroup(h, 0)
	_, err = supervisor.Wait(context.Background(), h)
	requireNoError(t, err)
	var got []byte
	for _, ev := range drain(t, events) {
		if ev.Kind == process.EventFrameOut {
			got = ev.Frame
		}
	}
	if !bytes.Equal(got, []byte("bad\x00frame")) {
		t.Fatalf("frame=%q, want malformed bytes delivered verbatim", got)
	}
}

// TestSupervisorTimeoutFact asserts the timeout is a supervisor policy
// fact: after the virtual clock advances past the timeout, the group
// receives Terminate and Wait reports FactTimeout with the exit code.
func TestSupervisorTimeoutFact(t *testing.T) {
	fake, supervisor := process.NewFakeSupervisor()
	h, _, err := supervisor.Start(context.Background(), process.ProcessSpec{
		Executable: "/fixture/worker",
		Timeout:    10 * time.Second,
	})
	requireNoError(t, err)
	fake.Advance(11 * time.Second)
	if got := fake.Signals(h); len(got) != 1 || got[0] != process.Terminate {
		t.Fatalf("signals=%v, want exactly [Terminate] after timeout", got)
	}
	fake.ExitGroup(h, 143)
	exit, err := supervisor.Wait(context.Background(), h)
	requireNoError(t, err)
	if exit.Fact != process.FactTimeout || exit.Code != 143 {
		t.Fatalf("exit=%+v, want FactTimeout with code 143", exit)
	}
}

// TestSupervisorCancelFact asserts context cancellation is a supervisor
// policy fact: the group receives Terminate and Wait reports FactCancelled.
func TestSupervisorCancelFact(t *testing.T) {
	fake, supervisor := process.NewFakeSupervisor()
	ctx, cancel := context.WithCancel(context.Background())
	h, _, err := supervisor.Start(ctx, process.ProcessSpec{Executable: "/fixture/worker"})
	requireNoError(t, err)
	cancel()
	fake.ExitGroup(h, 143)
	exit, err := supervisor.Wait(context.Background(), h)
	requireNoError(t, err)
	if exit.Fact != process.FactCancelled || exit.Code != 143 {
		t.Fatalf("exit=%+v, want FactCancelled with code 143", exit)
	}
}

// TestSupervisorStalePIDWithChangedStartToken asserts PID alone is never
// trusted: after the original process exits, its identity reports
// not-running even when a new process reuses the same PID with a changed
// start token.
func TestSupervisorStalePIDWithChangedStartToken(t *testing.T) {
	fake, supervisor := process.NewFakeSupervisor()

	h1, events1, err := supervisor.Start(context.Background(), process.ProcessSpec{Executable: "/fixture/worker"})
	requireNoError(t, err)
	first := <-events1
	if first.Kind != process.EventStarted {
		t.Fatalf("event=%v, want EventStarted", first.Kind)
	}
	id1 := first.Identity
	fake.ExitGroup(h1, 0)
	_, err = supervisor.Wait(context.Background(), h1)
	requireNoError(t, err)
	drain(t, events1)

	// The fake reuses the freed PID with a new start token.
	h2, events2, err := supervisor.Start(context.Background(), process.ProcessSpec{Executable: "/fixture/worker"})
	requireNoError(t, err)
	second := <-events2
	if second.Kind != process.EventStarted {
		t.Fatalf("event=%v, want EventStarted", second.Kind)
	}
	id2 := second.Identity
	if id2.PID != id1.PID {
		t.Fatalf("expected the fake to reuse PID %d, got %d", id1.PID, id2.PID)
	}
	if id2.StartToken == id1.StartToken {
		t.Fatalf("expected a changed start token, got %d", id2.StartToken)
	}

	// The stale identity is not the old owner.
	fact, err := supervisor.Inspect(context.Background(), id1)
	requireNoError(t, err)
	if fact.Running {
		t.Fatalf("stale identity %+v reported running: %+v", id1, fact)
	}
	if fact.Managed {
		t.Fatalf("stale identity %+v reported managed: %+v", id1, fact)
	}
	// The live identity with the matching token is running and managed.
	fact, err = supervisor.Inspect(context.Background(), id2)
	requireNoError(t, err)
	if !fact.Running || !fact.Managed {
		t.Fatalf("live identity %+v not reported running+managed: %+v", id2, fact)
	}
	// A wrong token on the live PID is not the same process.
	fact, err = supervisor.Inspect(context.Background(), process.ProcessIdentity{PID: id2.PID, StartToken: id2.StartToken + 1})
	requireNoError(t, err)
	if fact.Running {
		t.Fatalf("identity with wrong start token reported running: %+v", fact)
	}

	fake.ExitGroup(h2, 0)
	_, err = supervisor.Wait(context.Background(), h2)
	requireNoError(t, err)
	drain(t, events2)
	fact, err = supervisor.Inspect(context.Background(), id2)
	requireNoError(t, err)
	if fact.Running || fact.Managed {
		t.Fatalf("reaped identity still running/managed: %+v", fact)
	}
}

// TestSupervisorInspectUnknownIdentity asserts a fabricated identity that
// was never supervised reports not-running and not-managed.
func TestSupervisorInspectUnknownIdentity(t *testing.T) {
	_, supervisor := process.NewFakeSupervisor()
	fact, err := supervisor.Inspect(context.Background(), process.ProcessIdentity{PID: 424242, StartToken: 1})
	requireNoError(t, err)
	if fact.Running || fact.Managed {
		t.Fatalf("unknown identity reported live: %+v", fact)
	}
}

// TestSupervisorWaitHonorsContext asserts Wait returns the context error
// while the process is still managed, and that a later Wait still reaps
// the exit fact.
func TestSupervisorWaitHonorsContext(t *testing.T) {
	fake, supervisor := process.NewFakeSupervisor()
	h, events, err := supervisor.Start(context.Background(), process.ProcessSpec{Executable: "/fixture/worker"})
	requireNoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := supervisor.Wait(ctx, h); err == nil {
		t.Fatal("expected Wait to fail when the context expires")
	}
	fake.ExitGroup(h, 0)
	exit, err := supervisor.Wait(context.Background(), h)
	requireNoError(t, err)
	if exit.Code != 0 {
		t.Fatalf("exit=%+v, want code 0", exit)
	}
	drain(t, events)
}

// TestSupervisorUnknownHandle asserts signaling and waiting on a handle
// the supervisor never issued fails.
func TestSupervisorUnknownHandle(t *testing.T) {
	_, supervisor := process.NewFakeSupervisor()
	if err := supervisor.Signal(context.Background(), process.Handle{}, process.Terminate); err == nil {
		t.Fatal("Signal on unknown handle succeeded")
	}
	if _, err := supervisor.Wait(context.Background(), process.Handle{}); err == nil {
		t.Fatal("Wait on unknown handle succeeded")
	}
}

// TestSupervisorRejectsInvalidSpec asserts argv-only safety validation:
// empty or relative executables and negative limits are rejected before
// any process is started.
func TestSupervisorRejectsInvalidSpec(t *testing.T) {
	_, supervisor := process.NewFakeSupervisor()
	base := process.ProcessSpec{Executable: "/fixture/worker"}
	cases := []process.ProcessSpec{
		{},
		{Executable: "relative/worker"},
		{Executable: base.Executable, Dir: "relative"},
		{Executable: base.Executable, MaxFrameBytes: -1},
		{Executable: base.Executable, MaxOutputBytes: -1},
		{Executable: base.Executable, Timeout: -1 * time.Second},
		{Executable: base.Executable, Group: process.GroupPolicy(99)},
	}
	for i, spec := range cases {
		if _, _, err := supervisor.Start(context.Background(), spec); err == nil {
			t.Fatalf("case %d: spec %+v accepted", i, spec)
		}
	}
}

// TestSupervisorNoGoroutineLeaks asserts every Start is fully reaped:
// after Wait returns, all per-process goroutines (pumps, watcher,
// finalizer) have exited.
func TestSupervisorNoGoroutineLeaks(t *testing.T) {
	fake, supervisor := process.NewFakeSupervisor()
	baseline := runtime.NumGoroutine()
	for i := 0; i < 20; i++ {
		h, events, err := supervisor.Start(context.Background(), process.ProcessSpec{
			Executable:     "/fixture/worker",
			MaxFrameBytes:  256,
			MaxOutputBytes: 1 << 16,
		})
		requireNoError(t, err)
		fake.EmitOutput(h, process.Stdout, []byte("line\n"))
		fake.ExitGroup(h, 0)
		_, err = supervisor.Wait(context.Background(), h)
		requireNoError(t, err)
		drain(t, events)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= baseline+2 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("goroutine leak: baseline=%d now=%d", baseline, runtime.NumGoroutine())
}
