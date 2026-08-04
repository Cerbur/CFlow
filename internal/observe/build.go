// Package observe owns observability: build identity, structured logs,
// events, the final report, and the Gate 3 release evidence (design 23).
// Task 1 provides build identity only; later tasks add the event and
// report modules; Task 22 adds the full release metadata and the release
// evidence validation.
package observe

import (
	"runtime"
	"runtime/debug"
	"strconv"
)

// Version is the release version of the binary. Release builds override it
// with -ldflags "-X cflow.local/cflow/internal/observe.Version=<version>".
// The release identity carries no timestamp: a rebuild from the same source
// and toolchain produces the same binary SHA-256 (design 23).
var Version = "0.0.0-dev"

// SourceCommit and sourceDirty are the fallback source identity for
// binaries built where the Go toolchain cannot stamp VCS metadata itself
// (notably git worktrees, which the toolchain does not recognize). They
// are overridden at build time with
// -ldflags "-X cflow.local/cflow/internal/observe.SourceCommit=<sha>"
// -ldflags "-X cflow.local/cflow/internal/observe.sourceDirty=1".
// Toolchain-stamped VCS metadata, when present, always wins.
var SourceCommit = "unset"
var sourceDirty = "false"

// schemaVersion is the applied SQLite schema version a release build pins
// (design 23 build metadata: the schema range of the embedded migration
// chain). It is overridden with
// -ldflags "-X cflow.local/cflow/internal/observe.schemaVersion=<n>".
var schemaVersion = "0"

// MigrationHash, ArtifactHash, ProviderHash, and PromptHash pin the
// revisions of the registries embedded in the binary (design 23 build
// metadata): the forward-only SQLite migration registry, the Artifact/IR
// schema compatibility contracts, the Provider protocol binding registry,
// and the prompt template registry. Each is the SHA-256 of the canonical
// registry content. Release builds override each with
// -ldflags "-X cflow.local/cflow/internal/observe.MigrationHash=<sha256>",
// and the release pipeline proves the stamping through the runnable
// binary's own version output (Go 1.24+ withholds the raw -ldflags string
// from `go version -m` for security, so `go version -m` proves the build
// configuration while the binary's version output proves the pinned
// values). Dev builds leave them "unset", rendered as "unset" in version
// and doctor output.
var (
	MigrationHash = "unset"
	ArtifactHash  = "unset"
	ProviderHash  = "unset"
	PromptHash    = "unset"
)

// RegistryHashes pins the revisions of the registries embedded in the
// binary. Each field is the SHA-256 of the canonical registry content.
// Fields stay "unset" until a release build stamps them through the linker
// flags, and render as "unset" in version and doctor output.
type RegistryHashes struct {
	Migration string // embedded forward-only SQLite migration registry
	Artifact  string // embedded Artifact compatibility registry
	Provider  string // embedded Provider protocol binding registry
	Prompt    string // embedded prompt template registry
}

// BuildInfo is the immutable build identity reported by version and
// doctor. It is assembled from Go build information, never by executing
// Git or any other external process.
type BuildInfo struct {
	Version       string
	SourceCommit  string // VCS revision stamped into the binary by the Go toolchain
	Dirty         bool   // VCS-modified flag stamped into the binary by the Go toolchain
	SchemaVersion int    // applied SQLite schema version pinned by the release build
	GoVersion     string
	OS            string
	Arch          string
	Registries    RegistryHashes
}

// Current assembles the build identity of the running binary from Go
// build information and the process platform. It never touches the
// filesystem, Git, or CFLOW_HOME.
func Current() BuildInfo {
	sv, _ := strconv.Atoi(schemaVersion)
	bi := BuildInfo{
		Version:       Version,
		SourceCommit:  SourceCommit,
		Dirty:         sourceDirty == "1" || sourceDirty == "true",
		SchemaVersion: sv,
		GoVersion:     "unset",
		OS:            runtime.GOOS,
		Arch:          runtime.GOARCH,
		Registries: RegistryHashes{
			Migration: MigrationHash,
			Artifact:  ArtifactHash,
			Provider:  ProviderHash,
			Prompt:    PromptHash,
		},
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		bi.GoVersion = info.GoVersion
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				bi.SourceCommit = s.Value
			case "vcs.modified":
				bi.Dirty = s.Value == "true"
			}
		}
	}
	return bi
}
