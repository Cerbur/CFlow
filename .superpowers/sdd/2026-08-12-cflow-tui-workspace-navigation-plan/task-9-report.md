# Task 9 Report: Readonly Route Rendering and Evidence Navigation

Date: 2026-08-12
Base: c7c024d
Implementation commit: b251a1e (`feat: add readonly workflow workspace routes`)

## Scope

Implemented only Task 9 from:

- Authoritative design: `docs/superpowers/specs/2026-08-12-cflow-tui-workspace-navigation-design.md`
- Navigation plan: `docs/superpowers/plans/2026-08-12-cflow-tui-workspace-navigation-plan.md`
- Task brief: `.superpowers/sdd/2026-08-12-cflow-tui-workspace-navigation-plan/task-9-brief.md`

Task 10 responsive redesign and Task 11 process work were not started.

## Implementation

- Added `internal/tui/readonly_workspace.go` with a UI-only bounded readonly model, route-to-query mapping, authoritative view mapping, inspector facts, local list selection, unavailable-state handling, and bounded rendering.
- Added focused readonly tests covering query-only Enter behavior, every readonly route, projection mapping, bounded rendering, mutation-free footer hints, local ↑↓ navigation, and NavigationStack parent Esc.
- Updated `internal/tui/app.go` to retain readonly projection state, route workflow-bound queries, apply readonly views/errors, render the readonly workspace, and handle local list navigation.
- Updated `internal/tui/workflow_menu.go` so Current Stage, Plan/Evidence, Specs, Catalog, DAG, Task Graph, Event Log, and Final Report enter the readonly workspace and issue only existing Application queries.
- Updated the existing TUI route expectations for Plan, Task Graph, and Final Report.
- Updated `internal/app/workflow_menu.go` so Specs/Catalog/DAG entries are emitted only when the existing bounded Execution Preview authority is complete; partial facts omit unsupported artifact routes rather than inventing views.
- Added an Application projection test for omission of artifact routes when preview facts are incomplete.

## Route mapping

| Menu route | Existing authoritative query | Readonly content |
|---|---|---|
| Current Stage | `StatusQuery` | stage/runtime/layout plus available inspector facts |
| Plan / Evidence | `PlanQuery` | revision/status/hash/approval |
| Specs | `ExecutionPreviewQuery` | approved Spec artifact reference and hashes |
| Verification Catalog | `ExecutionPreviewQuery` | Catalog reference/hash and command identities |
| Workflow DAG | `ExecutionPreviewQuery` | Workflow artifact/hash and parallel groups |
| Task Graph | `InspectQuery` | bounded Node IDs, kinds, and statuses |
| Event Log | `LogsQuery` | bounded authoritative Event window |
| Final Report | `ReportQuery` | result/summary and bounded report lines |

No new Runtime, SQLite, Git, Artifact, Provider, or mutation authority was added. Unsupported or empty route facts render as bounded unavailable state, and menu projection omits artifact routes when no complete authoritative view exists.

## Safety and navigation invariants

- Enter on readonly menu entries issues a Query and never calls `Execute`.
- Readonly pages preserve workflow, stage, and runtime context; inspector fields are rendered only from available authoritative status/report facts.
- ↑↓ changes only local readonly item selection.
- Esc is handled by the existing `NavigationStack` and returns to Workflow Menu.
- The readonly footer contains browse/back/command-palette hints only; it has no mutation or confirmation affordance.
- No Task 10 responsive redesign, Task 11 process work, real Provider E2E, Self-Dogfood, remote push, or PR was performed.

## Verification

All commands used a task-local Go cache at `/tmp/cflow-task9-gocache` because the default cache path was not writable in the managed environment.

Passed:

```text
GOCACHE=/tmp/cflow-task9-gocache go test ./internal/tui -run 'Readonly|Evidence|MenuRoute|Navigation' -count=1
ok   cflow.local/cflow/internal/tui  0.566s

GOCACHE=/tmp/cflow-task9-gocache go test ./internal/app -run 'WorkflowMenu|Projection' -count=1
ok   cflow.local/cflow/internal/app  6.682s

GOCACHE=/tmp/cflow-task9-gocache go test ./internal/tui ./internal/app ./internal/cli ./cmd/cflow -count=1
exit status 0

git diff --check
exit status 0
```

The required full-suite command was started once:

```text
GOCACHE=/tmp/cflow-task9-gocache go test ./... -p 1 -count=1 -timeout=3m
```

It was interrupted by the user after 8.7 seconds. It produced no test result, so the full suite is explicitly **not claimed as passed** and remains incomplete for this handoff.

The initial TDD RED run failed before compilation because the readonly model/renderer did not exist. After implementation, the focused readonly suite passed as recorded above.

## Git

Implementation commit: `b251a1e`.

The report is part of the Task 9 handoff and is present at the required path. The repository ignores `.superpowers/`, so it is not staged; the implementation commit remains scoped to the seven Task 9 source/test files. Final `git status --short` is verified clean because the report path is ignored.
