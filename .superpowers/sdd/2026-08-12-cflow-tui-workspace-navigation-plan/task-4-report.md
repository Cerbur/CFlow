# Task 4 Implementer Report

## Status

Implemented Task 4 only: the Home screen now maps a UI-only New Workflow row, renders the responsive WORKFLOWS / WORKSPACE / INSPECTOR hierarchy, and routes Home selection/Enter by row kind without mutating Runtime state.

Implementation commit: `8ff810b feat: make the tui home a workflow workspace`

## Scope Delivered

- Added `WorkflowRowKind`, `WorkflowRow`, and `WorkspaceViewModel.Rows` in the TUI mapping layer.
- `MapWorkspace` always inserts `NEW WORKFLOW` at row zero and preserves Application workflow projection order after it.
- The New Workflow row has no Workflow ID, Stage, Runtime, Lifecycle, or LegalActions facts.
- Preserved stale non-empty Workflow selection normalization to the first projected Workflow.
- Rebuilt wide Home as `WORKFLOWS | WORKSPACE | INSPECTOR`.
- Removed the interactive Lifecycle list from the left column; lifecycle facts remain a read-only summary in the central Workspace.
- Kept Medium, Compact, and Minimal layouts width/height bounded, including the required 88x6, 100x6, and 120x6 minimal sizes.
- Replaced Home footer hints with `/ command`, Enter, Esc, and up/down navigation; removed q, n-create, left/right-lifecycle, and direct action hints from the Home footer.
- Home up/down now selects `Rows`.
  - Selecting New Workflow clears only workflow-bound TUI presentation state and issues no Query or Command.
  - Selecting an existing Workflow issues only `ProjectWorkspaceQuery{Selected: <id>}`.
- Home Enter on New Workflow pushes `LayerCreateWorkspace` / `PageCreate` and requests the existing read-only `DiscoveryQuery`.
- Home Enter on an existing Workflow pushes `LayerWorkflowMenu` without executing a command.
- Removed `n` as the Home create command.

Not implemented: Workflow Menu rendering/query flow, Command Palette, readonly route rendering, or confirmation semantic changes.

## TDD Evidence

1. Home mapping RED:
   - `go test ./internal/tui -run 'MapWorkspace|NewWorkflowRow|HomeRows' -count=1`
   - Failed because `WorkspaceViewModel.Rows`, `WorkflowRowNew`, and `WorkflowRowExisting` did not exist.
2. Home mapping GREEN:
   - Same command passed: `ok cflow.local/cflow/internal/tui`.
3. Responsive Home RED:
   - `go test ./internal/tui -run 'HomeRows' -count=1`
   - Failed because the old renderer advertised lifecycle navigation and omitted New Workflow in Compact/Minimal layouts.
4. Responsive Home GREEN:
   - Same command passed.
5. Home routing RED:
   - `go test ./internal/tui -run 'HomeRows|HomeN' -count=1`
   - Failed because old selection queried the Application instead of selecting New Workflow, New Enter did not push Create Workspace, and `n` still opened Create.
6. Home routing GREEN:
   - Same command passed.

## Verification

- Baseline before edits:
  - `go test ./internal/tui ./internal/cli ./cmd/cflow -count=1`
  - PASS (`internal/tui` 18.897s, `internal/cli` 2.051s, `cmd/cflow` no test files).
- Required focused gate:
  - `go test ./internal/tui -run 'Workspace|Responsive|Layout|HomeRows|Navigation' -count=1`
  - PASS (`internal/tui` 1.869s).
- Bounded package gate excluding the known stale E2E:
  - `go test ./internal/tui ./internal/cli ./cmd/cflow -skip '^TestTUIPlanToApplyAndCleanup$' -count=1 -timeout=120s`
  - PASS (`internal/tui` 4.839s, `internal/cli` 1.679s, `cmd/cflow` no test files).
- `git diff --check`
  - PASS before commit.

## Timeout / Known Baseline

The exact package gate `go test ./internal/tui ./internal/cli ./cmd/cflow -count=1` did not finish after approximately 100 seconds and was stopped as requested. A bounded diagnostic run, `go test ./internal/tui -count=1 -timeout=60s`, identified the sole running test at timeout as `TestTUIPlanToApplyAndCleanup`, blocked in its screen-marker polling loop.

That E2E still encodes superseded Home behavior outside Task 4's permitted file list:

- waits for `no workflows yet`;
- sends `n` to open Create;
- later uses legacy Home/lifecycle navigation and old confirmation semantics.

Task 4 explicitly removes the `n` create command. Updating `internal/tui/e2e_test.go` was therefore not smuggled into this scoped implementation. The full skip run (`go test ./... -skip '^TestTUIPlanToApplyAndCleanup$' -count=1 -timeout=180s`) began successfully and reported passing packages through `internal/agent/fake`, but was stopped immediately on user instruction before completion. No claim is made that the exact package or full gates passed.

## Concerns / Handoff

- `internal/tui/e2e_test.go::TestTUIPlanToApplyAndCleanup` must be migrated to the approved Home -> New Workflow -> Create Workspace navigation before the unfiltered package/full gates can complete.
- No Runtime projection type or Application mapping was changed; `app.WorkspaceView` remains authoritative and unchanged.
- No direct mutation is performed by Home row selection or route entry.
- Task 5 was not started.

## Fix Round 1

Review feedback was applied without starting Task 5.

### Fixes

- Home `IsLeft` and `IsRight` are now inert. They no longer call `moveNav` or issue lifecycle projection queries.
- Create Workspace Esc now uses the stack-aware navigation path even while the Create name input is active. It clears transient Create input state, pops the `LayerCreateWorkspace` frame, and restores the existing Home frame with synchronized `page` and `NavigationStack.Current()` values.
- Added `WorkspaceViewModel.SelectedRow` as UI-only selection state. Home row highlighting updates immediately when an existing row is selected, while `Selected`, `Lifecycle`, and other projection facts remain unchanged until the authoritative query returns.
- Updated the old lifecycle navigation test to remove its superseded Home left/right expectation; Tab remains covered as the lifecycle-page traversal mechanism.

### Regression Tests

RED was observed before the fixes:

- `TestModelHomeLeftRightAreInert` failed because Home Right changed to a lifecycle page.
- `TestModelCreateWorkspaceEscPopsNavigationHome` failed because Create Esc assigned `PageWorkspace` while leaving the Create frame on the stack.
- `TestModelHomeExistingRowHighlightUpdatesBeforeProjection` initially failed to compile because `SelectedRow` did not exist; after adding the UI-only state, it verifies the old projection facts remain intact while the new row is highlighted.

GREEN after the fixes:

- `go test ./internal/tui -run 'HomeLeftRight|CreateWorkspaceEsc|ExistingRowHighlight' -count=1`
- `go test ./internal/tui -run 'MapWorkspace|NewWorkflowRow|HomeRows|HomeLeftRight|CreateWorkspaceEsc|ExistingRowHighlight' -count=1`
- `go test ./internal/tui -run 'Workspace|Responsive|Layout|HomeRows|Navigation|HomeLeftRight|CreateWorkspaceEsc|ExistingRowHighlight' -count=1`

All three completed successfully. The known old `TestTUIPlanToApplyAndCleanup` E2E was not run, consistent with the Task 4 gate constraint and prior report.

## Fix Round 2

Review feedback identified an Esc-state regression introduced by Fix Round 1: the stack-pop guard treated Create editing and Create Preview/confirmation as the same state.

### Fix

- Create Workspace Esc now branches by state:
  - Initial empty/typing state (`createConfirm == false`) clears transient Create state, pops `LayerCreateWorkspace`, and restores the Home frame.
  - Create Preview/confirmation (`createConfirm == true`) remains on `PageCreate`, clears `createConfirm`, and preserves `createInput` for editing. It does not pop the NavigationStack.
- Refreshed `TestModelNavigationReachesLifecyclePages` to describe Tab-based lifecycle traversal and explicitly note that Home left/right is inert.

### Regression Test

Added `TestModelCreatePreviewEscReturnsToEditing`, covering:

`New Workflow → Create Workspace → type calculator → Enter → Esc`

The RED run was:

```text
go test ./internal/tui -run 'CreatePreviewEsc|CreateWorkspaceEsc' -count=1
--- FAIL: TestModelCreatePreviewEscReturnsToEditing
    Create Preview Esc left Create: page=0 frame={Layer:0 Page:0 ...}
```

After narrowing the stack-pop guard to `!m.createConfirm`, the same focused command passed. The existing `TestModelCreateWorkspaceEscPopsNavigationHome` also remained green, proving the two Esc paths stay distinct.

### Fix Round 2 Verification

```text
go test ./internal/tui -run 'HomeLeftRight|CreateWorkspaceEsc|CreatePreviewEsc|ExistingRowHighlight|MapWorkspace|NewWorkflowRow|HomeRows|Workspace|Responsive|Layout|Navigation|CreateDirty|CreateClean|CreateMissingFacts' -count=1 -timeout=60s
```

Result: PASS (`cflow.local/cflow/internal/tui` 1.591s).

`git diff --check` also passed. No full suite or old E2E was run, and Task 5 was not started.
