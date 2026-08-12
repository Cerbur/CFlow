# CFlow TUI Hierarchical Workspace Implementation Plan

> For agentic workers: REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (- [ ]) syntax for tracking.

Goal: 将 CFlow 默认全屏 TUI 改造成固定 `WORKFLOWS | WORKSPACE | INSPECTOR`
工作台；Enter/Esc 只切换中间工作区内容，并以 Workflow Menu、动态 Stage
Workspace 和 Global Command Palette 为核心。

Architecture: 固定保留左侧 Workflow 选择、中央动态工作区和右侧只读
Inspector。新增由 Application 提供权威事实的 Workflow Menu Projection；TUI
维护纯 UI 工作区状态栈、选择状态和 Command Palette，所有 Runtime 状态变化
继续通过 Typed Application Command 进入 Decision Kernel。现有 Discussion、
Approval、Execution、Terminal 页面作为中间 Stage Workspace 内容接入；它们
不得替换或隐藏左右栏，也不再由左右键跨生命周期跳转。

Tech Stack: Go 1.26.0 / toolchain 1.26.5、charm.land/bubbletea/v2 v2.0.6、charm.land/lipgloss/v2、现有 Application/Decision Kernel/Store/Agent/GitFlow/Foreground Runner/Native Bridge、Go testing。

## Global Constraints

- Authoritative spec: docs/superpowers/specs/2026-08-12-cflow-tui-workspace-navigation-design.md。
- The 2026-08-11 visual-only plan is superseded for page hierarchy and key semantics; its responsive Lip Gloss constraints remain valid unless this plan says otherwise.
- Before implementation, use superpowers:using-git-worktrees and prove the implementation worktree is Git-visible clean.
- Every Task uses a fresh Implementer context, followed by independent specification and code-quality reviews; Critical/Important findings must be fixed and re-reviewed before the next Task.
- Every Task writes failing tests first, observes the expected failure, implements the smallest passing change, runs its target tests, runs go test ./... -count=1, commits, and proves the worktree clean.
- TUI navigation and rendering never write SQLite, Artifact, Git, Provider state, or final Workflow status directly.
- Application/Runtime/Decision Kernel remain the authority for Workflow state, legal actions, evidence, approval, Apply, Cleanup, and Runner lifecycle.
- Navigation, menu opening, menu selection, Home Esc, Command Palette Esc, and read-only views never issue mutation Commands.
- All confirmation flows use Enter in a dedicated preview/confirmation state; y/Y/n/N are not part of the confirmation protocol.
- q is not an exit key. / opens a Global Command Palette and /exit is the only supported global command in this plan.
- A slash typed while a text input owns focus is literal input, not a Command Palette trigger.
- Ctrl+C remains the existing controlled-stop signal; /exit while a Runner is active must pause through the existing controlled stop and wait for Runner join.
- External processes continue to use program plus argv; this feature adds no shell command execution or command-string registry.
- No automatic Push, PR creation, remote ref mutation, Force, Ignore, Best-effort, or bypass flag is allowed.
- Real Codex/Claude Native E2E and Self-Dogfood require separate explicit approval and are not ordinary test commands.
- Target Branch changes only through explicit Apply Execute; Cleanup always requires Dry Run facts and explicit Enter execution.
- The implementation must preserve width/height safety at 160×45, 120×30, 100×24, 80×24, 60×18, 88×6, 100×6, and 120×6.
- The three-column workbench shell remains visible while the center displays
  Workflow Menu, readonly facts, stage content, action previews, or create
  content at wide supported viewports; a responsive collapse may reduce the
  inspector only at the existing narrow thresholds.

---

## 1. File and module map

The implementation is decomposed into independently reviewable units:

~~~text
internal/app/commands.go
  Add WorkflowMenuQuery, WorkflowMenuView, typed menu entry enums, and the
  closed Query/View union members.

internal/app/application.go
  Route WorkflowMenuQuery to the Application projection.

internal/app/workflow_menu.go
  Build the authoritative Workflow Menu from aggregate facts, existing
  LegalActions, DiscussionReturn facts, artifact availability, and stable
  default-selection rules.

internal/app/workflow_menu_test.go
  Verify only authoritative legal actions and existing read-only facts become
  menu entries; verify ordering and default selection.

internal/tui/navigation.go
  Own the UI-only layer stack, parent return rules, menu selection, and
  route transitions. It must not own Runtime decisions.

internal/tui/workflow_menu.go
  Map WorkflowMenuView to a renderable TUI menu and render grouped entries.

internal/tui/workflow_menu_test.go
  Test grouping, default highlight, selection, Enter route dispatch, Esc pop,
  and no mutation during menu navigation.

internal/tui/command_palette.go
  Own the filtered / command list and visual overlay model. The only command
  is /exit.

internal/tui/command_palette_test.go
  Test slash trigger, filtering, selection, Enter execution, Esc close, q
  non-exit, literal slash in text input, and Runner-aware exit.

internal/tui/app.go
  Integrate navigation stack, Workflow Menu projections, Create routing,
  Stage parent returns, Command Palette dispatch, and q removal without
  bypassing existing command acknowledgement/recovery.

internal/tui/keys.go
  Remove q as an exit semantic while retaining Enter, Esc, arrows, slash,
  and Ctrl+C classification.

internal/tui/workspace_viewmodel.go
  Add the UI-only New Workflow row and map Home facts without inventing
  Runtime actions.

internal/tui/workspace_view.go
  Render Home as WORKFLOWS / WORKSPACE / INSPECTOR, remove the interactive
  left Lifecycle list, preserve progress as non-interactive summary, and
  render the /exit footer affordance.

internal/tui/pages_approval.go
internal/tui/pages_terminal.go
internal/tui/pages_discussion.go
  Keep their stage-specific content while changing parent Esc behavior and
  Enter-only preview/confirmation semantics.

internal/tui/app_test.go
internal/tui/e2e_test.go
internal/tui/workspace_test.go
  Update old lifecycle-navigation, q, n, y/n, and left/right expectations to
  the new hierarchy and add integration coverage.

docs/superpowers/specs/2026-08-11-cflow-tui-main-page-visual-design.md
docs/superpowers/plans/2026-08-11-cflow-tui-main-page-visual-implementation-plan.md
docs/cflow-prd.md
AGENTS.md
  Mark the old visual-only page-hierarchy plan superseded and point current
  work to the 2026-08-12 authoritative design.
~~~

The Application projection and TUI navigation are separate modules because the former answers what is legal and available, while the latter answers where the user currently is.

---

### Task 1: Align authoritative documentation and retire the conflicting visual-only plan

Spec references: 2026-08-12 design §2 and §11.

Files:
- Modify: AGENTS.md
- Modify: docs/cflow-prd.md
- Modify: docs/superpowers/specs/2026-08-11-cflow-tui-main-page-visual-design.md
- Modify: docs/superpowers/plans/2026-08-11-cflow-tui-main-page-visual-implementation-plan.md
- Test: repository text consistency checks

Interfaces:
- Consumes: docs/superpowers/specs/2026-08-12-cflow-tui-workspace-navigation-design.md.
- Produces: one unambiguous documentation chain for all later Tasks.

- [ ] Step 1: Write the failing consistency check

Run:

~~~sh
rg -n "q quit|n create|←→ lifecycle|Enter alone never confirms|只优化.*Workspace|不得修改.*按键语义|2026-08-12-cflow-tui-workspace-navigation-design" AGENTS.md docs/cflow-prd.md docs/superpowers/specs/2026-08-11-cflow-tui-main-page-visual-design.md docs/superpowers/plans/2026-08-11-cflow-tui-main-page-visual-implementation-plan.md
~~~

Expected: the old visual-only constraints and old key hints are still present, and the new design is not yet referenced from every authority entry point.

- [ ] Step 2: Update the authority chain

Add a dated supersession block to AGENTS.md, docs/cflow-prd.md, the 2026-08-11 visual design, and the 2026-08-11 visual plan:

~~~text
2026-08-12 已确认变更：TUI 主入口采用固定 WORKFLOWS | WORKSPACE | INSPECTOR
工作台；Enter 只替换中间工作区内容或确认；Esc 只让中间工作区返回；q 不再退出；
/ 打开 Global Command Palette，本期只支持 /exit。权威规格见
docs/superpowers/specs/2026-08-12-cflow-tui-workspace-navigation-design.md。
2026-08-11 的视觉约束、Lip Gloss 响应式要求和安全不变量继续保留，但其
“只做视觉刷新、不得改变页面层级和按键语义”的限制已 Superseded；页面状态
变化不得隐藏固定三栏外壳。
~~~

Do not delete the old document or its useful visual and safety constraints.

- [ ] Step 3: Verify the documentation chain

Run:

~~~sh
rg -n "2026-08-12-cflow-tui-workspace-navigation-design|Superseded|q 不再退出|/exit|Enter.*Esc" AGENTS.md docs/cflow-prd.md docs/superpowers/specs/2026-08-11-cflow-tui-main-page-visual-design.md docs/superpowers/plans/2026-08-11-cflow-tui-main-page-visual-implementation-plan.md
git diff --check
~~~

Expected: every authority file points to the new design; no whitespace errors remain.

- [ ] Step 4: Run the repository gate

Run:

~~~sh
go test ./... -count=1
~~~

Expected: PASS. This Task changes documentation only, so a test failure is a pre-existing or unrelated blocker that must be recorded before commit.

- [ ] Step 5: Commit

~~~sh
git add AGENTS.md docs/cflow-prd.md docs/superpowers/specs/2026-08-11-cflow-tui-main-page-visual-design.md docs/superpowers/plans/2026-08-11-cflow-tui-main-page-visual-implementation-plan.md
git commit -m "docs: supersede the visual-only tui navigation plan"
git status --short
~~~

Expected: one documentation commit and empty Git-visible status.

---

### Task 2: Add an authoritative Workflow Menu Projection

Spec references: 2026-08-12 design §3.2 and §8.

Files:
- Modify: internal/app/commands.go
- Modify: internal/app/application.go
- Create: internal/app/workflow_menu.go
- Create: internal/app/workflow_menu_test.go

Interfaces:
- Consumes: existing readAggregate, statusView, legalActions, DiscussionReturnQuery facts, existing Plan/Report/Logs availability, and model.WorkflowID.
- Produces:

~~~go
type WorkflowMenuQuery struct {
    Workflow model.WorkflowID
}

type MenuGroup uint8

const (
    MenuGroupContinue MenuGroup = iota
    MenuGroupView
    MenuGroupControl
)

type MenuEntryKind uint8

const (
    MenuEntryReadonly MenuEntryKind = iota
    MenuEntryAction
)

type MenuRoute uint8

const (
    MenuRouteCurrentStage MenuRoute = iota
    MenuRoutePlan
    MenuRouteSpecs
    MenuRouteCatalog
    MenuRouteDAG
    MenuRouteTaskGraph
    MenuRouteLogs
    MenuRouteReport
    MenuRouteDiscussion
    MenuRouteExecution
    MenuRouteApply
    MenuRouteCleanup
    MenuRouteCancel
    MenuRouteMigration
)

type MenuAction uint8

const (
    MenuActionNone MenuAction = iota
    MenuActionStartDiscussion
    MenuActionContinueDiscussion
    MenuActionResume
    MenuActionStartRunner
    MenuActionPause
    MenuActionCancel
    MenuActionApply
    MenuActionCleanup
    MenuActionMigrate
    MenuActionInspectBlocked
)

type WorkflowMenuEntry struct {
    ID     string
    Group  MenuGroup
    Kind   MenuEntryKind
    Label  string
    Route  MenuRoute
    Action MenuAction
}

type WorkflowMenuView struct {
    Workflow     model.WorkflowID
    Name         string
    Stage        model.WorkflowStage
    Runtime      model.RuntimeStatus
    Entries      []WorkflowMenuEntry
    DefaultIndex int
}
~~~

- [ ] Step 1: Write the failing type and projection tests

Add tests that call Application.Query with WorkflowMenuQuery and assert:

~~~go
func TestWorkflowMenuContainsOnlyProjectedActions(t *testing.T) {
    view := queryWorkflowMenuForPausedDiscussion(t)
    menu := view.(app.WorkflowMenuView)

    if menu.Entries[menu.DefaultIndex].Action != app.MenuActionResume &&
        menu.Entries[menu.DefaultIndex].Action != app.MenuActionContinueDiscussion {
        t.Fatalf("default entry = %+v", menu.Entries[menu.DefaultIndex])
    }
    for _, entry := range menu.Entries {
        if entry.Kind == app.MenuEntryAction &&
            entry.Action == app.MenuActionApply {
            t.Fatalf("illegal Apply entry appeared: %+v", entry)
        }
    }
}
~~~

Also add cases for a new discussion with no Session, a bound discussion Session, Blocked Runtime, a Plan with and without evidence, and a completed Workflow with Report facts.

- [ ] Step 2: Run the focused tests and observe failure

Run:

~~~sh
go test ./internal/app -run 'WorkflowMenu|MenuProjection' -count=1
~~~

Expected: FAIL because WorkflowMenuQuery, WorkflowMenuView, and the projection do not exist.

- [ ] Step 3: Add the closed Query/View union members

Add WorkflowMenuQuery to the Query union and route it in Application.Query:

~~~go
case WorkflowMenuQuery:
    return a.queryWorkflowMenu(ctx, qq)
~~~

Add isQuery/isView methods following the existing closed-union pattern. Keep the query bound to one WorkflowID; an empty ID must return the existing typed invalid-input fault rather than silently choosing a Workflow.

- [ ] Step 4: Implement queryWorkflowMenu

Build the menu from authoritative facts:

1. Load the selected aggregate through the same read path used by workspaceLifecycle.
2. Add Current Stage as the first readonly entry.
3. Add Plan/Evidence only when the aggregate contains the active Plan facts.
4. Add Specs/Catalog/DAG/Task Graph/Report routes only when the existing aggregate or Artifact/Report projection proves the corresponding object exists.
5. Translate only existing LegalActions into action entries.
6. Resolve discussion Start versus Continue from the existing DiscussionReturn facts, not from a Stage string.
7. Preserve stable group order: Continue, View, Control.
8. Set DefaultIndex to the first legal primary action; if no action exists, use the first readonly entry.
9. Return a bounded, redacted view and never return free argv, shell text, or an arbitrary command string.

- [ ] Step 5: Run the focused tests

Run:

~~~sh
go test ./internal/app -run 'WorkflowMenu|MenuProjection' -count=1
~~~

Expected: PASS for all new projection cases.

- [ ] Step 6: Run the full gate and commit

Run:

~~~sh
go test ./... -count=1
git diff --check
git add internal/app/commands.go internal/app/application.go internal/app/workflow_menu.go internal/app/workflow_menu_test.go
git commit -m "feat: expose authoritative workflow menu projection"
git status --short
~~~

Expected: PASS, one commit, and clean status.

---

### Task 3: Introduce UI-only center-workspace states and parent return rules

Spec references: 2026-08-12 design §3 and §4.

Files:
- Create: internal/tui/navigation.go
- Create: internal/tui/navigation_test.go
- Modify: internal/tui/app.go
- Modify: internal/tui/keys.go

Interfaces:
- Consumes: Page, model.WorkflowID, WorkflowMenuView, and existing projection/command acknowledgement mechanisms.
- Produces:

~~~go
type WorkspaceLayer uint8

const (
    LayerHome WorkspaceLayer = iota
    LayerWorkflowMenu
    LayerReadonlyWorkspace
    LayerStageWorkspace
    LayerActionPreview
    LayerCreateWorkspace
    LayerCreatePreview
)

type NavigationFrame struct {
    Layer    WorkspaceLayer
    Page     Page
    Workflow model.WorkflowID
    Index    int
}

type NavigationStack struct {
    Frames []NavigationFrame
}

func (s NavigationStack) Current() NavigationFrame
func (s NavigationStack) Push(frame NavigationFrame) NavigationStack
func (s NavigationStack) Pop() (NavigationStack, bool)
func (s NavigationStack) ParentPage() Page
~~~

- [ ] Step 1: Write failing navigation tests

Add pure tests for:

~~~go
func TestNavigationStackHomeMenuStageEsc(t *testing.T) {
    stack := NavigationStack{
        Frames: []NavigationFrame{{Layer: LayerHome, Page: PageWorkspace}},
    }
    stack = stack.Push(NavigationFrame{
        Layer: LayerWorkflowMenu, Page: PageWorkflowMenu, Workflow: "wf-1",
    })
    stack = stack.Push(NavigationFrame{
        Layer: LayerStageWorkspace, Page: PageDiscussion, Workflow: "wf-1",
    })

    if got := stack.Current().Page; got != PageDiscussion {
        t.Fatalf("current page = %v", got)
    }
    var ok bool
    stack, ok = stack.Pop()
    if !ok || stack.Current().Page != PageWorkflowMenu {
        t.Fatalf("first pop = %+v, ok=%v", stack, ok)
    }
    stack, ok = stack.Pop()
    if !ok || stack.Current().Page != PageWorkspace {
        t.Fatalf("second pop = %+v, ok=%v", stack, ok)
    }
    _, ok = stack.Pop()
    if ok {
        t.Fatal("Home pop unexpectedly exited the TUI")
    }
}
~~~

Add Model tests proving Home Esc does not call tea.Quit, and that navigation does not call Execute.

- [ ] Step 2: Run the focused tests and observe failure

Run:

~~~sh
go test ./internal/tui -run 'NavigationStack|HomeEsc|ParentReturn' -count=1
~~~

Expected: FAIL because the layer types and PageWorkflowMenu route do not exist.

- [ ] Step 3: Add the navigation types and Model fields

Add PageWorkflowMenu, PageReadonlyWorkspace, PageActionPreview, and PageCreatePreview where they fit the existing Page union. Add NavigationStack to Model and initialize it with one Home frame.

Do not remove existing stage page models yet; they remain the renderable payloads for Stage Workspace routes.

- [ ] Step 4: Route Enter and Esc through the stack

Implement:

- Home Enter: push a Workflow Menu center-workspace state for the selected Workflow;
- Workflow Menu Enter: push the selected readonly, stage, or preview center-workspace state;
- Stage/Readonly/Preview Esc: pop exactly one center-workspace state;
- Home Esc: return the unchanged Model without tea.Quit;
- slash dispatch is reserved for Task 7 and must not be handled as a page mutation in this Task.

Parent return must restore the previous page and menu index; it must not issue a mutation Command.

- [ ] Step 5: Remove q from exit classification

Remove the root exit meaning from IsQuit and update call sites so q is ordinary input unless a page explicitly uses it for a non-exit purpose. Keep Ctrl+C classification and the existing controlled-stop code path.

Update root error and not-ready text from “press q to quit” to “use /exit to exit”.

- [ ] Step 6: Run tests and commit

Run:

~~~sh
go test ./internal/tui -run 'NavigationStack|HomeEsc|ParentReturn|Quit' -count=1
go test ./... -count=1
git diff --check
git add internal/tui/navigation.go internal/tui/navigation_test.go internal/tui/app.go internal/tui/keys.go
git commit -m "feat: add tui workspace navigation layers"
git status --short
~~~

Expected: PASS and clean status.

---

### Task 4: Rebuild Home as Workflow / Workspace / Inspector

Spec references: 2026-08-12 design §3.1 and §10.4.

Files:
- Modify: internal/tui/workspace_viewmodel.go
- Modify: internal/tui/viewmodel_test.go
- Modify: internal/tui/workspace_view.go
- Modify: internal/tui/workspace_test.go
- Modify: internal/tui/app.go
- Modify: internal/tui/app_test.go

Interfaces:
- Consumes: app.WorkspaceView and existing responsive Lip Gloss helpers.
- Produces: a Home renderer with a UI-only New Workflow row, no interactive left Lifecycle list, and a central WORKSPACE panel.

- [ ] Step 1: Write failing Home mapping tests

Add a UI-only row model:

~~~go
type WorkflowRowKind uint8

const (
    WorkflowRowNew WorkflowRowKind = iota
    WorkflowRowExisting
)

type WorkflowRow struct {
    Kind    WorkflowRowKind
    ID      model.WorkflowID
    Name    string
    Stage   model.WorkflowStage
    Runtime model.RuntimeStatus
}

type WorkspaceViewModel struct {
    Project   app.ProjectView
    Rows      []WorkflowRow
    Selected  WorkflowItem
    Workflows []WorkflowItem
    Lifecycle *LifecycleItem
    Health    app.HealthView
    Actions   []Action
}
~~~

Test that MapWorkspace always inserts New Workflow as row zero, existing Workflows follow in projection order, and selecting New Workflow leaves Selected.ID empty without inventing Runtime facts.

- [ ] Step 2: Run the focused mapping tests and observe failure

Run:

~~~sh
go test ./internal/tui -run 'MapWorkspace|NewWorkflowRow|HomeRows' -count=1
~~~

Expected: FAIL because WorkspaceViewModel has no Rows field and the current renderer has no New Workflow row.

- [ ] Step 3: Implement the UI-only row mapping

Keep app.WorkspaceView unchanged in this Task. Map the existing projection to Rows and preserve all existing stale-selection normalization. Do not make the New Workflow row an Application Workflow or assign it a fake ID.

- [ ] Step 4: Write failing responsive renderer tests

Cover 160×45, 120×30, 100×24, 80×24, 60×18, 88×6, 100×6, and 120×6. Assert:

~~~go
if strings.Contains(frame, "←→ lifecycle") {
    t.Fatal("Home still advertises lifecycle navigation")
}
if !strings.Contains(visibleTerminalText(frame), "NEW WORKFLOW") {
    t.Fatal("Home misses the New Workflow row")
}
if !strings.Contains(visibleTerminalText(frame), "WORKSPACE") {
    t.Fatal("Home misses the central workspace panel")
}
if lipgloss.Width(line) > width {
    t.Fatalf("line width=%d exceeds width=%d: %q", lipgloss.Width(line), width, line)
}
~~~

- [ ] Step 5: Implement the Home renderer

Change the wide layout to:

~~~text
WORKFLOWS | WORKSPACE | INSPECTOR
~~~

Keep Medium/Compact/Minimal behavior width-safe. Render lifecycle progress only as non-interactive summary in the central Workspace. Replace footer hints with / command, Enter, Esc, and ↑↓ navigation; remove q, n create, and left/right lifecycle hints.

The renderer remains pure: it receives only WorkspaceViewModel, status, width, and height.

- [ ] Step 6: Update Home key handling

Remove n as the New Workflow command. Home ↑↓ selects Rows; Enter on WorkflowRowNew pushes Create Workspace; Enter on an existing row pushes Workflow Menu. Home selection remains read-only and reloads the Workspace projection only for existing Workflow IDs.

- [ ] Step 7: Run responsive and full gates

Run:

~~~sh
go test ./internal/tui -run 'Workspace|Responsive|Layout|HomeRows|Navigation' -count=1
go test ./internal/tui ./internal/cli ./cmd/cflow -count=1
go test ./... -count=1
git diff --check
git add internal/tui/workspace_viewmodel.go internal/tui/viewmodel_test.go internal/tui/workspace_view.go internal/tui/workspace_test.go internal/tui/app.go internal/tui/app_test.go
git commit -m "feat: make the tui home a workflow workspace"
git status --short
~~~

Expected: PASS and clean status.

---

### Task 5: Render and route Workflow Menu entries

Spec references: 2026-08-12 design §3.2, §4, and §8.

Files:
- Create: internal/tui/workflow_menu.go
- Create: internal/tui/workflow_menu_test.go
- Modify: internal/tui/app.go
- Modify: internal/tui/app_test.go
- Modify: internal/tui/operation_log.go

Interfaces:
- Consumes: app.WorkflowMenuView and NavigationStack.
- Produces:

~~~go
type MenuItem struct {
    SourceIndex int
    ID          string
    Group       app.MenuGroup
    Kind        app.MenuEntryKind
    Label       string
    Route       app.MenuRoute
    Action      app.MenuAction
}

type WorkflowMenuModel struct {
    Workflow     model.WorkflowID
    Name         string
    Stage        model.WorkflowStage
    Runtime      model.RuntimeStatus
    Items        []MenuItem
    Selected     int
    Loaded       bool
}
~~~

- [ ] Step 1: Write failing mapping and render tests

Add tests that map a WorkflowMenuView into grouped Items, preserve the Application DefaultIndex, and render a selected marker only on the selected item:

~~~go
func TestMapWorkflowMenuPreservesGroupsAndDefault(t *testing.T) {
    menu := MapWorkflowMenu(app.WorkflowMenuView{
        Workflow: "wf-1",
        Name:     "calculator",
        Entries: []app.WorkflowMenuEntry{
            {ID: "resume", Group: app.MenuGroupContinue, Kind: app.MenuEntryAction, Label: "Resume Workflow", Action: app.MenuActionResume},
            {ID: "stage", Group: app.MenuGroupView, Kind: app.MenuEntryReadonly, Label: "Current Stage", Route: app.MenuRouteCurrentStage},
        },
        DefaultIndex: 0,
    })
    if menu.Selected != 0 || menu.Items[0].Group != app.MenuGroupContinue {
        t.Fatalf("menu = %+v", menu)
    }
    if !strings.Contains(RenderWorkflowMenu(menu), "CONTINUE") ||
       !strings.Contains(RenderWorkflowMenu(menu), "CURRENT STAGE") {
        t.Fatal("grouped menu render is incomplete")
    }
}
~~~

- [ ] Step 2: Run the focused tests and observe failure

Run:

~~~sh
go test ./internal/tui -run 'WorkflowMenu|MenuItem|GroupedMenu' -count=1
~~~

Expected: FAIL because the menu model, mapper, and renderer do not exist.

- [ ] Step 3: Implement pure menu mapping and rendering

Map only app-provided entries. Do not add entries from Stage or Runtime strings inside TUI. Render:

- Workflow name, Stage, Runtime;
- CONTINUE, VIEW, CONTROL group headers only when that group has entries;
- selected marker and non-color text state;
- a stable footer with ↑↓, Enter, Esc, and /;
- loading/error states without advertising unavailable mutations.

- [ ] Step 4: Load the WorkflowMenuView on Home Enter

Add a WorkflowMenuQuery projection route to Model query handling. Home Enter pushes the PageWorkflowMenu center-workspace state, marks the menu loading, and requests the bound query. A failed query stays on that center state with the Workflow identity and error; it does not execute a Command.

- [ ] Step 5: Route menu Enter without bypassing action previews

Implement a typed route switch:

~~~go
switch item.Action {
case app.MenuActionStartDiscussion, app.MenuActionContinueDiscussion:
    push the PageDiscussion center-workspace state and query DiscussionReturnQuery
case app.MenuActionCancel:
    push the PageCancel center-workspace state and query CancelSummaryQuery
case app.MenuActionMigrate:
    push the PageMigration center-workspace state and query LayoutMigrationPreviewQuery
case app.MenuActionResume, app.MenuActionPause, app.MenuActionStartRunner:
    push the PageActionPreview center-workspace state and render the bound WorkflowMenu facts; no
    additional mutation or shell command is created before Preview Enter
case app.MenuActionApply, app.MenuActionCleanup:
    push the PageTerminal center-workspace state and query the existing Apply/Cleanup preview source
default:
    push the readonly or stage route identified by item.Route
}
~~~

The route switch may select a page and issue a read-only Query, but it must not call Execute until a later Enter on the action preview or native stage action.

- [ ] Step 6: Add selection-only tests

Prove ↑↓ changes WorkflowMenuModel.Selected and rendering only. The recording controller must have zero Execute calls after:

~~~go
m = press(t, m, tea.KeyDown, 0)
m = press(t, m, tea.KeyUp, 0)
~~~

Prove Esc pops to Home and restores the selected Workflow row.

- [ ] Step 7: Run gates and commit

Run:

~~~sh
go test ./internal/tui -run 'WorkflowMenu|MenuItem|GroupedMenu|Navigation' -count=1
go test ./internal/tui ./internal/app ./internal/cli ./cmd/cflow -count=1
go test ./... -count=1
git diff --check
git add internal/tui/workflow_menu.go internal/tui/workflow_menu_test.go internal/tui/app.go internal/tui/app_test.go internal/tui/operation_log.go
git commit -m "feat: add grouped workflow menu navigation"
git status --short
~~~

Expected: PASS and clean status.

---

### Task 6: Convert New Workflow to an Enter-only two-step flow

Spec references: 2026-08-12 design §5 and §4.3.

Files:
- Modify: internal/tui/app.go
- Modify: internal/tui/app_test.go
- Modify: internal/tui/operation_log.go

Interfaces:
- Consumes: New Workflow row, DiscoveryQuery, CreateWorkflowCommand, and Workspace/Menu acknowledgement queries.
- Produces: Create Workspace → Create Preview → Enter execution → new Workflow Menu with Start Native Discussion selected.

- [ ] Step 1: Write failing Create tests

Replace the old default-No tests with these exact expectations:

~~~go
func TestCreateNameEnterOpensPreviewButDoesNotCreate(t *testing.T) {
    ctrl := &createController{}
    m := newModel(Dependencies{})
    m.ctrl = ctrl
    m.page = PageCreate
    m.createInput = "calculator"
    m.createDirty = &app.DiscoveryView{Branch: "main", Head: "abc123"}

    m = press(t, m, tea.KeyEnter, 0)
    if !m.createConfirm {
        t.Fatal("Enter did not open Create Preview")
    }
    if len(ctrl.executed) != 0 {
        t.Fatalf("name Enter executed commands: %v", ctrl.executed)
    }
}

func TestCreatePreviewEnterExecutes(t *testing.T) {
    ctrl := &createController{dirty: false}
    m := newModel(Dependencies{})
    m.ctrl = ctrl
    m.page = PageCreate
    m.createInput = "calculator"
    m.createDirty = &app.DiscoveryView{Branch: "main", Head: "abc123"}

    m.createConfirm = true
    m = press(t, m, tea.KeyEnter, 0)
    created := false
    for _, cmd := range ctrl.executed {
        if _, ok := cmd.(app.CreateWorkflowCommand); ok {
            created = true
            break
        }
    }
    if !created {
        t.Fatalf("preview Enter did not create: %v", ctrl.executed)
    }
}
~~~

Add a test that y, n, and q do not confirm creation.

- [ ] Step 2: Run the focused tests and observe failure

Run:

~~~sh
go test ./internal/tui -run 'CreateNameEnter|CreatePreviewEnter|Create.*Confirm|Create.*Y|Create.*N' -count=1
~~~

Expected: FAIL because the current Create Preview treats Enter as No and y as the only confirmation.

- [ ] Step 3: Implement Create Preview semantics

Change handleCreateKey:

- editing state Enter sets createConfirm and never executes;
- preview state Enter executes CreateWorkflowCommand;
- preview state Esc returns to editing without mutation;
- y/Y/n/N are ordinary non-confirming input;
- keep the existing dirty Target facts and ConfirmDirty field;
- do not auto-start Native Discussion.

Change renderCreate/createHints to say “Enter review” and “Enter create”; remove all y/n copy.

- [ ] Step 4: Add the post-create menu transition

After CreateWorkflowCommand completes and its Workspace acknowledgement is accepted:

1. set selected to the returned Workflow;
2. reload the bound Workspace projection;
3. push PageWorkflowMenu;
4. query WorkflowMenuQuery;
5. use the Application-provided default index, which must select Start Native Discussion for a newly created discussion Workflow.

Do not set PageDiscussion or start a Native Bridge automatically.

- [ ] Step 5: Test the complete Create path

Run:

~~~sh
go test ./internal/tui -run 'Create|NewWorkflow|WorkflowMenuAfterCreate' -count=1
~~~

Expected: PASS; no command runs on the first Enter, CreateWorkflowCommand runs only on the second Enter, and the resulting page is Workflow Menu.

- [ ] Step 6: Run gates and commit

Run:

~~~sh
go test ./internal/tui ./internal/app ./internal/cli ./cmd/cflow -count=1
go test ./... -count=1
git diff --check
git add internal/tui/app.go internal/tui/app_test.go internal/tui/operation_log.go
git commit -m "feat: make workflow creation enter-confirmed"
git status --short
~~~

Expected: PASS and clean status.

---

### Task 7: Add Global Command Palette and replace q with /exit

Spec references: 2026-08-12 design §7 and §4.

Files:
- Create: internal/tui/command_palette.go
- Create: internal/tui/command_palette_test.go
- Modify: internal/tui/app.go
- Modify: internal/tui/keys.go
- Modify: internal/tui/workspace_view.go
- Modify: internal/tui/app_test.go
- Modify: internal/tui/e2e_test.go

Interfaces:
- Consumes: Model page/layer state, existing controlled-stop methods, Runner state, and tea.Quit.
- Produces:

~~~go
type CommandPaletteModel struct {
    Open     bool
    Input    string
    Selected int
    Commands []GlobalCommand
}

type GlobalCommand struct {
    Name        string
    Description string
}

func NewCommandPalette() CommandPaletteModel
func (p CommandPaletteModel) Update(msg tea.KeyPressMsg) (CommandPaletteModel, CommandPaletteEvent)
func RenderCommandPalette(p CommandPaletteModel, width, height int) string
~~~

The only command registry entry is Name /exit with Description Exit CFlow.

- [ ] Step 1: Write failing Command Palette tests

Add tests:

~~~go
func TestSlashOpensOnlyOutsideTextInput(t *testing.T) {
    m := newModel(Dependencies{})
    m.page = PageWorkspace
    m = step(t, m, tea.KeyPressMsg{Text: "/"})
    if !m.commandPalette.Open {
        t.Fatal("slash did not open Command Palette")
    }

    m.page = PageCreate
    m.commandPalette.Open = false
    m.createConfirm = false
    m = step(t, m, tea.KeyPressMsg{Text: "/"})
    if m.commandPalette.Open || !strings.Contains(m.createInput, "/") {
        t.Fatalf("slash was not literal in text input: palette=%+v input=%q", m.commandPalette, m.createInput)
    }
}

func TestQDoesNotQuit(t *testing.T) {
    m := newModel(Dependencies{})
    m.page = PageWorkspace
    updated, cmd := m.Update(tea.KeyPressMsg{Code: 'q'})
    m = updated.(Model)
    if cmd != nil {
        t.Fatal("q returned a quit command")
    }
}
~~~

Add tests for /exit Enter in idle state and Esc close without state change.

- [ ] Step 2: Run the focused tests and observe failure

Run:

~~~sh
go test ./internal/tui -run 'CommandPalette|Slash|QDoesNotQuit|Exit' -count=1
~~~

Expected: FAIL because CommandPaletteModel does not exist, slash is not globally routed, and q still invokes the quit path.

- [ ] Step 3: Implement the pure palette model and renderer

Render a centered overlay over the current page with:

- /exit command row;
- current input;
- ↑↓ Navigate;
- Enter Select;
- Esc Close.

Use the existing Lip Gloss width-aware helpers. A palette that is too narrow or short must clamp to the available viewport without changing the underlying page.

- [ ] Step 4: Route slash before page handlers

At the root key dispatch:

1. if Command Palette is open, send the key only to the palette;
2. if the current page owns a text input, send slash to that input;
3. otherwise slash opens the palette;
4. no other page receives the same slash key.

The palette Esc restores the exact prior page, selection, menu index, and input state.

- [ ] Step 5: Implement /exit idle and Runner paths

Idle /exit Enter returns tea.Quit.

When m.running is true, /exit Enter pushes PagePauseExit or the equivalent action preview, preserving the existing controlled stop protocol. The second Enter requests PauseWorkflowCommand, cancels the Runner context, waits for runnerDoneMsg, and exits only after the Runner join channel completes. Esc returns to the exact prior page.

Do not use q, y, or n as alternate exit/confirm keys.

- [ ] Step 6: Update all footer and root copy

Replace:

- q quit;
- y/n confirm;
- press q to quit;
- Enter alone never confirms;

with /, Enter, and Esc copy matching the current layer. Keep Ctrl+C copy for controlled stop.

- [ ] Step 7: Run gates and commit

Run:

~~~sh
go test ./internal/tui -run 'CommandPalette|Slash|Exit|Quit|Stop|Confirm' -count=1
go test ./internal/tui ./internal/cli ./cmd/cflow -count=1
go test ./... -count=1
git diff --check
git add internal/tui/command_palette.go internal/tui/command_palette_test.go internal/tui/app.go internal/tui/keys.go internal/tui/workspace_view.go internal/tui/app_test.go internal/tui/e2e_test.go
git commit -m "feat: add the global exit command palette"
git status --short
~~~

Expected: PASS and clean status.

---

### Task 8: Integrate Stage Workspace parent returns and Enter-only confirmations

Spec references: 2026-08-12 design §4, §6, and §7.1.

Files:
- Modify: internal/tui/app.go
- Modify: internal/tui/pages_discussion.go
- Modify: internal/tui/pages_approval.go
- Modify: internal/tui/pages_terminal.go
- Modify: internal/tui/app_test.go
- Modify: internal/tui/pages_approval_test.go
- Modify: internal/tui/workspace_test.go

Interfaces:
- Consumes: NavigationStack, WorkflowMenuView routes, existing stage page models, typed Application Commands, and command acknowledgement state.
- Produces: stage pages that return to their actual parent and never use left/right for lifecycle transitions.

- [ ] Step 1: Write failing parent-return tests

Add tests proving:

~~~go
func TestDiscussionEscReturnsToWorkflowMenu(t *testing.T) {
    m := newModel(Dependencies{})
    m.page = PageDiscussion
    m.navigation = NavigationStack{Frames: []NavigationFrame{
        {Layer: LayerHome, Page: PageWorkspace},
        {Layer: LayerWorkflowMenu, Page: PageWorkflowMenu, Workflow: "wf-1"},
        {Layer: LayerStageWorkspace, Page: PageDiscussion, Workflow: "wf-1"},
    }}
    m = press(t, m, tea.KeyEsc, 0)
    if m.page != PageWorkflowMenu {
        t.Fatalf("page after Esc = %v, want WorkflowMenu", m.page)
    }
}
~~~

Add tests that Tab/left/right from Home never jumps into Discussion, Plan, Execution, or Terminal pages.

- [ ] Step 2: Run the focused tests and observe failure

Run:

~~~sh
go test ./internal/tui -run 'ParentReturn|Stage.*Esc|Lifecycle.*Navigation|Enter.*Confirm' -count=1
~~~

Expected: FAIL because current stage pages return to PageWorkspace and navPages still enables cross-lifecycle navigation.

- [ ] Step 3: Change page parent behavior

Update Discussion, Plan Approval, Execution Approval, Execution, Blocked, Terminal, Cancel, and Migration Esc handlers to pop the NavigationStack. Preserve page-local arrows for local tabs, panes, or lists only; they must never switch lifecycle stages.

Remove navPages and moveNav from Home lifecycle routing. Update tests and footer hints accordingly.

- [ ] Step 4: Convert Approval, Terminal, Cancel, and Migration confirmations

For each confirmation page:

- first Enter enters or displays the explicit Preview state;
- second Enter issues the typed Command bound to the exact Preview facts;
- Esc returns without executing;
- y/Y/n/N do not confirm and are absent from copy.

Preserve existing stale-revision, hash, Apply Preflight, Cleanup Manifest, migration manifest, and command acknowledgement checks. Do not weaken any Runtime gate to make the new key flow pass.

- [ ] Step 5: Route action menu entries to existing stage pages

Implement the typed route mapping:

~~~text
Start/Continue Discussion → center PageDiscussion
Plan / Evidence          → center PagePlanApproval or readonly Plan route
Execution Preview        → center PageExecutionApproval
Execute / Task Graph     → center PageExecution
Report / Apply / Cleanup → center PageTerminal section
Pause / Cancel           → center existing typed preview pages
Migration                → center PageMigration
~~~

Every mutating route must enter its preview before the typed Command runs.

- [ ] Step 6: Run targeted stage tests

Run:

~~~sh
go test ./internal/tui -run 'Discussion|Approval|Execution|Terminal|Cancel|Migration|ParentReturn|Confirm' -count=1
~~~

Expected: PASS with no y/n confirmation and correct parent pages.

- [ ] Step 7: Run gates and commit

Run:

~~~sh
go test ./internal/tui ./internal/app ./internal/cli ./cmd/cflow -count=1
go test ./... -count=1
git diff --check
git add internal/tui/app.go internal/tui/pages_discussion.go internal/tui/pages_approval.go internal/tui/pages_terminal.go internal/tui/app_test.go internal/tui/pages_approval_test.go internal/tui/workspace_test.go
git commit -m "feat: route stage workspaces through the hierarchy"
git status --short
~~~

Expected: PASS and clean status.

---

### Task 9: Add readonly route rendering and authoritative evidence navigation

Spec references: 2026-08-12 design §3.2, §3.3, and §4.2.

Files:
- Create: internal/tui/readonly_workspace.go
- Create: internal/tui/readonly_workspace_test.go
- Modify: internal/tui/app.go
- Modify: internal/tui/app_test.go
- Modify: internal/app/workflow_menu.go
- Modify: internal/app/workflow_menu_test.go

Interfaces:
- Consumes: WorkflowMenuEntry.Route, existing PlanQuery, LogsQuery, ReportQuery, InspectQuery, and bounded view models.
- Produces: read-only workspace routes that never call Execute.

- [ ] Step 1: Write failing readonly route tests

Test that each available readonly route maps to its existing query and that Enter does not execute a mutation:

~~~go
func TestReadonlyMenuEntryIssuesOnlyQuery(t *testing.T) {
    ctrl := &recordingController{}
    m := newModel(Dependencies{})
    m.ctrl = ctrl
    m.page = PageWorkflowMenu
    m.menu = WorkflowMenuModel{
        Workflow: "wf-1",
        Loaded: true,
        Items: []MenuItem{{
            Kind: app.MenuEntryReadonly,
            Route: app.MenuRouteCurrentStage,
            Label: "Current Stage",
        }},
    }
    updated, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
    m = updated.(Model)
    if cmd == nil || len(ctrl.executed) != 0 {
        t.Fatalf("readonly Enter mutated: cmd=%v executed=%v", cmd, ctrl.executed)
    }
}
~~~

- [ ] Step 2: Run the focused tests and observe failure

Run:

~~~sh
go test ./internal/tui -run 'Readonly|Evidence|MenuRoute' -count=1
~~~

Expected: FAIL because there is no readonly route model or route dispatch.

- [ ] Step 3: Implement read-only route mapping

Map only to existing Application queries:

- Current Stage → StatusQuery or the bounded current Workspace facts;
- Plan / Evidence → PlanQuery or existing Inspect/Artifact projection;
- Event Log → LogsQuery;
- Final Report → ReportQuery;
- Specs/Catalog/DAG/Task Graph → existing corresponding Projection/View when present.

If a route has no existing authoritative view, omit it from WorkflowMenuView rather than inventing data.

- [ ] Step 4: Render readonly content with the same width/height contract

Readonly Workspace must preserve:

- header context;
- workflow/stage/runtime;
- bounded content;
- Inspector facts;
- ↑↓ for local list navigation;
- Esc for parent return;
- no mutation footer hints.

- [ ] Step 5: Run tests and commit

Run:

~~~sh
go test ./internal/tui -run 'Readonly|Evidence|MenuRoute|Navigation' -count=1
go test ./internal/app -run 'WorkflowMenu|Projection' -count=1
go test ./... -count=1
git diff --check
git add internal/tui/readonly_workspace.go internal/tui/readonly_workspace_test.go internal/tui/app.go internal/tui/app_test.go internal/app/workflow_menu.go internal/app/workflow_menu_test.go
git commit -m "feat: add readonly workflow workspace routes"
git status --short
~~~

Expected: PASS and clean status.

---

### Task 10: Complete responsive Command Palette and Home visual invariants

Spec references: 2026-08-12 design §7 and §10.4; retained 2026-08-11 Lip Gloss constraints.

Files:
- Modify: internal/tui/command_palette.go
- Modify: internal/tui/command_palette_test.go
- Modify: internal/tui/workspace_view.go
- Modify: internal/tui/workspace_test.go
- Modify: internal/tui/components.go
- Modify: internal/tui/theme.go

Interfaces:
- Consumes: the existing width-aware Lip Gloss helpers and pure ViewModel renderers.
- Produces: stable wide/medium/compact/minimal Home and palette rendering.

- [ ] Step 1: Write failing width/height invariant tests

Use the existing visible-width helper and add:

~~~go
func TestCommandPaletteFitsAllTargetSizes(t *testing.T) {
    palette := CommandPaletteModel{
        Open: true,
        Input: "/",
        Selected: 0,
        Commands: []GlobalCommand{{Name: "/exit", Description: "Exit CFlow"}},
    }
    for _, tc := range []struct{ width, height int }{
        {160, 45}, {120, 30}, {100, 24}, {80, 24}, {60, 18},
        {88, 6}, {100, 6}, {120, 6},
    } {
        frame := RenderCommandPalette(palette, tc.width, tc.height)
        lines := strings.Split(frame, "\n")
        if len(lines) > tc.height {
            t.Fatalf("%dx%d produced %d rows", tc.width, tc.height, len(lines))
        }
        for _, line := range lines {
            if lipgloss.Width(line) > tc.width {
                t.Fatalf("%dx%d line width=%d: %q", tc.width, tc.height, lipgloss.Width(line), line)
            }
        }
    }
}
~~~

- [ ] Step 2: Run the focused tests and observe failure

Run:

~~~sh
go test ./internal/tui -run 'CommandPalette.*Size|Workspace.*Size|Responsive|VisibleTerminalText' -count=1
~~~

Expected: FAIL for at least one narrow palette or Home layout until the overlay and footer are clamped.

- [ ] Step 3: Implement width-aware palette layout

Use Lip Gloss width/height APIs, not len or rune counts, to:

- clamp overlay width to terminal width;
- preserve /exit and Enter/Esc hints when space permits;
- drop optional descriptions before dropping the command name;
- keep one stable row in minimal height;
- return the underlying page unchanged when Open is false.

- [ ] Step 4: Implement Home visual invariants

Ensure:

- left header is WORKFLOWS;
- central header is WORKSPACE;
- right header is INSPECTOR when Wide;
- New Workflow row is visible when height permits;
- lifecycle progress is non-interactive;
- no q, n, or ←→ lifecycle text remains;
- all ANSI/CJK/long path lines are bounded;
- status overlay does not consume the palette or change selection.

- [ ] Step 5: Run visual gates and commit

Run:

~~~sh
go test ./internal/tui -run 'Workspace|CommandPalette|Responsive|Layout|VisibleTerminalText' -count=1
go test ./internal/tui ./internal/cli ./cmd/cflow -count=1
go test ./... -count=1
git diff --check
git add internal/tui/command_palette.go internal/tui/command_palette_test.go internal/tui/workspace_view.go internal/tui/workspace_test.go internal/tui/components.go internal/tui/theme.go
git commit -m "test: harden hierarchical tui responsive layouts"
git status --short
~~~

Expected: PASS and clean status.

---

### Task 11: Update E2E keyboard flows and operation evidence

Spec references: 2026-08-12 design §10.3 and §9.

Files:
- Modify: internal/tui/e2e_test.go
- Modify: internal/tui/app_test.go
- Modify: internal/tui/operation_log.go
- Modify: internal/tui/operation_log_file.go
- Create or modify: scripts/gate-tui.sh

Interfaces:
- Consumes: all completed TUI routes, Fake Provider fixtures, and operation trace schema.
- Produces: deterministic Enter/Esc E2E evidence for the new UI flow.

- [ ] Step 1: Write the failing keyboard journey

Replace the old direct lifecycle Tab/q/y flow with:

~~~text
Home
↓ to NEW WORKFLOW
Enter
type calculator
Enter
Enter
Workflow Menu
Enter on Start Native Discussion
Enter to start the Fake Native Session
Esc back to Discussion Return
Esc back to Workflow Menu
Esc back to Home
/exit
Enter
~~~

Add a second journey for an existing paused Workflow:

~~~text
Home
↓ to calculator
Enter
Workflow Menu
Enter on Resume
Action Preview
Enter
Execution Workspace
Esc
Workflow Menu
Esc
Home
/exit
Enter
~~~

- [ ] Step 2: Run the new E2E test and observe failure

Run:

~~~sh
go test ./internal/tui -run 'TestTUI.*Enter|TestTUI.*WorkflowMenu|TestTUI.*Exit' -count=1 -v
~~~

Expected: FAIL against the old Tab/q/y navigation until the new routes are complete.

- [ ] Step 3: Update operation trace assertions

Record the new UI actions without treating them as Runtime authority:

- command_palette.open;
- command_palette.execute;
- navigation.push;
- navigation.pop;
- workflow_menu.query;
- workflow_menu.select;
- action_preview.open;
- action_preview.confirm.

The log must include Workflow binding and operation ID where available, and never include unredacted user input or arbitrary command text.

- [ ] Step 4: Add Fake Provider gate coverage

Update scripts/gate-tui.sh to assert:

- q does not exit;
- /exit exits;
- Enter is the only confirmation key;
- Home Esc does not exit;
- Runner exit waits for controlled stop;
- no shell command is created from palette input.

Do not add real provider invocation.

- [ ] Step 5: Run the lifecycle E2E and package gates

Run:

~~~sh
go test ./internal/tui -run 'TestTUI.*PlanToApplyAndCleanup|TestTUI.*WorkflowMenu|TestTUI.*Exit' -count=3 -v
go test ./internal/tui ./internal/cli ./cmd/cflow -count=1
go test ./... -count=1
git diff --check
git add internal/tui/e2e_test.go internal/tui/app_test.go internal/tui/operation_log.go internal/tui/operation_log_file.go scripts/gate-tui.sh
git commit -m "test: cover hierarchical tui keyboard journeys"
git status --short
~~~

Expected: PASS and clean status. Do not claim the full suite passes if an unrelated pre-existing timeout remains; record the exact failing package and command.

---

### Task 12: Final review, documentation handoff, and verification

Spec references: all sections, especially §8–§11.

Files:
- Modify: docs/superpowers/specs/2026-08-12-cflow-tui-workspace-navigation-design.md only if review finds a concrete inconsistency.
- Modify: docs/superpowers/plans/2026-08-11-cflow-tui-main-page-visual-implementation-plan.md only to retain a superseded pointer and verification history.
- Modify: README.md and README-zh.md if the bare TUI usage or exit key is documented there.
- Test: all touched package tests and full suite.

- [ ] Step 1: Run the spec traceability scan

Run:

~~~sh
rg -n "Home Esc|Workflow Menu|New Workflow|/exit|q 不再退出|y/n|Enter|Readonly|Runner.*join|Application.*Projection" docs/superpowers/specs/2026-08-12-cflow-tui-workspace-navigation-design.md docs/superpowers/plans/2026-08-12-cflow-tui-workspace-navigation-plan.md internal/tui internal/app
~~~

Expected: every major design decision has an implementation reference and at least one test name.

- [ ] Step 2: Run targeted presentation and navigation tests

Run:

~~~sh
go test ./internal/tui -run 'Workspace|WorkflowMenu|Navigation|CommandPalette|Create|Readonly|Approval|Terminal|Exit|Responsive|Layout' -count=1
~~~

Expected: PASS.

- [ ] Step 3: Run package gates

Run:

~~~sh
go test ./internal/tui ./internal/app ./internal/cli ./cmd/cflow -count=1
~~~

Expected: PASS.

- [ ] Step 4: Run the full suite with a bounded timeout

Run:

~~~sh
go test ./... -p 1 -count=1 -timeout=3m
~~~

Expected: PASS. If the known internal/app cleanup timeout remains, record the exact command, test name, stack evidence, and affected scope; do not call the suite passing.

- [ ] Step 5: Run diff and Git visibility checks

Run:

~~~sh
git diff --check
git status --short
git log --oneline -12
~~~

Expected: no whitespace errors, all Task commits visible, and clean status.

- [ ] Step 6: Obtain independent reviews

Use fresh Reviewer contexts for:

1. specification compliance against docs/superpowers/specs/2026-08-12-cflow-tui-workspace-navigation-design.md;
2. code quality and deep-module boundaries;
3. safety review for Enter-only confirmations, /exit Runner stop, and no q bypass.

Fix every Critical/Important finding, rerun the affected tests, and obtain a re-review before delivery.

- [ ] Step 7: Commit only review fixes

Use an explicit file list after review:

~~~sh
git diff --name-only
git add AGENTS.md docs/cflow-prd.md docs/superpowers/specs/2026-08-12-cflow-tui-workspace-navigation-design.md docs/superpowers/plans/2026-08-11-cflow-tui-main-page-visual-implementation-plan.md internal/app/commands.go internal/app/application.go internal/app/workflow_menu.go internal/app/workflow_menu_test.go internal/tui/app.go internal/tui/app_test.go internal/tui/keys.go internal/tui/navigation.go internal/tui/navigation_test.go internal/tui/workflow_menu.go internal/tui/workflow_menu_test.go internal/tui/command_palette.go internal/tui/command_palette_test.go internal/tui/workspace_viewmodel.go internal/tui/viewmodel_test.go internal/tui/workspace_view.go internal/tui/workspace_test.go internal/tui/pages_discussion.go internal/tui/pages_approval.go internal/tui/pages_terminal.go internal/tui/readonly_workspace.go internal/tui/readonly_workspace_test.go internal/tui/e2e_test.go internal/tui/operation_log.go internal/tui/operation_log_file.go scripts/gate-tui.sh README.md README-zh.md
git commit -m "fix: close tui navigation review findings"
git status --short
~~~

The command stages only the explicitly listed implementation and documentation files; do not replace it with git add -A or a repository-wide glob.

- [ ] Step 8: Final handoff

Report:

- authoritative spec and plan paths;
- each Task commit;
- WorkflowMenuView and NavigationStack boundaries;
- Enter/Esc and /exit behavior;
- confirmation behavior with y/n removed;
- responsive sizes tested;
- full-suite result and any unresolved timeout;
- Git-visible clean proof;
- explicit statement that no remote push, PR, real Provider E2E, or Self-Dogfood was performed.

## Traceability Matrix

| Design section | Plan Tasks | Required evidence |
|---|---:|---|
| §2 superseded visual-only constraints | 1 | docs consistency scan and diff check |
| §3 Home/Menu/Stage layers | 2, 3, 4, 5, 8 | projection, navigation, render, and parent-return tests |
| §4 Enter/Esc | 3, 6, 8 | stack and confirmation tests |
| §5 New Workflow | 4, 6, 11 | Create Preview and post-create menu E2E |
| §6 stage flows | 5, 8, 9, 11 | typed route and Fake Provider journey tests |
| §7 Global Command Palette | 7, 10, 11 | slash, /exit, q, Runner-stop, and responsive tests |
| §8 Application/TUI boundary | 2, 5, 9 | WorkflowMenuView and no-Execute navigation tests |
| §9 recovery | 3, 5, 7, 8 | stale projection, command ack, Runner join, and drift tests |
| §10 testing | 4, 7, 10, 11, 12 | target-size, package, full-suite, and E2E evidence |
| §11 documentation and order | 1, 12 | authority chain and final clean proof |
