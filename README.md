# CFlow

[中文说明](README-zh.md)

CFlow is a local-first workflow runtime for coding agents. It turns a coding
requirement into an approved Plan, restricted execution graph, isolated coding
tasks, deterministic verification, independent review, and protected delivery
without introducing a cloud control plane.

CFlow is not another thin wrapper around `codex` or `claude`. It owns the
recoverable Plan-to-Done lifecycle and advances state only from persisted
evidence—not from an agent saying that the work is complete.

> **Project status:** the Demo implementation is present. The evidence-backed
> Gate 3 candidate is tied to source commit `68fd200`; see the
> [acceptance report](docs/cflow-demo-acceptance-report.md). The current branch
> contains later CLI/provider wiring and documentation changes that have not
> been re-attested by a new Gate 3 run. Treat it as post-candidate development,
> not as a released build.

## Why CFlow

- **Local-first:** workflow state, artifacts, sessions, logs, evidence, and
  worktrees stay on the local machine.
- **Approval-driven:** a checked Plan and the exact execution inputs require
  separate user approvals.
- **Agent-independent evidence:** commits, clean worktrees, scopes, tests, and
  independent reviews determine progress.
- **Recoverable execution:** SQLite state, immutable artifacts, Git facts,
  process facts, and evidence are reconciled after interruption or failure.
- **Isolated parallel work:** independent tasks run in separate Git worktrees
  and merge serially into a CFlow-owned Integration Branch.
- **Protected delivery:** completion never changes the user's Target Branch;
  `cflow apply` stages, re-verifies, and compare-and-swap fast-forwards it.
- **Bounded autonomy:** retries, budgets, provider routes, commands, and
  workflow patches are constrained and auditable.

## Lifecycle

```text
Requirement discussion
  -> Draft Plan
  -> independent Plan Check
  -> user Plan Approval
  -> Specs + Verification Catalog
  -> restricted Dynamic Workflow
  -> user Execution Approval
  -> isolated Task Worktrees
  -> deterministic verification + independent review
  -> serial Integration merges
  -> Final Verification + report
  -> completed Workflow
  -> explicit protected Apply
```

The Runtime—not a Planner, Implementer, or Reviewer—owns every authoritative
state transition.

## Architecture

| Module | Responsibility |
|---|---|
| Application | Command/query seam, locking, transactions, and typed effect loop |
| Decision Kernel | Pure lifecycle, retry, approval, and safety decisions |
| State Store | SQLite runtime state and authoritative event sequence |
| Artifact Store | Immutable, versioned, hash-addressed workflow artifacts |
| Agent Runtime | Fake, Codex, and Claude protocol adapters and session lineage |
| Compiler + Scheduler | Restricted DAG compilation and deterministic readiness |
| GitFlow | Repository facts, worktrees, commit gates, merge, quarantine, and Apply |
| Verification Engine | Approved command catalog, deterministic checks, and evidence |
| Recovery Engine | Reconciliation across database, artifacts, Git, processes, and evidence |

All external processes are intended to use executable-plus-argv invocation.
Dynamic workflows cannot contain arbitrary shell, Python, or TypeScript code.

## Requirements

- Go 1.26.5
- Git
- macOS or Linux; Windows is supported through WSL for the Demo
- Codex CLI and Claude Code installed and authenticated for real-provider runs
- A Git repository with a valid commit and an attached local branch

The target repository's Git author/committer identity must be configured.
When commit signing is enabled, it must work non-interactively within the
configured timeout.

## Build from source

```sh
CGO_ENABLED=0 go build -trimpath -o cflow ./cmd/cflow
./cflow version
./cflow doctor
```

Release-style builds additionally stamp the source commit and embedded
registry hashes. See [`scripts/check-cross-build.sh`](scripts/check-cross-build.sh)
and the [acceptance report](docs/cflow-demo-acceptance-report.md).

## Current Demo usage

The current interface is line-oriented and command-driven. Bare `cflow`
prints the command tree; the fully guided interactive lifecycle described in
the PRD is not yet exposed as one continuous loop.

Real providers are currently registered in the post-candidate CLI through
`CFLOW_PROVIDERS`. Without it, the deterministic Fake Adapter remains the
default.

```sh
export CFLOW_PROVIDERS=codex,claude

./cflow workflow-create my-change --provider codex
printf '%s\n' 'Describe the change and its constraints.' | \
  ./cflow discuss --provider codex
./cflow plan-generate --provider codex
./cflow plan-check --provider claude
./cflow plan-show
./cflow plan-approve
./cflow spec-generate --provider codex
./cflow compile-workflow --provider claude
./cflow execution-dry-run
./cflow execution-show
./cflow execution-approve
```

Approval commands are interactive and default to “no.” Provider execution may
use the network and incur model cost. Review the exact routes, budgets,
commands, Git identity/signing facts, and permission boundary before approval.

The Demo CLI is still an engineering candidate: several `doctor` stateful
checks currently report `NOT_YET_AVAILABLE`, and the current post-candidate
provider wiring has not received refreshed Gate 3 evidence.

## Command map

| Area | Commands |
|---|---|
| Read and diagnose | `list`, `status`, `inspect`, `inspect task`, `logs`, `report`, `doctor`, `version` |
| Plan lifecycle | `workflow-create`, `discuss`, `plan-generate`, `plan-check`, `plan-show`, `plan-approve` |
| Execution definition | `spec-generate`, `compile-workflow`, `execution-dry-run`, `execution-show`, `execution-approve` |
| Runtime control | `retry`, `pause`, `resume`, `cancel`, `policy-confirm` |
| Recovery | `replacement-preview`, `replacement-approve` |
| Delivery | `apply`, `apply --confirm`, `apply --execute` |
| Cleanup | `cleanup`, `cleanup --execute <manifest-id>` |

Use `cflow <command> --help` for exact arguments. When no Workflow ID is
provided, commands use the current project's applicable workflow projection.

## Trust boundary

CFlow provides strong workflow and repository evidence gates, but it is not an
OS sandbox.

- Provider CLIs run with the user's existing provider configuration and
  default permissions.
- Providers and approved project tools may access the network.
- CFlow does not add danger/bypass flags, copy credentials, or promise that an
  agent cannot read outside its worktree.
- CFlow-owned sensitive paths are designed for owner-only permissions, and
  persisted provider/tool content passes through the redaction pipeline.
- Local evidence is not encrypted by CFlow and may be included in operating
  system backups.
- Cancel preserves artifacts, sessions, commits, worktrees, and evidence.
  Cleanup is a separate, exact-target operation and never deletes audit
  history.

## Verification and release evidence

The repository contains deterministic Gate scripts and opt-in real-provider
tests:

```sh
./scripts/gate1.sh <artifact-dir>
./scripts/gate2.sh <artifact-dir>
./scripts/gate3.sh <artifact-dir>
```

Gate 1 and Gate 2 are internal candidates. Gate 3 can produce a Demo Complete
Candidate, never a release. Real Cross-Provider E2E and self-Dogfood require
explicit authorization because they invoke provider CLIs, may incur cost, and
the Dogfood flow performs protected Apply.

Historical evidence is evidence for its exact binary and source commit only;
it does not automatically attest later commits.

## Documentation

- [Product requirements](docs/cflow-prd.md)
- [Technical design](docs/cflow-demo-design.md)
- [Implementation plan](docs/cflow-demo-implementation-plan.md)
- [Gate 3 acceptance report](docs/cflow-demo-acceptance-report.md)
- [Local-first boundary](docs/cflow-local-first.md)
- [Domain language](CONTEXT.md)

## Non-goals for the Demo

No Web UI, cloud service, arbitrary workflow scripts, automatic push/PR,
cross-repository workflow, OpenCode adapter, Provider TUI attach, or unlimited
autonomous retry loop.

## License

[MIT](LICENSE)
