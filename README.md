# CFlow

CFlow is a local-first Go CLI for the Plan-to-Done lifecycle of coding-agent
workflows: requirement discussion, Plan generation and approval, Spec
compilation, restricted workflow execution across coding agents,
deterministic verification, and protected delivery. All CFlow-owned state
stays on the local machine; there is no daemon and no cloud control plane.

This repository is at the **Demo implementation stage**. It currently
contains the Task 1 foundation only: toolchain and build identity, strict
local configuration, and a read-only `doctor`. It is not a release and makes
no completion claims.

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
| `cflow help` | command tree help |
| `cflow version` | build identity: version, source commit, dirty flag, Go version, OS/arch, embedded-registry hashes |
| `cflow doctor` | read-only report of build identity, tool availability, and stateful check status |

`version`, `help`, and `doctor` never create, read, or modify `CFLOW_HOME`.

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

## Trust boundaries and limitations

- Local-first: one binary, no daemon, no cloud control plane, no automatic
  push, fetch, or pull request creation.
- External processes are invoked with a validated executable and argv
  slice; a shell is never used.
- `doctor` is strictly read-only. Stateful checks are reported as
  `NOT_YET_AVAILABLE` until their tasks land; results are never guessed.
- No workflow execution exists yet. Do not use this binary to manage real
  workflows.
