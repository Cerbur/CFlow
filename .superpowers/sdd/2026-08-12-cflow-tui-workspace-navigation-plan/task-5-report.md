# Task 5 Implementer Report

## Status

Task 5 is implemented and committed. Task 6 was not started.

Implementation commit: `ed6114c feat: add grouped workflow menu navigation`

## Delivered

- Added pure `WorkflowMenuView` → `WorkflowMenuModel` mapping that preserves Application-provided entry order, groups, source indexes, facts, and default selection.
- Added grouped CONTINUE / VIEW / CONTROL rendering with workflow identity, stage, runtime, selected marker, loading/error states, and stable `↑↓`, Enter, Esc, `/` footer.
- Home Enter now pushes Workflow Menu and issues only the bound `app.WorkflowMenuQuery`; no Execute path is involved.
- Workflow Menu ↑↓ changes only UI selection and Enter routes without mutation:
  - Discussion routes query `DiscussionReturnQuery`.
  - Cancel and migration use their existing preview pages and read-only queries.
  - Resume, pause, and runner start enter inert `PageActionPreview`.
  - Apply and cleanup use the existing Terminal preview source.
  - Read-only and stage routes use their existing page boundaries.
- Esc pops the UI navigation stack and restores the selected Home workflow row.
- Added operation-log names for the new page types.
- Updated stale Task 3/4 navigation expectations to the approved Home → Menu semantics; no Runtime authority or command behavior was changed.

## Tests and verification

RED was observed with the task-local Go cache. The focused test initially failed to compile because the Task 5 mapper, renderer, and model fields did not exist.

Passed:

```text
GOCACHE=/tmp/cflow-task5-gocache go test ./internal/tui -run 'WorkflowMenu|MenuItem|GroupedMenu|Navigation' -count=1
GOCACHE=/tmp/cflow-task5-gocache go test ./internal/tui ./internal/app ./internal/cli ./cmd/cflow -skip '^TestTUIPlanToApplyAndCleanup$' -count=1 -timeout=120s
git diff --check
```

The unfiltered `go test ./... -count=1 -timeout=180s` was started but interrupted by the user after the first 30-second output window. It had reported `cmd/cflow` and the agent packages passing; it was not allowed to continue as a long-running test. The known legacy `TestTUIPlanToApplyAndCleanup` still encodes the superseded `n`-based Home flow and may hang. The SDD ledger also records the earlier unrelated full-suite baseline timeout in `internal/app` at `TestNativeDiscussionReturnRejectsBindingDrift`.

## Files changed

- `internal/tui/workflow_menu.go`
- `internal/tui/workflow_menu_test.go`
- `internal/tui/app.go`
- `internal/tui/app_test.go`
- `internal/tui/navigation_test.go` (updated stale expectations required by the approved navigation semantics)
- `internal/tui/operation_log.go`

## Concerns

- The legacy end-to-end test remains intentionally skipped/unmigrated; its Home flow belongs to the later E2E/navigation updates.
- Read-only route content remains a bounded placeholder at this task boundary; authoritative evidence-specific rendering is owned by the planned readonly-route task.
- No real Codex/Claude E2E or self-dogfood was run.

## Task 6

Task 6 was untouched. No runner lifecycle implementation or runner behavior was changed.
