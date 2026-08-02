//go:build linux

package artifact

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// renameNoReplace atomically renames oldpath onto newpath without
// replacing an existing target (RENAME_NOREPLACE), so the existing-path
// rejection of the write protocol holds even under a rename race.
func renameNoReplace(oldpath, newpath string) error {
	if err := unix.Renameat2(unix.AT_FDCWD, oldpath, unix.AT_FDCWD, newpath, unix.RENAME_NOREPLACE); err != nil {
		if errors.Is(err, unix.EEXIST) {
			return os.ErrExist
		}
		return err
	}
	return nil
}
