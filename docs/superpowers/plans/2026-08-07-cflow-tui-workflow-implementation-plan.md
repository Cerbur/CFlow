# CFlow TUI Workflow Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 将 CFlow 从行式命令 Demo 改造成以全屏 TUI 为默认入口、以原生 Codex/Claude 需求讨论和唯一 Workflow Worktree 为核心、可以自动推进到受保护 Apply 与显式 Cleanup 的本地工作流工具。

**Architecture:** Bubble Tea TUI 与 Cobra Headless CLI 作为同级前端，共用现有 `app.Application`、Decision Kernel、Store、Agent、GitFlow、Recovery 与 Evidence。新增聚合目录解析器、Workspace/Change Set 事实、Native Session Bridge 和 Foreground Runner；TUI 只消费类型化 View/Outcome，所有状态变化仍由 Runtime 判定。

**Tech Stack:** Go 1.26.0 / toolchain 1.26.5、`charm.land/bubbletea/v2 v2.0.6`、Cobra、modernc SQLite、现有 Agent/Process/GitFlow seams、Go `testing`。

## Global Constraints

- 权威规格：`docs/superpowers/specs/2026-08-07-cflow-tui-workflow-design.md`。
- 开始实施前使用 `superpowers:using-git-worktrees` 创建独立、Git-visible clean 的实现 Worktree；不得在用户当前有未跟踪文件的工作目录直接执行本计划。
- 每个 Task 使用新的 Implementer 上下文；完成后依次进行独立规格符合性 Review 和代码质量 Review。
- Critical/Important 问题修复并复审通过前不得进入下一 Task。
- 每个 Task 必须先写失败测试、运行目标测试观察预期失败、写最小实现、运行目标测试、运行 `go test ./... -count=1`、提交并证明实现 Worktree clean。
- 所有外部命令通过 program + argv 调用，禁止 `shell: true`、隐式 Shell、Force、Ignore、Best-effort、Danger/Bypass Flag。
- TUI、CLI、Runner、Native Bridge 都不得直接写 SQLite、Artifact 或最终状态；只调用类型化 Application API。
- `$CFLOW_HOME/cflow.db` 继续是全局权威状态源；Workflow-local `state/` 只是投影与恢复辅助证据。
- 原始 Target Branch 在显式 Apply Execute 前保持不变；没有任何 Task 自动 Push 或创建 PR。
- 真实 Codex/Claude E2E 与 Self-Dogfood 不属于普通测试命令，必须获得单独明确授权。
- 计划中的 Commit Message 是固定意图；不得把多个 Task 压成一个提交。

## Target File Structure

```text
internal/layout/
  resolver.go                 聚合 Workflow 路径的唯一构造入口
  resolver_test.go
  migrate.go                  显式 Legacy Layout Migration 协调器
  migrate_test.go
internal/native/
  bridge.go                   TUI suspend / Provider native / reconcile 边界
  bridge_test.go
internal/foreground/
  runner.go                   有界 DriveOnce 循环
  runner_test.go
internal/tui/
  app.go                      Bubble Tea 根 Model
  app_test.go
  keys.go                     导航与安全 Action Keymap
  workspace.go               Project/Workflow/lifecycle 外壳
  workspace_test.go
  pages_discussion.go
  pages_approval.go
  pages_execution.go
  pages_terminal.go
  viewmodel.go                app.View → UI ViewModel
  viewmodel_test.go
migrations/
  005_workspace_layout.sql    Workspace 与 Layout 权威字段
scripts/
  gate-tui.sh                 Fake Provider TUI Gate
```

现有大文件只做定向拆分：新增逻辑不得继续堆入 `internal/cli/commands.go`、`internal/app/application.go` 或 `internal/app/dispatch.go`。

---

### Task 1: 更新权威文档与 Gate 约束

**Files:**
- Modify: `AGENTS.md`
- Modify: `docs/cflow-prd.md`
- Modify: `docs/cflow-demo-design.md`
- Modify: `docs/cflow-demo-implementation-plan.md`
- Modify: `README.md`
- Modify: `README-zh.md`

**Interfaces:**
- Consumes: 已确认 TUI 设计文档。
- Produces: 后续实现与 Review 使用的新权威约束；旧 Gate 3 明确标记为 line-oriented historical evidence。

- [ ] **Step 1: 写文档一致性失败检查**

Run:

```sh
rg -n "不实现完整 TUI|No full-screen TUI|line-oriented CLI" AGENTS.md docs/cflow-prd.md docs/cflow-demo-design.md docs/cflow-demo-implementation-plan.md
```

Expected: 至少命中当前的旧约束，证明权威文档尚未切换。

- [ ] **Step 2: 更新决策层级与范围**

在四份权威文档中明确写入：

```text
2026-08-07 已确认变更：全屏 TUI 成为默认主入口；Native Discussion、
聚合 Workflow 目录、唯一 Workspace、Foreground Runner、workspace-aware
Apply 与显式 Cleanup 取代旧的 line-oriented Demo 交互决策。
```

保留旧决策作为历史背景，但标记为 `Superseded`，不得删除安全不变量。

- [ ] **Step 3: 更新 README 状态**

README 必须说明当前实现仍是旧 Demo、TUI 设计已确认但未完成，避免在 Task 1 后提前宣称可用。

- [ ] **Step 4: 验证文档一致性**

Run:

```sh
rg -n "2026-08-07.*TUI|Superseded|已取代|historical" AGENTS.md docs/cflow-prd.md docs/cflow-demo-design.md docs/cflow-demo-implementation-plan.md README.md README-zh.md
git diff --check
```

Expected: 新决策在所有权威文档可定位；`git diff --check` 无输出。

- [ ] **Step 5: Review、全量测试与提交**

Run: `go test ./... -count=1`
Expected: PASS。

Commit:

```sh
git add AGENTS.md docs/cflow-prd.md docs/cflow-demo-design.md docs/cflow-demo-implementation-plan.md README.md README-zh.md
git commit -m "docs: adopt the TUI workflow direction"
```

---

### Task 2: 建立聚合目录解析器

**Files:**
- Create: `internal/layout/resolver.go`
- Create: `internal/layout/resolver_test.go`
- Modify: `internal/security/paths.go`
- Modify: `internal/security/paths_test.go`
- Modify: `internal/artifact/store.go`
- Modify: `internal/artifact/store_test.go`

**Interfaces:**
- Consumes: `CFLOW_HOME`、Project Key、Workflow ID。
- Produces: `layout.Resolver` 及 `WorkflowRoot/Workspace/Task/Apply/ArtifactDir` 方法；`artifact.NewWorkflow` 将 Artifact 写入聚合 Workflow 类型目录；所有后续 Task 只能通过这些类型化入口构造路径。

- [ ] **Step 1: 写失败测试**

```go
func TestResolverAggregatesWorkflowFiles(t *testing.T) {
    r := layout.Resolver{Home: "/home/u/.cflow", ProjectKey: "project-a"}
    wf := model.WorkflowID("wf-1")
    want := "/home/u/.cflow/projects/project-a/wf-1"
    if got := r.WorkflowRoot(wf); got != want { t.Fatalf("root=%q want=%q", got, want) }
    if got := r.Workspace(wf); got != want+"/workspace" { t.Fatalf("workspace=%q", got) }
    if got := r.Task(wf, "S01"); got != want+"/tmp/tasks/S01" { t.Fatalf("task=%q", got) }
    if got := r.Apply(wf, 2); got != want+"/tmp/apply-2" { t.Fatalf("apply=%q", got) }
}
```

- [ ] **Step 2: 运行失败测试**

Run: `go test ./internal/layout -run TestResolverAggregatesWorkflowFiles -count=1`
Expected: FAIL，因为 package/type 尚不存在。

- [ ] **Step 3: 写最小实现**

```go
type Resolver struct { Home, ProjectKey string }
func (r Resolver) WorkflowRoot(wf model.WorkflowID) string {
    return filepath.Join(r.Home, "projects", r.ProjectKey, string(wf))
}
func (r Resolver) Workspace(wf model.WorkflowID) string { return filepath.Join(r.WorkflowRoot(wf), "workspace") }
func (r Resolver) Task(wf model.WorkflowID, node model.NodeID) string {
    return filepath.Join(r.WorkflowRoot(wf), "tmp", "tasks", string(node))
}
func (r Resolver) Apply(wf model.WorkflowID, n int) string {
    return filepath.Join(r.WorkflowRoot(wf), "tmp", fmt.Sprintf("apply-%d", n))
}
```

同时提供 `DiscussionDir`、`PlansDir`、`SpecsDir`、`WorkflowsDir`、`ReviewsDir`、`SessionsDir`、`EvidenceDir`、`LogsDir`、`ReportsDir`、`StateDir`，并让安全路径注册表接受这些受管根。

> **实现注记 (2026-08-07)**：`internal/security/paths.go` 无需改动——`preservedTreeNames` 已含 `projects`，现有 `CheckCleanupScratch` 与 `CheckPath` 天然拒绝把聚合 Workflow 树当作 exact scratch。由新增测试 `TestCheckCleanupScratchRejectsAggregatedWorkflowRoot`（paths_test.go）证明，因此 Task 2 未修改 `paths.go`。

- [ ] **Step 4: 让 Artifact Store 使用聚合类型目录**

保留 Legacy `artifact.New` Reader，新增：

```go
func NewWorkflow(root string, wf model.WorkflowID, redaction security.Registry) (*Store, error)
```

新 Store 验证 `root` 正好是该 Workflow 聚合根，并用固定映射写入 `plans/`、`specs/`、`workflows/`、`discussion/`、`reviews/`、`reports/`、`evidence/`。路径不再出现 `artifacts/<workflow-id>/<type>`。本 Task 只建立 API；Task 4 在 Layout Version 字段落地后接入 Application。Legacy `artifact.New` 继续用于旧布局读取。

- [ ] **Step 5: 验证路径与安全边界**

Run: `go test ./internal/layout ./internal/security ./internal/artifact -count=1`
Expected: PASS，且 traversal、symlink escape、空 Project/Workflow ID 被拒绝。

- [ ] **Step 6: 全量测试、Review 与提交**

Run: `go test ./... -count=1`
Commit:

```sh
git add internal/layout internal/security/paths.go internal/security/paths_test.go internal/artifact/store.go internal/artifact/store_test.go
git commit -m "feat: define aggregated workflow paths"
```

---

### Task 3: 持久化 Workspace 与 Layout 事实

**Files:**
- Create: `migrations/005_workspace_layout.sql`
- Modify: `internal/store/schema.go`
- Modify: `internal/store/migration_test.go`
- Modify: `internal/store/mutations.go`
- Modify: `internal/store/queries.go`
- Modify: `internal/model/state.go`
- Modify: `internal/model/decision.go`

**Interfaces:**
- Consumes: Task 2 `layout.Resolver` paths。
- Produces: `Workflow.LayoutVersion/WorkspacePath/WorkspaceBranch/CandidateWorkspaceHead/VerifiedWorkspaceHead/WorkspaceDirtyFingerprint` 的权威 round-trip。

- [ ] **Step 1: 写 Store round-trip 失败测试**

```go
wf := model.Workflow{
    ID: "wf-1", LayoutVersion: 2, WorkspacePath: "/cflow/projects/p/wf-1/workspace",
    WorkspaceBranch: "cflow/wf-1/workspace", CandidateWorkspaceHead: "c2",
    VerifiedWorkspaceHead: "c1", WorkspaceDirtyFingerprint: "sha256:abc",
}
// Transact mutation, reopen, View, assert every field equals wf.
```

- [ ] **Step 2: 运行失败测试**

Run: `go test ./internal/store -run 'Test.*Workspace.*RoundTrip|TestFreshAndForwardMigration' -count=1`
Expected: FAIL，字段/迁移不存在。

- [ ] **Step 3: 添加不可变 Migration 005**

```sql
ALTER TABLE workflows ADD COLUMN layout_version INTEGER NOT NULL DEFAULT 1;
ALTER TABLE workflows ADD COLUMN workspace_path TEXT NOT NULL DEFAULT '';
ALTER TABLE workflows ADD COLUMN workspace_branch TEXT NOT NULL DEFAULT '';
ALTER TABLE workflows ADD COLUMN candidate_workspace_head TEXT NOT NULL DEFAULT '';
ALTER TABLE workflows ADD COLUMN verified_workspace_head TEXT NOT NULL DEFAULT '';
ALTER TABLE workflows ADD COLUMN workspace_dirty_fingerprint TEXT NOT NULL DEFAULT '';

CREATE TABLE layout_migrations (
    id TEXT PRIMARY KEY,
    workflow_id TEXT NOT NULL,
    status TEXT NOT NULL,
    manifest_path TEXT NOT NULL,
    manifest_sha256 TEXT NOT NULL,
    created_at TEXT NOT NULL,
    completed_at TEXT,
    FOREIGN KEY (workflow_id) REFERENCES workflows(id)
);
```

更新内嵌 Registry 的精确 SHA-256；旧迁移文件不可修改。

- [ ] **Step 4: 更新 Model/Mutation/Query**

将上述字段加入 `model.Workflow`、Snapshot Mutation、SQL INSERT/UPDATE/SELECT/Scan。`IntegrationBranch/IntegrationHead` 暂时保留为 Legacy 兼容读取字段，Task 6 完成后不再写入新 Workflow。

- [ ] **Step 5: 验证迁移与 round-trip**

Run: `go test ./internal/store ./internal/model -count=1`
Expected: PASS，Migration Registry 连续为 1–5，v004 数据迁移后默认 `layout_version=1`。

- [ ] **Step 6: 全量测试、Review 与提交**

Run: `go test ./... -count=1`
Commit: `git commit -m "feat: persist workflow workspace facts"`。

---

### Task 4: 创建唯一 Workflow Worktree

**Files:**
- Modify: `internal/model/effects.go`
- Modify: `internal/model/input.go`
- Modify: `internal/decision/planning.go`
- Modify: `internal/app/planning.go`
- Modify: `internal/app/effects.go`
- Modify: `internal/app/planning_test.go`
- Modify: `internal/gitflow/worktrees.go`
- Modify: `internal/recovery/recovery.go`
- Modify: `internal/recovery/recovery_test.go`

**Interfaces:**
- Consumes: `layout.Resolver.Workspace` 与 Task 3 Workspace fields。
- Produces: `WorkspaceWorktreeCreateIntent`、`WorkspaceWorktreeCreated`；Workflow 创建完成后已存在可写 `cflow/<wf>/workspace` Worktree。

- [ ] **Step 1: 写失败测试**

```go
func TestCreateWorkflowCreatesWritableWorkspaceAtBase(t *testing.T) {
    wf := fx.CreateWorkflow("native-discussion")
    root := fx.Layout.Workspace(wf)
    fx.RequireWorktree(root, "cflow/"+string(wf)+"/workspace", fx.BaseHead)
    if err := os.WriteFile(filepath.Join(root, "probe.txt"), []byte("candidate\n"), 0o600); err != nil { t.Fatal(err) }
    fx.RequireTargetUnchanged()
}
```

- [ ] **Step 2: 运行失败测试**

Run: `go test ./internal/app -run TestCreateWorkflowCreatesWritableWorkspaceAtBase -count=1`
Expected: FAIL；当前只创建 detached planning snapshot。

- [ ] **Step 3: 替换 Planning/Integration 创建效果**

定义：

```go
type WorkspaceWorktreeCreateIntent struct { Workflow WorkflowID; BaseHead, Branch, Path string }
const WorkspaceWorktreeCreated EffectResultKind = "workspace-worktree-created"
```

新 Workflow 创建时只创建一个 Branch/Worktree。Artifact/Plan 发现均以 Workspace 为 cwd。新 Workflow 不创建 `planning/` 或 `integration/`。

`Application.artifactStore` 对 Layout Version 2 使用 `artifact.NewWorkflow(a.layout.WorkflowRoot(wf), wf, a.redaction)`；Legacy Layout 继续使用旧 Store Reader，直到显式迁移。

- [ ] **Step 4: 添加 Recovery 检查**

Recovery 必须同时核对 SQLite path、Canonical Path、Git Registry、Branch 与 HEAD。Intent 已提交但目录部分存在时只能识别精确完成或 Block。

- [ ] **Step 5: 验证**

Run: `go test ./internal/app ./internal/gitflow ./internal/recovery -run 'Workspace|Planning|CreateWorkflow' -count=1`
Expected: PASS；原 Target Worktree 无变化。

- [ ] **Step 6: 全量测试、Review 与提交**

Commit: `git commit -m "feat: create the workflow workspace at startup"`。

---

### Task 5: 冻结候选 Change Set

**Files:**
- Modify: `internal/model/artifacts.go`
- Create: `internal/app/changeset.go`
- Create: `internal/app/changeset_test.go`
- Modify: `internal/app/commands.go`
- Modify: `internal/app/application.go`

**Interfaces:**
- Consumes: Workspace Git facts。
- Produces: `FreezeDiscussionCommand`、`ChangeSetView`、`ArtifactChangeSet`，包含 Base/Heads/Commit Range/Diff/Untracked/Fingerprint/Session/Hash。

- [ ] **Step 1: 写失败测试**

```go
out, err := a.Execute(ctx, app.FreezeDiscussionCommand{Workflow: wf, Session: sessionID})
if err != nil { t.Fatal(err) }
ref := requireArtifact(t, out, model.ArtifactChangeSet)
body := getArtifact(t, ref)
requireJSONFields(t, body, "base_commit", "candidate_head", "verified_head",
    "tracked_diff", "untracked", "dirty_fingerprint", "session_id", "content_hash")
```

- [ ] **Step 2: 运行失败测试**

Run: `go test ./internal/app -run TestFreezeDiscussionCapturesCompleteChangeSet -count=1`
Expected: FAIL。

- [ ] **Step 3: 实现不可变 Artifact**

```go
const ArtifactChangeSet ArtifactType = "change-set"
type FreezeDiscussionCommand struct { Workflow model.WorkflowID; Session model.SessionID }
type ChangeSetView struct { Ref model.ArtifactRef; Base, Candidate, Verified, Fingerprint string; Dirty bool }
```

GitFlow 观察必须覆盖 staged/unstaged/delete/rename/binary/untracked path+mode+size+hash；内容经过现有安全与脱敏边界。

Change Set 由 Runtime 从 Git 事实生成，不进入 Agent-authored `bodySchema`；其结构通过 Go 类型、Canonical JSON 与 Artifact Hash 固定。

- [ ] **Step 4: 添加 Revision 行为测试**

继续讨论并再次 Freeze 必须产生 revision 2；revision 1 内容与 Hash 不变。

- [ ] **Step 5: 验证与提交**

Run: `go test ./internal/app ./internal/artifact ./internal/gitflow -run ChangeSet -count=1`，随后 `go test ./... -count=1`。
Commit: `git commit -m "feat: freeze discussion change sets"`。

---

### Task 6: 增加 Workspace Adoption Gate

**Files:**
- Create: `internal/app/adoption.go`
- Create: `internal/app/adoption_test.go`
- Modify: `internal/model/decision.go`
- Modify: `internal/model/fault.go`
- Modify: `internal/model/effects.go`
- Modify: `internal/decision/verify.go`
- Modify: `internal/app/gates.go`
- Modify: `internal/app/execution.go`

**Interfaces:**
- Consumes: Execution Approval 绑定的 Change Set。
- Produces: `AdoptWorkspaceCommand` 与 verified workspace advancement；未通过前 Scheduler 不得创建普通 Task。

- [ ] **Step 1: 写失败测试**

```go
approveExecution(t, fx, wf, changeSetHash)
_, err := fx.App.Execute(ctx, app.DispatchCommand{Workflow: wf})
requireFaultCode(t, err, model.CodeWorkspaceAdoptionRequired)
requireNoTaskWorktrees(t, fx.Layout, wf)
```

- [ ] **Step 2: 运行失败测试**

Run: `go test ./internal/app -run TestDispatchWaitsForWorkspaceAdoption -count=1`
Expected: FAIL，当前 Execution Approval 直接打开调度。

- [ ] **Step 3: 实现 Adoption 状态与门禁**

Execution Approval 增加 `ChangeSetHash`。Adoption 依次重验 Change Set、Commit Policy、Identity/Signing、Clean/Scope、Catalog Verification 与独立 Review；只有全部通过才写入：

```go
WorkspaceMutation{CandidateHead: head, VerifiedHead: head, DirtyFingerprint: cleanFingerprint}
```

- [ ] **Step 4: 覆盖候选 Commit 与 Dirty 修改**

测试原生 Session 已提交、未提交、越 Scope、Review Reject 和 Approval 后漂移五种情况；失败保留 Workspace 且 Target 不变。

- [ ] **Step 5: 验证与提交**

Run: `go test ./internal/app ./internal/decision -run 'Adopt|Workspace' -count=1`，再跑全量。
Commit: `git commit -m "feat: gate workspace adoption before dispatch"`。

---

### Task 7: 将并行 Task 合并回唯一 Workspace

**Files:**
- Modify: `internal/app/dispatch.go`
- Modify: `internal/app/gates.go`
- Modify: `internal/app/effects.go`
- Modify: `internal/app/runtime_test.go`
- Modify: `internal/app/finalize_test.go`
- Modify: `internal/gitflow/merge.go`
- Modify: `internal/recovery/recovery.go`

**Interfaces:**
- Consumes: `verified_workspace_head` 与 `layout.Resolver.Task`。
- Produces: Task 从 verified head 分支；`WorkspaceMergeIntent{ExpectedWorkspaceHead, TaskHead}` 串行合并并推进同一 Workspace。

- [ ] **Step 1: 写并行兄弟失败测试**

```go
startParallelTasks(t, fx, "S01", "S02")
finishAndReview(t, fx, "S01")
finishAndReview(t, fx, "S02")
driveUntilComplete(t, fx)
requireWorkspaceContains(t, fx, "S01", "S02")
requireNoIntegrationWorktree(t, fx)
```

- [ ] **Step 2: 运行失败测试**

Run: `go test ./internal/app -run TestParallelTasksMergeSeriallyIntoWorkspace -count=1`
Expected: FAIL，路径仍指向 `worktrees/.../integration`。

- [ ] **Step 3: 切换路径和 Merge Intent**

```go
type WorkspaceMergeIntent struct {
    Workflow model.WorkflowID
    Node model.NodeID
    ExpectedWorkspaceHead string
    TaskHead string
}
```

兄弟 Task 可以共享旧 Base，但 Merge Intent 必须在调度合并时固定最新 Workspace Head；禁止自动 Rebase。

- [ ] **Step 4: 更新 Final Verify/Report/Recovery**

所有 Integration cwd/head/branch 读取改为 Workspace verified facts；Legacy Workflow 仍走旧只读投影直到 Task 8 迁移。

- [ ] **Step 5: 验证与提交**

Run: `go test ./internal/app ./internal/gitflow ./internal/recovery -run 'Parallel|WorkspaceMerge|Final' -count=1`，再跑全量。
Commit: `git commit -m "feat: merge verified tasks into the workspace"`。

---

### Task 8: 实现显式 Legacy Layout Migration

**Files:**
- Create: `internal/layout/migrate.go`
- Create: `internal/layout/migrate_test.go`
- Modify: `internal/app/commands.go`
- Modify: `internal/app/projections.go`
- Modify: `internal/recovery/recovery.go`
- Modify: `internal/gitflow/worktrees.go`
- Modify: `internal/store/effects.go`

**Interfaces:**
- Consumes: 旧 Artifact/Worktree 路径与 Layout Version 1。
- Produces: `LayoutMigrationPreviewQuery`、`PrepareLayoutMigrationCommand`、`ExecuteLayoutMigrationCommand`；新路径 Layout Version 2。

- [ ] **Step 1: 写失败测试**

构造 v004 DB、旧 `projects/<key>/workflows/<wf>` Artifact 与旧 planning/integration/task Worktree，断言只读 Preview 不移动任何文件。

- [ ] **Step 2: 运行失败测试**

Run: `go test ./internal/layout -run TestLegacyPreviewIsReadOnly -count=1`
Expected: FAIL。

- [ ] **Step 3: 实现 Preview 与 Intent/Result**

```go
type MigrationPreview struct { Workflow model.WorkflowID; From, To []PathMove; ManifestHash string }
type PathMove struct { Kind, Source, Destination, Branch, Head string }
```

执行前备份 DB、获取排他锁、写 Intent；Git Worktree 用受管 `git worktree move`，Artifact 用安全路径移动；最后事务更新 DB path/layout version。

- [ ] **Step 4: 添加 Crash Window 测试**

覆盖 Intent 后、首个 move 后、全部 move 后、DB commit 后四个故障点；Recovery 只能继续、识别完成或 Block。

- [ ] **Step 5: 验证与提交**

Run: `go test ./internal/layout ./internal/recovery ./internal/store -run 'LayoutMigration|Legacy' -count=1`，再跑全量。
Commit: `git commit -m "feat: migrate legacy workflow layouts explicitly"`。

---

### Task 9: 引入 Bubble Tea 并切换默认入口

**Files:**
- Modify: `go.mod`
- Modify: `go.sum`
- Create: `internal/tui/app.go`
- Create: `internal/tui/app_test.go`
- Create: `internal/tui/keys.go`
- Modify: `internal/cli/root.go`
- Modify: `internal/cli/root_test.go`
- Modify: `cmd/cflow/main.go`

**Interfaces:**
- Consumes: `cli.Dependencies.OpenApplication`。
- Produces: `tui.Run(context.Context, Dependencies) error`；bare `cflow` 在 TTY 中启动 TUI，子命令行为不变。

- [ ] **Step 1: 写 root dispatch 失败测试**

```go
func TestBareRootRunsTUIWithoutMutatingWorkflow(t *testing.T) {
    deps := fakeDeps()
    deps.RunTUI = func(context.Context) error { called = true; return nil }
    cmd := NewRoot(deps); cmd.SetArgs(nil)
    if err := cmd.Execute(); err != nil { t.Fatal(err) }
    if !called { t.Fatal("TUI not called") }
    requireNoMutations(t, deps)
}
```

- [ ] **Step 2: 运行失败测试**

Run: `go test ./internal/cli -run TestBareRootRunsTUIWithoutMutatingWorkflow -count=1`
Expected: FAIL，当前 bare root 只显示 help。

- [ ] **Step 3: 添加固定依赖与最小 Model**

Run: `go get charm.land/bubbletea/v2@v2.0.6`。

```go
type Model struct { width, height int; ready bool; err error }
func (m Model) Init() tea.Cmd { return nil }
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) { /* resize, quit, load */ }
func (m Model) View() tea.View { v := tea.NewView(render(m)); v.AltScreen = true; return v }
```

- [ ] **Step 4: 保留 Headless CLI**

只有 `len(args)==0` 且 stdin/stdout 是 TTY 时进入 TUI；`cflow status` 等完全复用旧命令。无 TTY bare root 返回稳定诊断，不启动 mutation。

- [ ] **Step 5: 验证与提交**

Run: `go test ./internal/tui ./internal/cli ./cmd/cflow -count=1`，再跑全量与 `go mod tidy` 后 diff。
Commit: `git commit -m "feat: launch the TUI from the bare command"`。

---

### Task 10: 实现只读项目工作台与 ViewModel

**Files:**
- Create: `internal/tui/viewmodel.go`
- Create: `internal/tui/viewmodel_test.go`
- Create: `internal/tui/workspace.go`
- Create: `internal/tui/workspace_test.go`
- Modify: `internal/app/commands.go`
- Modify: `internal/app/projections.go`

**Interfaces:**
- Consumes: `ProjectWorkspaceQuery`、现有 List/Status/Inspect/Logs Views。
- Produces: `WorkspaceView`，一次返回 Project、Workflow summaries、selected workflow lifecycle、provider/git health 和 legal actions。

- [ ] **Step 1: 写失败 ViewModel 测试**

```go
vm := tui.MapWorkspace(app.WorkspaceView{Workflows: []app.WorkflowSummary{{ID:"wf-1", Runtime:model.RuntimePaused}}})
if vm.Selected.ID != "wf-1" || vm.Selected.Action != tui.ActionResume { t.Fatalf("%+v", vm) }
```

- [ ] **Step 2: 运行失败测试**

Run: `go test ./internal/tui -run TestMapWorkspace -count=1`
Expected: FAIL。

- [ ] **Step 3: 添加聚合只读 Projection**

`ProjectWorkspaceQuery` 必须在一个有界 View 中返回渲染所需事实；TUI 不在每次 View 中发多个互相不一致的 Query。

- [ ] **Step 4: 实现三栏与窄屏布局**

宽屏显示 Workflow/Lifecycle、Main Page、Inspector；窄屏将 Inspector 变为详情页。导航只更新 UI Selection，不调用 Execute。

- [ ] **Step 5: Golden/目标/全量测试与提交**

Run: `go test ./internal/tui -run 'Workspace|Narrow|Navigation' -count=1`，再跑全量。
Commit: `git commit -m "feat: render the project workflow workspace"`。

---

### Task 11: 增加受管原生终端进程能力

**Files:**
- Modify: `internal/process/supervisor.go`
- Modify: `internal/process/os_adapter.go`
- Modify: `internal/process/fake_adapter.go`
- Modify: `internal/process/supervisor_test.go`
- Modify: `internal/agent/adapter.go`
- Modify: `internal/agent/fake/adapter.go`
- Modify: `internal/agent/codex/adapter.go`
- Modify: `internal/agent/claude/adapter.go`
- Add tests to: `internal/agent/codex/adapter_test.go`, `internal/agent/claude/adapter_test.go`

**Interfaces:**
- Consumes: 已验证 Provider Installation 与 Provider Session ID。
- Produces: `process.InteractiveSpec`、`Supervisor.StartInteractive`、可选扩展接口 `agent.InteractiveAdapter` 与 `agent.InteractiveResumeSpec`；完整继承当前 TTY，但仍记录 Process Identity 并支持 Stop/Wait/Inspect。

- [ ] **Step 1: 写 Fake Supervisor 失败测试**

```go
spec := process.InteractiveSpec{Executable:"/bin/provider", Args:[]string{"resume","sess-1"}, Dir:"/workspace", Terminal:fakeTTY}
h, err := sup.StartInteractive(ctx, spec)
if err != nil { t.Fatal(err) }
requireProcessIdentity(t, sup, h)
```

- [ ] **Step 2: 运行失败测试**

Run: `go test ./internal/process -run TestInteractiveProcessRemainsSupervised -count=1`
Expected: FAIL。

- [ ] **Step 3: 添加独立 Interactive seam**

```go
type Terminal struct { In io.Reader; Out, Err io.Writer }
type InteractiveSpec struct { Executable string; Args []string; Dir string; Env map[string]string; Terminal Terminal }
type InteractiveHandle struct { Handle Handle; Identity ProcessIdentity }
```

Interactive path 不经过 frame parser，但仍由 OS Adapter 建立独立 process group，并由 Supervisor Wait/Signal/Inspect 管理。

- [ ] **Step 4: Provider 生成精确 argv**

保留现有 `agent.Adapter` 五方法稳定接口，另定义：

```go
type InteractiveAdapter interface {
    InteractiveResume(context.Context, ProviderSessionID, string) (InteractiveResumeSpec, error)
}
```

Codex/Claude/Fake Adapter 实现该可选接口。只在 Registry 的 `NativeInteractiveResume` 为 true 时返回；测试 exact argv、cwd、无 bypass flag。

- [ ] **Step 5: 验证与提交**

Run: `go test ./internal/process ./internal/agent/codex ./internal/agent/claude -run Interactive -count=1`，再跑全量。
Commit: `git commit -m "feat: supervise native provider terminals"`。

---

### Task 12: 实现 Native Session Bridge 与 Discussion Handoff

**Files:**
- Create: `internal/native/bridge.go`
- Create: `internal/native/bridge_test.go`
- Modify: `internal/app/commands.go`
- Create: `internal/app/discussion_native.go`
- Create: `internal/app/discussion_native_test.go`
- Create: `internal/tui/pages_discussion.go`
- Modify: `internal/model/artifacts.go`
- Modify: `internal/artifact/schema.go`
- Create: `schemas/discussion-handoff.json`
- Modify: `internal/model/runtime.go`
- Modify: `internal/model/model_test.go`
- Modify: `internal/agent/runtime.go`
- Modify: `internal/agent/runtime_test.go`
- Modify: `internal/decision/planning.go`
- Modify: `internal/decision/kernel_test.go`

**Interfaces:**
- Consumes: Task 11 interactive specs、Task 5 Change Set。
- Produces: `PrepareNativeDiscussionCommand`、`DiscussionReturnQuery`、`FinishDiscussionCommand`、`native.Bridge.Run` 与非终态 `SessionInteractiveIdle`。

- [ ] **Step 1: 写 Bridge round-trip 失败测试**

```go
result, err := bridge.Run(ctx, native.Request{Workflow:wf, Session:session, Provider:"codex", Worktree:layout.Workspace(wf), Terminal:tty})
if err != nil { t.Fatal(err) }
if result.Session != session || !result.Reconciled { t.Fatalf("%+v", result) }
```

- [ ] **Step 2: 运行失败测试**

Run: `go test ./internal/native -run TestBridgeRestoresAndReconciles -count=1`
Expected: FAIL。

- [ ] **Step 3: 建立可恢复的交互 Session 状态**

```go
const SessionInteractiveIdle SessionStatus = "INTERACTIVE_IDLE"
```

它表示 Provider 回合已结束、当前无进程、但同一 Session 允许精确 Resume。合法转换至少包括 `STARTING → INTERACTIVE_IDLE → ACTIVE → INTERACTIVE_IDLE`，以及 Finish 后 `INTERACTIVE_IDLE → COMPLETED`；Completed/Failed/Cancelled/Lost 继续不可 Resume。更新状态矩阵与 Agent Runtime 测试，不得放宽其他 Purpose 的终态规则。

- [ ] **Step 4: 实现 Bootstrap → Native → Return**

Prepare 命令建立精确 Session ID；TUI 使用 Bubble Tea blocking Exec 暂停 renderer，并在该回调内调用 Bridge 的 `Run`；Bridge 调用 `StartInteractive`、等待退出并返回结果，Bubble Tea 随后恢复 renderer；Application 再重验 Session/Binding/Workspace facts。`internal/native` 不导入 `internal/tui` 或 Bubble Tea。

- [ ] **Step 5: 实现 Return Page 与 Finish**

Return Page 只提供 Continue Same Session、Finish、Switch Agent、Pause、Cancel。Finish 先冻结 Change Set，再通过同一 Session 的结构化 Resume 生成 `ArtifactDiscussionHandoff`；`discussion-handoff.json` 严格要求目标、约束、非目标、验收标准、未决项、Change Set Ref 与用户决策。Resume 前后 Workspace 漂移则拒绝 Plan 输出。

- [ ] **Step 6: 覆盖失败路径**

测试非零退出、Session 冲突、Provider Binding 漂移、切换 Provider Context Bundle 和意外 Plan Mutation。

- [ ] **Step 7: 验证与提交**

Run: `go test ./internal/native ./internal/app ./internal/tui -run 'Native|Discussion|Handoff' -count=1`，再跑全量。
Commit: `git commit -m "feat: run native requirement discussions"`。

---

### Task 13: 实现 Foreground Runner

**Files:**
- Create: `internal/foreground/runner.go`
- Create: `internal/foreground/runner_test.go`
- Modify: `internal/app/commands.go`
- Create: `internal/app/drive.go`
- Create: `internal/app/drive_test.go`

**Interfaces:**
- Consumes: `app.Driver`：`DriveOnce(context.Context, WorkflowID) (DriveOutcome, error)`。
- Produces: `foreground.Runner.Run`、类型化 committed event channel 和明确 Stop Reason。

- [ ] **Step 1: 写失败测试**

```go
runner := foreground.Runner{Driver: fakeDriver{outcomes: []app.DriveOutcome{{Kind:app.DriveProgressed},{Kind:app.DriveTerminal}}}}
result, err := runner.Run(ctx, "wf-1")
if err != nil || result.Reason != foreground.StopTerminal { t.Fatalf("%+v %v", result, err) }
```

- [ ] **Step 2: 运行失败测试**

Run: `go test ./internal/foreground -run TestRunnerDrivesUntilTerminal -count=1`
Expected: FAIL。

- [ ] **Step 3: 实现有界接口**

```go
type DriveKind uint8
const (DriveProgressed DriveKind=iota; DriveWaiting; DriveNeedsUser; DriveTerminal; DriveNoSafeProgress)
type DriveOutcome struct { Kind DriveKind; Outcome Outcome; Wait <-chan struct{} }
type Driver interface { DriveOnce(context.Context, model.WorkflowID) (DriveOutcome, error) }
```

`DriveOnce` 封装 Recovery、Adoption、Dispatch、Reconcile、Final Completion 的一次安全推进；Kernel 仍验证每个 Command。

- [ ] **Step 4: 防止空转和后台失联**

Waiting 必须等待进程/定时事件或 Context；NoSafeProgress 产生 Finding 并停止；Context Cancel 走受控 Pause，不留下后台 Run。

- [ ] **Step 5: 验证与提交**

Run: `go test ./internal/foreground ./internal/app -run 'Runner|DriveOnce|NoSafeProgress' -count=1`，再跑全量。
Commit: `git commit -m "feat: drive workflows in the foreground"`。

---

### Task 14: 完成 Approval、Execution、Blocked 与受控停止页面

**Files:**
- Create: `internal/tui/pages_approval.go`
- Create: `internal/tui/pages_execution.go`
- Create: `internal/tui/pages_terminal.go`
- Modify: `internal/tui/app.go`
- Modify: `internal/tui/workspace.go`
- Modify: `internal/tui/app_test.go`
- Modify: `internal/cli/stop_test.go`

**Interfaces:**
- Consumes: Plan/Execution Preview、Runner events、Fault/Legal Actions、Native Bridge。
- Produces: 完整 TUI 主链路与 `Pause and Exit`。

- [ ] **Step 1: 写 Approval 默认否测试**

```go
m := approvalModel(executionPreview)
m, _ = update(m, keyEnter)
if m.confirmed { t.Fatal("enter must not approve without selecting yes") }
```

- [ ] **Step 2: 写 Execution 只在关键事件打断测试**

普通 Task event 更新 DAG/Inspector；`DriveNeedsUser` 才切换到 Decision Panel。

- [ ] **Step 3: 实现页面**

Approval 页面提供 Plan/Spec/DAG/Diff tabs 和固定 Hash/Scope/Route/Budget/Git Policy Inspector。Execution 页面展示 DAG、Task、Agent、Cost、Log。Blocked 页面只渲染 Runtime Legal Actions。Terminal 页面覆盖 Final Report、Apply Preview/Execute 和 Cleanup Dry Run/Manifest Confirmation；所有确认默认否。

- [ ] **Step 4: 实现 Ctrl+C 与退出**

第一次 Ctrl+C 调用受控 Pause；第二次才 Force Stop。活动 Runner 上按 q 显示 `Pause and Exit`，不得直接退出留下进程。

- [ ] **Step 5: 目标/Golden/全量测试与提交**

Run: `go test ./internal/tui ./internal/cli -run 'Approval|Execution|Blocked|ControlledStop|Quit' -count=1`，再跑全量。
Commit: `git commit -m "feat: complete the TUI workflow controls"`。

---

### Task 15: 修正 Workspace-aware Apply 与聚合 Cleanup

**Files:**
- Modify: `internal/app/apply.go`
- Modify: `internal/app/apply_prepare.go`
- Modify: `internal/app/apply_test.go`
- Modify: `internal/gitflow/merge.go`
- Modify: `internal/gitflow/gitflow.go`
- Modify: `internal/app/cleanup.go`
- Modify: `internal/app/cleanup_test.go`

**Interfaces:**
- Consumes: verified Workspace Head、`layout.Resolver.Apply/Workspace/Task`。
- Produces: 原始 Working Tree HEAD/Index/files 同步的 `FastForwardWorkingTree`；Cleanup Manifest 精确覆盖聚合代码目录。

- [ ] **Step 1: 写 Apply 失败测试**

```go
executeApply(t, fx)
requireGitHead(t, fx.OriginalRoot, fx.ReviewedApplyHead)
requireGitClean(t, fx.OriginalRoot)
requireFileContent(t, filepath.Join(fx.OriginalRoot, "feature.txt"), "verified\n")
```

Expected current behavior: FAIL，因为 `update-ref` 不能保证 Index/文件同步。

- [ ] **Step 2: 实现 workspace-aware fast-forward**

GitFlow 新增：

```go
type FastForwardWorkingTree struct { Root, Branch, Expected, New string }
type FastForwardWorkingTreeResult struct { Head string; Clean bool }
```

在原始 Root 重验 clean/branch/expected head，执行 argv `git -C <root> merge --ff-only <new>`，然后重验 HEAD、Index、Worktree。禁止 reset/force/stash/checkout。

- [ ] **Step 3: 切换 Apply staging 源和路径**

Apply 从 verified Workspace Head 合并；暂存路径为 `<workflow>/tmp/apply-N`。

- [ ] **Step 4: 更新 Cleanup**

Dry Run Manifest 精确列出 `<workflow>/workspace`、`tmp/tasks/*`、`tmp/apply-*`；Artifact/Discussion/Plan/Spec/Review/Evidence/Report/DB/Refs 不进入删除集合。

- [ ] **Step 5: 验证失败与恢复**

覆盖 Target Drift、命令后状态不一致、dirty workspace、部分 Cleanup 失败和 crash recovery。

- [ ] **Step 6: 全量测试、Review 与提交**

Run: `go test ./internal/app ./internal/gitflow -run 'Apply|FastForwardWorkingTree|Cleanup' -count=1`，再跑全量。
Commit: `git commit -m "fix: deliver and clean aggregated workspaces safely"`。

---

### Task 16: 建立 Fake TUI E2E Gate 与发布证据

**Files:**
- Create: `scripts/gate-tui.sh`
- Create: `internal/tui/e2e_test.go`
- Modify: `scripts/gate3.sh`
- Modify: `docs/cflow-demo-acceptance-report.md`
- Modify: `README.md`
- Modify: `README-zh.md`

**Interfaces:**
- Consumes: Tasks 1–15 完整主链路。
- Produces: 确定性 Fake Provider TUI Gate、历史 Gate 3 与新候选证据清晰分离。

- [ ] **Step 1: 写失败 E2E**

测试使用 Fake Terminal 输入：创建 Workflow → Native Discussion Fake → Finish → Plan Approval → Execution Approval → Runner → Report → Apply Prepare/Execute → Cleanup Dry Run/Execute。

- [ ] **Step 2: 运行失败测试**

Run: `go test ./internal/tui -run TestTUIPlanToApplyAndCleanup -count=1`
Expected: 在尚未补齐的集成问题上 FAIL；不得放宽断言。

- [ ] **Step 3: 完成 Gate 脚本**

```sh
#!/bin/sh
set -eu
go test ./internal/tui ./internal/foreground ./internal/native -count=1
go test ./... -count=1
go build -trimpath -o "$1/cflow" ./cmd/cflow
```

脚本还必须记录 Source Commit、Binary SHA-256、Go Version、测试日志与 Artifact 目录；不得调用真实 Provider。

- [ ] **Step 4: 更新验收报告和 README**

只有 Fake Gate、跨平台构建、全部新安全 Fixture 和精确 Source/Binary Evidence 通过后，文档才能称为新的 Internal Candidate。真实 Provider E2E 仍标记为需要单独批准。

- [ ] **Step 5: 运行确定性 Gate**

Run: `./scripts/gate-tui.sh <new-empty-artifact-dir>`
Expected: PASS，Artifact 绑定当前精确 Commit/Binary。

- [ ] **Step 6: Review 与提交**

Commit: `git commit -m "test: gate the complete TUI workflow"`。

---

## Spec Coverage Matrix

| 已确认设计要求 | 实施 Task |
|---|---|
| 权威文档从 line-oriented 切换到 TUI | 1 |
| 聚合 Workflow 目录、全局 DB、非生产 Artifact 外置 | 2、3、8 |
| 唯一长期 Workflow Worktree | 3、4 |
| Candidate/Verified Head、Change Set、Adoption | 3、5、6 |
| 并行临时 Task Worktree 串行合回 Workspace | 7 |
| 显式 Legacy Layout Migration | 3、8 |
| bare `cflow` 启动 TUI，Headless CLI 保留 | 9 |
| 自适应项目工作台、响应式 Inspector | 10、14 |
| 受管 Codex/Claude 原生 Terminal | 11、12 |
| Discussion Return、Finish 与结构化 Handoff | 5、12 |
| Foreground Runner 自动推进且不空转 | 13 |
| Approval、Execution、Blocked Attach、受控停止 | 6、12、14 |
| Workspace-aware Apply 更新原始 HEAD/Index/files | 15 |
| 显式 Cleanup 释放代码、保留 Artifact/Evidence | 14、15 |
| Fake TUI E2E、发布证据与真实 Provider 授权边界 | 16 |

---

## Final Program Gate

所有 Task 完成后，在新的候选 Commit 上执行：

```sh
go test ./... -count=1
./scripts/check-cross-build.sh
./scripts/gate-tui.sh <new-empty-artifact-dir>
git status --short
```

Expected:

- 全量测试、Cross Build 和 Fake TUI Gate 全部 PASS；
- 实现 Worktree Git-visible clean；
- Acceptance Report 的 Source Commit 与候选 HEAD 一致；
- 原始 Target Worktree 只在显式 Apply Execute 测试步骤改变；
- 不存在真实 Provider 调用、Push 或 PR。

真实 Codex/Claude Native + Headless E2E、Self-Dogfood 和远端发布需要用户再次明确授权，并在新的精确候选 Commit 上单独执行。
