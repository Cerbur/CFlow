# CFlow TUI Main Workspace Visual Refresh Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [x]`) syntax for tracking.

**Goal:** Rebuild only the default TUI Workspace page as a width/height-safe, Lip Gloss v2 styled workflow workbench while preserving the existing Runtime projection, command semantics, navigation, and all other pages.

**Architecture:** Keep `internal/tui/app.go` as the Bubble Tea root Model, message loop, routing, dimensions, and selection state. Move the pure `app.WorkspaceView` mapping into `workspace_viewmodel.go`, the pure responsive renderer into `workspace_view.go`, and shared visual tokens/stateless primitives into `theme.go` and `components.go`; the renderer receives only the mapped ViewModel plus terminal width and height.

**Tech Stack:** Go 1.26; Bubble Tea v2.0.6; Lip Gloss v2 pinned to a Bubble Tea v2-compatible release; ANSI/Unicode-aware Lip Gloss width/height measurement and rendering.

## Global Constraints

- Modify only the main Workspace presentation and its tests; do not modify Application, Runtime, Decision Kernel, Store, Projection, Provider, command semantics, focus rules, page hierarchy, or confirmation flow.
- `MapWorkspace` remains a pure projection-to-ViewModel mapping and may expose only facts already present in `app.WorkspaceView`.
- Legal Actions must come only from `app.WorkspaceView.LegalActions`; never infer Route, Budget, Evidence, or actions from stage/runtime/text.
- Use `program + argv` for external commands; this visual change introduces no external command.
- Render must be safe at 160×45, 120×30, 100×24, 80×24, and 60×18: no panic, no row wider than width, no output taller than height, and a visible footer when space permits.
- Wide is width ≥120 and height ≥28; Medium is width 88–119 or height <28; Compact is width <88; Minimal is too small for the Compact content.
- Do not run real Codex/Claude E2E or Self-Dogfood, push, create a PR, or modify remotes.

### Lifecycle synchronization follow-up (2026-08-11)

The Workspace visual refresh exposed deterministic Bubble Tea lifecycle-test races. The
follow-up changes in `internal/tui/app.go`, `internal/tui/app_test.go`, and
`internal/tui/e2e_test.go` are intentionally limited to TUI-side command/projection
acknowledgement, stale-result rejection, runner-join cleanup, and test barriers. They
do not change Application/Runtime/Projection semantics, page hierarchy, key meanings,
confirmation rules, or final-state authority. The Execution Approval
`execution inputs are incomplete; no preview is available` result is treated only as
that page's documented empty pre-dry-run state; other projection errors remain
non-acknowledging failures. This addendum records the scope explicitly rather than
silently treating lifecycle synchronization as a visual change.

## File Structure

- Modify `internal/tui/app.go`: retain root state and route Workspace rendering through the new pure renderer; do not put styles or panel composition here.
- Create `internal/tui/workspace_viewmodel.go`: move/rename the existing pure Workspace model and mapping without changing its source facts or legal-action behavior.
- Create `internal/tui/workspace_view.go`: implement layout selection and width/height-bounded Workspace rendering.
- Create `internal/tui/theme.go`: define Lip Gloss colors, borders, spacing, and text hierarchy.
- Create `internal/tui/components.go`: define stateless panel, badge, key hint, progress, and bounded-line helpers.
- Remove the old `internal/tui/workspace.go` and `internal/tui/viewmodel.go` after equivalent tests pass.
- Modify/add `internal/tui/workspace_test.go` and `internal/tui/viewmodel_test.go`: add ANSI/Unicode-aware responsive invariants and projection-fact coverage.
- Modify `go.mod` and `go.sum`: add a fixed Lip Gloss v2 dependency compatible with Bubble Tea v2.0.6.

### Task 1: Establish the MVVM file boundary and Lip Gloss dependency

**Files:** `go.mod`, `go.sum`, `internal/tui/workspace_viewmodel.go`, `internal/tui/workspace_viewmodel_test.go`, `internal/tui/app.go`, old `internal/tui/viewmodel.go`.

- [x] Add the pinned Lip Gloss v2 dependency and verify module resolution.
- [x] Move the existing Workspace ViewModel types and `MapWorkspace` into `workspace_viewmodel.go` with no business-field additions and no semantic changes.
- [x] Update root Model references to the new type while keeping selection and navigation behavior unchanged.
- [x] Run `go test ./internal/tui -run 'MapWorkspace|Navigation' -count=1` and the existing TUI package tests.
- [x] Remove the obsolete ViewModel file only after all references compile, then commit: `refactor: separate workspace view model`.

### Task 2: Add theme and stateless rendering components with width-safe behavior

**Files:** `internal/tui/theme.go`, `internal/tui/components.go`, component tests in `internal/tui/workspace_test.go`.

- [x] Write failing tests for Lip Gloss display-width truncation/padding with CJK, ANSI styling, and long identifiers.
- [x] Implement theme tokens for neutral dark surfaces, blue selection, green health/completion, yellow pause/attention, and red errors/danger, with non-color prefixes/labels.
- [x] Implement pure bounded helpers for visible width, height, truncation, padding, panels, badges, key hints, and progress; use Lip Gloss width-aware APIs rather than `len`/`rune` counts for visible alignment.
- [x] Verify each component independently and commit: `feat: add workspace visual tokens and components`.

### Task 3: Implement responsive Workspace View

**Files:** `internal/tui/workspace_view.go`, `internal/tui/app.go`, old `internal/tui/workspace.go`.

- [x] Add failing tests for the five target sizes and Wide/Medium/Compact/Minimal structural markers.
- [x] Implement a pure `RenderWorkspace(vm WorkspaceViewModel, width, height int) string` entry point.
- [x] Wide: retain Workflow/Lifecycle, Main, and Inspector columns; Medium: retain Workflow/Lifecycle plus Main and fold Inspector facts into a read-only summary; Compact: prioritize selected workflow, Stage/Runtime, Lifecycle, and Runtime Legal Actions; Minimal: render stable context plus footer without partial borders.
- [x] Preserve header context, existing health facts, lifecycle facts, legal actions, footer key semantics, empty/Paused/Blocked/Running states, and deterministic truncation.
- [x] Ensure output is clamped to both dimensions after styling, with no negative widths, half panels, or panic.
- [x] Route `PageWorkspace` in `app.go` through the new renderer and leave all other page renderers untouched; remove old `workspace.go` after compilation.
- [x] Run targeted responsive/layout/navigation tests and commit: `feat: render responsive workspace workbench`.

### Task 4: Verify, review, fix, and deliver

**Files:** only files from Tasks 1–3 if review fixes are required.

- [x] Run `go test ./internal/tui -run 'Workspace|Responsive|Layout|Navigation' -count=1`.
- [x] Run `go test ./internal/tui ./internal/cli ./cmd/cflow -count=1`.
- [ ] Run the full suite — blocked by the `internal/app` cleanup test-suite timeout; see Verification Evidence.
- [x] Use independent Reviewer contexts for specification compliance and code quality; record findings and evidence without changing other pages.
- [x] Fix all Critical/Important findings, rerun affected tests, and obtain reviewer re-checks.
- [x] Inspect `git diff --check`, `git diff`, `git status --short`, and commit the completed implementation only after tests and reviews pass.
- [x] Verify Git-visible clean state and report changed files, MVVM responsibilities, actual responsive behavior, test/review evidence, commit, and clean proof.


## Verification Evidence

- Targeted presentation/regression tests: `go test ./internal/tui -run 'Workspace|Responsive|Layout|Navigation|VisibleTerminalText|Command' -count=1` — passed on the current working tree.
- TUI/package gate: `go test ./internal/tui ./internal/cli ./cmd/cflow -count=1` — passed on the current working tree.
- Lifecycle synchronization: `go test ./internal/tui -run '^TestTUIPlanToApplyAndCleanup$' -count=3 -v` — three consecutive passes after workspace-backed acknowledgement mapping, projection-final status markers, and the cleanup project-writer barrier. Mutating commands remain single-shot.
- Full suite: `go test ./... -p 1 -count=1 -timeout=3m` — **failed by timeout** in `internal/app` while running `TestDriveOnceBlockedWorkflowNeedsUser` (the stack showed provider parsing, Git subprocess work, and many `database/sql` connection-opener goroutines). This is not evidence of a passing full suite and remains an external gate outside the touched TUI files.
- Isolated timeout diagnosis: `go test ./internal/app -run '^TestCleanupRemovesExactScratchPath$' -count=1 -timeout=90s -v` — passed in 4.42s; the package-wide/full-suite timeout remains unresolved.
- Focused race lifecycle run: `go test -race ./internal/tui -run '^TestTUIPlanToApplyAndCleanup$' -count=1 -timeout=4m` — passed after the command-generation, context-cancellation, and projection-boundary fixes; no race report emitted. The package gate `go test ./internal/tui ./internal/cli ./cmd/cflow -count=1` also passed.
- Review status: independent specification and code-quality reviewers found no remaining Critical issues; the reported Important issues were fixed in the current tree (strict Workflow identity, Create mutation gating, bounded acknowledgement retry/recovery, quit/error refresh safety, and E2E barriers), then affected package/race/lifecycle tests were rerun successfully. The known full-suite timeout remains an external `internal/app` gate.
- Latest lifecycle synchronization fixes: after Plan approval, the test waits for the refreshed `SPEC_GENERATION` stage marker rather than relying on a complete `APPROVED` fragment in the append-only diff-renderer capture; after Discussion completion and Apply staging, it waits for the corresponding TUI acknowledgement status before sending the next mutating key. These are test-only synchronization changes and do not change TUI semantics.
- Latest TUI state-safety fixes: command acknowledgement now carries an explicit command generation, origin page, and Workflow binding; read-only queries have independent sequence IDs; expected-empty Execution Approval requires the exact Specs acknowledgement generation or confirmed SpecGeneration lifecycle; Runner stop/report messages are workflow-bound; workflow selection resets Execution/Terminal state; root cancellation now requests a normal joined Runner stop and `Run` waits on the Runner completion channel as a final safety net.
- Width/height coverage: `160×45`, `120×30`, `100×24`, `80×24`, `60×18`, plus small-height `88×6`, `100×6`, `120×6` and root status heights `1`, `2`, `3`.
- Current delivery commits: `bfde5c0` (`feat: refresh workspace tui presentation`), `e1f1f40` (`test: stabilize tui lifecycle synchronization`), `977fa85` (`fix: harden compact blocked state and tui e2e synchronization`), `fcc3477` (`fix: finalize tui e2e synchronization cleanup`), and `f44a84b` (`fix: harden tui lifecycle synchronization`). Git-visible clean proof was obtained after `f44a84b`.
