//go:build darwin

package security

import (
	"errors"
	"os"
	"syscall"
)

// errUnprovenSemantics is the sentinel for filesystem probes that cannot
// prove local POSIX ownership and advisory-lock semantics.
var errUnprovenSemantics = errors.New("unproven semantics")

// probeLocalFileSystem reports the mounted filesystem type name of path
// when it is a known-local POSIX filesystem. Network, virtual, and
// unknown filesystems fail closed (PRD 约束 113): their POSIX ownership
// and lock semantics cannot be proven. This is a bounded, documented
// probe of the local filesystem, not a general mount audit.
func probeLocalFileSystem(path string) (string, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return "", err
	}
	name := stringFromInt8(st.Fstypename[:])
	switch name {
	case "apfs", "hfs":
		// Local macOS volumes with real POSIX ownership and flock(2)
		// advisory locks. "smbfs", "nfs", "afpfs", "autofs", and
		// "fuse"-style mounts are deliberately absent.
		return name, nil
	}
	return name, errUnprovenSemantics
}

// probeAdvisoryLock proves the filesystem honors POSIX advisory locks:
// two separate opens of the same inode must conflict. The probe holds no
// lock after returning and never creates anything.
func probeAdvisoryLock(path string) error {
	first, err := os.Open(path)
	if err != nil {
		return err
	}
	defer first.Close()
	if err := syscall.Flock(int(first.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return err
	}
	second, err := os.Open(path)
	if err != nil {
		return err
	}
	defer second.Close()
	err = syscall.Flock(int(second.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		return errors.New("second locker succeeded: advisory locking not enforced")
	}
	if err != syscall.EWOULDBLOCK {
		return err
	}
	return nil
}

// ownerUID extracts the owning uid from the platform stat.
func ownerUID(info os.FileInfo) int {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return -1
	}
	return int(st.Uid)
}

// stringFromInt8 renders a fixed-size C string.
func stringFromInt8(b []int8) string {
	out := make([]byte, 0, len(b))
	for _, c := range b {
		if c == 0 {
			break
		}
		out = append(out, byte(c))
	}
	return string(out)
}
