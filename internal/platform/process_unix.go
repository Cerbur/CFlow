//go:build darwin || linux

// Process identity and process-group primitives (design 13.2): liveness,
// group-wide signaling, and the current process's own identity. Start
// tokens and process groups are OS-specific (process_linux.go,
// process_darwin.go); this file is the shared surface.
package platform

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/sys/unix"
)

// Alive reports whether a live process with this PID exists. A reaped
// process is not alive; a zombie briefly is until its parent reaps it.
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return unix.Kill(pid, 0) == nil
}

// KillGroup sends sig to every process in the group pgid. A group that
// no longer exists is already gone: nil.
func KillGroup(pgid int, sig syscall.Signal) error {
	if pgid <= 0 {
		return fmt.Errorf("platform: invalid process group %d", pgid)
	}
	if err := unix.Kill(-pgid, sig); err == unix.ESRCH {
		return nil
	} else {
		return err
	}
}

// TerminalProcessGroup returns the foreground process group attached to fd.
func TerminalProcessGroup(fd int) (int, error) {
	return unix.IoctlGetInt(fd, unix.TIOCGPGRP)
}

// SetTerminalProcessGroup transfers foreground terminal ownership to pgid.
func SetTerminalProcessGroup(fd, pgid int) error {
	if pgid <= 0 {
		return fmt.Errorf("platform: invalid foreground process group %d", pgid)
	}
	// The caller is normally in the background after the child exits.
	// POSIX terminals otherwise deliver SIGTTOU to a background process
	// that changes the foreground process group. CFlow owns this terminal
	// handoff, so keep SIGTTOU ignored for the process lifetime just as an
	// interactive shell does.
	signal.Ignore(syscall.SIGTTOU)
	return unix.IoctlSetPointerInt(fd, unix.TIOCSPGRP, pgid)
}

// SelfIdentity returns the current process PID and its start token.
func SelfIdentity() (int, uint64, error) {
	pid := os.Getpid()
	token, err := StartToken(pid)
	return pid, token, err
}
