package model

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"time"
)

// ---------------------------------------------------------------------------
// Apply and Cleanup
// ---------------------------------------------------------------------------

// ApplyStatus is the status of one Apply Attempt: a post-completion,
// user-initiated delivery attempt that revalidates Integration output and
// may fast-forward the Target Branch (CONTEXT.md: Apply).
type ApplyStatus string

const (
	ApplyStaging              ApplyStatus = "STAGING"
	ApplyAwaitingConfirmation ApplyStatus = "AWAITING_CONFIRMATION"
	ApplyRunning              ApplyStatus = "RUNNING"
	ApplySucceeded            ApplyStatus = "SUCCEEDED"
	ApplyFailed               ApplyStatus = "FAILED"
	ApplyBlocked              ApplyStatus = "BLOCKED"
	ApplyCancelled            ApplyStatus = "CANCELLED"
)

// Valid reports whether s is a declared Apply Status.
func (s ApplyStatus) Valid() bool {
	switch s {
	case ApplyStaging, ApplyAwaitingConfirmation, ApplyRunning,
		ApplySucceeded, ApplyFailed, ApplyBlocked, ApplyCancelled:
		return true
	}
	return false
}

// String renders the Apply Status.
func (s ApplyStatus) String() string { return string(s) }

// ApplyAttempt is one Apply execution record. Its confirmation binds the
// Apply Attempt, the Target HEAD, the Integration HEAD, and the exact
// Preflight Revision/hash/fingerprint; a completed Workflow's state is
// never altered by Apply (PRD 约束 39-41).
type ApplyAttempt struct {
	ID              ApplyAttemptID
	Status          ApplyStatus
	TargetHead      string
	IntegrationHead string
	Preflight       ArtifactRef
	PreflightHash   string
	Fingerprint     string
	StartedAt       time.Time
	EndedAt         time.Time
}

// CleanupStatus is the status of one Cleanup Attempt. Cleanup first
// produces an immutable Dry Run Manifest; execution requires a second
// confirmation binding the exact Manifest identity and hash, then
// revalidates every item's facts (design 17.4).
type CleanupStatus string

const (
	CleanupStatusDryRun               CleanupStatus = "DRY_RUN"
	CleanupStatusAwaitingConfirmation CleanupStatus = "AWAITING_CONFIRMATION"
	CleanupStatusRunning              CleanupStatus = "RUNNING"
	CleanupStatusSucceeded            CleanupStatus = "SUCCEEDED"
	CleanupStatusFailed               CleanupStatus = "FAILED"
	CleanupStatusBlocked              CleanupStatus = "BLOCKED"
	CleanupStatusCancelled            CleanupStatus = "CANCELLED"
)

// Valid reports whether s is a declared Cleanup Status.
func (s CleanupStatus) Valid() bool {
	switch s {
	case CleanupStatusDryRun, CleanupStatusAwaitingConfirmation, CleanupStatusRunning,
		CleanupStatusSucceeded, CleanupStatusFailed, CleanupStatusBlocked, CleanupStatusCancelled:
		return true
	}
	return false
}

// String renders the Cleanup Status.
func (s CleanupStatus) String() string { return string(s) }

// CleanupItemKind distinguishes managed Worktrees from scratch paths.
type CleanupItemKind string

const (
	CleanupWorktree CleanupItemKind = "worktree"
	CleanupScratch  CleanupItemKind = "scratch"
)

// Valid reports whether k is a declared Cleanup Item Kind.
func (k CleanupItemKind) Valid() bool {
	switch k {
	case CleanupWorktree, CleanupScratch:
		return true
	}
	return false
}

// String renders the Cleanup Item Kind.
func (k CleanupItemKind) String() string { return string(k) }

// CleanupItemStatus is one item's independent result. Partial completion
// is explicit and retryable without expanding the confirmed target set
// (design 17.4).
type CleanupItemStatus string

const (
	CleanupItemPending   CleanupItemStatus = "PENDING"
	CleanupItemRequested CleanupItemStatus = "REQUESTED"
	CleanupItemCompleted CleanupItemStatus = "COMPLETED"
	CleanupItemFailed    CleanupItemStatus = "FAILED"
)

// Valid reports whether s is a declared Cleanup Item Status.
func (s CleanupItemStatus) Valid() bool {
	switch s {
	case CleanupItemPending, CleanupItemRequested, CleanupItemCompleted, CleanupItemFailed:
		return true
	}
	return false
}

// String renders the Cleanup Item Status.
func (s CleanupItemStatus) String() string { return string(s) }

// IsTerminal reports whether the item result is fixed. Terminal items are
// never reopened; only pending items of the same confirmed Manifest may be
// retried.
func (s CleanupItemStatus) IsTerminal() bool {
	return s == CleanupItemCompleted || s == CleanupItemFailed
}

// CleanupItem is one manifest target with the facts the execution
// revalidation must match exactly (canonical path, Branch, expected HEAD,
// Dirty Fingerprint).
type CleanupItem struct {
	Index         int
	Kind          CleanupItemKind
	CanonicalPath string
	Branch        string
	ExpectedHead  string
	Fingerprint   string
	Dirty         bool
	Status        CleanupItemStatus
	FailureCode   Code
}

// CleanupAttempt is one Cleanup execution record carrying its immutable
// Manifest reference and per-item results.
type CleanupAttempt struct {
	ID        CleanupAttemptID
	Status    CleanupStatus
	Manifest  ArtifactRef
	Items     []CleanupItem
	StartedAt time.Time
	EndedAt   time.Time
}

// CleanupManifestHash is the canonical SHA-256 of the confirmed target
// set: identity and fact fields only, never status. The manifest is
// immutable; the execution confirmation binds this exact hash.
func CleanupManifestHash(items []CleanupItem) string {
	sorted := append([]CleanupItem(nil), items...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Index < sorted[j].Index })
	h := sha256.New()
	for _, it := range sorted {
		fmt.Fprintf(h, "%d|%s|%s|%s|%s|%v\n", it.Index, it.Kind, it.CanonicalPath, it.Branch, it.ExpectedHead, it.Fingerprint)
	}
	return hex.EncodeToString(h.Sum(nil))
}
