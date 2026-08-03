package cli

// The SIGINT two-phase translation tests (Task 7 obligation, PRD 已确认：
// Ctrl+C 两阶段有限停止): the first signal cancels the command context, the
// second escalates the controlled stop to the force-kill phase.

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestTranslateInterruptsFirstCancelsSecondEscalates asserts the
// two-phase translation: the first signal cancels the context; the second
// escalates the controlled stop exactly once.
func TestTranslateInterruptsFirstCancelsSecondEscalates(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	defer close(done)
	sig := make(chan os.Signal, 2)
	escalated := make(chan struct{}, 1)
	stopped := make(chan struct{})
	translateInterrupts(ctx, cancel, done, sig, func() { escalated <- struct{}{} }, func() { close(stopped) })

	sig <- os.Interrupt // first Ctrl+C: the command context cancels
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatalf("the first signal must cancel the command context")
	}
	select {
	case <-escalated:
		t.Fatalf("the first signal must not escalate")
	default:
	}
	sig <- os.Interrupt // second Ctrl+C: escalate to the force-kill phase
	select {
	case <-escalated:
	case <-time.After(2 * time.Second):
		t.Fatalf("the second signal must escalate the controlled stop")
	}
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatalf("the translation must unregister its signal source")
	}
}

// TestTranslateInterruptsSingleSignalPauses asserts the translation with
// only one signal: the context cancels and nothing escalates (the second
// signal never arrived).
func TestTranslateInterruptsSingleSignalPauses(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	defer close(done)
	sig := make(chan os.Signal, 2)
	escalated := make(chan struct{}, 1)
	translateInterrupts(ctx, cancel, done, sig, func() { escalated <- struct{}{} }, func() {})
	sig <- os.Interrupt
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatalf("the first signal must cancel the command context")
	}
	select {
	case <-escalated:
		t.Fatalf("a single signal must never escalate")
	case <-time.After(100 * time.Millisecond):
	}
}
