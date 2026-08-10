package cli

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync/atomic"
	"time"

	"cflow.local/cflow/internal/model"
)

var runtimeIDFallback atomic.Uint64

// runtimeIDSource produces opaque IDs that remain unique when a new CLI
// process opens the same CFLOW_HOME. SequentialIDSource is intentionally
// reserved for deterministic tests and fixtures.
func runtimeIDSource() model.IDSource {
	return func(kind model.IDKind) string {
		var raw [16]byte
		if _, err := rand.Read(raw[:]); err == nil {
			return fmt.Sprintf("%s-%s", kind, hex.EncodeToString(raw[:]))
		}
		// crypto/rand failure is exceptionally unlikely, but ID allocation
		// must still never return an empty or predictably reused identity.
		n := runtimeIDFallback.Add(1)
		return fmt.Sprintf("%s-%x-%x", kind, time.Now().UnixNano(), n)
	}
}
