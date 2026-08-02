//go:build darwin

package artifact

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// renameNoReplace atomically renames oldpath onto newpath without
// replacing an existing target (RENAME_EXCL), so the existing-path
// rejection of the write protocol holds even under a rename race.
func renameNoReplace(oldpath, newpath string) error {
	if err := unix.RenamexNp(oldpath, newpath, unix.RENAME_EXCL); err != nil {
		if errors.Is(err, unix.EEXIST) {
			return os.ErrExist
		}
		return err
	}
	return nil
}
