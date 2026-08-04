// The Gate 3 release-evidence validation tests (Task 22, brief Step 1,
// design 23, PRD 已确认：三层内部交付 Gate §Gate 3): ValidateReleaseEvidence
// validates a Gate manifest against the actual candidate facts so the Gate
// 3 evidence is exact and current — a manifest recorded by a different
// subject (dirty source, wrong Commit, missing registry hash, Gate 1/2
// label mismatch, real E2E produced by a different binary, stale Provider
// binding, missing review/evidence, or a contaminated Dogfood target
// workspace) is rejected with CodeEvidenceSubjectChanged. ValidateDogfoodPreflight
// checks the deterministic self-Dogfood preflight facts.
package observe

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cflow.local/cflow/internal/model"
)

// assertFaultCode asserts that err carries the exact Fault Code.
func assertFaultCode(t *testing.T, err error, want model.Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected fault %s, got nil", want)
	}
	code, ok := model.CodeOf(err)
	if !ok || code != want {
		t.Fatalf("fault = %v (code %s), want %s", err, code, want)
	}
}

// validGate2Manifest is the redacted Gate 2 manifest every release-evidence
// case mutates: a clean source Commit, every embedded registry hash pinned,
// the exact Gate 2 label, and every required Gate 2 check passing.
func validGate2Manifest() GateManifest {
	return GateManifest{
		Candidate:     Gate2CandidateLabel,
		ExpectedLabel: Gate2CandidateLabel,
		SourceCommit:  "abcd1234",
		SourceDirty:   false,
		BinarySHA256:  strings.Repeat("1", 64),
		SchemaVersion: "3",
		Registries: RegistryHashes{
			Migration: "reg-migration",
			Artifact:  "reg-artifact",
			Provider:  "reg-provider",
			Prompt:    "reg-prompt",
		},
		ProviderBindings: map[string]string{"codex": "reg-codex", "claude": "reg-claude"},
		Checks: map[string]string{
			"gate1":                 "pass",
			"fault_recovery_matrix": "pass",
			"routing_matrix":        "pass",
			"dialect_equivalent":    "pass",
		},
	}
}

// actualCandidateFacts are the candidate facts that match validGate2Manifest.
func actualCandidateFacts() ReleaseFacts {
	return ReleaseFacts{
		SourceCommit:  "abcd1234",
		SourceDirty:   false,
		SchemaVersion: "3",
		Registries: RegistryHashes{
			Migration: "reg-migration",
			Artifact:  "reg-artifact",
			Provider:  "reg-provider",
			Prompt:    "reg-prompt",
		},
		BinarySHA256:     strings.Repeat("1", 64),
		ProviderBindings: map[string]string{"codex": "reg-codex", "claude": "reg-claude"},
	}
}

// TestReleaseEvidenceAcceptsCurrentManifest: a manifest recorded by the
// exact candidate validates without error.
func TestReleaseEvidenceAcceptsCurrentManifest(t *testing.T) {
	if err := ValidateReleaseEvidence(validGate2Manifest(), actualCandidateFacts()); err != nil {
		t.Fatalf("valid manifest rejected: %v", err)
	}
}

// TestReleaseEvidenceRejectsDifferentBinaryHash is the brief-mandated test
// verbatim: a manifest whose binary SHA-256 was produced by a different
// binary is rejected with CodeEvidenceSubjectChanged.
func TestReleaseEvidenceRejectsDifferentBinaryHash(t *testing.T) {
	manifest := validGate2Manifest()
	manifest.BinarySHA256 = strings.Repeat("0", 64)
	err := ValidateReleaseEvidence(manifest, actualCandidateFacts())
	assertFaultCode(t, err, model.CodeEvidenceSubjectChanged)
}

// TestReleaseEvidenceRejectsDirtySource: the manifest records a clean source
// Commit but the actual candidate source is dirty — the evidence cannot
// vouch for a candidate built from uncommitted source.
func TestReleaseEvidenceRejectsDirtySource(t *testing.T) {
	facts := actualCandidateFacts()
	facts.SourceDirty = true
	err := ValidateReleaseEvidence(validGate2Manifest(), facts)
	assertFaultCode(t, err, model.CodeEvidenceSubjectChanged)
}

// TestReleaseEvidenceRejectsWrongSourceCommit: the manifest's source Commit
// is not the candidate's source Commit.
func TestReleaseEvidenceRejectsWrongSourceCommit(t *testing.T) {
	manifest := validGate2Manifest()
	manifest.SourceCommit = "deadbeef"
	err := ValidateReleaseEvidence(manifest, actualCandidateFacts())
	assertFaultCode(t, err, model.CodeEvidenceSubjectChanged)
}

// TestReleaseEvidenceRejectsMissingRegistryHash: the candidate must embed
// every registry hash; a manifest recording none (or "unset") cannot prove
// the registries are pinned.
func TestReleaseEvidenceRejectsMissingRegistryHash(t *testing.T) {
	for _, field := range []string{"migration", "artifact", "provider", "prompt"} {
		manifest := validGate2Manifest()
		switch field {
		case "migration":
			manifest.Registries.Migration = ""
		case "artifact":
			manifest.Registries.Artifact = "unset"
		case "provider":
			manifest.Registries.Provider = ""
		case "prompt":
			manifest.Registries.Prompt = "unset"
		}
		err := ValidateReleaseEvidence(manifest, actualCandidateFacts())
		assertFaultCode(t, err, model.CodeEvidenceSubjectChanged)
	}
}

// TestReleaseEvidenceRejectsGateLabelMismatch: a Gate manifest whose label
// is not the exact label of the gate that produced it — or an overclaim
// like "Released" — is rejected.
func TestReleaseEvidenceRejectsGateLabelMismatch(t *testing.T) {
	// A Gate 2 manifest labeled as the Gate 1 candidate.
	mismatch := validGate2Manifest()
	mismatch.Candidate = Gate1CandidateLabel
	if err := ValidateReleaseEvidence(mismatch, actualCandidateFacts()); err == nil {
		t.Fatalf("Gate 1/2 label mismatch not rejected")
	}
	// An overclaiming label that is not a known gate candidate label.
	overclaim := validGate2Manifest()
	overclaim.Candidate = "Released"
	if err := ValidateReleaseEvidence(overclaim, actualCandidateFacts()); err == nil {
		t.Fatalf("overclaiming label not rejected")
	}
}

// TestReleaseEvidenceRejectsRealE2EDifferentBinary: the approval-gated real
// Cross-Provider E2E evidence records the binary that produced it; a binary
// that is not the release candidate cannot vouch for it.
func TestReleaseEvidenceRejectsRealE2EDifferentBinary(t *testing.T) {
	manifest := validGate2Manifest()
	manifest.RealE2E = EvidenceRun{
		Present:      true,
		BinarySHA256: strings.Repeat("9", 64),
		SourceCommit: "abcd1234",
		ReportHash:   "report-1",
		Reviewed:     true,
	}
	facts := actualCandidateFacts()
	facts.ReleaseBinarySHA256 = strings.Repeat("1", 64)
	err := ValidateReleaseEvidence(manifest, facts)
	assertFaultCode(t, err, model.CodeEvidenceSubjectChanged)
}

// TestReleaseEvidenceRejectsStaleProviderBinding: the manifest's embedded
// Provider binding hash no longer matches the candidate's binding — the
// evidence was produced under a different Provider protocol binding.
func TestReleaseEvidenceRejectsStaleProviderBinding(t *testing.T) {
	manifest := validGate2Manifest()
	manifest.ProviderBindings["codex"] = "stale-codex-binding"
	err := ValidateReleaseEvidence(manifest, actualCandidateFacts())
	assertFaultCode(t, err, model.CodeEvidenceSubjectChanged)
}

// TestReleaseEvidenceRejectsMissingReviewEvidence: the manifest claims the
// deterministic Gate evidence completed, but a required check is missing or
// failed, or an approval-gated run records no independent review — the
// evidence cannot be trusted.
func TestReleaseEvidenceRejectsMissingReviewEvidence(t *testing.T) {
	// A required deterministic check recorded as failed.
	failed := validGate2Manifest()
	failed.Checks["dialect_equivalent"] = "fail"
	if err := ValidateReleaseEvidence(failed, actualCandidateFacts()); err == nil {
		t.Fatalf("failed required check not rejected")
	}
	// A required deterministic check absent entirely.
	absent := validGate2Manifest()
	delete(absent.Checks, "routing_matrix")
	if err := ValidateReleaseEvidence(absent, actualCandidateFacts()); err == nil {
		t.Fatalf("missing required check not rejected")
	}
	// Real E2E evidence present but not independently reviewed.
	unreviewed := validGate2Manifest()
	unreviewed.RealE2E = EvidenceRun{
		Present:      true,
		BinarySHA256: strings.Repeat("1", 64),
		SourceCommit: "abcd1234",
		ReportHash:   "report-1",
		Reviewed:     false,
	}
	facts := actualCandidateFacts()
	facts.ReleaseBinarySHA256 = strings.Repeat("1", 64)
	if err := ValidateReleaseEvidence(unreviewed, facts); err == nil {
		t.Fatalf("unreviewed real E2E evidence not rejected")
	}
}

// TestReleaseEvidenceRejectsDogfoodTargetContamination: the self-Dogfood
// evidence must have run against a target workspace distinct from the
// original developer workspace, with the immutable binary outside the
// repository and both codex and claude routes.
func TestReleaseEvidenceRejectsDogfoodTargetContamination(t *testing.T) {
	base := validGate2Manifest()
	base.Dogfood = DogfoodEvidence{
		Present:           true,
		BinarySHA256:      strings.Repeat("1", 64),
		TargetWorkspace:   "/home/dev/CFlow",
		OriginalWorkspace: "/home/dev/CFlow",
		RequirementHash:   "req-1",
		Routes:            []string{"codex", "claude"},
		WorkflowHash:      "wf-1",
		ApplyOldHead:      "old-1",
		ApplyNewHead:      "new-1",
		Reviewed:          true,
	}
	facts := actualCandidateFacts()
	facts.ReleaseBinarySHA256 = strings.Repeat("1", 64)
	// The Dogfood target IS the original developer workspace.
	if err := ValidateReleaseEvidence(base, facts); err == nil {
		t.Fatalf("contaminated dogfood target not rejected")
	}
	// The binary was copied inside the target repository.
	inside := validGate2Manifest()
	inside.Dogfood = base.Dogfood
	inside.Dogfood.TargetWorkspace = "/home/dev/CFlow"
	inside.Dogfood.OriginalWorkspace = "/home/dev/other"
	inside.Dogfood.BinaryPath = "/home/dev/CFlow/bin/cflow"
	if err := ValidateReleaseEvidence(inside, facts); err == nil {
		t.Fatalf("dogfood binary inside the target repository not rejected")
	}
}

// dogfoodPreflightFixture is a real preflight: the running test binary
// copied to an immutable directory outside the target workspace, hashed, and
// validated against a bounded docs-or-tests-only requirement.
func dogfoodPreflightFixture(t *testing.T) DogfoodPreflight {
	t.Helper()
	canon, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("canonical temp dir: %v", err)
	}
	target := filepath.Join(canon, "target-repo")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}
	original := filepath.Join(canon, "original-workspace")
	if err := os.MkdirAll(original, 0o700); err != nil {
		t.Fatalf("mkdir original: %v", err)
	}
	// The immutable binary directory: writable only during the copy, then
	// read-only and outside the target.
	immutable := filepath.Join(canon, "immutable")
	if err := os.MkdirAll(immutable, 0o700); err != nil {
		t.Fatalf("mkdir immutable: %v", err)
	}
	binPath := filepath.Join(immutable, "cflow")
	src, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatalf("read running binary: %v", err)
	}
	if err := os.WriteFile(binPath, src, 0o555); err != nil {
		t.Fatalf("copy candidate binary: %v", err)
	}
	if err := os.Chmod(immutable, 0o500); err != nil {
		t.Fatalf("seal immutable dir: %v", err)
	}
	// Restore the sealed permissions so the temporary directory can be
	// removed at test cleanup.
	t.Cleanup(func() { os.Chmod(immutable, 0o700) })
	sum := sha256.Sum256(src)
	return DogfoodPreflight{
		BinaryPath:          binPath,
		BinarySHA256:        hex.EncodeToString(sum[:]),
		SourceCommit:        "abcd1234",
		SourceClean:         true,
		TargetWorkspace:     target,
		OriginalWorkspace:   original,
		RequirementBound:    "docs-or-tests-only",
		RequirementApproved: true,
		Routes:              []string{"codex", "claude"},
	}
}

// TestParseGateManifestAttribution: the parser must attribute indented
// section members to checks:/provider_bindings:/registries: and every
// top-level key to the manifest fields — a trailing top-level key (like
// the gate scripts' fixture_node/fixture_npm lines after provider_bindings:)
// must never leak into a section.
func TestParseGateManifestAttribution(t *testing.T) {
	data := `candidate: Internal Core Candidate
generated_at: 2026-08-04T12:00:00Z
source_commit: abcd1234
source_subject: fixture subject
git_clean: true
source_dirty: false
binary_sha256: ` + strings.Repeat("1", 64) + `
go_version: go version go1.26.5 darwin/arm64
schema_version: 3
registries:
  migration: m-hash
  artifact: a-hash
  provider: p-hash
  prompt: t-hash
provider_bindings:
  codex: c-hash
  claude: d-hash
fixture_node: v26.3.1
fixture_npm: 11.16.0
checks:
  gofmt: pass
  internal_race: pass
  fake_e2e: pass
  full_suite: pass
  vet: pass
`
	m, err := ParseGateManifest([]byte(data))
	if err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	if m.Candidate != Gate1CandidateLabel || m.SourceCommit != "abcd1234" || m.SourceDirty {
		t.Fatalf("manifest identity = %+v", m)
	}
	if m.BinarySHA256 != strings.Repeat("1", 64) || m.SchemaVersion != "3" {
		t.Fatalf("manifest binary/schema = %s/%s", m.BinarySHA256, m.SchemaVersion)
	}
	if m.Registries != (RegistryHashes{Migration: "m-hash", Artifact: "a-hash", Provider: "p-hash", Prompt: "t-hash"}) {
		t.Fatalf("manifest registries = %+v", m.Registries)
	}
	if m.ProviderBindings["codex"] != "c-hash" || m.ProviderBindings["claude"] != "d-hash" {
		t.Fatalf("manifest provider bindings = %+v", m.ProviderBindings)
	}
	// The top-level fixture keys must not be attributed to a section.
	if _, ok := m.ProviderBindings["fixture_node"]; ok {
		t.Fatalf("fixture_node leaked into provider bindings: %+v", m.ProviderBindings)
	}
	for _, check := range []string{"gofmt", "internal_race", "fake_e2e", "full_suite", "vet"} {
		if m.Checks[check] != "pass" {
			t.Fatalf("check %s = %q, want pass", check, m.Checks[check])
		}
	}
	// The parsed manifest validates against the matching candidate facts.
	if err := ValidateReleaseEvidence(m, ReleaseFacts{
		SourceCommit:     "abcd1234",
		SourceDirty:      false,
		SchemaVersion:    "3",
		Registries:       RegistryHashes{Migration: "m-hash", Artifact: "a-hash", Provider: "p-hash", Prompt: "t-hash"},
		BinarySHA256:     strings.Repeat("1", 64),
		ProviderBindings: map[string]string{"codex": "c-hash", "claude": "d-hash"},
	}); err != nil {
		t.Fatalf("parsed manifest rejected against matching facts: %v", err)
	}
}

// TestValidateDogfoodPreflightAcceptsValidFacts: the deterministic preflight
// facts of a genuine dogfood run validate.
func TestValidateDogfoodPreflightAcceptsValidFacts(t *testing.T) {
	if err := ValidateDogfoodPreflight(dogfoodPreflightFixture(t)); err != nil {
		t.Fatalf("valid dogfood preflight rejected: %v", err)
	}
}

// TestValidateDogfoodPreflightRejectsContamination: every contaminated or
// unbounded preflight fact is rejected with CodeEvidenceSubjectChanged.
func TestValidateDogfoodPreflightRejectsContamination(t *testing.T) {
	base := dogfoodPreflightFixture(t)
	cases := []struct {
		name   string
		mutate func(*DogfoodPreflight)
	}{
		{"dirty candidate source", func(p *DogfoodPreflight) { p.SourceClean = false }},
		{"binary hash mismatch", func(p *DogfoodPreflight) { p.BinarySHA256 = strings.Repeat("0", 64) }},
		{"binary inside the target repository", func(p *DogfoodPreflight) {
			p.BinaryPath = filepath.Join(p.TargetWorkspace, "bin", "cflow")
		}},
		{"target is the original developer workspace", func(p *DogfoodPreflight) {
			p.TargetWorkspace = p.OriginalWorkspace
		}},
		{"requirement not approved", func(p *DogfoodPreflight) { p.RequirementApproved = false }},
		{"unbounded requirement", func(p *DogfoodPreflight) { p.RequirementBound = "" }},
		{"single provider route", func(p *DogfoodPreflight) { p.Routes = []string{"codex"} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := base
			tc.mutate(&p)
			err := ValidateDogfoodPreflight(p)
			assertFaultCode(t, err, model.CodeEvidenceSubjectChanged)
		})
	}
}
