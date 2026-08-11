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

## Fix Round 1

Fix commit: `5479c66` (`fix: close task 9 readonly review findings`).

Addressed `.superpowers/sdd/2026-08-12-cflow-tui-workspace-navigation-plan/task-9-review.md`:

1. Updated `TestModelParentReturnRestoresWorkflowMenuIndexWithoutExecute` to consume the readonly Query command, assert it is a `StatusQuery`, assert `Execute` remains unused, then continue the Esc and menu-index restoration assertions.
2. Classified Application-produced `Execution Preview` as `MenuEntryAction` with typed `MenuActionReviewExecution`, preserving the Task 8 Approval preview route. A malformed readonly entry for that route is bounded to the readonly unavailable state and cannot reach the mutation-capable Approval page.
3. Made Specs/Catalog/DAG/Execution Preview availability depend on the existing authoritative `ExecutionPreviewQuery` resolving its artifact-backed view. Missing or unresolvable artifacts now omit those menu entries. Added a regression that removes the Spec artifact after complete-looking execution facts and verifies all four routes are omitted.

Fix Round 1 focused verification passed:

```text
GOCACHE=/tmp/cflow-task9-fix1-gocache go test ./internal/tui -run 'Readonly|Evidence|MenuRoute|Navigation' -count=1
ok   cflow.local/cflow/internal/tui  0.910s

GOCACHE=/tmp/cflow-task9-fix1-gocache go test ./internal/app -run 'WorkflowMenu|Projection' -count=1
ok   cflow.local/cflow/internal/app  8.620s
```

No full-suite command was run for Fix Round 1. Task 10 and Task 11 remain untouched.

The report is part of the Task 9 handoff and is present at the required path. It is committed separately because `.superpowers/` is ignored by default; the report was explicitly force-added for the handoff.
