//go:build linux

package platform

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// AtomicRenameNoReplace atomically moves source to destination and never
// overwrites an existing destination.
func AtomicRenameNoReplace(source, destination string) error {
	if err := unix.Renameat2(unix.AT_FDCWD, source, unix.AT_FDCWD, destination, unix.RENAME_NOREPLACE); err != nil {
		if errors.Is(err, unix.EEXIST) {
			return os.ErrExist
		}
		return err
	}
	return nil
}
