// Package model defines the canonical CFlow domain aggregate types and
// invariants (CONTEXT.md, design 7 and 8). It is pure: no function in this
// package imports database/sql, os, os/exec, path/filepath, or any CFlow
// infrastructure package. The model contains no clock, randomness, or
// filesystem access; timestamps arrive through State.Now injected by the
// caller, and opaque IDs arrive through Inputs or are derived
// deterministically from the aggregate.
package model

import (
	"fmt"
	"sync/atomic"
)

// ID kinds recognised by the injected ID source. The model never generates
// IDs itself; the Application injects a deterministic IDSource in tests
// (design 7.2, 22.2).
type IDKind string

const (
	IDProject        IDKind = "project"
	IDWorkflow       IDKind = "workflow"
	IDNode           IDKind = "node"
	IDSession        IDKind = "session"
	IDRun            IDKind = "run"
	IDProcess        IDKind = "process"
	IDFinding        IDKind = "finding"
	IDApproval       IDKind = "approval"
	IDApplyAttempt   IDKind = "apply"
	IDCleanupAttempt IDKind = "cleanup"
)

// IDSource produces opaque, locally generated IDs. It must be
// deterministic when injected into tests.
type IDSource func(IDKind) string

// SequentialIDSource returns a deterministic IDSource producing opaque
// sequential IDs (design 22.2 fixture protocol). The counter is atomic:
// the live parallel dispatch (Task 16) allocates Attempt/Session IDs
// from concurrent node chains, and the source must stay race-free. The
// order in which concurrent chains receive their IDs is not part of any
// fixture contract.
func SequentialIDSource() IDSource {
	var n atomic.Uint64
	return func(kind IDKind) string {
		return fmt.Sprintf("%s-%d", kind, n.Add(1))
	}
}

// The opaque identity types. IDs are locally generated, never derived from
// mutable display names, and stable across Workflow Revisions (design 7.2).
type (
	ProjectID        string
	WorkflowID       string
	NodeID           string
	SessionID        string
	RunID            string
	ProcessID        string
	FindingID        string
	ApprovalID       string
	ApplyAttemptID   string
	CleanupAttemptID string
)

// Valid reports whether an opaque ID is present. The zero value is never a
// valid identity.
func (id ProjectID) Valid() bool { return id != "" }

// Valid reports whether an opaque ID is present.
func (id WorkflowID) Valid() bool { return id != "" }

// Valid reports whether an opaque ID is present.
func (id NodeID) Valid() bool { return id != "" }

// Valid reports whether an opaque ID is present.
func (id SessionID) Valid() bool { return id != "" }

// Valid reports whether an opaque ID is present.
func (id RunID) Valid() bool { return id != "" }

// Valid reports whether an opaque ID is present.
func (id ProcessID) Valid() bool { return id != "" }

// Valid reports whether an opaque ID is present.
func (id FindingID) Valid() bool { return id != "" }

// Valid reports whether an opaque ID is present.
func (id ApprovalID) Valid() bool { return id != "" }

// Valid reports whether an opaque ID is present.
func (id ApplyAttemptID) Valid() bool { return id != "" }

// Valid reports whether an opaque ID is present.
func (id CleanupAttemptID) Valid() bool { return id != "" }

// AttemptNumber numbers the Attempts of one Node starting at 1. Attempt
// identity is (node_id, attempt_number) and is never reused (design 7.2).
type AttemptNumber int

// String renders the attempt number.
func (n AttemptNumber) String() string { return fmt.Sprintf("%d", int(n)) }

// AttemptKey identifies one immutable execution record of a Node.
type AttemptKey struct {
	Node   NodeID
	Number AttemptNumber
}

// String renders the key as node#number.
func (k AttemptKey) String() string { return fmt.Sprintf("%s#%d", k.Node, k.Number) }
