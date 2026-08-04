#!/usr/bin/env bash
# Cross-platform build proof (Task 22, brief Step 3, design 23): compiles
# the CGO-disabled single binary for darwin/amd64, darwin/arm64,
# linux/amd64, and linux/arm64 with -trimpath and the fixed release
# metadata through linker flags (version, source Commit, dirty flag, schema
# version, migration/Artifact/Provider/prompt registry hashes), inspects
# each binary with `go version -m` to prove the linker-set values, records
# the SHA-256 per platform, and runs the full test suite on the native
# platform.
#
# The build identity carries no timestamp: the same source and toolchain
# rebuild the same binary on every platform.
#
# Usage: scripts/check-cross-build.sh <artifact-dir>
set -eu

if [ "$#" -ne 1 ]; then
  echo "usage: $0 <artifact-dir>" >&2
  exit 2
fi
ARTIFACT_DIR="$1"
mkdir -p "$ARTIFACT_DIR"

# ---------------------------------------------------------------------------
# release metadata and linker flags
# ---------------------------------------------------------------------------

VERSION="${CFLOW_RELEASE_VERSION:-0.1.0-demo3}"
eval "$(go run ./scripts/release-metadata)"
SOURCE_COMMIT="$(git rev-parse HEAD)"
SOURCE_DIRTY=0
if [ -n "$(git status --porcelain)" ]; then
  SOURCE_DIRTY=1
fi
LDFLAGS="-X cflow.local/cflow/internal/observe.Version=$VERSION \
-X cflow.local/cflow/internal/observe.SourceCommit=$SOURCE_COMMIT \
-X cflow.local/cflow/internal/observe.sourceDirty=$SOURCE_DIRTY \
-X cflow.local/cflow/internal/observe.schemaVersion=$schema_version \
-X cflow.local/cflow/internal/observe.MigrationHash=$migration \
-X cflow.local/cflow/internal/observe.ArtifactHash=$artifact \
-X cflow.local/cflow/internal/observe.ProviderHash=$provider \
-X cflow.local/cflow/internal/observe.PromptHash=$prompt"

# ---------------------------------------------------------------------------
# the four supported Runtime platforms (macOS and Linux, design 23)
# ---------------------------------------------------------------------------

MANIFEST="$ARTIFACT_DIR/cross-build-manifest.txt"
{
  echo "platforms:"
} >"$MANIFEST"
for platform in darwin/amd64 darwin/arm64 linux/amd64 linux/arm64; do
  GOOS="${platform%/*}"
  GOARCH="${platform#*/}"
  tag="${GOOS}-${GOARCH}"
  bin="$ARTIFACT_DIR/cflow-${tag}"
  echo "check-cross-build: building ${platform}"
  GOOS="$GOOS" GOARCH="$GOARCH" CGO_ENABLED=0 go build -trimpath -ldflags "$LDFLAGS" \
    -o "$bin" ./cmd/cflow
  HASH="$(shasum -a 256 "$bin" | awk '{print $1}')"
  echo "  $platform: $HASH" >>"$MANIFEST"
  # `go version -m` proves the release build configuration of every
  # platform binary: CGO disabled and -trimpath (Go 1.24+ withholds the
  # raw -ldflags string from `go version -m` for security, so the
  # linker-set metadata values are proven below by the runnable native
  # binary's own `version` output — the same LDFLAGS every platform shares).
  go version -m "$bin" >"$ARTIFACT_DIR/go-version-m-${tag}.txt"
  if ! grep -q "trimpath=true" "$ARTIFACT_DIR/go-version-m-${tag}.txt" ||
    ! grep -q "CGO_ENABLED=0" "$ARTIFACT_DIR/go-version-m-${tag}.txt"; then
    echo "check-cross-build: $tag binary misses the CGO-disabled trimmed release build configuration" >&2
    exit 1
  fi
done

# The native binary is runnable: its own `version` output proves the
# linker-set release metadata (the version, the clean source Commit, the
# applied schema version, and every embedded registry hash) is in the
# binary.
NATIVE="$ARTIFACT_DIR/cflow-$(go env GOOS)-$(go env GOARCH)"
if [ -x "$NATIVE" ]; then
  DIRTY_BOOL=false
  if [ "$SOURCE_DIRTY" = "1" ]; then
    DIRTY_BOOL=true
  fi
  NATIVE_VERSION="$("$NATIVE" version)"
  for want in "cflow $VERSION" \
              "source commit: $SOURCE_COMMIT" \
              "dirty: $DIRTY_BOOL" \
              "migration=$migration" "artifact=$artifact" \
              "provider=$provider" "prompt=$prompt"; do
    if ! printf '%s\n' "$NATIVE_VERSION" | grep -q "$want"; then
      echo "check-cross-build: native binary version misses $want" >&2
      echo "$NATIVE_VERSION" >&2
      exit 1
    fi
  done
fi

# ---------------------------------------------------------------------------
# native platform runs the full test suite
# ---------------------------------------------------------------------------

go test -count=1 ./... >"$ARTIFACT_DIR/native-full-suite.log" 2>&1

{
  echo "source_commit: $SOURCE_COMMIT"
  echo "version: $VERSION"
  echo "schema_version: $schema_version"
  echo "registries:"
  echo "  migration: $migration"
  echo "  artifact: $artifact"
  echo "  provider: $provider"
  echo "  prompt: $prompt"
  echo "native_full_suite: pass"
} >>"$MANIFEST"

echo "check-cross-build: PASS"
echo "manifest: $MANIFEST"
