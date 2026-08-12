# Task 12 Report: Review Fixes and Bounded Verification

Date: 2026-08-12  
Base: `c18c561` plus the pre-existing uncommitted test fixes in
`internal/tui/app_test.go` and `internal/tui/workflow_menu_test.go`.

## Review fixes applied

- Removed Home `r/R`, `p/P`, `x/X`, and `m/M` mutation/navigation shortcuts.
  Home now remains selection, Enter-to-Workflow-Menu, Esc no-op, `/`, Ctrl+C,
  and ordinary non-exiting `q`. Added negative coverage for all eight rune
  variants and moved affected command coverage to Workflow Menu action-preview
  routing.
- `workflowMenuExecutionPreviewAvailable` now omits optional artifact routes
  only for the artifact store's classified not-found error. Corruption,
  permission, and other store faults propagate through the Workflow Menu
  query. Added a corrupt-artifact regression.
- Updated English and Chinese README exit documentation to `/exit`,
  Runner-aware Pause/Exit with Runner join, and Ctrl+C behavior.
- `/exit` is blocked while a typed Application command/ack, pending pause, or
  controlled stop is in flight. A failed `/exit` Pause force-stops managed
  processes but keeps the TUI on a diagnosable Pause/Exit recovery state with
  the error and retry/recovery copy; it does not return `tea.Quit` directly.
- Replaced stale `q Back/Exit` execution copy and stale Pause/Exit q wording.
- Strengthened readonly route query helpers to assert `Workflow == "wf-1"`.
- Changed authoritative navigation spec metadata from `proposed` to
  `approved`, matching the confirmed authority chain in AGENTS.md/PRD and the
  repository's approved document convention.
- Updated the Task 4 ledger note to historical after Task 11 migrated the E2E.
- Updated migration tests to enter through Workflow Menu/stage routing rather
  than using removed Home shortcuts.

## Apply identity binding disposition

The existing typed `ExecuteApplyCommand` contract contains only `Workflow`;
the Application currently selects and revalidates the latest persisted Apply
Attempt internally. Adding an unrecognized attempt/preflight authority field
in Task 12 would invent a new command contract and could weaken the existing
Application revalidation boundary. Per the final-owner instruction, exact
Apply preview identity binding is explicitly deferred and remains a known
follow-up; no unsafe authority was added.

## Verification

TDD RED evidence:

- The initial Apply identity regression could not compile because
  `TerminalModel` has no Apply Attempt identity field. This confirmed the
  contract gap; the test was removed from the bounded fix set rather than
  inventing a production field.
- The Workflow Menu corruption regression initially failed because the
  implementation converted every preview error into `available=false`.
- The `/exit` and pause-failure regressions initially failed against the old
  quit behavior.

Fresh bounded GREEN results:

```text
GOCACHE=/tmp/cflow-task12-app-cache go test ./internal/app -run 'TestWorkflowMenuOmitsArtifactRoutesWhenPreviewArtifactIsMissing|TestWorkflowMenuPropagatesNonNotFoundPreviewArtifactErrors' -count=1 -timeout=30s
ok   cflow.local/cflow/internal/app  2.286s

GOCACHE=/tmp/cflow-task12-tui-cache go test ./internal/tui -run 'TestModelHomeMutationShortcutsAreInert|TestModelHomeResumeShortcutIsInert|TestModelActionsMapToTypedCommands|TestExitCommandDoesNotQuitWhileApplicationCommandIsInFlight|TestPauseFailureAfterForceStopLeavesDiagnosableExitState|TestModelQIsOrdinaryInput|TestIdleExitCommandQuitsTUI|TestRunningExitWaitsForRunnerDoneAfterControlledPause|TestWorkflowMenuEnterRoutesReadOnlyAndPreviewWithoutExecute|TestWorkflowMenuRoutesTask8StageEntriesToTypedPages|TestTerminalYNAreOrdinaryInput' -count=1 -timeout=30s
ok   cflow.local/cflow/internal/tui  0.461s

bash -n scripts/gate-tui.sh
passed

git diff --check
passed before commit
```

The full suite and three-minute package gate were not run, per the latest
owner instruction. No real Provider E2E, Self-Dogfood, remote push, or PR was
performed.
