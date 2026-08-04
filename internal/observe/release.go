// The Gate 3 release evidence module (Task 22, design 23, PRD 已确认：三层内
// 部交付 Gate §Gate 3, PRD 已确认：双层 Demo 验收): the typed Gate Manifest,
// the actual candidate facts, the release-evidence validation, and the
// deterministic self-Dogfood preflight. ValidateReleaseEvidence compares a
// Gate manifest to the actual candidate facts so the Gate 3 evidence is
// exact and current: a manifest recorded by a different subject — dirty
// source, a wrong source Commit, a missing registry hash, a Gate 1/2 label
// mismatch, real E2E or Dogfood evidence produced by a different binary, a
// stale Provider binding, missing review/evidence, or a contaminated
// Dogfood target workspace — is rejected with CodeEvidenceSubjectChanged.
// The validation is a pure function of its input; it never reads files or
// Git, so the offline tests prove the full case list and the release
// pipeline (scripts/validate-evidence) feeds it the observed facts.
package observe

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"cflow.local/cflow/internal/model"
)

// The exact Gate candidate labels (design 22.3): Gate 1 and Gate 2
// artifacts are internal candidates, never Demo releases; the Gate 3
// candidate is "Demo Complete Candidate", and the final user release
// sign-off is a separate human step (the manifest NEVER says "Released").
const (
	Gate1CandidateLabel = "Internal Core Candidate"
	Gate2CandidateLabel = "Internal Runtime Candidate"
	Gate3CandidateLabel = "Demo Complete Candidate"
)

// GateManifest is the parsed redacted release Gate Manifest produced by
// scripts/gate1.sh, gate2.sh, and gate3.sh. The fields are exactly the
// facts the scripts record; ValidateReleaseEvidence compares them to the
// actual candidate facts.
type GateManifest struct {
	// Candidate is the manifest label: one of the exact Gate labels.
	Candidate string
	// ExpectedLabel is the label the gate that produced this manifest was
	// required to write (a Gate 2 manifest must carry the Gate 2 label).
	ExpectedLabel string
	SourceCommit  string
	SourceDirty   bool
	// BinarySHA256 is the binary that produced this manifest's evidence.
	BinarySHA256  string
	SchemaVersion string
	Registries    RegistryHashes
	// ProviderBindings pins the embedded Provider binding hashes the
	// manifest recorded (provider name -> binding hash).
	ProviderBindings map[string]string
	// Checks records the named deterministic checks the gate ran and
	// whether each passed.
	Checks map[string]string
	// RealE2E is the approval-gated real Cross-Provider E2E evidence.
	RealE2E EvidenceRun
	// Dogfood is the approval-gated self-Dogfood evidence.
	Dogfood DogfoodEvidence
}

// EvidenceRun records one approval-gated real run's evidence: the binary
// that produced it, its source Commit, and whether an independent reviewer
// judged the output.
type EvidenceRun struct {
	Present      bool
	BinarySHA256 string
	SourceCommit string
	ReportHash   string
	Reviewed     bool
}

// DogfoodEvidence records the self-Dogfood evidence (design 23: the binary
// copied outside the target repository and treated as immutable; the
// bounded docs-or-tests-only requirement; at least two Tasks routed across
// codex and claude; the protected Apply's old and new Target heads).
type DogfoodEvidence struct {
	Present           bool
	BinarySHA256      string
	BinaryPath        string
	TargetWorkspace   string
	OriginalWorkspace string
	RequirementHash   string
	Routes            []string
	WorkflowHash      string
	ApplyOldHead      string
	ApplyNewHead      string
	Reviewed          bool
}

// ReleaseFacts are the actual candidate facts the Gate 3 manifest is
// validated against: the clean source Commit, the embedded registry and
// schema hashes, and the binary identities. They are assembled by the
// release pipeline from the binaries and the embedded registries — never
// from the manifest itself.
type ReleaseFacts struct {
	SourceCommit  string
	SourceDirty   bool
	SchemaVersion string
	Registries    RegistryHashes
	// BinarySHA256 is the binary that must have produced the gate evidence
	// (the reproducible rebuild of the gate's own build command).
	BinarySHA256 string
	// ReleaseBinarySHA256 is the final release candidate binary the
	// approval-gated real E2E and Dogfood evidence must have run against.
	ReleaseBinarySHA256 string
	ProviderBindings    map[string]string
}

// DogfoodPreflight is the deterministic preflight facts of a self-Dogfood
// run (design 23, PRD 已确认：Dogfood 不针对未提交工作区): the immutable
// copied binary and its pinned hash, the clean candidate source, the target
// workspace distinct from the original developer workspace, the bounded
// user-approved requirement, and the cross-provider routes.
type DogfoodPreflight struct {
	BinaryPath          string
	BinarySHA256        string
	SourceCommit        string
	SourceClean         bool
	TargetWorkspace     string
	OriginalWorkspace   string
	RequirementBound    string
	RequirementApproved bool
	Routes              []string
}

// requiredChecksByLabel is the set of deterministic checks each gate's
// evidence must record as "pass" before it can vouch for the candidate.
// The optional approval-gated checks (real_cross_provider, dogfood) are
// validated separately and never required here — a skipped real run is an
// honest pending state, not missing evidence.
var requiredChecksByLabel = map[string][]string{
	Gate1CandidateLabel: {"gofmt", "internal_race", "fake_e2e", "full_suite", "vet"},
	Gate2CandidateLabel: {"gate1", "fault_recovery_matrix", "routing_matrix", "dialect_equivalent"},
	Gate3CandidateLabel: {"gate1", "gate2", "cross_build", "native_race"},
}

// knownGateLabels is the exact set of candidate labels a Gate manifest may
// carry. Anything else — most importantly "Released" — is an overclaim.
var knownGateLabels = map[string]bool{
	Gate1CandidateLabel: true,
	Gate2CandidateLabel: true,
	Gate3CandidateLabel: true,
}

// ValidateReleaseEvidence validates a Gate manifest against the actual
// candidate facts. Every mismatch is a Fault with CodeEvidenceSubjectChanged:
// the evidence was produced by a different subject and cannot vouch for
// this candidate.
func ValidateReleaseEvidence(m GateManifest, facts ReleaseFacts) error {
	// Gate label: the manifest must carry the exact label of the gate that
	// produced it, and never an overclaiming label.
	if m.ExpectedLabel != "" && m.Candidate != m.ExpectedLabel {
		return evidenceFault("the manifest is labeled %q; %s evidence requires the exact label %q (Gate 1/2 label mismatch)",
			m.Candidate, m.ExpectedLabel, m.ExpectedLabel)
	}
	if !knownGateLabels[m.Candidate] {
		return evidenceFault("the manifest label %q is not a known gate candidate label (never a release claim)", m.Candidate)
	}
	// Source identity: the evidence must come from the candidate's clean
	// source Commit.
	if m.SourceCommit != facts.SourceCommit {
		return evidenceFault("the manifest records source commit %s; the candidate is %s", m.SourceCommit, facts.SourceCommit)
	}
	if m.SourceDirty != facts.SourceDirty {
		return evidenceFault("the manifest claims a %v source dirty flag; the candidate is %v", m.SourceDirty, facts.SourceDirty)
	}
	// Schema version: the manifest must pin the candidate's applied schema.
	if m.SchemaVersion != "" && m.SchemaVersion != facts.SchemaVersion {
		return evidenceFault("the manifest schema version %s does not match the candidate %s", m.SchemaVersion, facts.SchemaVersion)
	}
	// Embedded registry hashes: every registry must be pinned in the
	// manifest and the pins must match the candidate's embedded registries.
	for _, name := range []string{"migration", "artifact", "provider", "prompt"} {
		mh, fh := registryValue(m.Registries, name), registryValue(facts.Registries, name)
		if mh == "" || mh == "unset" {
			return evidenceFault("the manifest records no %s registry hash (the candidate must embed it)", name)
		}
		if mh != fh {
			return evidenceFault("the manifest %s registry hash %s does not match the candidate %s", name, mh, fh)
		}
	}
	// Binary identity of the gate evidence: the manifest must have been
	// produced by the candidate binary, not a different one.
	if m.BinarySHA256 != "" && m.BinarySHA256 != facts.BinarySHA256 {
		return evidenceFault("the manifest binary sha256 %s was produced by a different binary than the candidate %s", m.BinarySHA256, facts.BinarySHA256)
	}
	// Provider bindings: every binding the manifest pins must be current.
	for name, want := range m.ProviderBindings {
		got := facts.ProviderBindings[name]
		if got == "" {
			return evidenceFault("the candidate has no %s provider binding to match the manifest", name)
		}
		if want != got {
			return evidenceFault("the manifest %s provider binding %s is stale; the candidate pins %s", name, want, got)
		}
	}
	// Deterministic checks: the required Gate evidence must be present and
	// passing (missing or failed evidence is rejected).
	for _, check := range requiredChecksByLabel[labelOf(m)] {
		if m.Checks[check] != "pass" {
			got := m.Checks[check]
			if got == "" {
				got = "absent"
			}
			return evidenceFault("the manifest %s evidence is %q, not \"pass\" (missing review/evidence)", check, got)
		}
	}
	// Real Cross-Provider E2E evidence (approval-gated): when present, it
	// must have been produced by the release candidate and independently
	// reviewed.
	if m.RealE2E.Present {
		if m.RealE2E.BinarySHA256 != facts.ReleaseBinarySHA256 {
			return evidenceFault("the real E2E evidence was produced by a different binary %s than the release candidate %s", m.RealE2E.BinarySHA256, facts.ReleaseBinarySHA256)
		}
		if m.RealE2E.SourceCommit != facts.SourceCommit {
			return evidenceFault("the real E2E evidence source commit %s does not match the candidate %s", m.RealE2E.SourceCommit, facts.SourceCommit)
		}
		if m.RealE2E.ReportHash == "" {
			return evidenceFault("the real E2E evidence records no report hash")
		}
		if !m.RealE2E.Reviewed {
			return evidenceFault("the real E2E evidence is not independently reviewed")
		}
	}
	// Self-Dogfood evidence (approval-gated): when present, it must have
	// run the release candidate against a distinct, uncontaminated target
	// workspace with both codex and claude routes and an independently
	// reviewed result.
	if m.Dogfood.Present {
		if m.Dogfood.BinarySHA256 != facts.ReleaseBinarySHA256 {
			return evidenceFault("the dogfood evidence was produced by a different binary %s than the release candidate %s", m.Dogfood.BinarySHA256, facts.ReleaseBinarySHA256)
		}
		if dogfoodTargetContaminated(m.Dogfood) {
			return evidenceFault("the dogfood target workspace %s is contaminated by the original developer workspace %s", m.Dogfood.TargetWorkspace, m.Dogfood.OriginalWorkspace)
		}
		if !hasProvider(m.Dogfood.Routes, "codex") || !hasProvider(m.Dogfood.Routes, "claude") {
			return evidenceFault("the dogfood evidence does not route at least two tasks across codex and claude")
		}
		if m.Dogfood.RequirementHash == "" || m.Dogfood.WorkflowHash == "" || m.Dogfood.ApplyOldHead == "" || m.Dogfood.ApplyNewHead == "" {
			return evidenceFault("the dogfood evidence records incomplete workflow facts")
		}
		if !m.Dogfood.Reviewed {
			return evidenceFault("the dogfood evidence is not independently reviewed")
		}
	}
	return nil
}

// ValidateDogfoodPreflight validates the deterministic preflight facts of a
// self-Dogfood run. Every violation is a Fault with
// CodeEvidenceSubjectChanged: the run cannot be genuine evidence for this
// candidate. Unlike ValidateReleaseEvidence it reads one file — the
// immutable copied binary — to prove the pinned SHA-256 is the actual
// content.
func ValidateDogfoodPreflight(p DogfoodPreflight) error {
	if p.BinaryPath == "" || p.BinarySHA256 == "" || len(p.BinarySHA256) != 64 {
		return evidenceFault("the dogfood preflight requires an immutable binary path and its pinned SHA-256")
	}
	if p.TargetWorkspace == "" || p.OriginalWorkspace == "" {
		return evidenceFault("the dogfood preflight requires the target and original workspaces")
	}
	// The immutable binary must live outside the target repository.
	if pathWithin(p.BinaryPath, p.TargetWorkspace) {
		return evidenceFault("the dogfood candidate binary must live outside the target repository %s", p.TargetWorkspace)
	}
	// The target workspace must be distinct from the original developer
	// workspace: never the same path, never one containing the other.
	if pathWithin(p.TargetWorkspace, p.OriginalWorkspace) || pathWithin(p.OriginalWorkspace, p.TargetWorkspace) {
		return evidenceFault("the dogfood target workspace %s is contaminated by the original developer workspace %s", p.TargetWorkspace, p.OriginalWorkspace)
	}
	// A dogfood run requires a committed workspace (PRD 已确认：Dogfood 不针
	// 对未提交工作区).
	if !p.SourceClean {
		return evidenceFault("the dogfood candidate source is not clean; a dogfood run requires a committed workspace")
	}
	if p.SourceCommit == "" {
		return evidenceFault("the dogfood preflight requires the candidate source commit")
	}
	// The bounded docs-or-tests-only requirement must be user-approved.
	if !p.RequirementApproved {
		return evidenceFault("the dogfood requirement is not user-approved")
	}
	if p.RequirementBound == "" {
		return evidenceFault("the dogfood requirement is not bounded (docs-or-tests-only)")
	}
	// At least two Tasks must route across codex and claude.
	if !hasProvider(p.Routes, "codex") || !hasProvider(p.Routes, "claude") {
		return evidenceFault("the dogfood preflight requires at least two tasks routed across codex and claude")
	}
	// The pinned hash must be the actual hash of the copied binary.
	data, err := os.ReadFile(p.BinaryPath)
	if err != nil {
		return evidenceFault("the dogfood candidate binary cannot be read: %v", err)
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != p.BinarySHA256 {
		return evidenceFault("the dogfood candidate binary hash does not match the pinned SHA-256")
	}
	return nil
}

// ParseGateManifest parses the redacted text Manifest the gate scripts
// write into the typed GateManifest. An unparseable or label-less manifest
// is a Fault with CodeEvidenceSubjectChanged. Section members are the
// indented lines under checks:/provider_bindings:/registries:; top-level
// keys are always attributed to the manifest fields, so a trailing
// top-level key can never leak into a section.
func ParseGateManifest(data []byte) (GateManifest, error) {
	var m GateManifest
	m.Checks = map[string]string{}
	m.ProviderBindings = map[string]string{}
	section := ""
	for _, raw := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(raw) == "" || strings.HasPrefix(strings.TrimSpace(raw), "#") {
			continue
		}
		indented := strings.HasPrefix(raw, " ") || strings.HasPrefix(raw, "\t")
		line := strings.TrimSpace(raw)
		if strings.HasSuffix(line, ":") {
			section = strings.TrimSuffix(line, ":")
			continue
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key, val = strings.TrimSpace(key), strings.TrimSpace(val)
		if indented {
			switch section {
			case "checks":
				m.Checks[key] = val
				continue
			case "provider_bindings":
				m.ProviderBindings[key] = val
				continue
			}
		}
		switch key {
		case "candidate":
			m.Candidate = val
		case "source_commit":
			m.SourceCommit = val
		case "source_dirty":
			m.SourceDirty = val == "true" || val == "1"
		case "git_clean":
			m.SourceDirty = val != "true"
		case "binary_sha256":
			m.BinarySHA256 = val
		case "schema_version":
			m.SchemaVersion = val
		case "migration":
			m.Registries.Migration = val
		case "artifact":
			m.Registries.Artifact = val
		case "provider":
			m.Registries.Provider = val
		case "prompt":
			m.Registries.Prompt = val
		}
	}
	if m.Candidate == "" {
		return m, evidenceFault("the manifest records no candidate label")
	}
	return m, nil
}

// ReleaseEvidenceFile is the on-disk JSON evidence an authorized approval-
// gated real run (the real Cross-Provider E2E or the self-Dogfood) writes
// so the Gate 3 validation can prove it was produced by the exact release
// candidate. Kind is "real-cross-provider" or "dogfood".
type ReleaseEvidenceFile struct {
	Kind              string   `json:"kind"`
	BinarySHA256      string   `json:"binary_sha256"`
	SourceCommit      string   `json:"source_commit"`
	Reviewed          bool     `json:"reviewed"`
	ReportHash        string   `json:"report_hash,omitempty"`
	TargetWorkspace   string   `json:"target_workspace,omitempty"`
	OriginalWorkspace string   `json:"original_workspace,omitempty"`
	RequirementHash   string   `json:"requirement_hash,omitempty"`
	Routes            []string `json:"routes,omitempty"`
	WorkflowHash      string   `json:"workflow_hash,omitempty"`
	ApplyOldHead      string   `json:"apply_old_head,omitempty"`
	ApplyNewHead      string   `json:"apply_new_head,omitempty"`
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// labelOf is the label the required-checks set is keyed on: the caller's
// expected label wins, falling back to the manifest's own candidate label.
func labelOf(m GateManifest) string {
	if m.ExpectedLabel != "" {
		return m.ExpectedLabel
	}
	return m.Candidate
}

func registryValue(r RegistryHashes, name string) string {
	switch name {
	case "migration":
		return r.Migration
	case "artifact":
		return r.Artifact
	case "provider":
		return r.Provider
	case "prompt":
		return r.Prompt
	}
	return ""
}

// dogfoodTargetContaminated reports whether the Dogfood evidence ran
// against a target workspace that is the original developer workspace, one
// that contains it (or is contained by it), or whose binary was copied
// inside the target repository.
func dogfoodTargetContaminated(d DogfoodEvidence) bool {
	if d.TargetWorkspace == "" || d.OriginalWorkspace == "" {
		return true
	}
	if pathWithin(d.TargetWorkspace, d.OriginalWorkspace) || pathWithin(d.OriginalWorkspace, d.TargetWorkspace) {
		return true
	}
	if d.BinaryPath != "" && pathWithin(d.BinaryPath, d.TargetWorkspace) {
		return true
	}
	return false
}

// pathWithin reports whether child is the same path as parent or contained
// inside it. Both are made absolute and symlink-resolved; containment is
// judged on canonical paths only.
func pathWithin(child, parent string) bool {
	c, err1 := filepath.Abs(child)
	p, err2 := filepath.Abs(parent)
	if err1 != nil || err2 != nil {
		return false
	}
	// Resolve symlinks where possible so a workspace reached through a
	// symlink cannot hide its true location.
	if r, err := filepath.EvalSymlinks(c); err == nil {
		c = r
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	rel, err := filepath.Rel(p, c)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func hasProvider(routes []string, name string) bool {
	for _, r := range routes {
		if r == name {
			return true
		}
	}
	return false
}

// evidenceFault constructs the single release-evidence Fault code: the
// evidence subject changed, so the evidence cannot vouch for the candidate.
func evidenceFault(format string, args ...any) error {
	return model.NewFault(model.CodeEvidenceSubjectChanged, fmt.Sprintf(format, args...))
}
