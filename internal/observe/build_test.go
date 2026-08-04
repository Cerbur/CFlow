// The build metadata tests (Task 22, brief Step 1, design 23): Current()
// must assemble the full release identity — the linker-set version, source
// Commit, dirty flag, applied schema version, and every embedded registry
// hash — with no timestamp in the binary identity. The release pipeline
// proves the same values through `go version -m` on the built binary
// (scripts/check-cross-build.sh); this unit test pins the assembly itself.
package observe

import (
	"testing"
)

// TestCurrentReadsLinkerSetReleaseMetadata: release builds override the
// package-level variables through -ldflags "-X ..."; Current() must report
// exactly those values.
func TestCurrentReadsLinkerSetReleaseMetadata(t *testing.T) {
	saveVersion := Version
	saveSource := SourceCommit
	saveDirty := sourceDirty
	saveSchema := schemaVersion
	saveMig, saveArt, saveProv, savePrompt := MigrationHash, ArtifactHash, ProviderHash, PromptHash
	defer func() {
		Version, SourceCommit = saveVersion, saveSource
		sourceDirty = saveDirty
		schemaVersion = saveSchema
		MigrationHash, ArtifactHash, ProviderHash, PromptHash = saveMig, saveArt, saveProv, savePrompt
	}()

	// The exact values the release pipeline stamps through linker flags.
	Version = "0.1.0-demo3"
	SourceCommit = "abcd1234"
	sourceDirty = "1"
	schemaVersion = "3"
	MigrationHash = "m-hash"
	ArtifactHash = "a-hash"
	ProviderHash = "p-hash"
	PromptHash = "t-hash"

	bi := Current()
	if bi.Version != "0.1.0-demo3" {
		t.Fatalf("version = %s", bi.Version)
	}
	if bi.SourceCommit != "abcd1234" {
		t.Fatalf("source commit = %s", bi.SourceCommit)
	}
	if !bi.Dirty {
		t.Fatalf("dirty = %v, want true", bi.Dirty)
	}
	if bi.SchemaVersion != 3 {
		t.Fatalf("schema version = %d, want 3", bi.SchemaVersion)
	}
	want := RegistryHashes{Migration: "m-hash", Artifact: "a-hash", Provider: "p-hash", Prompt: "t-hash"}
	if bi.Registries != want {
		t.Fatalf("registries = %+v, want %+v", bi.Registries, want)
	}
}

// TestCurrentDefaultsToUnsetWithoutReleaseFlags: a dev build carries no
// release pins and no schema version — the identity never fabricates values.
func TestCurrentDefaultsToUnsetWithoutReleaseFlags(t *testing.T) {
	saveVersion := Version
	saveSource := SourceCommit
	saveDirty := sourceDirty
	saveSchema := schemaVersion
	saveMig, saveArt, saveProv, savePrompt := MigrationHash, ArtifactHash, ProviderHash, PromptHash
	defer func() {
		Version, SourceCommit = saveVersion, saveSource
		sourceDirty = saveDirty
		schemaVersion = saveSchema
		MigrationHash, ArtifactHash, ProviderHash, PromptHash = saveMig, saveArt, saveProv, savePrompt
	}()

	Version, SourceCommit = "0.0.0-dev", "unset"
	sourceDirty, schemaVersion = "false", "0"
	MigrationHash, ArtifactHash, ProviderHash, PromptHash = "unset", "unset", "unset", "unset"

	bi := Current()
	if bi.SchemaVersion != 0 {
		t.Fatalf("schema version = %d, want 0 in a dev build", bi.SchemaVersion)
	}
	for _, v := range []string{bi.Registries.Migration, bi.Registries.Artifact, bi.Registries.Provider, bi.Registries.Prompt} {
		if v != "unset" {
			t.Fatalf("dev build reports a pinned registry hash %q", v)
		}
	}
}
