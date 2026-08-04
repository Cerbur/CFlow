#!/usr/bin/env bash
# Gate 3 evidence script (Task 22, brief Step 5; PRD 已确认：三层内部交付
# Gate §Gate 3). Reruns the Gate 1 and Gate 2 deterministic suites, the
# release fault matrices, the cross-platform build matrix with the native
# race tests, validates the already-authorized real Cross-Provider E2E and
# self-Dogfood evidence (which never run here — they require separate
# explicit user approval), builds the CGO-disabled release candidate with
# the fixed version/source/dirty/schema/migration/Artifact/Provider/prompt
# hashes through linker flags, pins the binary SHA-256, the source Commit,
# the clean status, and runs a recursive secret scan over the redacted
# evidence.
#
# The Manifest declares the candidate "Demo Complete Candidate" and NEVER
# "Released" (Global Constraint: the final user release sign-off is a
# separate human step; this script produces the candidate, not the release).
#
# The real E2E and Dogfood evidence validation is honest about the
# not-yet-run state: when the user has authorized the runs and the evidence
# files exist (CFLOW_REAL_E2E_EVIDENCE and CFLOW_DOGFOOD_EVIDENCE, or the
# default paths $ARTIFACT_DIR/real-cross-provider.json and
# $ARTIFACT_DIR/dogfood.json), they are validated against the release
# candidate binary; otherwise the manifest records the pending state.
#
# Usage: scripts/gate3.sh <artifact-dir>
set -eu

if [ "$#" -ne 1 ]; then
  echo "usage: $0 <artifact-dir>" >&2
  exit 2
fi
ARTIFACT_DIR="$1"
mkdir -p "$ARTIFACT_DIR"
START_EPOCH="$(date +%s)"

# ---------------------------------------------------------------------------
# Gate 1 and Gate 2 reruns (the Internal Core / Runtime Candidate evidence)
# ---------------------------------------------------------------------------

if [ ! -x scripts/gate1.sh ] || [ ! -x scripts/gate2.sh ]; then
  echo "gate3: scripts/gate1.sh and scripts/gate2.sh are required" >&2
  exit 1
fi
scripts/gate1.sh "$ARTIFACT_DIR/gate1" >"$ARTIFACT_DIR/gate1.log" 2>&1
GATE1="pass"
scripts/gate2.sh "$ARTIFACT_DIR/gate2" >"$ARTIFACT_DIR/gate2.log" 2>&1
GATE2="pass"

# ---------------------------------------------------------------------------
# cross-platform build matrix + native race tests
# ---------------------------------------------------------------------------

scripts/check-cross-build.sh "$ARTIFACT_DIR/cross" >"$ARTIFACT_DIR/cross.log" 2>&1
CROSS_BUILD="pass"
go test -race ./internal/... >"$ARTIFACT_DIR/native-race.log" 2>&1
NATIVE_RACE="pass"

# ---------------------------------------------------------------------------
# release metadata and the CGO-disabled release candidate build
# ---------------------------------------------------------------------------

VERSION="${CFLOW_RELEASE_VERSION:-0.1.0-demo3}"
eval "$(go run ./scripts/release-metadata)"
SOURCE_COMMIT="$(git rev-parse HEAD)"
SOURCE_SUBJECT="$(git log -1 --format=%s)"
if [ -n "$(git status --porcelain)" ]; then
  echo "gate3: the working tree is not clean:" >&2
  git status --porcelain >&2
  exit 1
fi
GIT_CLEAN="true"

LDFLAGS="-X cflow.local/cflow/internal/observe.Version=$VERSION \
-X cflow.local/cflow/internal/observe.SourceCommit=$SOURCE_COMMIT \
-X cflow.local/cflow/internal/observe.sourceDirty=0 \
-X cflow.local/cflow/internal/observe.schemaVersion=$schema_version \
-X cflow.local/cflow/internal/observe.MigrationHash=$migration \
-X cflow.local/cflow/internal/observe.ArtifactHash=$artifact \
-X cflow.local/cflow/internal/observe.ProviderHash=$provider \
-X cflow.local/cflow/internal/observe.PromptHash=$prompt"

BIN="$ARTIFACT_DIR/cflow-release"
CGO_ENABLED=0 go build -trimpath -ldflags "$LDFLAGS" -o "$BIN" ./cmd/cflow
RELEASE_BIN_SHA256="$(shasum -a 256 "$BIN" | awk '{print $1}')"
GO_VERSION="$(go version)"
go version -m "$BIN" >"$ARTIFACT_DIR/go-version-m-release.txt"
"$BIN" version >"$ARTIFACT_DIR/release-version.txt"
# The release binary itself proves the pinned metadata (Go 1.24+ withholds
# the raw -ldflags string from `go version -m` for security, so the
# runnable binary's version output is the proof).
for want in "cflow $VERSION" "source commit: $SOURCE_COMMIT" "dirty: false" \
            "migration=$migration" "artifact=$artifact" \
            "provider=$provider" "prompt=$prompt"; do
  if ! grep -q "$want" "$ARTIFACT_DIR/release-version.txt"; then
    echo "gate3: the release candidate binary misses $want" >&2
    exit 1
  fi
done

# ---------------------------------------------------------------------------
# deterministic Gate evidence validation (exact and current)
# ---------------------------------------------------------------------------

# The Gate manifests must be exact and current: the binary_sha256 each gate
# recorded must equal a reproducible rebuild of that gate's own build
# command in this same environment, and the manifests must match the actual
# candidate facts (source Commit, clean flag, embedded registries, binding
# hashes, labels, and the required checks).
GATE1_REBUILD="$ARTIFACT_DIR/rebuild/gate1"
GATE2_REBUILD="$ARTIFACT_DIR/rebuild/gate2"
mkdir -p "$ARTIFACT_DIR/rebuild"
CGO_ENABLED=0 go build -trimpath -o "$GATE1_REBUILD" ./cmd/cflow
CGO_ENABLED=0 go build -trimpath -o "$GATE2_REBUILD" ./cmd/cflow
GATE1_BIN_SHA="$(shasum -a 256 "$GATE1_REBUILD" | awk '{print $1}')"
GATE2_BIN_SHA="$(shasum -a 256 "$GATE2_REBUILD" | awk '{print $1}')"

COMMON_FACTS=(-source-commit "$SOURCE_COMMIT" -source-dirty false \
  -schema-version "$schema_version" \
  -migration-hash "$migration" -artifact-hash "$artifact" \
  -provider-hash "$provider" -prompt-hash "$prompt" \
  -codex-binding "$codex" -claude-binding "$claude")

go run ./scripts/validate-evidence \
  -manifest "$ARTIFACT_DIR/gate1/gate1-manifest.txt" \
  -expect "Internal Core Candidate" \
  -binary-sha256 "$GATE1_BIN_SHA" \
  "${COMMON_FACTS[@]}" >"$ARTIFACT_DIR/validate-gate1.log" 2>&1
GATE1_EVIDENCE="pass"
go run ./scripts/validate-evidence \
  -manifest "$ARTIFACT_DIR/gate2/gate2-manifest.txt" \
  -expect "Internal Runtime Candidate" \
  -binary-sha256 "$GATE2_BIN_SHA" \
  "${COMMON_FACTS[@]}" >"$ARTIFACT_DIR/validate-gate2.log" 2>&1
GATE2_EVIDENCE="pass"

# ---------------------------------------------------------------------------
# optional real Cross-Provider E2E and self-Dogfood evidence validation
# (approval-gated: these NEVER execute here)
# ---------------------------------------------------------------------------

REAL_E2E_EVIDENCE="${CFLOW_REAL_E2E_EVIDENCE:-$ARTIFACT_DIR/real-cross-provider.json}"
DOGFOOD_EVIDENCE="${CFLOW_DOGFOOD_EVIDENCE:-$ARTIFACT_DIR/dogfood.json}"

if [ -f "$REAL_E2E_EVIDENCE" ]; then
  go run ./scripts/validate-evidence \
    -manifest "$ARTIFACT_DIR/gate2/gate2-manifest.txt" \
    -expect "Internal Runtime Candidate" \
    -binary-sha256 "$GATE2_BIN_SHA" \
    -real-e2e "$REAL_E2E_EVIDENCE" \
    -release-binary-sha256 "$RELEASE_BIN_SHA256" \
    "${COMMON_FACTS[@]}" >"$ARTIFACT_DIR/validate-real-e2e.log" 2>&1
  REAL_E2E="pass"
else
  REAL_E2E="pending (the real Cross-Provider E2E requires explicit user approval of the exact Dry Run, routes/models/budgets, the default-permission trust boundary, and the network/cost; it has not been authorized)"
fi
if [ -f "$DOGFOOD_EVIDENCE" ]; then
  go run ./scripts/validate-evidence \
    -manifest "$ARTIFACT_DIR/gate2/gate2-manifest.txt" \
    -expect "Internal Runtime Candidate" \
    -binary-sha256 "$GATE2_BIN_SHA" \
    -dogfood "$DOGFOOD_EVIDENCE" \
    -release-binary-sha256 "$RELEASE_BIN_SHA256" \
    "${COMMON_FACTS[@]}" >"$ARTIFACT_DIR/validate-dogfood.log" 2>&1
  DOGFOOD="pass"
else
  DOGFOOD="pending (the self-Dogfood requires explicit user approval of the exact Dry Run, routes/models/budgets, the default-permission trust boundary, the network/cost, the bounded docs-or-tests-only requirement, and the Apply target; it has not been authorized)"
fi

# ---------------------------------------------------------------------------
# recursive secret scan over the redacted evidence
# ---------------------------------------------------------------------------

# High-confidence raw-secret patterns, scanned over the text evidence only
# (the CGO-disabled binaries are excluded from the scan). The redaction
# placeholders ([REDACTED:category]) never match; a raw secret in any
# evidence file fails the Gate closed.
SCAN_PATTERNS=(
  'sk-ant-[A-Za-z0-9]{10,}'
  'sk-[A-Za-z0-9]{20,}'
  'ghp_[A-Za-z0-9]{30,}'
  'github_pat_[A-Za-z0-9_]{20,}'
  'AKIA[0-9A-Z]{16}'
  'AIza[0-9A-Za-z_-]{30,}'
  'xox[baprs]-[A-Za-z0-9-]{10,}'
  '-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----'
  '(api[_-]?key|password|passwd|secret|token)[[:space:]]*[:=][[:space:]]*[^[:space:]]{16,}'
)
PATTERN="$(IFS='|'; echo "${SCAN_PATTERNS[*]}")"
HITS="$(grep -rlE "$PATTERN" "$ARTIFACT_DIR" \
  --include='*.txt' --include='*.log' --include='*.json' --include='*.md' 2>/dev/null || true)"
if [ -n "$HITS" ]; then
  echo "gate3: raw secret pattern found in the redacted evidence:" >&2
  printf '%s\n' "$HITS" >&2
  exit 1
fi
SECRET_SCAN="pass"

# ---------------------------------------------------------------------------
# redacted Manifest (Demo Complete Candidate; never "Released")
# ---------------------------------------------------------------------------

ELAPSED="$(( $(date +%s) - START_EPOCH ))"
MANIFEST="$ARTIFACT_DIR/gate3-manifest.txt"
{
  echo "candidate: Demo Complete Candidate"
  echo "generated_at: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "elapsed_seconds: $ELAPSED"
  echo "source_commit: $SOURCE_COMMIT"
  echo "source_subject: $SOURCE_SUBJECT"
  echo "git_clean: $GIT_CLEAN"
  echo "source_dirty: false"
  echo "binary_sha256: $RELEASE_BIN_SHA256"
  echo "go_version: $GO_VERSION"
  echo "schema_version: $schema_version"
  echo "registries:"
  echo "  migration: $migration"
  echo "  artifact: $artifact"
  echo "  provider: $provider"
  echo "  prompt: $prompt"
  echo "provider_bindings:"
  echo "  codex: $codex"
  echo "  claude: $claude"
  echo "checks:"
  echo "  gate1: $GATE1"
  echo "  gate2: $GATE2"
  echo "  cross_build: $CROSS_BUILD"
  echo "  native_race: $NATIVE_RACE"
  echo "  gate1_evidence: $GATE1_EVIDENCE"
  echo "  gate2_evidence: $GATE2_EVIDENCE"
  echo "  real_cross_provider: $REAL_E2E"
  echo "  dogfood: $DOGFOOD"
  echo "  secret_scan: $SECRET_SCAN"
} >"$MANIFEST"

echo "gate3: PASS"
echo "manifest: $MANIFEST"
