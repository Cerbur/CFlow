# Task 6 Report: Enter-only New Workflow creation

## Scope

Implemented only Task 6 from the 2026-08-12 TUI workspace navigation plan.
Changed:

- `internal/tui/app.go`
- `internal/tui/app_test.go`
- this report

`internal/tui/operation_log.go` did not require a change.

Task 7 Global Command Palette work was not started.

## Implementation

- New Workflow name editing Enter opens the Create Preview without executing a
  command.
- Create Preview executes `app.CreateWorkflowCommand` only on Enter.
- Esc returns from preview to editing and preserves the workflow name.
- `y`, `Y`, `n`, `N`, and `q` do not confirm creation.
- Existing Discovery/dirty Target facts remain the source of
  `CreateWorkflowCommand.ConfirmDirty`.
- Create hints and render text no longer advertise y/n confirmation.
- After the Create command's bound Workspace acknowledgement is accepted, the
  model resets the transient Create navigation, selects the returned Workflow,
  enters `PageWorkflowMenu`, reloads `WorkflowMenuQuery`, and applies the
  Application-provided `DefaultIndex`.
- Creation does not enter Discussion or start the Native Bridge.
- No SQLite, Runtime authority, Git, Artifact, or external command path was
  added to the TUI.

## TDD evidence

The focused tests were first changed to the Task 6 expectations and run against
the old implementation. The expected red result was observed:

```text
TestCreatePreviewEnterExecutes: preview Enter did not create
TestCreatePreviewYNAQDoNotConfirm: 'y' confirmed creation
TestWorkflowMenuAfterCreateAcknowledgement: create acknowledgement route stayed on Home
```

The minimal implementation was then added and the focused tests passed.

## Verification

Passed:

```text
env GOCACHE=/private/tmp/cflow-go-cache go test ./internal/tui -run 'Create|NewWorkflow|WorkflowMenuAfterCreate' -count=1
ok   cflow.local/cflow/internal/tui  0.227s

env GOCACHE=/private/tmp/cflow-go-cache go test ./internal/tui -count=1
PASS

env GOCACHE=/private/tmp/cflow-go-cache go test ./internal/tui ./internal/app ./internal/cli ./cmd/cflow -count=1
PASS

git diff --check
PASS
```

The repository-wide baseline was attempted with a bounded timeout so the known
long-running test could not block Task 6 completion:

```text
env GOCACHE=/private/tmp/cflow-go-cache go test ./... -count=1 -timeout=30s
FAIL: panic: test timed out after 30s
running test: TestRestrictedSafetyPathStopsManagedProcesses
package: cflow.local/cflow/tests/integration
```

The timeout stack was in the existing integration Git/process fixture
(`internal/app/application_test.go` and
`tests/integration/fault_matrix_provider_test.go`), not in the focused TUI
tests. The earlier unbounded baseline was stopped rather than awaited
indefinitely, as required by the Task 6 brief.

The environment denied the final `ps` process listing with
`operation not permitted`; the bounded Go test command exited on its timeout
and no test session remained available to poll.

## Commit

This report and the scoped implementation changes are included in the Task 6
commit created immediately after this report was written.
