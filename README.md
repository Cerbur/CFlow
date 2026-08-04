# CFlow

CFlow is a local-first Go CLI for the Plan-to-Done lifecycle of coding-agent
workflows: requirement discussion, Plan generation and approval, Spec
compilation, restricted workflow execution across coding agents,
deterministic verification, and protected delivery. All CFlow-owned state
stays on the local machine; there is no daemon and no cloud control plane.

This repository is at the **Demo implementation stage**. The current
candidate is the **Demo Complete Candidate** (Gate 3): every deterministic
acceptance gate passes, the cross-platform CGO-disabled binary builds on
macOS and Linux (amd64/arm64), the release metadata is pinned, the real
Cross-Provider E2E and the self-Dogfood runs are **approval-gated** (they
cost real model requests and apply to the target branch, so they run only
with explicit user authorization), and the protected Apply and Safe Cleanup
are implemented. It is a Gate 3 candidate, **not a release**: the final
user release sign-off is a separate human step, and no artifact claims
`Released`.

## Build

Requires Go 1.26.5 (see `go` and `toolchain` in `go.mod`).

```sh
CGO_ENABLED=0 go build -trimpath -o cflow ./cmd/cflow
```

The release remains one statically linked executable with no CGO dependency.
Release builds stamp the full identity explicitly, because the Go toolchain
does not stamp VCS metadata in git worktrees, and they carry no timestamp in
the binary identity (a rebuild from the same source and toolchain produces
the same SHA-256). The registry/schema/migration/Artifact/Provider/prompt
hashes come from `scripts/release-metadata`:

```sh
eval "$(go run ./scripts/release-metadata)"
CGO_ENABLED=0 go build -trimpath \
  -ldflags "-X cflow.local/cflow/internal/observe.Version=0.1.0-demo3 \
            -X cflow.local/cflow/internal/observe.SourceCommit=$(git rev-parse HEAD) \
            -X cflow.local/cflow/internal/observe.sourceDirty=0 \
            -X cflow.local/cflow/internal/observe.schemaVersion=$schema_version \
            -X cflow.local/cflow/internal/observe.MigrationHash=$migration \
            -X cflow.local/cflow/internal/observe.ArtifactHash=$artifact \
            -X cflow.local/cflow/internal/observe.ProviderHash=$provider \
            -X cflow.local/cflow/internal/observe.PromptHash=$prompt" \
  -o cflow ./cmd/cflow
```

`cflow version` reports the stamped identity. Note that Go 1.24+ withholds
the raw `-ldflags` string from `go version -m` for security, so the
linker-set values are proven by the runnable binary's own `version` output
(and the cross-platform matrix proves the CGO-disabled `-trimpath` build
configuration for every target).

## Cross-platform build proof

`scripts/check-cross-build.sh <artifact-dir>` compiles the CGO-disabled
single binary for `darwin/amd64`, `darwin/arm64`, `linux/amd64`, and
`linux/arm64` with the release metadata linker flags, records each SHA-256,
proves the release build configuration through `go version -m`, proves the
pinned metadata through the native binary's `version` output, and runs the
full test suite on the native platform. Windows is supported through WSL for
the Demo.

## Usage

| Command | Purpose |
|---|---|
| `cflow` | command tree help |
| `cflow list` | list the project's workflows |
| `cflow status [workflow-id]` | show one workflow's stage, runtime, plan, and integration facts |
| `cflow inspect [workflow-id]` | show the full workflow aggregate |
| `cflow inspect task <task-id> [workflow-id]` | show one task's delivery evidence (attempts, heads, evidence) |
| `cflow logs [workflow-id]` | show the redacted authoritative event log |
| `cflow workflow-create <name>` | create a workflow over the current git repository |
| `cflow discuss [workflow-id]` | submit one requirement discussion turn |
| `cflow plan-generate [workflow-id]` | produce a new plan revision |
| `cflow plan-check [workflow-id]` | run an independent plan check |
| `cflow plan-approve [workflow-id]` | approve the exact active plan revision and hash |
| `cflow spec-generate [workflow-id]` | discover the verification catalog and generate the specs |
| `cflow compile-workflow [workflow-id]` | compile the approved specs into the restricted workflow |
| `cflow execution-dry-run [workflow-id]` | run the commit preflight and pause at the execution approval gate |
| `cflow execution-approve [workflow-id]` | approve the exact displayed execution inputs |
| `cflow retry <task-id> [workflow-id]` | drive one dispatch pass for a task with a ready retry |
| `cflow pause [workflow-id]` | pause a running workflow (two-phase controlled stop) |
| `cflow resume [workflow-id]` | resume a paused workflow |
| `cflow cancel [workflow-id]` | cancel a workflow; every artifact, session, worktree, branch, commit, and audit ref is preserved |
| `cflow apply-prepare [workflow-id]` | stage the completed integration output in an isolated Apply Worktree and revalidate it |
| `cflow apply-execute [workflow-id]` | deliver through the compare-and-swap fast-forward (refuses on target drift) |
| `cflow cleanup [workflow-id]` | produce the cleanup dry-run manifest |
| `cflow cleanup --execute [workflow-id]` | execute the confirmed cleanup (exact-target, partial-safe) |
| `cflow report [workflow-id]` | render the final execution report read model (redacted) |
| `cflow doctor` | read-only report of build identity, tool availability, provider protocol bindings, and check status |
| `cflow version` | build identity: version, source commit, dirty flag, Go version, OS/arch, embedded-registry hashes |

`version`, `help`, `doctor`, and `list` never create CFLOW_HOME; reads never
migrate. Every command answers `--help` and returns one of the stable exit
classes below.

## Execution lifecycle

An approved workflow executes through the deterministic delivery chain:
agent Tasks code only inside their own Task Worktrees created from the
verified Integration HEAD, the approved Verification Catalog commands run
deterministically, independent Reviewer Sessions judge each Task, and
serial `--no-ff` merges advance the CFlow-owned Integration Branch. After
every merge, the Final Verify Node runs the approved final-verify Catalog
command over the full Integration range inside the Integration Worktree,
an independent Final Reviewer Session (bound to the exact
Plan/Spec/Catalog/Workflow refs and the Integration HEAD) judges the
result, and the Workflow records COMPLETED only with exact evidence —
every Node SUCCEEDED, no Blocking Finding, and the bound Integration HEAD
unchanged (`EVIDENCE_SUBJECT_CHANGED` otherwise). Completion never changes
the Target Branch. The protected Apply (`apply-prepare` then
`apply-execute`) stages the Integration output in an isolated Apply
Worktree, revalidates the Catalog and an independent Apply Verification
Session, then fast-forwards the Target Branch through a compare-and-swap —
refusing with `TARGET_HEAD_DRIFTED` (or the dirty/identity codes) when the
Target changed after the staging verification, and leaving the Target
exactly at the late advance. The immutable Execution Report (`cflow
report`, also written as a report artifact at completion) covers approvals,
commits, commit policy, verification, sessions, findings, migration
compatibility, security posture, the Provider default-permission trust
boundary, the Apply outcome, and the cleanup posture.

## Real Cross-Provider E2E

`tests/e2e/cross_provider_test.go` carries the opt-in real Codex/Claude
E2E: two parallel Tasks routed to the real providers with real Commits,
independent Reviews, deterministic Verification, serial merges, the Final
Verify/Review, the final report, and an unchanged Target Branch. It NEVER
runs without `CFLOW_E2E_REAL=1`, because it costs real model requests and
runs with the providers' default permissions — the user must approve the
exact Dry Run, the provider routes/models/budgets, the default-permission
trust boundary, and the potential network/cost before the gate is set.
The offline deterministic equivalent (`TestDialectEquivalentCrossProvider`)
runs in the Gate 2 suite, and the Gate 3 validation consumes the authorized
real-run evidence (`observe.ReleaseEvidenceFile`, kind
`real-cross-provider`).

## Self-Dogfood

`tests/e2e/dogfood_test.go` carries the opt-in self-Dogfood harness: it
builds the candidate binary from the current source, copies it to an
immutable path outside the CFlow repository, hashes it, and runs a
CFlow-managed workflow against the CFlow repository itself with a bounded
documentation-or-test-only requirement, at least two Tasks routed across
Codex and Claude, independent Reviews, full deterministic Verification,
serial Integration, the final report, and the protected Apply. It NEVER
runs without `CFLOW_DOGFOOD_REAL=1`: the same approvals apply as the real
E2E, plus the bounded requirement and the Apply target (the dogfood Apply
advances the target branch). The offline deterministic dogfood-equivalent
(`TestDogfoodPreflight`) runs in the Gate 3 suite and proves the immutable
binary copy, the pinned hash, the target/workspace contamination guards,
and the bounded-requirement preflight. CFlow never runs Dogfood over an
uncommitted workspace.

## Configuration

`CFLOW_HOME/config.yaml` is the one strict local configuration file. Its
schema is closed: unknown keys and invalid values are rejected (exit class 4),
and credentials, scripts, and raw command strings are impossible because no
such key exists in the schema. Precedence is explicit per-command input,
then the file, then an embedded safe default.

## Process exit classes

| Exit class | Meaning |
|---|---|
| 0 | requested read or mutation reached its defined successful outcome |
| 2 | invalid command or user input |
| 3 | safe user action is required; Workflow is Paused or Blocked |
| 4 | local environment or compatibility precondition failed |
| 5 | runtime invariant failed or facts cannot be safely reconciled |
| 130 | user interruption completed through the controlled-stop protocol |

## Trust boundary and limitations

- Agents run with the provider's default permissions and the user's existing
  provider configuration; CFlow provides no sandbox guarantee.
- The CLI registers the deterministic Fake Adapter under the provider names;
  the real Codex and Claude adapters are exercised in the approval-gated
  in-process E2E and self-Dogfood runs, not through the interactive CLI.
- Git signing probes can depend on external agents or hardware and need
  strict non-interactive timeouts.
- Windows is supported through WSL only.
- The real E2E and self-Dogfood evidence are recorded only when the user
  authorizes and runs them; the Gate 3 manifest reports the pending state
  until then.

## Gate evidence

`scripts/gate1.sh <artifact-dir>` writes the Gate 1 evidence of the
Internal Core Candidate. `scripts/gate2.sh <artifact-dir>` reruns Gate 1,
adds the offline protocol/routing/recovery matrices and the deterministic
dialect-equivalent Cross-Provider flow, optionally validates the approved
real Cross-Provider evidence (`CFLOW_E2E_REAL=1`), builds the CGO-disabled
binary, pins the binary SHA-256, the Git source Commit, the clean status,
and the embedded registry hashes, and writes a redacted Manifest declaring
the candidate **Internal Runtime Candidate** — never "Demo Complete".
`scripts/gate3.sh <artifact-dir>` reruns Gate 1 and Gate 2, adds the
cross-platform build matrix, the native race tests, the release-evidence
validation (a Gate manifest is rejected with `EVIDENCE_SUBJECT_CHANGED`
when it was recorded by a different subject), the optional real-E2E and
self-Dogfood evidence validation, the CGO-disabled release candidate build
with the pinned metadata, and a recursive secret scan over the redacted
evidence, and writes a redacted Manifest declaring the candidate **Demo
Complete Candidate** — never `Released`. The final user release sign-off is
a separate human step.
