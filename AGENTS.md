# CFlow 项目协作指引

## 项目定位

CFlow 是一个 local-first 的 Coding Agent Workflow CLI，负责从 Plan 到 Done 的完整生命周期管理。它的核心职责包括工作流生命周期、Agent Session 持久化与恢复、Plan/Spec/Workflow 生成与验收、多 Agent 路由、Git Worktree 隔离、DAG 调度、失败恢复、独立验收和可审计证据；它不是普通的 Agent CLI Wrapper。

## 当前阶段

项目当前处于**Demo 实现阶段**。PRD v0.2、Demo 技术设计 v0.1 和实现 Plan v0.1 已获得用户明确确认；用户选择在独立 Claude Code Session 中采用 Subagent-Driven 模式执行。实现必须严格遵循 `docs/cflow-demo-implementation-plan.md` 的任务顺序和 Gate：

- 每个任务使用新的 Implementer 上下文，完成后必须经过独立的规格符合性和代码质量审查。
- Critical/Important 审查问题修复并复审通过前，不得进入下一任务。
- 每个任务必须有目标测试、全量测试、Git Commit 和 Git-visible Clean 证据。
- Gate 1 和 Gate 2 只能标记为 Internal Candidate；不得提前宣称 Demo 完成。
- 真实 Provider E2E 和 Self-Dogfood 仍需执行时的单独明确批准。
- 不实现完整 TUI、OpenCode P1、复杂插件系统或其他已列非目标。
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

严格按已批准实现 Plan 的 Task 1–22 与 Gate 1–3 执行。若实现发现产品不变量、深模块边界、审批模型或事实权威无法满足，必须停止并返回文档审查；不得为了让代码通过而静默削弱 PRD、设计或 Plan。
