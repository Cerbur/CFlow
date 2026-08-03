package model

// The Verification Evidence Manifest (design 16.2): the immutable,
// hashed record one Verification run produces. The Manifest is data —
// the Kernel and Recovery judge the facts it carries; it never claims
// lifecycle state. The Hash binds the exact evidence revision (the
// canonical JSON serialization excluding the field itself, so identical
// facts produce byte-identical manifests).

// GitFactsSummary is the normalized pre/post Git fact snapshot of one
// Verification run (design 16.2 steps 1 and 5): the observed HEAD and
// whether the working tree was Git-clean at that instant.
type GitFactsSummary struct {
	Head  string
	Clean bool
}

// EvidenceManifest is one Verification run's immutable evidence record
// (design 16.2): the Catalog identity and purpose revalidated before the
// run, the exact executable/argv/cwd/environment-name identity, the
// bounded redacted output, the exit and timeout facts, the pre/post Git
// facts, the evidence hashes, and the result classification.
type EvidenceManifest struct {
	SchemaVersion string
	// Node is the verify Node the run served.
	Node NodeID
	// CatalogRef is the immutable Catalog identity the run revalidated.
	CatalogRef CatalogRef
	// CommandID is the approved Catalog entry that ran.
	CommandID string
	// Purpose is the revalidated Catalog purpose of the entry.
	Purpose string
	// CommitRange is the Task range the run verified ("base..head").
	CommitRange string
	// Passed classifies the result: exit within the expected codes and
	// the post-run Git gate clean.
	Passed bool
	// Reason records why the run did not pass ("", "exit", "timeout",
	// "executable-identity", "head-changed", "tracked-output",
	// "untracked-output", "ignored-output-outside-transient-paths", or a
	// refused run).
	Reason string
	// ExitCode and ExitFact are the supervised exit facts; ExitFact is
	// "exit", "timeout", "signal", or "cancelled".
	ExitCode int
	ExitFact string
	// DurationMs is the wall duration of the command.
	DurationMs int64
	// PreGit and PostGit are the pre/post run Git facts.
	PreGit  GitFactsSummary
	PostGit GitFactsSummary
	// Output is the bounded, redacted captured output.
	Output string
	// OutputHash binds the redacted output.
	OutputHash string
	// Hash binds the exact manifest revision (self-hash excluding the
	// field itself).
	Hash string
}
