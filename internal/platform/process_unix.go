//go:build darwin || linux

// Process identity and process-group primitives (design 13.2): liveness,
// group-wide signaling, and the current process's own identity. Start
// tokens and process groups are OS-specific (process_linux.go,
// process_darwin.go); this file is the shared surface.
package platform

import (
	"fmt"
	"os"
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

// SelfIdentity returns the current process PID and its start token.
func SelfIdentity() (int, uint64, error) {
	pid := os.Getpid()
	token, err := StartToken(pid)
	return pid, token, err
}
