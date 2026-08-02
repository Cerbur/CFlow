//go:build linux

package security

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// errUnprovenSemantics is the sentinel for filesystem probes that cannot
// prove local POSIX ownership and advisory-lock semantics.
var errUnprovenSemantics = errors.New("unproven semantics")

// knownLocalFilesystems maps statfs magic numbers of filesystems whose
// POSIX ownership and flock(2) semantics CFlow can prove. ext2/3/4 share
// one magic. Network filesystems (NFS, CIFS/SMB), FUSE mounts, and
// anything unknown are deliberately absent: they fail closed (PRD 约束
// 113). This is a bounded, documented probe, not a general mount audit.
var knownLocalFilesystems = map[int64]string{
	0xEF53:     "ext4", // ext2/ext3/ext4
	0x9123683E: "btrfs",
	0x58465342: "xfs",
	0x2FC12FC1: "zfs",
	0x01021994: "tmpfs",
	0x794C7630: "overlayfs",
}

// probeLocalFileSystem reports the mounted filesystem type name of path
// when it is a known-local POSIX filesystem.
func probeLocalFileSystem(path string) (string, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return "", err
	}
	if name, ok := knownLocalFilesystems[st.Type]; ok {
		return name, nil
	}
	return fmt.Sprintf("type-0x%x", st.Type), errUnprovenSemantics
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
