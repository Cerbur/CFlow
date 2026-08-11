# Task 11 Report: Update E2E Keyboard Flows and Operation Evidence

Date: 2026-08-12
Base: `64e0968`

## Scope

Implemented only Task 11. The change updates the TUI E2E keyboard vocabulary
to Home/Menu/Stage navigation, adds a deterministic paused-Workflow Resume
journey, and records bounded UI-only operation breadcrumbs. No Runtime state is
written by the trace, and no real Provider or remote operation is invoked.

Changed implementation files:

- `internal/tui/e2e_test.go`
- `internal/tui/app_test.go`
- `internal/tui/app.go`
- `internal/tui/navigation.go`
- `internal/tui/workflow_menu.go`
- `internal/tui/operation_log.go`
- `internal/tui/operation_log_file.go`
- `scripts/gate-tui.sh`

## Implementation

- Replaced the old lifecycle E2E's `n`/Tab/`y`/idle-q flow with Enter/Esc,
  Home→Workflow Menu routing, `/exit` Enter, and Enter-only preview/execute
  confirmations.
- Added `TestTUIWorkflowMenuResumeActionPreview` using a deterministic fake
  controller for Home → Workflow Menu → Resume → Action Preview → Execution,
  Esc parent returns, and `/exit` Enter.
- Added `TestHierarchicalOperationTraceRecordsSafeUIActions`, covering:
  `command_palette.open`, `command_palette.execute`, `navigation.push`,
  `navigation.pop`, `workflow_menu.query`, `workflow_menu.select`,
  `action_preview.open`, and `action_preview.confirm`.
- Trace entries use fixed allow-listed action names, workflow binding, and the
  current operation ID when available. Palette input and arbitrary command
  text are never copied into the trace.
- Resume confirmation now routes the UI to Execution through the existing
  typed `ResumeWorkflowCommand`; it does not add a new Runtime transition.
- `scripts/gate-tui.sh` now runs the fake-only keyboard/exit/trace focused gate
  before its existing broader suites.

## TDD evidence

RED results:

- `TestHierarchicalOperationTraceRecordsSafeUIActions` failed because the
  required UI trace actions were absent.
- `TestTUIWorkflowMenuResumeActionPreview` failed at the expected missing
  Resume → Execution route (`confirmed Resume page = 3`, wanted Execution).

GREEN result:

```text
GOCACHE=/tmp/cflow-task11-gocache go test ./internal/tui -run '^(TestTUIWorkflowMenuResumeActionPreview|TestHierarchicalOperationTraceRecordsSafeUIActions|TestModelQIsOrdinaryInput|TestModelHomeEscIsNoOp|TestIdleExitCommandQuitsTUI|TestRunningExitWaitsForRunnerDoneAfterControlledPause|TestRunningExitPreviewUsesOnlyEnter)$' -count=1 -v
PASS: 6 tests
```

Additional focused checks passed:

```text
GOCACHE=/tmp/cflow-task11-gocache go test ./internal/tui -run '^(TestOpenOperationLog|TestCommandPalette|TestNavigationStackHomeMenuStageEsc|TestApprovalSecondEnterRequestsExecution)$' -count=1
ok
bash -n scripts/gate-tui.sh
git diff --check
```

## Incomplete verification

Per the user instruction, long-running tests were stopped and not retried.
The package command below was started but interrupted after approximately
eight seconds while the existing long lifecycle E2E was still running:

```text
GOCACHE=/tmp/cflow-task11-gocache go test ./internal/tui -count=1
```

Therefore the following were not claimed or run to completion:

- the old full `TestTUIPlanToApplyAndCleanup` lifecycle E2E;
- the complete `./internal/tui` package test run;
- `go test ./internal/tui ./internal/cli ./cmd/cflow`;
- `go test ./...`;
- the full `scripts/gate-tui.sh` candidate gate.

No Task 12 documentation handoff or final review was started. No real
Codex/Claude Provider E2E, Self-Dogfood, push, PR, or remote operation was
performed.

## Git evidence

The scoped Task 11 files and this report are committed separately from all
unrelated work. Final commit and clean-status evidence are recorded in the
handoff after the commit succeeds.
