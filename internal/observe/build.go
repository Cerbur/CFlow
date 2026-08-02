// Package observe owns observability: build identity, structured logs,
// events, and the final report. Task 1 provides build identity only;
// later tasks add the event and report modules.
package observe

import (
	"runtime"
	"runtime/debug"
)

// Version is the release version of the binary. Release builds override it
// with -ldflags "-X cflow.local/cflow/internal/observe.Version=<version>".
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

// RegistryHashes pins the revisions of the registries embedded in the
// binary. Each field is the SHA-256 of the canonical registry content.
// Fields stay empty until the owning registry loader fills them, and
// render as "unset" in version and doctor output.
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
	Version      string
	SourceCommit string // VCS revision stamped into the binary by the Go toolchain
	Dirty        bool   // VCS-modified flag stamped into the binary by the Go toolchain
	GoVersion    string
	OS           string
	Arch         string
	Registries   RegistryHashes
}

// Current assembles the build identity of the running binary from Go
// build information and the process platform. It never touches the
// filesystem, Git, or CFLOW_HOME.
func Current() BuildInfo {
	bi := BuildInfo{
		Version:      Version,
		SourceCommit: SourceCommit,
		Dirty:        sourceDirty == "1" || sourceDirty == "true",
		GoVersion:    "unset",
		OS:           runtime.GOOS,
		Arch:         runtime.GOARCH,
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
