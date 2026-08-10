package process

// OSAdapter owns the OS mechanics of one supervised process (design
// 13.2): argv-only launch, an isolated process group, PID plus OS
// Process Start Token identity, and group-wide signaling. It never
// invokes a shell and never builds a command line string: exec.Cmd is
// constructed only from Executable and Args.
//
// CFlow lock descriptors never cross exec: the platform LockSet opens
// its files with O_CLOEXEC, and Go's exec honors it. Streams are
// captured with manually created pipes whose read ends the Supervisor
// owns, so no read-end closing race can discard buffered output.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"sync"
	"syscall"

	"cflow.local/cflow/internal/platform"
)

// OSAdapter is the production Adapter.
type OSAdapter struct{}

// NewOSAdapter returns the production Adapter.
func NewOSAdapter() *OSAdapter {
	return &OSAdapter{}
}

// osProcess is one live OS process under supervision.
type osProcess struct {
	cmd      *exec.Cmd
	identity ProcessIdentity
	pgid     int
	outR     *os.File
	errR     *os.File
	terminal *terminalHandoff

	waitOnce sync.Once
	exit     Exit
	waitErr  error
}

type terminalHandoff struct {
	fd           int
	previousPgid int
}

func (h *terminalHandoff) restore() error {
	if h == nil {
		return nil
	}
	return platform.SetTerminalProcessGroup(h.fd, h.previousPgid)
}

func (p *osProcess) Identity() ProcessIdentity { return p.identity }
func (p *osProcess) GroupID() int              { return p.pgid }
func (p *osProcess) Stdout() io.Reader         { return p.outR }
func (p *osProcess) Stderr() io.Reader         { return p.errR }

// closeStreams releases the read ends after the framed streams have
// drained. The Supervisor calls it once both pumps finished.
func (p *osProcess) closeStreams() {
	_ = p.outR.Close()
	_ = p.errR.Close()
}

// streamCloser is the internal seam for releasing stream resources.
type streamCloser interface {
	closeStreams()
}

// Start launches the process from the spec. The identity is captured at
// launch: PID plus the OS start token, never PID alone.
func (a *OSAdapter) Start(ctx context.Context, h Handle, spec ProcessSpec) (Process, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// argv-only construction: the spec's Executable and Args are passed
	// verbatim; no command line is ever assembled.
	cmd := exec.Command(spec.Executable, spec.Args...)
	cmd.Dir = spec.Dir
	cmd.Env = envSlice(spec.Env)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: spec.Group == GroupIsolated}

	var stdinW io.WriteCloser
	if spec.Stdin != nil {
		in, err := cmd.StdinPipe()
		if err != nil {
			return nil, err
		}
		stdinW = in
	}

	outR, outW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		_ = outR.Close()
		_ = outW.Close()
		return nil, err
	}
	cmd.Stdout = outW
	cmd.Stderr = errW

	if err := cmd.Start(); err != nil {
		_ = outR.Close()
		_ = outW.Close()
		_ = errR.Close()
		_ = errW.Close()
		if stdinW != nil {
			_ = stdinW.Close()
		}
		return nil, fmt.Errorf("process: start %s: %w", spec.Executable, err)
	}
	// The child holds its own copies; the parent ends must close now so
	// the pumps see EOF exactly when the child (and its descendants)
	// close theirs.
	_ = outW.Close()
	_ = errW.Close()
	if stdinW != nil {
		go func() {
			_, _ = io.Copy(stdinW, spec.Stdin)
			_ = stdinW.Close()
		}()
	}

	pid := cmd.Process.Pid
	token, err := platform.StartToken(pid)
	if err != nil {
		_ = platform.KillGroup(pid, syscall.SIGKILL)
		_ = cmd.Wait()
		return nil, fmt.Errorf("process: read start token for pid %d: %w", pid, err)
	}
	pgid := pid
	if spec.Group == GroupInherit {
		if pgid, err = platform.ProcessGroup(pid); err != nil {
			_ = platform.KillGroup(pid, syscall.SIGKILL)
			_ = cmd.Wait()
			return nil, fmt.Errorf("process: read process group for pid %d: %w", pid, err)
		}
	}
	return &osProcess{
		cmd:      cmd,
		identity: ProcessIdentity{PID: pid, StartToken: token},
		pgid:     pgid,
		outR:     outR,
		errR:     errR,
	}, nil
}

// envSlice renders the explicit environment map deterministically. When
// the spec carries no environment, the child gets an empty one: parent
// values never leak into supervised processes.
func envSlice(env map[string]string) []string {
	if len(env) == 0 {
		return []string{}
	}
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

// StartInteractive launches one native interactive process (a provider
// terminal, TUI task 11): the child inherits the terminal streams
// directly (cmd.Stdin/Stdout/Stderr attached to the Terminal triple), so
// no frame parser runs. The process is isolated in its own process group;
// when the inherited input is a real terminal, foreground ownership is
// transferred to the child for the duration of the turn and restored after
// Wait. The Supervisor still supports Stop/Wait/Inspect.
func (a *OSAdapter) StartInteractive(ctx context.Context, h Handle, spec InteractiveSpec) (Process, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var handoff *terminalHandoff
	var terminalFD int
	if f, ok := spec.Terminal.In.(*os.File); ok {
		previousPgid, ioctlErr := platform.TerminalProcessGroup(int(f.Fd()))
		if ioctlErr == nil {
			terminalFD = int(f.Fd())
			handoff = &terminalHandoff{fd: terminalFD, previousPgid: previousPgid}
		}
	}
	cmd := exec.Command(spec.Executable, spec.Args...)
	cmd.Dir = spec.Dir
	cmd.Env = envSlice(spec.Env)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if handoff != nil {
		// Foreground asks os.StartProcess to create the isolated process
		// group and assign it the controlling terminal during child
		// startup. Doing this in the child avoids the parent/child race
		// where an interactive provider can read before the handoff.
		cmd.SysProcAttr.Foreground = true
		cmd.SysProcAttr.Ctty = terminalFD
	}
	if spec.Terminal.In != nil {
		cmd.Stdin = spec.Terminal.In
	}
	if spec.Terminal.Out != nil {
		cmd.Stdout = spec.Terminal.Out
	}
	if spec.Terminal.Err != nil {
		cmd.Stderr = spec.Terminal.Err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("process: start interactive %s: %w", spec.Executable, err)
	}
	pid := cmd.Process.Pid
	token, err := platform.StartToken(pid)
	if err != nil {
		_ = platform.KillGroup(pid, syscall.SIGKILL)
		_ = cmd.Wait()
		_ = handoff.restore()
		return nil, fmt.Errorf("process: read start token for pid %d: %w", pid, err)
	}
	return &osProcess{
		cmd:      cmd,
		identity: ProcessIdentity{PID: pid, StartToken: token},
		pgid:     pid,
		terminal: handoff,
	}, nil
}

// Signal delivers sig to the exact process group of the process. A group
// that no longer exists is already gone: nil.
func (a *OSAdapter) Signal(ctx context.Context, p Process, sig Signal) error {
	op := p.(*osProcess)
	return platform.KillGroup(op.pgid, signalOf(sig))
}

// signalOf maps the supervisor signal to the platform signal.
func signalOf(sig Signal) syscall.Signal {
	switch sig {
	case Interrupt:
		return syscall.SIGINT
	case ForceKill:
		return syscall.SIGKILL
	default:
		return syscall.SIGTERM
	}
}

// Wait reaps the process and reports the raw exit fact. The process is
// always reaped; the context only aborts the wait. A non-zero or
// signal-killed exit is a fact (FactProcessExit/FactSignaled), not an
// error: the error is reserved for reaping failures.
func (a *OSAdapter) Wait(ctx context.Context, p Process) (Exit, error) {
	op := p.(*osProcess)
	op.waitOnce.Do(func() {
		waitErr := op.cmd.Wait()
		op.exit = mapExit(waitErr)
		restoreErr := op.terminal.restore()
		if waitErr != nil {
			var ee *exec.ExitError
			if !errors.As(waitErr, &ee) {
				op.waitErr = waitErr
			}
		}
		if restoreErr != nil && op.waitErr == nil {
			op.waitErr = fmt.Errorf("process: restore terminal foreground process group: %w", restoreErr)
		}
	})
	return op.exit, op.waitErr
}

// mapExit turns a cmd.Wait error into typed exit facts (design 13.2:
// process exit and signal termination are distinct).
func mapExit(err error) Exit {
	if err == nil {
		return Exit{Code: 0, Fact: FactProcessExit}
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return Exit{Code: -1, Fact: FactSignaled, Signal: int(ws.Signal())}
		}
		return Exit{Code: ee.ExitCode(), Fact: FactProcessExit}
	}
	return Exit{Code: -1, Fact: FactProcessExit}
}

// Inspect reports the live truth about an identity. A stale PID with a
// changed start token is not the old owner.
func (a *OSAdapter) Inspect(ctx context.Context, id ProcessIdentity) (ProcessFact, error) {
	fact := ProcessFact{Identity: id}
	if id.PID <= 0 || !platform.Alive(id.PID) {
		return fact, nil
	}
	token, err := platform.StartToken(id.PID)
	if err != nil {
		return fact, err
	}
	if token != id.StartToken {
		return fact, nil
	}
	fact.Running = true
	pgid, err := platform.ProcessGroup(id.PID)
	if err != nil {
		return fact, err
	}
	fact.GroupID = pgid
	return fact, nil
}
