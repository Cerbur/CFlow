//go:build !linux && !darwin

package platform

import (
	"fmt"

	"cflow.local/cflow/internal/model"
)

// AtomicRenameNoReplace fails closed where the OS has no supported atomic
// no-replace rename primitive.
func AtomicRenameNoReplace(source, destination string) error {
	return model.InvariantFault(fmt.Errorf("atomic no-replace rename is unsupported on this platform"))
}
