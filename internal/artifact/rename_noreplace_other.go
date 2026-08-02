//go:build !linux && !darwin

package artifact

import (
	"errors"
	"os"
)

// renameNoReplace falls back to os.Rename on platforms without a
// no-replace rename. The Demo targets darwin and linux (design 23), where
// the atomic no-replace rename is used; the pre-write existence check
// remains the conflict guard on other platforms.
func renameNoReplace(oldpath, newpath string) error {
	if err := os.Rename(oldpath, newpath); err != nil {
		if errors.Is(err, os.ErrExist) {
			return os.ErrExist
		}
		return err
	}
	return nil
}
