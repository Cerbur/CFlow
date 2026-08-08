// Package process implements the Process Supervisor (design 13): an
// argv-only launch seam that never invokes a shell. The Supervisor owns
// lifecycle policy (timeout, cancellation, bounded framed output,
// distinct exit facts); the OS Adapter owns process-group, signal, and
// identity mechanics; tests use the deterministic Fake Adapter with
// virtual time (design 22.1).
//
// Identity is PID plus an OS Process Start Token; PID alone is never
// trusted (design 13.2). Stdout and stderr are framed and bounded: raw
// bytes exist only in bounded memory until the owning Adapter validates
// and redacts them. Timeout, Cancel, EOF, and process exit are distinct
// facts; a zero exit code can never override an invalid protocol or a
// failed evidence gate (those decisions belong to the Adapter).
//
// The Application-level 10-second plus 2-second controlled-stop policy
// (design 13.3) is implemented in Task 17 on top of this seam: Signal
// Terminate, Signal ForceKill, Wait with a deadline, and Inspect provide
// the primitives.
package process

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"sync"
	"time"

	"cflow.local/cflow/internal/model"
)

// ProcessSpec is the complete, shell-free description of one supervised
// process (design 13.1). It contains no shell string: the executable
// path and argv array are passed to exec verbatim.
type ProcessSpec struct {
	// Executable is the absolute path of the binary to run.
	Executable string
	// Args is the argv array passed to the executable.
	Args []string
	// Dir is the optional absolute working directory.
	Dir string
	// Env is the exact child environment. No parent environment is
	// inherited: values that exist in the parent cannot leak into the
	// child through this seam.
	Env map[string]string
	// Stdin is the optional stdin source. nil means closed stdin.
	Stdin io.Reader
	// MaxFrameBytes bounds one delivered frame (0 uses the default of
	// 1 MiB, the Redactor's own frame bound).
	MaxFrameBytes int64
	// MaxOutputBytes bounds the total delivered bytes per stream (0
	// uses the default of 64 MiB). Excess output is dropped and
	// reported as an overflow fact, never buffered.
	MaxOutputBytes int64
	// Timeout is the supervisor policy: after Timeout the process
	// group receives Terminate and Wait reports FactTimeout. 0 means
	// no timeout.
	Timeout time.Duration
	// Group is the process-group policy. The zero value isolates the
	// process in its own group.
	Group GroupPolicy
}

// GroupPolicy is the process-group policy of a ProcessSpec.
type GroupPolicy int

const (
	// GroupIsolated (default) creates a new process group whose
	// leader is the child. Signals target the exact group.
	GroupIsolated GroupPolicy = iota
	// GroupInherit runs the child in the caller's process group.
	GroupInherit
)

// Default frame and stream bounds. The frame default matches the
// Redactor's maximum safe WriteFrame size (internal/security).
const (
	defaultMaxFrameBytes  = 1 << 20
	defaultMaxOutputBytes = 64 << 20
	eventBufferSize       = 64
)

// Signal is a supervisor-level signal. The OS Adapter maps each value to
// the platform signal that targets the exact process group.
type Signal int

const (
	// Interrupt requests cooperative attention (SIGINT).
	Interrupt Signal = iota
	// Terminate requests orderly shutdown (SIGTERM).
	Terminate
	// ForceKill terminates without giving the group a chance to react
	// (SIGKILL).
	ForceKill
)

// Handle identifies one supervised process. Only the Supervisor issues
// handles; a zero Handle is never valid.
type Handle struct {
	id uint64
}

// StopPolicy is the staged budget of the two-phase controlled stop
// (design 13.3, PRD 已确认：Ctrl+C 两阶段有限停止): the Adapter-cancel
// grace, the post-Terminate wait, and the post-ForceKill wait.
type StopPolicy struct {
	// Grace is the drain window after the Adapter Cancel (Interrupt):
	// valid framed events settle within it before termination. The PRD
	// fixes it at 10 seconds.
	Grace time.Duration
	// TerminateWait is the bounded wait after the group Terminate before
	// the force-kill phase (PRD: 2 seconds).
	TerminateWait time.Duration
	// ForceKillWait is the bounded wait after the group ForceKill before
	// the PID/start-token identity facts are inspected.
	ForceKillWait time.Duration
}

// DefaultStopPolicy is the PRD controlled-stop budget: 10 seconds of
// grace, 2 seconds after Terminate, 2 seconds after ForceKill.
func DefaultStopPolicy() StopPolicy {
	return StopPolicy{Grace: 10 * time.Second, TerminateWait: 2 * time.Second, ForceKillWait: 2 * time.Second}
}

// ErrNotReaped is returned by Stop when the identity was still alive
// after the force-kill phase (or the stop context cancelled the wait):
// the caller must inspect the exact PID/start-token identity it recorded
// at start and report the orphan fact when it still matches (design
// 13.2: PID alone is never trusted).
var ErrNotReaped = errors.New("process: not reaped within the stop budget")

// Stop performs the two-phase controlled stop of one supervised process
// group (design 13.3): Interrupt (the Adapter Cancel) and drain valid
// events for the grace window, Terminate the remaining group and wait the
// escalation window, then ForceKill what remains. A cancelled ctx (the
// second Ctrl+C) skips the remaining waits and escalates to ForceKill
// immediately. It returns the final Exit when the process was reaped;
// ErrNotReaped means the identity may still be alive and the caller must
// Inspect it (the orphan path).
func Stop(ctx context.Context, sup Supervisor, h Handle, p StopPolicy) (Exit, error) {
	if err := sup.Signal(ctx, h, Interrupt); err != nil {
		return Exit{}, err
	}
	// The grace window drains valid framed events; the process may exit
	// cooperatively within it (Adapter Cancel).
	if _, err := waitExit(ctx, sup, h, p.Grace); err == nil {
		return sup.Wait(ctx, h)
	}
	if ctx.Err() != nil {
		// Second Ctrl+C: escalation skips Terminate and force-kills.
		return forceKillStop(ctx, sup, h, p)
	}
	if err := sup.Signal(ctx, h, Terminate); err != nil {
		return Exit{}, err
	}
	if _, err := waitExit(ctx, sup, h, p.TerminateWait); err == nil {
		return sup.Wait(ctx, h)
	}
	return forceKillStop(ctx, sup, h, p)
}

// forceKillStop delivers ForceKill and waits the final window.
func forceKillStop(ctx context.Context, sup Supervisor, h Handle, p StopPolicy) (Exit, error) {
	if err := sup.Signal(ctx, h, ForceKill); err != nil {
		return Exit{}, err
	}
	exit, err := waitExit(ctx, sup, h, p.ForceKillWait)
	if err != nil {
		return Exit{}, ErrNotReaped
	}
	return exit, nil
}

// waitExit waits for the process to be reaped within d; a nil error means
// the process exited. A cancelled ctx returns the cancellation so the
// staged protocol escalates.
func waitExit(ctx context.Context, sup Supervisor, h Handle, d time.Duration) (Exit, error) {
	waitCtx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	exit, err := sup.Wait(waitCtx, h)
	if err != nil {
		return Exit{}, err
	}
	return exit, nil
}

// Events is the receive side of the supervisor's framed output pipeline.
// The Supervisor delivers events in order and closes the channel when
// the process has been fully reaped; consumers must drain until close
// (Wait returns only after the channel is closed).
type Events <-chan Event

// EventKind is one event class on the output pipeline.
type EventKind int

const (
	// EventStarted is always the first event: the process identity
	// and process group the consumer must record.
	EventStarted EventKind = iota
	// EventFrameOut/EventFrameErr carry one bounded raw frame.
	EventFrameOut
	EventFrameErr
	// EventEOFOut/EventEOFErr mark the end of a stream, distinct
	// from process exit.
	EventEOFOut
	EventEOFErr
	// EventOverflowOut/EventOverflowErr report bytes dropped because
	// the stream exceeded its budget.
	EventOverflowOut
	EventOverflowErr
)

// Event is one framed pipeline event. Only the fields of the event's
// Kind are meaningful.
type Event struct {
	Kind EventKind
	// Identity and GroupID are set on EventStarted.
	Identity ProcessIdentity
	GroupID  int
	// Frame is set on EventFrame*: bounded raw bytes, never filtered
	// or redacted here. The owning Adapter validates and redacts.
	Frame []byte
	// Count is set on EventOverflow*: total bytes dropped from the
	// stream so far.
	Count int64
}

// ExitFact distinguishes why a process is no longer running (design
// 13.2: timeout, cancel, EOF, and process exit are distinct facts).
type ExitFact int

const (
	// FactProcessExit: the process exited with Code.
	FactProcessExit ExitFact = iota
	// FactSignaled: the process was terminated by Signal.
	FactSignaled
	// FactTimeout: the supervisor's timeout policy fired.
	FactTimeout
	// FactCancelled: the supervisor's context was cancelled.
	FactCancelled
)

// Exit is the typed fact Wait returns once a process is gone.
type Exit struct {
	// Code is the exit code for FactProcessExit and -1 otherwise.
	Code int
	// Fact is why the process is no longer running.
	Fact ExitFact
	// Signal is the terminating signal for FactSignaled.
	Signal int
}

// ProcessIdentity is PID plus the OS Process Start Token. PID alone is
// never trusted: a stale PID with a changed start token is NOT the old
// owner (design 13.2).
type ProcessIdentity struct {
	PID        int
	StartToken uint64
}

// ProcessFact is the inspected truth about one identity.
type ProcessFact struct {
	Identity ProcessIdentity
	// GroupID is the process group of the live process (0 when gone).
	GroupID int
	// Running is true only when a live process exists with exactly
	// this PID and start token.
	Running bool
	// Managed is true while the Supervisor still supervises this
	// identity as a live child.
	Managed bool
}

// Supervisor is the stable process lifecycle seam (design 13.1).
type Supervisor interface {
	// Start launches a process from a shell-free ProcessSpec and
	// returns its handle and framed output pipeline.
	Start(context.Context, ProcessSpec) (Handle, Events, error)
	// StartInteractive launches one native interactive process (a
	// provider terminal: design §9, TUI task 11) with the terminal
	// streams attached directly — no frame parser — while still
	// recording its Process Identity and supporting Stop/Wait/Inspect.
	// The process runs in its own process group.
	StartInteractive(context.Context, InteractiveSpec) (InteractiveHandle, error)
	// Signal delivers sig to the exact process group of Handle.
	Signal(context.Context, Handle, Signal) error
	// Wait blocks until the process is gone and returns its Exit.
	// When ctx expires first, Wait returns ctx.Err(); the process
	// stays managed and is still reaped.
	Wait(context.Context, Handle) (Exit, error)
	// Inspect reports whether an identity is live and still managed.
	Inspect(context.Context, ProcessIdentity) (ProcessFact, error)
}

// Terminal is the inherited stdio triple of one interactive process.
type Terminal struct {
	In  io.Reader
	Out io.Writer
	Err io.Writer
}

// InteractiveSpec is the complete, shell-free description of one native
// interactive process (TUI task 11). It mirrors ProcessSpec but attaches
// the terminal streams directly; the child inherits them instead of
// writing through the framed pipeline.
type InteractiveSpec struct {
	Executable string
	Args       []string
	Dir        string
	Env        map[string]string
	Terminal   Terminal
}

// InteractiveHandle is the handle plus the captured identity of one
// native interactive process.
type InteractiveHandle struct {
	Handle   Handle
	Identity ProcessIdentity
}

// InteractiveProcess is the live native interactive process an Adapter
// starts.
type InteractiveProcess interface {
	Identity() ProcessIdentity
	GroupID() int
}

// interactiveProcess is the supervisor-side state of one interactive
// process (a native provider terminal: TUI task 11). It registers in the
// same managed ledger as a framed process so Signal/Wait/Inspect work
// unchanged, but it carries no framed event channel: the terminal streams
// are inherited directly and no frame parser runs.
type interactiveProcess = managed

// StartInteractive launches a native interactive process through the
// adapter's interactive seam and records its identity in the supervisor
// ledger. The process is fully supervised: Signal/Wait/Inspect work
// exactly as for a framed process.
func (s *supervisor) StartInteractive(ctx context.Context, spec InteractiveSpec) (InteractiveHandle, error) {
	if err := ctx.Err(); err != nil {
		return InteractiveHandle{}, err
	}
	if spec.Executable == "" {
		return InteractiveHandle{}, model.InvalidInputFault("process: interactive executable is required")
	}
	ai, ok := s.adapter.(InteractiveAdapter)
	if !ok {
		return InteractiveHandle{}, model.InvalidInputFault("process: the adapter has no interactive seam")
	}
	s.mu.Lock()
	s.nextID++
	h := Handle{id: s.nextID}
	s.mu.Unlock()
	proc, err := ai.StartInteractive(ctx, h, spec)
	if err != nil {
		return InteractiveHandle{}, err
	}
	m := &managed{
		h:      h,
		proc:   proc,
		exitCh: make(chan struct{}),
		interactive: true,
	}
	s.mu.Lock()
	if s.procs == nil {
		s.procs = map[uint64]*managed{}
	}
	s.procs[h.id] = m
	s.mu.Unlock()
	go s.watchCancel(ctx, m)
	go s.finalize(ctx, m)
	return InteractiveHandle{Handle: h, Identity: proc.Identity()}, nil
}

// InteractiveAdapter is the optional Adapter seam for native interactive
// processes (TUI task 11). A process started through this seam attaches
// the terminal directly and never produces framed events.
type InteractiveAdapter interface {
	StartInteractive(context.Context, Handle, InteractiveSpec) (Process, error)
}

// Process is the live process an Adapter starts. Adapters own the OS
// mechanics; the Supervisor owns lifecycle policy on top of it.
type Process interface {
	Identity() ProcessIdentity
	GroupID() int
	Stdout() io.Reader
	Stderr() io.Reader
}

// Adapter is the OS seam: production uses the OS Adapter, tests use the
// deterministic Fake Adapter (design 13.1, 22.1).
type Adapter interface {
	Start(context.Context, Handle, ProcessSpec) (Process, error)
	Signal(context.Context, Process, Signal) error
	Wait(context.Context, Process) (Exit, error)
	Inspect(context.Context, ProcessIdentity) (ProcessFact, error)
}

// clock is the injectable timer seam: real time in production, virtual
// time in the Fake Adapter (design 22.1).
type clock interface {
	AfterFunc(d time.Duration, f func()) timer
}

type timer interface {
	Stop() bool
}

// realClock is the production clock.
type realClock struct{}

func (realClock) AfterFunc(d time.Duration, f func()) timer {
	return time.AfterFunc(d, f)
}

// clockProvider lets an Adapter hand its own clock to the Supervisor so
// that NewSupervisor(NewFakeAdapter()) still runs on virtual time.
type clockProvider interface {
	processClock() clock
}

// supervisor implements the process lifecycle policy over an Adapter.
type supervisor struct {
	adapter Adapter
	clock   clock

	mu     sync.Mutex
	nextID uint64
	procs  map[uint64]*managed
}

// managed is the supervisor-side state of one supervised process.
type managed struct {
	h      Handle
	spec   ProcessSpec
	proc   Process
	events chan Event

	// interactive marks a native interactive process (TUI task 11): its
	// terminal streams are inherited directly, so no frame pumps run and
	// no events channel exists.
	interactive bool

	exitCh   chan struct{}
	outDone  chan struct{}
	errDone  chan struct{}
	timeout  timer
	timedOut bool
	canceled bool
	exit     Exit
	err      error
}

// NewSupervisor wires an Adapter into a Supervisor. NewFakeSupervisor is
// the deterministic test pairing; NewSupervisor(NewOSAdapter()) is the
// production path.
func NewSupervisor(a Adapter) Supervisor {
	s := &supervisor{adapter: a, clock: realClock{}, procs: map[uint64]*managed{}}
	if cp, ok := a.(clockProvider); ok {
		s.clock = cp.processClock()
	}
	return s
}

// Start launches one supervised process (design 13.1/13.2): argv-only
// construction, an isolated process group, bounded framed output, and a
// typed identity. The first event is EventStarted; the final Exit fact
// comes from Wait.
func (s *supervisor) Start(ctx context.Context, spec ProcessSpec) (Handle, Events, error) {
	if err := validateSpec(spec); err != nil {
		return Handle{}, nil, err
	}
	if err := ctx.Err(); err != nil {
		return Handle{}, nil, err
	}
	s.mu.Lock()
	s.nextID++
	h := Handle{id: s.nextID}
	s.mu.Unlock()

	events := make(chan Event, eventBufferSize)
	proc, err := s.adapter.Start(ctx, h, spec)
	if err != nil {
		return Handle{}, nil, err
	}

	m := &managed{
		h:       h,
		spec:    spec,
		proc:    proc,
		events:  events,
		exitCh:  make(chan struct{}),
		outDone: make(chan struct{}),
		errDone: make(chan struct{}),
	}
	s.mu.Lock()
	s.procs[h.id] = m
	s.mu.Unlock()

	// The identity is the first fact a consumer learns: PID plus OS
	// Process Start Token, never PID alone.
	events <- Event{Kind: EventStarted, Identity: proc.Identity(), GroupID: proc.GroupID()}

	if spec.Timeout > 0 {
		m.timeout = s.clock.AfterFunc(spec.Timeout, func() {
			s.mu.Lock()
			m.timedOut = true
			s.mu.Unlock()
			_ = s.adapter.Signal(ctx, proc, Terminate)
		})
	}
	go s.watchCancel(ctx, m)
	go s.pumpStream(m, EventFrameOut, EventEOFOut, EventOverflowOut, proc.Stdout(), m.outDone)
	go s.pumpStream(m, EventFrameErr, EventEOFErr, EventOverflowErr, proc.Stderr(), m.errDone)
	go s.finalize(ctx, m)
	return h, events, nil
}

// validateSpec rejects specs that cannot be launched safely: the
// executable must be absolute (argv-only, never a shell string) and all
// limits must be sane.
func validateSpec(spec ProcessSpec) error {
	if spec.Executable == "" {
		return model.InvalidInputFault("process spec: executable is empty")
	}
	if !filepath.IsAbs(spec.Executable) {
		return model.InvalidInputFault("process spec: executable must be an absolute path")
	}
	if spec.Dir != "" && !filepath.IsAbs(spec.Dir) {
		return model.InvalidInputFault("process spec: working directory must be an absolute path")
	}
	if spec.MaxFrameBytes < 0 {
		return model.InvalidInputFault("process spec: MaxFrameBytes must not be negative")
	}
	if spec.MaxOutputBytes < 0 {
		return model.InvalidInputFault("process spec: MaxOutputBytes must not be negative")
	}
	if spec.Timeout < 0 {
		return model.InvalidInputFault("process spec: Timeout must not be negative")
	}
	if spec.Group != GroupIsolated && spec.Group != GroupInherit {
		return model.InvalidInputFault("process spec: unknown process-group policy")
	}
	return nil
}

// watchCancel turns context cancellation into the supervisor's Terminate
// policy and stops the timeout timer once the process is reaped.
func (s *supervisor) watchCancel(ctx context.Context, m *managed) {
	select {
	case <-ctx.Done():
		if m.timeout != nil {
			m.timeout.Stop()
		}
		s.mu.Lock()
		m.canceled = true
		s.mu.Unlock()
		_ = s.adapter.Signal(ctx, m.proc, Terminate)
	case <-m.exitCh:
		if m.timeout != nil {
			m.timeout.Stop()
		}
	}
}

// finalize reaps the process unconditionally, attributes the exit fact
// (timeout or cancel overlay), waits for the framed streams to drain,
// then closes the events channel and releases Wait.
func (s *supervisor) finalize(ctx context.Context, m *managed) {
	// Reaping must never be skipped: the process is reaped even when
	// the Start context was cancelled.
	exit, err := s.adapter.Wait(context.Background(), m.proc)

	s.mu.Lock()
	if ctx.Err() != nil {
		m.canceled = true
	}
	if m.canceled {
		exit.Fact = FactCancelled
	} else if m.timedOut {
		exit.Fact = FactTimeout
	}
	m.exit = exit
	m.err = err
	s.mu.Unlock()

	if m.interactive {
		// A native interactive process: the terminal streams were
		// inherited directly, so there are no framed streams to drain
		// and no events channel to close.
		close(m.exitCh)
		return
	}
	<-m.outDone
	<-m.errDone
	if sc, ok := m.proc.(streamCloser); ok {
		sc.closeStreams()
	}
	close(m.events)
	close(m.exitCh)
}

// Signal delivers sig to the exact process group of h (design 13.3
// primitives: Terminate, then ForceKill after the wait budget).
func (s *supervisor) Signal(ctx context.Context, h Handle, sig Signal) error {
	m := s.lookup(h)
	if m == nil {
		return model.InvalidInputFault("process: unknown handle")
	}
	switch sig {
	case Interrupt, Terminate, ForceKill:
	default:
		return model.InvalidInputFault("process: invalid signal")
	}
	return s.adapter.Signal(ctx, m.proc, sig)
}

// Wait returns the Exit fact once the process is fully reaped, or
// ctx.Err() when the context expires first.
func (s *supervisor) Wait(ctx context.Context, h Handle) (Exit, error) {
	m := s.lookup(h)
	if m == nil {
		return Exit{}, model.InvalidInputFault("process: unknown handle")
	}
	select {
	case <-m.exitCh:
		s.mu.Lock()
		defer s.mu.Unlock()
		return m.exit, m.err
	case <-ctx.Done():
		return Exit{}, ctx.Err()
	}
}

// Inspect reports the live and managed truth about an identity. A stale
// PID with a changed start token is never the old owner.
func (s *supervisor) Inspect(ctx context.Context, id ProcessIdentity) (ProcessFact, error) {
	fact, err := s.adapter.Inspect(ctx, id)
	if err != nil {
		return fact, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.procs {
		if m.proc.Identity() == id {
			select {
			case <-m.exitCh: // already reaped
			default:
				fact.Managed = true
			}
			break
		}
	}
	return fact, nil
}

func (s *supervisor) lookup(h Handle) *managed {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.procs[h.id]
}

// pumpStream reads one stream, frames it into bounded frames, enforces
// the total byte budget, and delivers events in order. Raw bytes exist
// only in bounded memory: each delivered frame is a fresh copy, and the
// events channel itself applies backpressure to the reader.
func (s *supervisor) pumpStream(m *managed, kind, eofKind, overflowKind EventKind, r io.Reader, done chan struct{}) {
	defer close(done)
	maxFrame := int(m.spec.MaxFrameBytes)
	if maxFrame <= 0 {
		maxFrame = defaultMaxFrameBytes
	}
	remaining := m.spec.MaxOutputBytes
	if remaining <= 0 {
		remaining = defaultMaxOutputBytes
	}
	dropped := int64(0)
	overflowed := false
	var frame []byte

	emit := func(f []byte) {
		if int64(len(f)) > remaining {
			// The budget is exhausted: everything from here on is
			// dropped and reported as one overflow fact.
			overflowed = true
			dropped += int64(len(f))
			remaining = 0
			return
		}
		remaining -= int64(len(f))
		m.events <- Event{Kind: kind, Frame: bytes.Clone(f)}
	}

	buf := make([]byte, 32*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if overflowed {
				dropped += int64(n)
			} else {
				frame = append(frame, buf[:n]...)
				for {
					i := bytes.IndexByte(frame, '\n')
					if i >= 0 && i <= maxFrame {
						line := frame[:i]
						frame = frame[i+1:]
						if len(line) > 0 {
							emit(line)
						}
						continue
					}
					if len(frame) >= maxFrame {
						emit(frame[:maxFrame])
						frame = frame[maxFrame:]
						continue
					}
					break
				}
				if overflowed && len(frame) > 0 {
					dropped += int64(len(frame))
					frame = frame[:0]
				}
			}
		}
		if err != nil {
			if !overflowed && len(frame) > 0 {
				emit(frame)
			} else if len(frame) > 0 {
				dropped += int64(len(frame))
			}
			if overflowed {
				m.events <- Event{Kind: overflowKind, Count: dropped}
			}
			if err == io.EOF {
				m.events <- Event{Kind: eofKind}
			}
			return
		}
	}
}
