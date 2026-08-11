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

## Fix Round 1

Addressed all findings from `task-8-review.md` without starting Task 9 or
Task 10:

- Added explicit Workflow Menu routing for Application menu routes: Plan /
  Evidence to Plan Approval, Execution Preview to Execution Approval with
  `ExecutionPreviewQuery`, Task Graph and Execution to Execution with the
  workspace query, and Report / Apply / Cleanup to Terminal with the correct
  section and workspace query. Other available readonly routes now have an
  explicit readonly branch instead of relying on accidental fallthrough.
- Added the Application-owned Execution Preview menu entry when the required
  execution facts exist.
- Terminal section changes invalidate the current Previewed/Confirmed state,
  preventing a preview in one section from authorizing a different section.
- Updated stale two-step Enter-only focused expectations and removed
  superseded confirmation comments/fields from active approval and terminal
  code.

### Fix Round 1 verification

```text
GOCACHE=/tmp/cflow-task8-round1-cache go test ./internal/tui -run 'WorkflowMenu|MenuRoutes|ActionPreview|Approval|Terminal|MigrationEntryPoints|Cancel' -count=1
ok  cflow.local/cflow/internal/tui  2.449s
```

Per the user's stop instruction, no package-wide or full-suite test was run.
The remaining unrun gate is `go test ./... -count=1`.

## Fix Round 2

Cleaned only active test names, comments, and failure messages identified by
`task-8-rereview.md`. They now describe the actual confirmation protocol:
first Enter opens the exact preview, second Enter executes, and y/Y/n/N do
not confirm. Assertions, key sequences, and production behavior were not
changed.

### Fix Round 2 verification

```text
GOCACHE=/tmp/cflow-task8-round2-cache go test ./internal/tui -run 'WorkflowMenu|MenuRoutes|ActionPreview|Approval|Terminal|MigrationEntryPoints|Cancel' -count=1
ok  cflow.local/cflow/internal/tui  2.550s

git diff --check
```

The package-wide and full-suite gates remain unrun; Task 9 and Task 10 were
not started.

## Fix Round 3

Renamed `TestModelMigrationEntryPointsDefaultNo` to
`TestModelMigrationEntryPointsRequirePreviewThenExecute` and updated its
directly associated comment to state the actual protocol: first Enter opens
the selected preview, second Enter executes the typed command, and y/Y/n/N do
not confirm. Assertions and behavior were unchanged.

### Fix Round 3 verification

```text
GOCACHE=/tmp/cflow-task8-round3-cache go test ./internal/tui -run 'WorkflowMenu|MenuRoutes|ActionPreview|Approval|Terminal|MigrationEntryPoints|Cancel' -count=1
ok  cflow.local/cflow/internal/tui  2.488s

git diff --check
```

The package-wide and full-suite gates remain unrun; Task 9 and Task 10 were
not started.
