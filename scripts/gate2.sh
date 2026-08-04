#!/usr/bin/env bash
# Gate 2 evidence script (Task 18, brief Step 7; PRD 第二层：内部 Runtime
# Gate). Reruns the Gate 1 evidence, adds the offline protocol/routing/
# recovery matrices and the deterministic dialect-equivalent Cross-Provider
# flow, optionally validates the approved real Cross-Provider evidence
# (CFLOW_E2E_REAL=1, which must never be set without explicit user
# approval of the Dry Run, routes/models/budgets, trust boundary, and
# network/cost), builds the CGO-disabled binary, pins the binary SHA-256,
# the Git source Commit and the clean status, and writes a redacted
# Manifest to a caller-provided artifact directory.
#
# The Manifest declares the candidate "Internal Runtime Candidate" and
# NEVER "Demo Complete" (Global Constraint: Gate 2 artifacts are internal
# candidates, not the final demo).
#
# Usage: scripts/gate2.sh <artifact-dir>
set -eu

if [ "$#" -ne 1 ]; then
  echo "usage: $0 <artifact-dir>" >&2
  exit 2
fi
ARTIFACT_DIR="$1"
mkdir -p "$ARTIFACT_DIR"

# ---------------------------------------------------------------------------
# Gate 1 rerun (the Internal Core Candidate evidence)
# ---------------------------------------------------------------------------

if [ -x scripts/gate1.sh ]; then
  scripts/gate1.sh "$ARTIFACT_DIR/gate1" >"$ARTIFACT_DIR/gate1.log" 2>&1
  GATE1="pass"
else
  echo "gate2: scripts/gate1.sh is missing" >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# offline protocol / routing / recovery matrices
# ---------------------------------------------------------------------------

# The fault and recovery matrices (Task 17/21): the decision fault table
# and the Recovery sweep over real repositories.
go test ./internal/recovery ./internal/decision -run 'TestFault|TestRecovery|TestMatrix' \
  >"$ARTIFACT_DIR/fault-recovery-matrix.log" 2>&1
FAULT_RECOVERY="pass"

# The cross-provider routing and drift gates (Task 16) plus the policy
# drift and stop matrices (Task 17).
go test ./tests/integration/... -run 'TestProviderRouting|TestPolicyDrift|TestStop|TestCancel' \
  >"$ARTIFACT_DIR/routing-matrix.log" 2>&1
ROUTING_MATRIX="pass"

# The deterministic dialect-equivalent Cross-Provider flow and the Fake
# end-to-end deliveries (Gate 2 offline equivalent of the real E2E).
go test ./tests/e2e/... -run 'TestDialectEquivalent|TestFake' \
  >"$ARTIFACT_DIR/dialect-equivalent.log" 2>&1
DIALECT_EQUIVALENT="pass"

# ---------------------------------------------------------------------------
# optional real Cross-Provider evidence validation (approval-gated)
# ---------------------------------------------------------------------------

if [ "${CFLOW_E2E_REAL:-}" = "1" ]; then
  echo "gate2: running the approved real Cross-Provider E2E (CFLOW_E2E_REAL=1)" >&2
  go test ./tests/e2e -run TestRealCrossProvider -count=1 -v \
    >"$ARTIFACT_DIR/real-cross-provider.log" 2>&1
  REAL_E2E="pass"
else
  REAL_E2E="skipped (the real run requires explicit user approval of the exact Dry Run, routes/models/budgets, the default-permission trust boundary, and the potential network/cost; set CFLOW_E2E_REAL=1 only after approval)"
fi

# ---------------------------------------------------------------------------
# CGO-disabled build and binary hash
# ---------------------------------------------------------------------------

# The release metadata (design 23): the applied schema version, the embedded
# registry hashes, and the enabled Provider binding hashes, derived from the
# embedded registries by scripts/release-metadata. gate2 records them so the
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
  echo "gate2: the working tree is not clean:" >&2
  git status --porcelain >&2
  exit 1
fi
GIT_CLEAN="true"

# ---------------------------------------------------------------------------
# redacted Manifest (Internal Runtime Candidate; never "Demo Complete")
# ---------------------------------------------------------------------------

MANIFEST="$ARTIFACT_DIR/gate2-manifest.txt"
{
  echo "candidate: Internal Runtime Candidate"
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
  echo "checks:"
  echo "  gate1: $GATE1"
  echo "  fault_recovery_matrix: $FAULT_RECOVERY"
  echo "  routing_matrix: $ROUTING_MATRIX"
  echo "  dialect_equivalent: $DIALECT_EQUIVALENT"
  echo "  real_cross_provider: $REAL_E2E"
} >"$MANIFEST"

echo "gate2: PASS"
echo "manifest: $MANIFEST"
