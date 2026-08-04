#!/usr/bin/env bash
# Gate 1 evidence script (Task 13, brief Step 6; PRD 第一层：确定性 Fixture
# Gate). Runs the deterministic checks of the Internal Core Candidate —
# formatting, unit/integration/E2E tests, vet, a CGO-disabled build, the
# binary SHA-256, the Git source Commit and clean status, and the fixture
# tool prerequisites — and writes a redacted Manifest to a caller-provided
# artifact directory.
#
# The Manifest declares the candidate "Internal Core Candidate" and NEVER
# "Demo Complete" (Global Constraint: Gate 1/2 artifacts are internal
# candidates, not the final demo).
#
# Usage: scripts/gate1.sh <artifact-dir>
set -eu

if [ "$#" -ne 1 ]; then
  echo "usage: $0 <artifact-dir>" >&2
  exit 2
fi
ARTIFACT_DIR="$1"
mkdir -p "$ARTIFACT_DIR"

# ---------------------------------------------------------------------------
# prerequisites
# ---------------------------------------------------------------------------

# The calculator fixture runs `npm test` through the Catalog; Gate 1
# reports a clear prerequisite failure when the fixture tools are absent.
for tool in node npm; do
  if ! command -v "$tool" >/dev/null 2>&1; then
    echo "Gate 1 prerequisite missing: $tool is not on PATH" >&2
    echo "the calculator fixture needs node (Node 26.x) and npm (11.x)" >&2
    exit 1
  fi
done
NODE_VERSION="$(node --version)"
NPM_VERSION="$(npm --version)"

# ---------------------------------------------------------------------------
# formatting check
# ---------------------------------------------------------------------------

UNFORMATTED="$(gofmt -l internal tests)"
if [ -n "$UNFORMATTED" ]; then
  echo "gate1: unformatted files:" >&2
  echo "$UNFORMATTED" >&2
  exit 1
fi
GOFMT_OK="pass"

# ---------------------------------------------------------------------------
# tests
# ---------------------------------------------------------------------------

# Unit and integration tests including the race detector over internal/.
go test -race ./internal/... >"$ARTIFACT_DIR/internal-race.log" 2>&1
INTERNAL_RACE="pass"

# The Fake end-to-end flows (Task 12/13 fixtures).
go test ./tests/integration/... ./tests/e2e/... -run 'TestFake' >"$ARTIFACT_DIR/fake-e2e.log" 2>&1
FAKE_E2E="pass"

# The full suite.
go test ./... >"$ARTIFACT_DIR/full-suite.log" 2>&1
FULL_SUITE="pass"

# ---------------------------------------------------------------------------
# vet
# ---------------------------------------------------------------------------

go vet ./... >"$ARTIFACT_DIR/vet.log" 2>&1
VET="pass"

# ---------------------------------------------------------------------------
# CGO-disabled build and binary hash
# ---------------------------------------------------------------------------

# The release metadata (design 23): the applied schema version, the embedded
# registry hashes, and the enabled Provider binding hashes, derived from the
# embedded registries by scripts/release-metadata. gate1 records them so the
# Gate 3 validation can prove the evidence is exact and current.
RELEASE_METADATA="$(go run ./scripts/release-metadata)"
eval "$RELEASE_METADATA"
SCHEMA_VERSION="$schema_version"
REG_MIGRATION="$migration"
REG_ARTIFACT="$artifact"
REG_PROVIDER="$provider"
REG_PROMPT="$prompt"
BINDING_CODEX="$codex"
BINDING_CLAUDE="$claude"

# The CGO-disabled build is reproducible: -trimpath strips the build
# directory, so the same source and toolchain produce the same binary.
BIN="$ARTIFACT_DIR/cflow"
CGO_ENABLED=0 go build -trimpath -o "$BIN" ./cmd/cflow
BINARY_SHA256="$(shasum -a 256 "$BIN" | awk '{print $1}')"
GO_VERSION="$(go version)"

# ---------------------------------------------------------------------------
# Git source facts
# ---------------------------------------------------------------------------

SOURCE_COMMIT="$(git rev-parse HEAD)"
SOURCE_SUBJECT="$(git log -1 --format=%s)"
if [ -n "$(git status --porcelain)" ]; then
  echo "gate1: the working tree is not clean:" >&2
  git status --porcelain >&2
  exit 1
fi
GIT_CLEAN="true"

# ---------------------------------------------------------------------------
# redacted Manifest (Internal Core Candidate; never "Demo Complete")
# ---------------------------------------------------------------------------

MANIFEST="$ARTIFACT_DIR/gate1-manifest.txt"
{
  echo "candidate: Internal Core Candidate"
  echo "generated_at: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "source_commit: $SOURCE_COMMIT"
  echo "source_subject: $SOURCE_SUBJECT"
  echo "git_clean: $GIT_CLEAN"
  echo "source_dirty: false"
  echo "binary_sha256: $BINARY_SHA256"
  echo "go_version: $GO_VERSION"
  echo "schema_version: $SCHEMA_VERSION"
  echo "registries:"
  echo "  migration: $REG_MIGRATION"
  echo "  artifact: $REG_ARTIFACT"
  echo "  provider: $REG_PROVIDER"
  echo "  prompt: $REG_PROMPT"
  echo "provider_bindings:"
  echo "  codex: $BINDING_CODEX"
  echo "  claude: $BINDING_CLAUDE"
  echo "fixture_node: $NODE_VERSION"
  echo "fixture_npm: $NPM_VERSION"
  echo "checks:"
  echo "  gofmt: $GOFMT_OK"
  echo "  internal_race: $INTERNAL_RACE"
  echo "  fake_e2e: $FAKE_E2E"
  echo "  full_suite: $FULL_SUITE"
  echo "  vet: $VET"
} >"$MANIFEST"

echo "gate1: PASS"
echo "manifest: $MANIFEST"
