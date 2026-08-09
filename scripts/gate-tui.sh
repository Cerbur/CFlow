#!/usr/bin/env bash
# Fake TUI Gate (TUI task 16): the deterministic, fully-Fake-provider end
# to end evidence of the complete TUI workflow — the TUI/foreground/native
# suites, the full test suite, the cross-platform build proof
# (scripts/check-cross-build.sh), and a reproducible build of the candidate
# binary, with the Source Commit, Binary SHA-256, the full Go Version, the
# cross-build result, and the test logs bound into a redacted artifact
# directory. The artifact directory must be empty (or absent) before the
# run: a pre-populated directory is refused (fail closed) so stale
# artifacts can never be mistaken for this run's evidence. The Gate NEVER
# invokes a real Provider; the real Cross-Provider E2E and self-Dogfood
# remain separate, approval-gated runs (Gate 3 handles them).
#
# Usage: scripts/gate-tui.sh <new-empty-artifact-dir>
set -eu

if [ "$#" -ne 1 ]; then
  echo "usage: $0 <artifact-dir>" >&2
  exit 2
fi
ARTIFACT_DIR="$1"

# Fail closed on a pre-populated artifact directory: stale artifacts must
# never be mistaken for this run's evidence. If absent, create it; if
# present and non-empty (or not a directory), refuse to proceed. The Gate
# never deletes anything.
if [ -d "$ARTIFACT_DIR" ]; then
  if [ -n "$(ls -A "$ARTIFACT_DIR" 2>/dev/null)" ]; then
    echo "gate-tui: refusing to run: artifact directory is not empty: $ARTIFACT_DIR" >&2
    echo "gate-tui: move or remove its contents, or choose a new empty artifact directory; nothing was deleted" >&2
    exit 1
  fi
elif [ -e "$ARTIFACT_DIR" ]; then
  echo "gate-tui: refusing to run: artifact path exists and is not a directory: $ARTIFACT_DIR" >&2
  exit 1
else
  mkdir -p "$ARTIFACT_DIR"
fi
START_EPOCH="$(date +%s)"

# ---------------------------------------------------------------------------
# the deterministic Fake suites
# ---------------------------------------------------------------------------

go test ./internal/tui ./internal/foreground ./internal/native -count=1 \
  >"$ARTIFACT_DIR/tui-suites.log" 2>&1
TUI_SUITES="pass"

go test ./... -count=1 -timeout 900s >"$ARTIFACT_DIR/full-suite.log" 2>&1
FULL_SUITE="pass"

# ---------------------------------------------------------------------------
# source facts and the reproducible candidate build
# ---------------------------------------------------------------------------

SOURCE_COMMIT="$(git rev-parse HEAD)"
SOURCE_SUBJECT="$(git log -1 --format=%s)"
if [ -n "$(git status --porcelain)" ]; then
  echo "gate-tui: the working tree is not clean:" >&2
  git status --porcelain >&2
  exit 1
fi
GIT_CLEAN="true"

# The cross-platform build proof (the same check the release gates run):
# CGO-disabled -trimpath builds for all four supported Runtime platforms,
# `go version -m` build-configuration inspection, the native binary's own
# release-metadata output, and the native full test suite. A cross-build
# failure fails the Gate closed.
scripts/check-cross-build.sh "$ARTIFACT_DIR/cross" >"$ARTIFACT_DIR/cross-build.log" 2>&1
CROSS_BUILD="pass"

BIN="$ARTIFACT_DIR/cflow"
go build -trimpath -o "$BIN" ./cmd/cflow
BIN_SHA256="$(shasum -a 256 "$BIN" | awk '{print $1}')"
GO_VERSION="$(go version)"
if ! printf '%s\n' "$GO_VERSION" | grep -q "^go version go"; then
  echo "gate-tui: the manifest must bind the full go version output, got: $GO_VERSION" >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# redacted Manifest (a NEW Internal Candidate; never "Released")
# ---------------------------------------------------------------------------

ELAPSED="$(( $(date +%s) - START_EPOCH ))"
MANIFEST="$ARTIFACT_DIR/gate-tui-manifest.txt"
{
  echo "candidate: Internal Candidate (Fake TUI Gate)"
  echo "generated_at: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "elapsed_seconds: $ELAPSED"
  echo "source_commit: $SOURCE_COMMIT"
  echo "source_subject: $SOURCE_SUBJECT"
  echo "git_clean: $GIT_CLEAN"
  echo "source_dirty: false"
  echo "binary_sha256: $BIN_SHA256"
  echo "go_version: $GO_VERSION"
  echo "checks:"
  echo "  tui_suites: $TUI_SUITES"
  echo "  full_suite: $FULL_SUITE"
  echo "  cross_build: $CROSS_BUILD"
  echo "evidence:"
  echo "  tui_suites_log: tui-suites.log"
  echo "  full_suite_log: full-suite.log"
  echo "  cross_build_log: cross-build.log"
  echo "  binary: cflow"
  echo "real_provider: not_run (the real Cross-Provider E2E and self-Dogfood require separate explicit user approval)"
} >"$MANIFEST"

echo "gate-tui: PASS"
echo "manifest: $MANIFEST"
