# CFlow Demo Acceptance Report (Gate 3 candidate)

- **Candidate:** Demo Complete Candidate
- **Date:** 2026-08-04
- **Repository:** CFlow worktree `cflow/demo-implementation`
- **Status:** Gate 3 candidate produced; **NOT a release**. The final user
  release sign-off is the separate human step at the end of this document.

This report records the exact commands, exits, durations, Commit IDs, binary
hashes, embedded registry hashes, platforms, real Provider facts, workflow
evidence, Apply facts, and known limits of the Gate 3 candidate. Sections
whose evidence requires an approval-gated real run (the real Cross-Provider
E2E and the self-Dogfood) record the honest pending state until the user
authorizes and runs them; the offline deterministic equivalents run in the
Gate 3 suite.

## 1. Build metadata

The release candidate embeds the full build identity through linker flags
(`-X cflow.local/cflow/internal/observe.<Var>=<value>`), with **no timestamp
in the binary identity**: a rebuild from the same source and toolchain
produces the same SHA-256. Go 1.24+ withholds the raw `-ldflags` string from
`go version -m` for security, so the linker-set values are proven by the
runnable binary's own `cflow version` output, and `go version -m` proves the
release build configuration (`-trimpath`, `CGO_ENABLED=0`).

| Metadata | Value |
|---|---|
| Version | `0.1.0-demo3` |
| Source Commit | `48252832f61839854bf3d5007b4e8322ecd7e72c` (`fix: harden apply cas, orphan processes, and process map races`) |
| Source dirty | `false` (Git-clean workspace) |
| Schema version | `4` (embedded migration chain `001..004`) |
| Migration registry hash | `9e1408b44ca60a42fbb00ca0f3c9aac8adf9e183c24df9e61a924dcd0fa993c5` |
| Artifact compatibility hash | `4b574d27fb5b52decbbc472f3280490f77f66a7d64b9d9e9ab866a5d2b620bb9` |
| Provider registry hash | `5a15d8052dac090e139cc783e04f0f2f49eb9c938db53c69743a3b11aa5f9b8f` |
| Prompt registry hash | `d18562d2f278345ca9d5e527c80c30143c56468b1fea7072ca08329785b863d4` |
| Provider binding hashes | codex `948075b98cd91f41f2b8349aab02e15b8edfad6183c4230ab55716e620affb2a`, claude `a6badc4f2b1d7bd65c38de93bdeb0dd4819d4a3fa12e9269ba8c5793b323d143` |

These values are derived from the embedded registries by
`scripts/release-metadata` (they never come from Git or the environment), so
Gate 1, Gate 2, and Gate 3 Manifests recorded in the same repository agree
byte-for-byte with the candidate binary. `scripts/check-cross-build.sh` and
`scripts/gate3.sh` prove the stamping by running the native binary's
`version` output and asserting every pinned value.

## 2. Deterministic Gate evidence

`scripts/gate3.sh <artifact-dir>` reruns Gate 1 and Gate 2, the
cross-platform matrix, the native race tests, the release-evidence
validation, and the recursive secret scan. The authoritative manifest it
wrote at the final source Commit `48252832f61839854bf3d5007b4e8322ecd7e72c`
(elapsed 858s) is:

```
candidate: Demo Complete Candidate
git_clean: true
source_dirty: false
binary_sha256: 2d0ec9f76f7cdb924f23584e48e811c9bd78f0d722b1af858bf8c280ce521b41
schema_version: 4
registries:
  migration: 9e1408b44ca60a42fbb00ca0f3c9aac8adf9e183c24df9e61a924dcd0fa993c5
  artifact: 4b574d27fb5b52decbbc472f3280490f77f66a7d64b9d9e9ab866a5d2b620bb9
  provider: 5a15d8052dac090e139cc783e04f0f2f49eb9c938db53c69743a3b11aa5f9b8f
  prompt: d18562d2f278345ca9d5e527c80c30143c56468b1fea7072ca08329785b863d4
provider_bindings:
  codex: 948075b98cd91f41f2b8349aab02e15b8edfad6183c4230ab55716e620affb2a
  claude: a6badc4f2b1d7bd65c38de93bdeb0dd4819d4a3fa12e9269ba8c5793b323d143
checks:
  gate1: pass            gate2: pass
  cross_build: pass      native_race: pass
  gate1_evidence: pass   gate2_evidence: pass
  real_cross_provider: pending (approval-gated; not authorized)
  dogfood: pending (approval-gated; not authorized)
  secret_scan: pass
```

The full manifest (with `generated_at` and the pending-state wording) is
recorded in `test-artifacts/gate3-final/gate3-manifest.txt`.

The release-evidence validation (`observe.ValidateReleaseEvidence`,
`internal/observe/release_test.go`) rejects a Gate manifest recorded by a
different subject with `EVIDENCE_SUBJECT_CHANGED`. The offline case list
proves: dirty source, wrong source Commit, missing registry hash, Gate 1/2
label mismatch, real E2E produced by a different binary, stale Provider
binding, missing review/evidence, and a contaminated Dogfood target
workspace.

## 3. Cross-platform matrix

`scripts/check-cross-build.sh` compiled the CGO-disabled single binary for
all four supported Runtime platforms and recorded the SHA-256 per platform
from the authoritative final run (source Commit `4825283`):

| Platform | SHA-256 |
|---|---|
| darwin/amd64 | `aed8e90512668db9ab3ddd9bc82532caed57528aca0a88f3bbb91f1da74cb286` |
| darwin/arm64 | `2d0ec9f76f7cdb924f23584e48e811c9bd78f0d722b1af858bf8c280ce521b41` |
| linux/amd64 | `91694ab905935d7df7176e32a22312b9ad3435f727308fc141e273d0463ee4c7` |
| linux/arm64 | `ccbc91127c81fd3163306c3c9c190fb053683357b198136fa728a39ed47f20e4` |

The darwin/arm64 cross-build hash equals the Gate 3 candidate binary hash
(`2d0ec9f7…`): the native release candidate is byte-identical to the
darwin/arm64 matrix build, direct reproducibility evidence. Every platform
binary was inspected with `go version -m` (proving `-trimpath=true` and
`CGO_ENABLED=0`), and the native binary's `version` output proved the pinned
version, source Commit, dirty flag, and every embedded registry hash. The
native full test suite passed (`native_full_suite: pass`).

## 4. Real Cross-Provider E2E evidence

**Status: pending (approval-gated).** `TestRealCrossProvider`
(`tests/e2e/cross_provider_test.go`) runs only with `CFLOW_E2E_REAL=1`,
which requires explicit user approval of the exact Dry Run, the provider
routes/models/budgets, the default-permission trust boundary, and the
potential network/cost. It has **not** been authorized.

When authorized, the controller runs it and records the evidence as an
`observe.ReleaseEvidenceFile` (kind `real-cross-provider`) — the release
candidate binary SHA-256, the source Commit, the report hash, and the
independent review — which `scripts/gate3.sh` validates against the release
candidate. The offline deterministic equivalent (`TestDialectEquivalent
CrossProvider`) passes in the Gate 2 suite and covers routing, parallel
dispatch, independent reviews, serial merges, Final Verify/Review, and the
immutable report.

## 5. Self-Dogfood evidence

**Status: pending (approval-gated).** `TestDogfood`
(`tests/e2e/dogfood_test.go`) runs only with `CFLOW_DOGFOOD_REAL=1`, which
requires explicit user approval of the exact Dry Run, the provider
routes/models/budgets, the default-permission trust boundary, the
network/cost, the bounded docs-or-tests-only requirement, and the Apply
target (the dogfood Apply advances the target branch of the CFlow
repository itself). It has **not** been authorized.

The offline deterministic dogfood-equivalent (`TestDogfoodPreflight`) runs
in the Gate 3 suite and proves the harness: the candidate binary is copied
to an immutable path outside the target repository, its SHA-256 is pinned,
and the preflight (`observe.ValidateDogfoodPreflight`) rejects a dirty
source, a binary inside the repository, a target workspace that is (or
contains) the original developer workspace, an unbounded or unapproved
requirement, and a missing cross-provider route with
`EVIDENCE_SUBJECT_CHANGED`.

When authorized, the run records the evidence as an `observe.
ReleaseEvidenceFile` (kind `dogfood`) — binary SHA-256, source Commit,
target and original workspaces, the bounded requirement hash, the codex and
claude routes, the workflow hash, and the Apply old/new Target heads — which
`scripts/gate3.sh` validates against the release candidate.

## 6. Workflow / Approval / Artifact evidence

The deterministic Gate 1 and Gate 2 suites prove the full lifecycle offline
(the calculator fixture end-to-end, the dialect-equivalent cross-provider
flow, the protected Apply, the Safe Cleanup, the fault/recovery matrices).
The approval-gated real runs add real-Provider Workflow/Approval/Artifact
hashes; until they are authorized, the offline deterministic evidence
stands (the specific hash values of an authorized run are recorded in the
evidence files above when produced).

## 7. Apply old/new Target heads

The protected Apply is proven offline by the apply integration tests
(`tests/integration/apply_test.go`): `TestApplyDeliversCompletedWorkflowTo
Target` fast-forwards the Target Branch to the verified staging head and
records the old/new heads, and `TestApplyTargetCASLateAdvanceBlocksDelivery`
proves the compare-and-swap refuses a late advance with
`TARGET_HEAD_DRIFTED`, leaving the Target exactly at the late advance. The
real self-Dogfood Apply old/new heads are recorded in the dogfood evidence
when the user authorizes the run.

## 8. Commands, exits, and durations

| Command | Exit | Duration |
|---|---|---|
| `gofmt -l internal tests scripts` (format check) | 0 (no unformatted files) | seconds |
| `go test -race ./...` | 0 | minutes |
| `go vet ./...` | 0 | seconds |
| `go test ./...` | 0 | minutes |
| `./scripts/check-cross-build.sh <artifact-dir>` | 0 | ~2-3 minutes |
| `./scripts/gate3.sh <artifact-dir>` | 0 | 858 seconds (~14 min) |
| `git status --porcelain` | 0 (empty) | seconds |

The authoritative gate3 manifest (source Commit `4825283`, `elapsed_seconds:
858`) is recorded in `test-artifacts/gate3-final/gate3-manifest.txt`.

## 9. Known limits

- Go 1.24+ withholds the raw `-ldflags` string from `go version -m`, so the
  linker-set release metadata is proven by the runnable binary's `version`
  output rather than by `go version -m` alone (the build configuration is
  still proven by `go version -m`).
- The CLI registers the deterministic Fake Adapter under the provider names;
  the real Codex and Claude adapters are exercised in the approval-gated
  in-process E2E and self-Dogfood runs, not through the interactive CLI.
- Windows is supported through WSL only.
- The real E2E and self-Dogfood evidence sections remain pending until the
  user authorizes the runs; the Gate 3 manifest reports the pending state
  and is re-run by the controller after authorization.
- P1 OpenCode, the Provider TUI, and cost analytics are excluded from the
  Demo candidate by design.

## 10. User release sign-off

The Gate 3 candidate is **Demo Complete Candidate**. This section is
intentionally blank until the user performs the final release acceptance.
No push, tag, PR, or remote publication has occurred.

- [ ] The user has reviewed this report and the Gate 3 evidence.
- [ ] The user authorizes the real Cross-Provider E2E and self-Dogfood runs.
- [ ] The user performs the final release sign-off and records it here.
