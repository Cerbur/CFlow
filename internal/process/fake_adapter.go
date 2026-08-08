package process

// FakeAdapter is the deterministic test Adapter (design 13.1, 22.1):
// scripted process groups on virtual time, with PID reuse that always
// bumps the start token so stale identities can be proven not to be the
// old owner. No real processes are involved.
//
// Tests drive it through exported control methods (Advance, ExitGroup,
// EmitOutput, EmitEOF, Signals) and pair it with a Supervisor via
// NewFakeSupervisor, which also wires the virtual clock so timeout
// policy runs on virtual time.

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"
)

// Stream selects a process stream for scripted output.
type Stream int

const (
	// Stdout is the process's standard output.
	Stdout Stream = iota
	// Stderr is the process's standard error.
	Stderr
)

// FakeAdapter is the deterministic Adapter for tests.
type FakeAdapter struct {
	clock *virtualClock

	mu           sync.Mutex
	procs        map[uint64]*fakeProcess
	pidIndex     map[int]uint64 // live PID -> start token
	tokenCounter uint64
}

// NewFakeAdapter returns an empty fake. Pair it with a Supervisor via
// NewFakeSupervisor so timeout policy uses the same virtual clock.
func NewFakeAdapter() *FakeAdapter {
	return &FakeAdapter{
		clock:    newVirtualClock(),
		procs:    map[uint64]*fakeProcess{},
		pidIndex: map[int]uint64{},
	}
}

// NewFakeSupervisor returns a Supervisor wired to a fresh FakeAdapter
// sharing one virtual clock. The fake is returned first.
func NewFakeSupervisor() (*FakeAdapter, Supervisor) {
	f := NewFakeAdapter()
	return f, NewSupervisor(f)
}

// fakeProcess is one scripted process group.
type fakeProcess struct {
	h        Handle
	identity ProcessIdentity
	pgid     int
	outR     *io.PipeReader
	outW     *io.PipeWriter
	errR     *io.PipeReader
	errW     *io.PipeWriter
	outDone  bool
	errDone  bool
	signals  []Signal
	exitCh   chan Exit
}

func (p *fakeProcess) Identity() ProcessIdentity { return p.identity }
func (p *fakeProcess) GroupID() int              { return p.pgid }
func (p *fakeProcess) Stdout() io.Reader         { return p.outR }
func (p *fakeProcess) Stderr() io.Reader         { return p.errR }

// processClock hands the virtual clock to the Supervisor so that
// NewSupervisor(NewFakeAdapter()) also runs on virtual time.
func (f *FakeAdapter) processClock() clock { return f.clock }

// Start registers a scripted process group. PIDs are the smallest free
// IDs from 1000, so an exited group's PID is deterministically reused
// with a fresh start token.
func (f *FakeAdapter) Start(ctx context.Context, h Handle, spec ProcessSpec) (Process, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	pid := 1000
	for f.pidIndex[pid] != 0 {
		pid++
	}
	f.tokenCounter++
	outR, outW := io.Pipe()
	errR, errW := io.Pipe()
	p := &fakeProcess{
		h:        h,
		identity: ProcessIdentity{PID: pid, StartToken: f.tokenCounter},
		pgid:     pid,
		outR:     outR,
		outW:     outW,
		errR:     errR,
		errW:     errW,
		exitCh:   make(chan Exit, 1),
	}
	f.procs[h.id] = p
	f.pidIndex[pid] = f.tokenCounter
	return p, nil
}

// Signal records the supervisor signal on the process group. Groups that
// already exited are already gone: nil.
func (f *FakeAdapter) Signal(ctx context.Context, proc Process, sig Signal) error {
	p := proc.(*fakeProcess)
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, live := f.procs[p.h.id]; !live {
		return nil
	}
	p.signals = append(p.signals, sig)
	return nil
}

// StartInteractive registers a scripted interactive process (TUI task
// 11): the same fakeProcess shape, so ExitGroup and EmitOutput script
// the native terminal's end and output deterministically. The terminal
// triple of the spec is recorded for the test's assertions.
func (f *FakeAdapter) StartInteractive(ctx context.Context, h Handle, spec InteractiveSpec) (Process, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	pid := 1000
	for f.pidIndex[pid] != 0 {
		pid++
	}
	f.tokenCounter++
	outR, outW := io.Pipe()
	errR, errW := io.Pipe()
	p := &fakeProcess{
		h:        h,
		identity: ProcessIdentity{PID: pid, StartToken: f.tokenCounter},
		pgid:     pid,
		outR:     outR,
		outW:     outW,
		errR:     errR,
		errW:     errW,
		exitCh:   make(chan Exit, 1),
	}
	f.procs[h.id] = p
	f.pidIndex[pid] = f.tokenCounter
	return p, nil
}

// Wait returns the exit recorded by ExitGroup, or ctx.Err().
func (f *FakeAdapter) Wait(ctx context.Context, proc Process) (Exit, error) {
	p := proc.(*fakeProcess)
	select {
	case exit := <-p.exitCh:
		return exit, nil
	case <-ctx.Done():
		return Exit{}, ctx.Err()
	}
}

// Inspect reports the live truth about an identity in the fake's world.
func (f *FakeAdapter) Inspect(ctx context.Context, id ProcessIdentity) (ProcessFact, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fact := ProcessFact{Identity: id}
	token, live := f.pidIndex[id.PID]
	if !live || token != id.StartToken {
		return fact, nil
	}
	fact.Running = true
	for _, p := range f.procs {
		if p.identity == id {
			fact.GroupID = p.pgid
			break
		}
	}
	return fact, nil
}

// Advance moves the virtual clock forward, firing due supervisor timers
// synchronously: after Advance returns, the timeout policy's Terminate
// has already been delivered to the group.
func (f *FakeAdapter) Advance(d time.Duration) {
	f.clock.advance(d)
}

// ExitGroup scripts the whole process group exiting with code. The
// streams close (EOF facts) and the exit is recorded for Wait. The
// group's PID is freed for deterministic reuse.
func (f *FakeAdapter) ExitGroup(h Handle, code int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.procs[h.id]
	if !ok {
		panic(fmt.Sprintf("fake: ExitGroup on unknown handle %d", h.id))
	}
	if !p.outDone {
		p.outDone = true
		_ = p.outW.Close()
	}
	if !p.errDone {
		p.errDone = true
		_ = p.errW.Close()
	}
	delete(f.pidIndex, p.identity.PID)
	delete(f.procs, h.id)
	p.exitCh <- Exit{Code: code, Fact: FactProcessExit}
}

// EmitOutput scripts one chunk of raw output on a stream. It blocks
// while the supervisor's frame pipeline applies backpressure.
func (f *FakeAdapter) EmitOutput(h Handle, stream Stream, data []byte) {
	f.mu.Lock()
	p, ok := f.procs[h.id]
	if !ok {
		f.mu.Unlock()
		panic(fmt.Sprintf("fake: EmitOutput on unknown handle %d", h.id))
	}
	w := p.outW
	if stream == Stderr {
		w = p.errW
	}
	f.mu.Unlock()
	if _, err := w.Write(data); err != nil {
		panic(fmt.Sprintf("fake: EmitOutput on closed stream: %v", err))
	}
}

// EmitEOF scripts end-of-stream on one stream, distinct from process
// exit.
func (f *FakeAdapter) EmitEOF(h Handle, stream Stream) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.procs[h.id]
	if !ok {
		panic(fmt.Sprintf("fake: EmitEOF on unknown handle %d", h.id))
	}
	if stream == Stdout {
		if !p.outDone {
			p.outDone = true
			_ = p.outW.Close()
		}
	} else {
		if !p.errDone {
			p.errDone = true
			_ = p.errW.Close()
		}
	}
}

// Signals returns a copy of the signals the live group has received so
// far. It panics on a handle whose group already exited: script the
// assertion before ExitGroup.
func (f *FakeAdapter) Signals(h Handle) []Signal {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.procs[h.id]
	if !ok {
		panic(fmt.Sprintf("fake: Signals on unknown handle %d", h.id))
	}
	return append([]Signal(nil), p.signals...)
}

// ---------------------------------------------------------------------------
// Virtual clock (design 22.1: deterministic fake time)
// ---------------------------------------------------------------------------

// virtualClock fires AfterFunc callbacks synchronously from Advance, so
// tests observe policy side effects the moment virtual time moves.
type virtualClock struct {
	mu      sync.Mutex
	now     time.Time
	pending map[*virtualTimer]struct{}
}

// virtualTimer is one pending AfterFunc callback.
type virtualTimer struct {
	clk   *virtualClock
	when  time.Time
	f     func()
	fired bool
}

func newVirtualClock() *virtualClock {
	return &virtualClock{now: time.Unix(0, 0), pending: map[*virtualTimer]struct{}{}}
}

func (vc *virtualClock) AfterFunc(d time.Duration, f func()) timer {
	vc.mu.Lock()
	defer vc.mu.Unlock()
	t := &virtualTimer{clk: vc, when: vc.now.Add(d), f: f}
	vc.pending[t] = struct{}{}
	return t
}

// Stop cancels a pending callback. It reports whether the callback was
// still pending.
func (t *virtualTimer) Stop() bool {
	t.clk.mu.Lock()
	defer t.clk.mu.Unlock()
	if t.fired {
		return false
	}
	if _, ok := t.clk.pending[t]; !ok {
		return false
	}
	delete(t.clk.pending, t)
	return true
}

// advance moves the clock and fires every due callback synchronously.
func (vc *virtualClock) advance(d time.Duration) {
	vc.mu.Lock()
	vc.now = vc.now.Add(d)
	for t := range vc.pending {
		if !t.when.After(vc.now) {
			t.fired = true
			delete(vc.pending, t)
			t.f()
		}
	}
	vc.mu.Unlock()
}
