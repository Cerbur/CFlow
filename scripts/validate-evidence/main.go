// Command validate-evidence validates the redacted Gate evidence against
// the actual candidate facts (Task 22, design 23, PRD 已确认：三层内部交付
// Gate §Gate 3). gate3.sh feeds it the Gate manifests it reran and the
// actual candidate facts — the source Commit, the clean flag, the embedded
// registry/schema hashes, the reproducible gate rebuild binary SHA-256, and
// the release candidate binary SHA-256 — plus, when the user has authorized
// them, the real Cross-Provider E2E and self-Dogfood evidence files
// (observe.ReleaseEvidenceFile). Every mismatch exits non-zero with the
// EVIDENCE_SUBJECT_CHANGED Fault.
//
// Usage (one or both evidence files optional; the gate manifest is
// required):
//
//	go run ./scripts/validate-evidence \
//	  -manifest "$ARTIFACT_DIR/gate2/gate2-manifest.txt" \
//	  -expect "Internal Runtime Candidate" \
//	  -source-commit "$SOURCE_COMMIT" \
//	  -binary-sha256 "$GATE2_BIN_SHA" \
//	  -release-binary-sha256 "$RELEASE_BIN_SHA" \
//	  -schema-version "$SCHEMA_VERSION" \
//	  -migration-hash "$MIGRATION" \
//	  -artifact-hash "$ARTIFACT" \
//	  -provider-hash "$PROVIDER" \
//	  -prompt-hash "$PROMPT" \
//	  -codex-binding "$CODEX" \
//	  -claude-binding "$CLAUDE" \
//	  [-real-e2e "$ARTIFACT_DIR/real-cross-provider.json"] \
//	  [-dogfood "$ARTIFACT_DIR/dogfood.json"]
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"cflow.local/cflow/internal/observe"
)

func main() {
	manifestPath := flag.String("manifest", "", "path to the redacted Gate manifest")
	expected := flag.String("expect", "", "the exact Gate candidate label the manifest must carry")
	sourceCommit := flag.String("source-commit", "", "actual candidate source commit")
	sourceDirty := flag.String("source-dirty", "false", "actual candidate source dirty flag (true/false)")
	schemaVersion := flag.String("schema-version", "", "actual candidate schema version")
	migrationHash := flag.String("migration-hash", "", "actual candidate migration registry hash")
	artifactHash := flag.String("artifact-hash", "", "actual candidate artifact registry hash")
	providerHash := flag.String("provider-hash", "", "actual candidate provider registry hash")
	promptHash := flag.String("prompt-hash", "", "actual candidate prompt registry hash")
	binarySHA := flag.String("binary-sha256", "", "actual gate binary sha256 (the reproducible rebuild of the gate's own build)")
	releaseBinarySHA := flag.String("release-binary-sha256", "", "actual release candidate binary sha256 (for the real E2E/dogfood evidence)")
	codexBinding := flag.String("codex-binding", "", "actual codex binding hash")
	claudeBinding := flag.String("claude-binding", "", "actual claude binding hash")
	realE2E := flag.String("real-e2e", "", "optional real Cross-Provider E2E evidence file (observe.ReleaseEvidenceFile, kind real-cross-provider)")
	dogfood := flag.String("dogfood", "", "optional self-Dogfood evidence file (observe.ReleaseEvidenceFile, kind dogfood)")
	flag.Parse()

	if *manifestPath == "" || *expected == "" {
		die("usage: validate-evidence -manifest <gate-manifest> -expect <gate-label> [facts...]")
	}
	data, err := os.ReadFile(*manifestPath)
	if err != nil {
		die(err)
	}
	m, err := observe.ParseGateManifest(data)
	if err != nil {
		die(err)
	}
	m.ExpectedLabel = *expected

	if *realE2E != "" {
		ev, err := readEvidence(*realE2E)
		if err != nil {
			die(err)
		}
		if ev.Kind != "real-cross-provider" {
			die(fmt.Sprintf("evidence file %s has kind %q, want real-cross-provider", *realE2E, ev.Kind))
		}
		m.RealE2E = observe.EvidenceRun{
			Present: true, BinarySHA256: ev.BinarySHA256, SourceCommit: ev.SourceCommit,
			ReportHash: ev.ReportHash, Reviewed: ev.Reviewed,
		}
	}
	if *dogfood != "" {
		ev, err := readEvidence(*dogfood)
		if err != nil {
			die(err)
		}
		if ev.Kind != "dogfood" {
			die(fmt.Sprintf("evidence file %s has kind %q, want dogfood", *dogfood, ev.Kind))
		}
		m.Dogfood = observe.DogfoodEvidence{
			Present: true, BinarySHA256: ev.BinarySHA256,
			TargetWorkspace: ev.TargetWorkspace, OriginalWorkspace: ev.OriginalWorkspace,
			RequirementHash: ev.RequirementHash, Routes: ev.Routes,
			WorkflowHash: ev.WorkflowHash, ApplyOldHead: ev.ApplyOldHead, ApplyNewHead: ev.ApplyNewHead,
			Reviewed: ev.Reviewed,
		}
	}

	facts := observe.ReleaseFacts{
		SourceCommit:        *sourceCommit,
		SourceDirty:         *sourceDirty == "true" || *sourceDirty == "1",
		SchemaVersion:       *schemaVersion,
		BinarySHA256:        *binarySHA,
		ReleaseBinarySHA256: *releaseBinarySHA,
		Registries: observe.RegistryHashes{
			Migration: *migrationHash, Artifact: *artifactHash,
			Provider: *providerHash, Prompt: *promptHash,
		},
		ProviderBindings: map[string]string{"codex": *codexBinding, "claude": *claudeBinding},
	}
	if err := observe.ValidateReleaseEvidence(m, facts); err != nil {
		fmt.Fprintln(os.Stderr, "validate-evidence:", err)
		os.Exit(1)
	}
	fmt.Printf("validate-evidence: PASS (%s)\n", *expected)
}

func readEvidence(path string) (observe.ReleaseEvidenceFile, error) {
	var ev observe.ReleaseEvidenceFile
	body, err := os.ReadFile(path)
	if err != nil {
		return ev, err
	}
	err = json.Unmarshal(body, &ev)
	return ev, err
}

func die(v any) {
	fmt.Fprintln(os.Stderr, "validate-evidence:", v)
	os.Exit(1)
}
