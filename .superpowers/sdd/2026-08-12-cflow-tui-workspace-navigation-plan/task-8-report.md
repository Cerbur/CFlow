# Task 8 Report: Stage Workspace Navigation and Enter-only Confirmations

## Scope

Implemented only the Task 8 navigation and confirmation changes from the
2026-08-12 workspace-navigation brief. Task 9 and Task 10 were not started.

## Implementation

- Removed Home lifecycle traversal through Tab and removed the shared
  `navPages`/`moveNav` route.
- Kept page-local left/right behavior for approval tabs, execution panes, and
  terminal sections.
- Made stage-page Esc handling follow the existing `NavigationStack`, so
  Discussion, Plan Approval, Execution Approval, Execution, Blocked, Terminal,
  Cancel, and Migration return to the actual Workflow Menu parent.
- Routed Home Cancel and Migration entry points through stage frames and kept
  command completion returns stack-aware.
- Added explicit Action Preview handling for menu Resume, Pause, and Start
  Runner routes.
- Converted Approval, Terminal, Cancel, and Migration flows to Preview ->
  Enter-only execution. `y/Y/n/N` no longer confirm and are absent from the
  affected rendered copy.
- Preserved typed command payloads and existing revision/hash, Apply preflight,
  Cleanup manifest, migration manifest, and acknowledgement gates.
- Updated focused TUI tests for parent returns, Home lifecycle isolation, and
  Enter-only confirmation behavior.

## Verification

The required focused red test was first run against the pre-change code and
failed for the expected legacy behaviors: Home Tab entered a lifecycle page,
Cancel accepted `y`, and Approval accepted `y`.

The focused command below then passed after the core implementation:

```text
GOCACHE=/tmp/cflow-gocache go test ./internal/tui -run 'HomeTabDoesNot|StageWorkspaceEsc|CancelRequires|ApprovalYNAre' -count=1
ok  cflow.local/cflow/internal/tui  0.208s
```

Per the final execution instruction, no package-level or full-suite test was
run after the final copy/reset edits. `go test ./... -count=1` is therefore
not claimed as passing; it remains an unrun gate for a later approved run.

`gofmt` completed for the changed Go files and the final `git diff --check`
completed without output.

## Commit

The scoped changes and this report are committed as:

```text
feat: route stage workspaces through the hierarchy
```
