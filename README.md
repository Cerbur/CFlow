# CFlow

CFlow is a local-first Go CLI for the Plan-to-Done lifecycle of coding-agent
workflows: requirement discussion, Plan generation and approval, Spec
compilation, restricted workflow execution across coding agents,
deterministic verification, and protected delivery. All CFlow-owned state
stays on the local machine; there is no daemon and no cloud control plane.

This repository is at the **Demo implementation stage**. The current
candidate is the **Internal Runtime Candidate** (Gate 2): the deterministic
plan-to-integration core, the cross-provider routing runtime, and the final
acceptance (Final Verify, independent Final Reviewer, exact-evidence
completion, and the immutable Execution Report) are implemented and gated
offline. It is not a release and makes no completion claims; the protected
Apply, the Safe Cleanup execute, and the real Cross-Provider E2E remain
approval-gated or later tasks.

## Build

Requires Go 1.26.5 (see `go` and `toolchain` in `go.mod`).

```sh
CGO_ENABLED=0 go build -trimpath -o cflow ./cmd/cflow
```

The release remains one statically linked executable with no CGO dependency.
Release builds should stamp source identity explicitly, because the Go
toolchain does not stamp VCS metadata in git worktrees:

```sh
CGO_ENABLED=0 go build -trimpath \
  -ldflags "-X cflow.local/cflow/internal/observe.Version=0.1.0 \
            -X cflow.local/cflow/internal/observe.SourceCommit=$(git rev-parse HEAD)" \
  -o cflow ./cmd/cflow
```

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
| `cflow cleanup [workflow-id]` | produce the cleanup dry-run manifest (`--execute` is a later task) |
| `cflow dry-run [workflow-id]` | produce the cleanup dry-run manifest |
| `cflow report [workflow-id]` | render the final execution report read model (redacted) |
| `cflow apply [workflow-id]` | apply preflight entry; the protected apply (Gate 3) returns NOT_YET_AVAILABLE |
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
the Target Branch, and the immutable Execution Report (`cflow report`, also
written as a report artifact at completion) covers approvals, commits,
commit policy, verification, sessions, findings, migration compatibility,
security posture, the Provider default-permission trust boundary, and the
Apply outcome (shown as not run until Gate 3).

## Real Cross-Provider E2E

`tests/e2e/cross_provider_test.go` carries the opt-in real Codex/Claude
E2E: two parallel Tasks routed to the real providers with real Commits,
independent Reviews, deterministic Verification, serial merges, the Final
Verify/Review, the final report, and an unchanged Target Branch. It NEVER
runs without `CFLOW_E2E_REAL=1`, because it costs real model requests and
runs with the providers' default permissions — the user must approve the
exact Dry Run, the provider routes/models/budgets, the default-permission
trust boundary, and the potential network/cost before the gate is set.
The offline deterministic equivalent
(`TestDialectEquivalentCrossProvider`) runs in the Gate 2 suite.

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

## Gate evidence

`scripts/gate1.sh <artifact-dir>` writes the Gate 1 evidence of the
Internal Core Candidate. `scripts/gate2.sh <artifact-dir>` reruns Gate 1,
adds the offline protocol/routing/recovery matrices and the deterministic
dialect-equivalent Cross-Provider flow, optionally validates the approved
real Cross-Provider evidence (`CFLOW_E2E_REAL=1`), builds the CGO-disabled
binary, pins the binary SHA-256, the Git source Commit, and the clean
status, and writes a redacted Manifest declaring the candidate
**Internal Runtime Candidate** — never "Demo Complete".
