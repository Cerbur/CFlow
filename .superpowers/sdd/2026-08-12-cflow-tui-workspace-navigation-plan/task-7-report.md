# Task 7 Report: Global Command Palette and `/exit`

## Scope

Implemented Task 7 from base `8626f5f` only. Task 8 was not started.

Changed:

- `internal/tui/command_palette.go`
- `internal/tui/command_palette_test.go`
- `internal/tui/app.go`
- `internal/tui/keys.go`
- `internal/tui/workspace_view.go`
- `internal/tui/app_test.go`
- this report

## Implementation

- Added the pure `CommandPaletteModel` and `GlobalCommand` registry with the
  single `/exit` command.
- Added bounded centered palette rendering with command filtering, selection,
  Enter selection, and Esc close behavior.
- Routed `/` before ordinary page handlers on non-text-input pages; text input
  pages receive `/` literally.
- While the palette is open, ordinary keys are consumed by the palette and do
  not alter the underlying page, selection, or navigation state.
- Idle `/exit` returns `tea.Quit`.
- Running `/exit` enters the existing Pause-and-Exit preview. Enter requests
  `PauseWorkflowCommand`, cancels the runner, and final exit is deferred until
  `runnerDoneMsg` and the pause acknowledgement complete.
- q, y, and n do not confirm the running exit preview. Ctrl+C retains its
  existing controlled-stop priority and the Home footer retains its stop hint.
- Updated root/footer copy to use `/`, Enter, and Esc. No Runtime authority,
  SQLite, Artifact, Git, Provider, stage-parent, or Task 8 confirmation logic
  was added.

## TDD and verification

The initial focused test run was red because `CommandPaletteModel`, the model
field, and the palette routing did not exist. After the minimal implementation,
the final focused run passed (10 tests):

```text
env GOCACHE=/private/tmp/cflow-go-cache go test ./internal/tui -run 'Test(NewCommandPaletteRegistersOnlyExit|CommandPaletteEnterSelectsExit|CommandPaletteEscClosesWithoutSelectionChange|RenderCommandPaletteIsCenteredAndBounded|SlashOpensOnlyOutsideTextInput|CommandPaletteEscRestoresPriorPageAndSelection|IdleExitCommandQuitsTUI|RunningExitWaitsForRunnerDoneAfterControlledPause|RunningExitPreviewUsesOnlyEnter|ModelQIsOrdinaryInput)$' -count=1
ok   cflow.local/cflow/internal/tui  0.182s

git diff --check
PASS
```

The requested package gate was run with a bounded 30-second timeout and did
not complete:

```text
env GOCACHE=/private/tmp/cflow-go-cache go test ./internal/tui ./internal/cli ./cmd/cflow -count=1 -timeout=30s
FAIL: internal/tui TestTUIPlanToApplyAndCleanup timed out after 30s
```

The full suite was also run with a bounded 30-second timeout and did not
complete:

```text
env GOCACHE=/private/tmp/cflow-go-cache go test ./... -p 1 -count=1 -timeout=30s
FAIL: internal/app TestApplyBlocksTamperedApplyBranchHead/
      crash_recovery_never_reports_a_tampered_subject_delivered timed out
```

The timeout stacks showed the existing database connection-opener and process
supervisor waits. These failures are recorded as unresolved; the full suite is
not reported as passing.

No real Provider E2E, Self-Dogfood, remote push, or PR was performed.

## Fix Round 1

Addressed the independent review without entering Task 8:

- Removed stale `y/Y/n/N`, `[y/N]`, and Enter-confirmation claims from the
  Task 7-rendered Cancel and Migration surfaces.
- Left `handleCancelKey` and `handleMigrationKey` unchanged; their broader
  confirmation semantics remain Task 8 scope.
- Added a direct root-render regression test proving an open palette preserves
  underlying Workspace content, palette content, and viewport bounds.
- Preserved Task 6 Create behavior, Home left/right inertness, and all Runner
  stop behavior.

TDD evidence: the new stale-copy test first failed on the old Cancel text
(`confirm: no (Enter to choose; Enter alone never cancels)`), then passed after
the copy-only fix.

Focused verification:

```text
env GOCACHE=/private/tmp/cflow-go-cache go test ./internal/tui -run 'Test(NewCommandPaletteRegistersOnlyExit|CommandPaletteEnterSelectsExit|CommandPaletteEscClosesWithoutSelectionChange|RenderCommandPaletteIsCenteredAndBounded|SlashOpensOnlyOutsideTextInput|CommandPaletteEscRestoresPriorPageAndSelection|IdleExitCommandQuitsTUI|RunningExitWaitsForRunnerDoneAfterControlledPause|RunningExitPreviewUsesOnlyEnter|ModelQIsOrdinaryInput|Task7RenderedCancelAndMigrationCopyDoesNotAdvertiseConfirmationKey|RenderOpenCommandPaletteKeepsUnderlyingPageBounded|Create|Home|QuitClassificationDoesNotTreatQAsExit)$' -count=1
ok   cflow.local/cflow/internal/tui  0.233s

git diff --check
PASS
```
