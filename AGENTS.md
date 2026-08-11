# CFlow 项目协作指引

## 项目定位

CFlow 是一个 local-first 的 Coding Agent Workflow CLI，负责从 Plan 到 Done 的完整生命周期管理。它的核心职责包括工作流生命周期、Agent Session 持久化与恢复、Plan/Spec/Workflow 生成与验收、多 Agent 路由、Git Worktree 隔离、DAG 调度、失败恢复、独立验收和可审计证据；它不是普通的 Agent CLI Wrapper。

## 当前阶段

> **2026-08-07 已确认变更**：全屏 TUI 成为默认主入口；Native Discussion、聚合 Workflow 目录、唯一 Workspace、Foreground Runner、workspace-aware Apply 与显式 Cleanup 取代旧的 line-oriented Demo 交互决策。权威规格见 `docs/superpowers/specs/2026-08-07-cflow-tui-workflow-design.md`，任务顺序见 `docs/superpowers/plans/2026-08-07-cflow-tui-workflow-implementation-plan.md`。旧决策（"Demo 不使用全屏 TUI"等）标记为 `Superseded`，仅作历史背景；其安全不变量全部保留。

> **2026-08-12 已确认变更**：TUI 主入口采用 Home → Workflow Menu → 动态 Stage Workspace 层级；Enter 进入或确认，Esc 返回；q 不再退出；/ 打开 Global Command Palette，本期只支持 /exit。权威规格见 `docs/superpowers/specs/2026-08-12-cflow-tui-workspace-navigation-design.md`。2026-08-11 的视觉约束、Lip Gloss 响应式要求和安全不变量继续保留，但其“只做视觉刷新、不得改变页面层级和按键语义”的限制已 Superseded。

项目当前处于**TUI 工作流实现阶段**。此前已确认的 PRD v0.2、Demo 技术设计 v0.1 和实现 Plan v0.1（line-oriented Demo）构成历史基线；实现必须严格遵循 `docs/superpowers/plans/2026-08-07-cflow-tui-workflow-implementation-plan.md` 的 Task 1–16 顺序与如下 Gate：

- 每个 Task 使用新的 Implementer 上下文，完成后必须经过独立的规格符合性和代码质量审查。
- Critical/Important 审查问题修复并复审通过前，不得进入下一 Task。
- 每个 Task 必须有目标测试、全量测试、Git Commit 和 Git-visible Clean 证据。
- 外部命令一律 program + argv，禁止 `shell: true`、Force、Ignore、Best-effort、Danger/Bypass Flag。
- Bubble Tea TUI/Headless CLI 共用权威 Application/Runtime；TUI 不直接写 SQLite、Artifact、Git 或最终状态。
- 全局 `$CFLOW_HOME/cflow.db` 继续是权威状态库；Workflow-local `state/` 只是投影与恢复辅助证据。
- 真实 Codex/Claude E2E 和 Self-Dogfood 仍需执行时的单独明确批准。
- 新 Workflow 立即使用聚合目录；Legacy Layout 只读可识别，迁移必须显式执行。
- 原始 Target Branch 只在显式 Apply Execute 时改变；Apply 必须同步更新原始 Target Working Tree 的 HEAD、Index 与文件。
- Cleanup 必须显式 Dry Run 和确认。
- 不创建云端服务。
- 不自动 push、创建 PR 或修改远程仓库。

## 工作方式

1. 开始工作前先读取本文件和 `docs/cflow-prd.md`。
2. 先分析和讨论，再编写实现。
3. 初始 PRD 是可修改的产品构想和草案，不是不可变的最终规格。
4. 发现矛盾时必须明确提出，不得自行选择并隐藏假设。
5. 重大决策必须记录到 PRD 或独立设计文档；尚未确认的内容应明确标记为提案或待决。
6. 每次修改应控制范围，避免顺手实现无关功能。
7. 不得将 Agent 的自然语言声明视为验收通过。
8. 状态变化必须由 CFlow Runtime 根据证据判定，不得由 Agent 自行写入最终状态。
9. Dynamic Workflow 默认采用受限声明式 IR；不得让 Agent 直接生成任意 Shell、Python 或 TypeScript 执行脚本。
10. 所有外部命令未来应以 argv 数组调用，默认禁止 `shell: true`。
11. Coding Task 未来应在独立 Git Worktree 中执行。
12. Planner、Implementer 和 Reviewer 必须使用独立 Session。
13. 完成状态必须同时具备 Git Commit、测试结果和 Review 证据。
14. 所有重试必须有明确上限。
15. 恢复流程必须根据文件、Git、Worktree 和数据库事实重建状态，不能只信任单个状态字段。

## 文档优先级

发生冲突时按以下优先级处理：

```text
用户当前明确指令
> 已确认的 PRD
> 已确认的设计文档
> AGENTS.md
> 临时讨论结论
> Agent 自己的推断
```

尚未确认的内容不得伪装成正式决策。

## 当前阶段的交付门槛

严格按已批准 TUI 实现 Plan 的 Task 1–16 执行。若实现发现产品不变量、深模块边界、审批模型或事实权威无法满足，必须停止并返回文档审查；不得为了让代码通过而静默削弱设计或 Plan。旧的 line-oriented Demo 的 Task 1–22 / Gate 1–3 已作为历史证据保留，未被本阶段重新验收。
