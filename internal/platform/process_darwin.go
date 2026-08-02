//go:build darwin

package platform

import "golang.org/x/sys/unix"

// StartToken returns the kernel start time of a live process in
// microseconds since boot (sysctl kern.proc.pid). The token is stable
// for the process lifetime and changes when the PID is reused.
func StartToken(pid int) (uint64, error) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return 0, err
	}
	t := kp.Proc.P_starttime
	return uint64(t.Sec)*1_000_000 + uint64(t.Usec), nil
}

// ProcessGroup returns the process-group ID of a live process.
func ProcessGroup(pid int) (int, error) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return 0, err
	}
	return int(kp.Eproc.Pgid), nil
}
