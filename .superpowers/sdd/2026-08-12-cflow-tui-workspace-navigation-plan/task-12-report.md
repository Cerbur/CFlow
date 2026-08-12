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

## Apply identity binding disposition (superseded by Fix Round 2)

The earlier review disposition deferred exact Apply preview identity binding
because the original typed command contained only `Workflow`. Fix Round 2
resolved that gap with the minimal identity-bearing fields described below;
Application and Decision revalidation remain authoritative and fail closed.

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

## Fix Round 2

Implemented the remaining Important findings from the final re-reviews:

- `ExecuteApplyCommand` now carries the exact bound Apply Attempt ID,
  Target/Integration heads, Preflight reference/revision/hash, and policy
  fingerprint. The TUI stores the authoritative `ApplyAttempt` returned by
  Prepare, renders the bound facts, and sends all of them on the second Enter.
  Application and Decision reject missing, changed, or mismatched facts; the
  legacy workflow-only headless command remains available for CLI compatibility.
- Discussion Finish/Switch/Pause and Continue/Start actions, Execution
  Resume/Start Runner/Adopt Workspace, Blocked Resume, and Terminal Apply /
  Cleanup shortcuts now enter the common typed Action Preview. The first
  Enter only previews; the second issues the typed command. Start Runner uses
  the shared navigation helper and returns to Workflow Menu on Esc.
- Readonly projections now validate Workflow identity for Status, Plan,
  Execution Preview, Inspect, and Report. Logs remains query-bound and the
  typed query regression includes `LogsQuery.Workflow`.

Round 2 focused verification:

```text
GOCACHE=/tmp/cflow-task12-final-cache go test ./internal/tui -run '^(TestApplyExecuteBindsExactAttemptFactsFromPreview|TestStageLocalMutationsOpenActionPreviewBeforeExecute|TestStartRunnerActionPreviewEscReturnsToWorkflowMenu|TestStageResumeConfirmationKeepsExistingExecutionParent|TestReadonlyProjectionRejectsForeignWorkflowView|TestWorkflowMenu|TestActionPreview)$' -count=1 -timeout=20s
ok   cflow.local/cflow/internal/tui  0.198s

GOCACHE=/tmp/cflow-task12-final-cache go test ./internal/app ./internal/decision -run '^$' -count=1 -timeout=20s
ok   cflow.local/cflow/internal/app      [no tests to run]
ok   cflow.local/cflow/internal/decision [no tests to run]

gofmt
passed

bash -n scripts/gate-tui.sh
passed

git diff --check
passed
```

The full suite, package-wide tests, and long-running E2E were not run. The
broader regex `Apply|ActionPreview|Readonly|WorkflowMenu` was intentionally
not used for final evidence because it also selects the existing long
`TestTUIPlanToApplyAndCleanup` E2E, which timed out while waiting for its
Bubble Tea test harness. No real Provider E2E, Self-Dogfood, remote push, or
PR was performed.

## Fix Round 3

Closed the final quality-review findings:

- Decision Kernel bound `ApplyExecute` now requires a complete Preflight
  reference (`Workflow`, valid `Type`, `Revision >= 1`, non-empty `Hash`) and
  all bound heads/hash/fingerprint fields; incomplete or mismatched facts are
  rejected. The zero-Attempt workflow-only path remains the explicitly
  documented headless compatibility path.
- Added Decision and Application regressions for incomplete bound Apply
  facts.
- Every Action Preview confirmation path (Apply, Cleanup, Adopt Workspace,
  Start/Continue/Finish/Switch Discussion, Resume, Pause, Start Runner) now
  records exactly one `action_preview.confirm` trace with only Workflow and
  operation ID before/with the typed command. Raw input is not logged.
- Removed unused `MenuActionResumeRunner`.

Fix Round 3 RED evidence:

```text
GOCACHE=/tmp/cflow-task12-red-cache go test ./internal/decision ./internal/tui -run 'TestApplyExecuteBoundAttemptRequiresCompletePreflightRef|TestActionPreviewConfirmationTracesEveryTypedActionOnce' -count=1 -timeout=30s
FAIL: Decision accepted missing/partial bound Preflight refs; TUI recorded 0 confirmations for Apply/Cleanup/Adopt/Discussion and an unbound operation ID for Start Runner.
```

Fix Round 3 focused GREEN and final bounded verification:

```text
GOCACHE=/tmp/cflow-task12-final-cache go test ./internal/decision -run 'TestApplyExecuteBoundAttemptRequiresCompletePreflightRef' -count=1 -timeout=30s
ok   cflow.local/cflow/internal/decision  0.439s

GOCACHE=/tmp/cflow-task12-final-cache go test ./internal/tui -run 'TestActionPreviewConfirmationTracesEveryTypedActionOnce|TestHierarchicalOperationTraceRecordsSafeUIActions' -count=1 -timeout=30s
ok   cflow.local/cflow/internal/tui  0.438s

GOCACHE=/tmp/cflow-task12-final-cache go test ./internal/app -run 'TestExecuteApplyBoundCommandRejectsIncompletePreflight' -count=1 -timeout=30s
ok   cflow.local/cflow/internal/app  3.699s

gofmt -w internal/app/apply_test.go internal/decision/apply_cleanup.go internal/decision/kernel_test.go internal/tui/app.go internal/tui/app_test.go internal/app/commands.go
bash -n scripts/gate-tui.sh
passed

git diff --check
passed
```

Final delivery evidence:

```text
git status --short --branch
## main...origin/main [ahead 29]
```

Per the explicit fix-round instruction, no package-wide/full suite, heavy
TUI E2E, real Provider E2E, Self-Dogfood, remote push, or PR was run.
