//go:build !darwin && !linux

package security

import (
	"errors"
	"os"
)

// errUnprovenSemantics is the sentinel for filesystem probes that cannot
// prove local POSIX ownership and advisory-lock semantics.
var errUnprovenSemantics = errors.New("unproven semantics")

// CFlow Demo targets POSIX filesystems (design 23). On any other
// platform the probes cannot prove owner-only permissions or advisory
// locks, so CheckHome fails closed; CheckPath still enforces the
// owner/mode/symlink checks via the portable stat path.

func probeLocalFileSystem(path string) (string, error) {
	return "", errUnprovenSemantics
}

func probeAdvisoryLock(path string) error {
	return errUnprovenSemantics
}

func ownerUID(info os.FileInfo) int {
	return -1
}
