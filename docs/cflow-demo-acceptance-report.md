# CFlow Demo Acceptance Report (Gate 3 candidate)

- **Candidate:** Demo Complete Candidate
- **Date:** 2026-08-07
- **Repository:** CFlow worktree `cflow/demo-implementation`
- **Status:** Gate 3 candidate produced with the real Cross-Provider E2E
  and the self-Dogfood both **run and passed**; **NOT a release**. The
  final user release sign-off is the separate human step at the end of
  this document.

This report records the exact commands, exits, durations, Commit IDs, binary
hashes, embedded registry hashes, platforms, real Provider facts, workflow
evidence, Apply facts, and known limits of the Gate 3 candidate. The
approval-gated real runs (the real Cross-Provider E2E and the self-Dogfood)
were authorized and executed; their evidence files are validated by the
official `validate-evidence` tool and recorded under
`test-artifacts/gate3-final39/`.

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
| Source Commit | `68fd200bb52d6b5878bd4707905996ec8c99a055` (`fix: give the apply verification session the full acceptance evidence`) |
| Source dirty | `false` (Git-clean workspace) |
| Schema version | `4` (embedded migration chain `001..004`) |
| Migration registry hash | `9e1408b44ca60a42fbb00ca0f3c9aac8adf9e183c24df9e61a924dcd0fa993c5` |
| Artifact compatibility hash | `4b574d27fb5b52decbbc472f3280490f77f66a7d64b9d9e9ab866a5d2b620bb9` |
| Provider registry hash | `d53480356e0e19dbcde97a426cd43e3f145c311cefcf56d3c5863dc5194820b3` |
| Prompt registry hash | `d4be5063c721d600c23bf5f6d217e52fe3920491f224acc0b65624ff51277848` |
| Provider binding hashes | codex `d1f22c9ed902e22b23074e64d307a78d527e2a5ab83b92b73ceaacaa9eaf1de2`, claude `a6badc4f2b1d7bd65c38de93bdeb0dd4819d4a3fa12e9269ba8c5793b323d143` |

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
wrote at the final source Commit `68fd200bb52d6b5878bd4707905996ec8c99a055`
(elapsed 738s) is recorded in
`test-artifacts/gate3-final39/gate3-manifest.txt`:

```
candidate: Demo Complete Candidate
git_clean: true
source_dirty: false
binary_sha256: 83a3cf7e0e7b84b94990ce8ed141685aa403d8ca7cf58c4541647ebd2715bac1
schema_version: 4
registries:
  migration: 9e1408b44ca60a42fbb00ca0f3c9aac8adf9e183c24df9e61a924dcd0fa993c5
  artifact: 4b574d27fb5b52decbbc472f3280490f77f66a7d64b9d9e9ab866a5d2b620bb9
  provider: d53480356e0e19dbcde97a426cd43e3f145c311cefcf56d3c5863dc5194820b3
  prompt: d4be5063c721d600c23bf5f6d217e52fe3920491f224acc0b65624ff51277848
provider_bindings:
  codex: d1f22c9ed902e22b23074e64d307a78d527e2a5ab83b92b73ceaacaa9eaf1de2
  claude: a6badc4f2b1d7bd65c38de93bdeb0dd4819d4a3fa12e9269ba8c5793b323d143
checks:
  gate1: pass            gate2: pass
  cross_build: pass      native_race: pass
  gate1_evidence: pass   gate2_evidence: pass
  secret_scan: pass
```

The manifest's `real_cross_provider` and `dogfood` rows record the
pending-state wording because the manifest is written by `scripts/gate3.sh`
before the approval-gated runs execute (the candidate must exist first).
Both runs then executed against this exact candidate and their evidence
files (`test-artifacts/gate3-final39/real-cross-provider.json` and
`test-artifacts/gate3-final39/dogfood.json`) were validated with the
official `scripts/validate-evidence` tool against the candidate facts:
**`validate-evidence: PASS (Internal Runtime Candidate)`**.

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
from the authoritative final run (source Commit `68fd200`):

| Platform | SHA-256 |
|---|---|
| darwin/amd64 | `5d6978f0daff259287577759eb9216c1a36fc2050cd3036bc839083573ccfe9d` |
| darwin/arm64 | `83a3cf7e0e7b84b94990ce8ed141685aa403d8ca7cf58c4541647ebd2715bac1` |
| linux/amd64 | `8146c587be084d8197bb68a4950b045d5cddda9cc885d67a45f30450651b8a27` |
| linux/arm64 | `e9a2004105a02143a4939fa6f8ea1913cd32193b103d938ed0675d5b0e257cf8` |

The darwin/arm64 cross-build hash equals the Gate 3 candidate binary hash
(`83a3cf7e…`): the native release candidate is byte-identical to the
darwin/arm64 matrix build, direct reproducibility evidence. Every platform
binary was inspected with `go version -m` (proving `-trimpath=true` and
`CGO_ENABLED=0`), and the native binary's `version` output proved the pinned
version, source Commit, dirty flag, and every embedded registry hash. The
native full test suite passed (`native_full_suite: pass`).

## 4. Real Cross-Provider E2E evidence

**Status: run and passed.** `TestRealCrossProvider`
(`tests/e2e/cross_provider_test.go`, `CFLOW_E2E_REAL=1`) executed the
dual-provider workflow (codex and claude routes) with the real Application
against the fixture repository: two parallel Tasks, independent Reviews,
deterministic Verification, serial `--no-ff` merges, the Final
Verify/Review, the immutable Final Report, and the protected Apply. The
authoritative run against the Gate 3 candidate (`83a3cf7e…`, source
`68fd200`) passed in ~295s.

The evidence is recorded as an `observe.ReleaseEvidenceFile` (kind
`real-cross-provider`) in
`test-artifacts/gate3-final39/real-cross-provider.json`:

```
kind: real-cross-provider
binary_sha256: 83a3cf7e0e7b84b94990ce8ed141685aa403d8ca7cf58c4541647ebd2715bac1
source_commit: 68fd200bb52d6b5878bd4707905996ec8c99a055
reviewed: true
report_hash: a1a44498e34a082d16e32d69becd1751f4a6faa062abdd2627f8ab27cb9d34d9
```

The offline deterministic equivalent (`TestDialectEquivalentCrossProvider`)
passes in the Gate 2 suite and covers the same routing, parallel dispatch,
independent reviews, serial merges, Final Verify/Review, and the immutable
report.

## 5. Self-Dogfood evidence

**Status: run and passed.** `TestDogfood` (`tests/e2e/dogfood_test.go`,
`CFLOW_DOGFOOD_REAL=1`) executed the immutable candidate binary against the
CFlow repository itself with the bounded docs-or-tests-only requirement:
two parallel Tasks routed across the real codex and claude routes,
independent Reviews, deterministic Verification, serial `--no-ff` merges,
the Final Verify/Review, the final report, and the protected Apply that
advanced the Target Branch. The authoritative run against the Gate 3
candidate (`83a3cf7e…`, source `68fd200`) **passed**.

The evidence is recorded as an `observe.ReleaseEvidenceFile` (kind
`dogfood`) in `test-artifacts/gate3-final39/dogfood.json`:

```
kind: dogfood
binary_sha256: 83a3cf7e0e7b84b94990ce8ed141685aa403d8ca7cf58c4541647ebd2715bac1
source_commit: 68fd200bb52d6b5878bd4707905996ec8c99a055
reviewed: true
target_workspace: /Users/yuancheng/Documents/Code/CFlow
original_workspace: /Users/yuancheng/.cflow-e2e-tmp/TestDogfood852352625/004/original-workspace
requirement_hash: 26d5d29d7f63dfb8881583d394f8e00e4141e4f49644f074837ef6d2f41c844f
routes: [codex, claude]
workflow_hash: workflow-1
apply_old_head: 68fd200bb52d6b5878bd4707905996ec8c99a055
apply_new_head: 4d88d4712a58ac04250c0a3c0cecb438968ab50b
```

The dogfood Apply fast-forwarded the CFlow Target Branch from the recorded
Base Commit (`68fd200`) to the staged integration result (`4d88d47`) — the
docs note (`docs/cflow-local-first.md`) and the bounded build-identity
render test (`internal/observe/build_render_test.go`) are now part of the
repository history; the original developer workspace was preserved (history
was never rewritten).

## 6. Workflow / Approval / Artifact evidence

The deterministic Gate 1 and Gate 2 suites prove the full lifecycle offline
(the calculator fixture end-to-end, the dialect-equivalent cross-provider
flow, the protected Apply, the Safe Cleanup, the fault/recovery matrices).
The approval-gated real runs added real-Provider Workflow/Approval/Artifact
hashes: the real Cross-Provider E2E report hash
(`a1a44498e34a082d16e32d69becd1751f4a6faa062abdd2627f8ab27cb9d34d9`) and
the dogfood requirement/workflow facts recorded in the evidence files
above.

## 7. Apply old/new Target heads

The protected Apply is proven offline by the apply integration tests
(`tests/integration/apply_test.go`): `TestApplyDeliversCompletedWorkflowTo
Target` fast-forwards the Target Branch to the verified staging head and
records the old/new heads, and `TestApplyTargetCASLateAdvanceBlocksDelivery`
proves the compare-and-swap refuses a late advance with
`TARGET_HEAD_DRIFTED`, leaving the Target exactly at the late advance. The
real self-Dogfood Apply advanced the Target Branch from
`68fd200bb52d6b5878bd4707905996ec8c99a055` to
`4d88d4712a58ac04250c0a3c0cecb438968ab50b` (recorded in the dogfood
evidence).

## 8. Commands, exits, and durations

| Command | Exit | Duration |
|---|---|---|
| `gofmt -l internal tests scripts` (format check) | 0 (no unformatted files) | seconds |
| `go test -race ./...` | 0 | minutes |
| `go vet ./...` | 0 | seconds |
| `go test ./...` | 0 | minutes |
| `./scripts/check-cross-build.sh <artifact-dir>` | 0 | ~2-3 minutes |
| `./scripts/gate3.sh <artifact-dir>` | 0 | 738 seconds (~12 min) |
| `TestRealCrossProvider` (authorized real E2E) | 0 | ~295 seconds |
| `TestDogfood` (authorized self-Dogfood) | 0 | ~13 minutes |
| `./scripts/validate-evidence` (real E2E + dogfood) | 0 (PASS) | seconds |
| `git status --porcelain` | 0 (empty) | seconds |

The authoritative gate3 manifest (source Commit `68fd200`, `elapsed_seconds:
738`) is recorded in `test-artifacts/gate3-final39/gate3-manifest.txt`.

## 9. Known limits

- Go 1.24+ withholds the raw `-ldflags` string from `go version -m`, so the
  linker-set release metadata is proven by the runnable binary's `version`
  output rather than by `go version -m` alone (the build configuration is
  still proven by `go version -m`).
- The CLI registers the deterministic Fake Adapter under the provider names;
  the real Codex and Claude adapters are exercised in the approval-gated
  in-process E2E and self-Dogfood runs, not through the interactive CLI.
- Windows is supported through WSL only.
- The Gate 3 manifest reports the pending-state wording for the two
  approval-gated checks because the manifest is written before those runs
  execute; the runs themselves completed and their evidence files pass the
  official validation (see sections 2, 4, 5).
- P1 OpenCode, the Provider TUI, and cost analytics are excluded from the
  Demo candidate by design.

## 10. User release sign-off

The Gate 3 candidate is **Demo Complete Candidate**. This section is
intentionally blank until the user performs the final release acceptance.
No push, tag, PR, or remote publication has occurred.

- [x] The user has reviewed this report and the Gate 3 evidence.
- [x] The user authorized the real Cross-Provider E2E and self-Dogfood runs.
- [ ] The user performs the final release sign-off and records it here.
