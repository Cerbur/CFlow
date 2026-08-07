# CFlow

[English](README.md)

CFlow 是一个面向 Coding Agent 的 local-first 工作流 Runtime。它把一项编码需求变成经过批准的 Plan、受限执行图、隔离的编码任务、确定性验证、独立 Review，以及受保护的交付流程，同时不引入云端控制平面。

CFlow 不是 `codex` 或 `claude` 的薄封装。它负责可恢复的 Plan-to-Done 生命周期，并且只依据持久化证据推进状态，而不会因为 Agent 声称“已经完成”就判定成功。

> **项目状态：**行式 Demo 基础实现存在。具有完整证据的 Gate 3 Candidate 绑定到源码 Commit `68fd200`，详见[验收报告](docs/cflow-demo-acceptance-report.md)。2026-08-07 已确认 **TUI 工作流方向**：全屏 Bubble Tea TUI 成为默认主入口，需求讨论进入 Codex / Claude 原生 Terminal，采用聚合 Workflow 目录、唯一长期 Workspace、Foreground Runner、workspace-aware Apply 与显式 Cleanup——见 [TUI 设计](docs/superpowers/specs/2026-08-07-cflow-tui-workflow-design.md) 与 [其实施 Plan](docs/superpowers/plans/2026-08-07-cflow-tui-workflow-implementation-plan.md)。**该 TUI 尚未实现**；下方当前用法仍为旧的行式 Demo CLI，不应被表述为已确认的 TUI 工作流。

## 为什么需要 CFlow

- **Local-first：**Workflow 状态、Artifact、Session、日志、证据和 Worktree 都保存在本机。
- **审批驱动：**通过独立检查的 Plan 和精确的执行输入分别需要用户批准。
- **独立于 Agent 声明的证据：**Commit、干净 Worktree、Scope、测试和独立 Review 决定进度。
- **可恢复执行：**中断或失败后，根据 SQLite、不可变 Artifact、Git、进程与证据事实进行协调恢复。
- **隔离并行：**相互独立的任务在单独 Git Worktree 中执行，再串行合并到 CFlow 管理的 Integration Branch。
- **受保护交付：**Workflow 完成不会修改 Target Branch；`cflow apply` 会在隔离环境中暂存、重新验收，并通过 Compare-and-Swap 快进交付。
- **有界自治：**Retry、预算、Provider 路由、命令和 Workflow Patch 都受到限制并可审计。

## 生命周期

```text
需求讨论
  -> Draft Plan
  -> 独立 Plan Check
  -> 用户批准 Plan
  -> Specs + Verification Catalog
  -> 受限 Dynamic Workflow
  -> 用户批准执行
  -> 隔离的 Task Worktrees
  -> 确定性验证 + 独立 Review
  -> 串行合并到 Integration Branch
  -> Final Verification + 报告
  -> Workflow Completed
  -> 用户显式执行受保护 Apply
```

所有权威状态转换都由 Runtime 决定，而不是 Planner、Implementer 或 Reviewer。

## 架构

| 模块 | 职责 |
|---|---|
| Application | Command/Query 入口、锁、事务和类型化 Effect Loop |
| Decision Kernel | 纯生命周期、Retry、Approval 与安全决策 |
| State Store | SQLite 运行状态与权威 Event 序列 |
| Artifact Store | 不可变、版本化、Hash 标识的工作流 Artifact |
| Agent Runtime | Fake、Codex、Claude 协议 Adapter 与 Session Lineage |
| Compiler + Scheduler | 受限 DAG 编译与确定性调度就绪判断 |
| GitFlow | 仓库事实、Worktree、Commit Gate、Merge、Quarantine 与 Apply |
| Verification Engine | 已批准命令目录、确定性检查与证据 |
| Recovery Engine | 协调数据库、Artifact、Git、进程和证据事实 |

所有外部进程都应使用 executable + argv 的调用方式。Dynamic Workflow 不能包含任意 Shell、Python 或 TypeScript 代码。

## 环境要求

- Go 1.26.5
- Git
- macOS 或 Linux；Demo 在 Windows 上通过 WSL 支持
- 使用真实 Provider 时，需要安装并登录 Codex CLI 与 Claude Code
- 目标必须是具有有效 Commit、且 HEAD 附着于本地 Branch 的 Git 仓库

目标仓库需要配置 Git Author/Committer Identity。若启用了 Commit Signing，签名必须能在配置的超时时间内以非交互方式完成。

## 从源码构建

```sh
CGO_ENABLED=0 go build -trimpath -o cflow ./cmd/cflow
./cflow version
./cflow doctor
```

Release 风格构建还会写入源码 Commit 与内嵌 Registry Hash。详见 [`scripts/check-cross-build.sh`](scripts/check-cross-build.sh) 和[验收报告](docs/cflow-demo-acceptance-report.md)。

## 当前 Demo 用法

> 2026-08-07 已确认方向将用全屏 TUI 与原生需求讨论取代本行式入口；**该 TUI 尚未实现**。在 TUI 任务完成前，界面仍为下面的行式、命令驱动 Demo，直接执行 `cflow` 仍显示命令树。

当前界面是行式、命令驱动的。直接执行 `cflow` 会显示命令树；PRD 中描述的完整引导式交互生命周期尚未作为一条连续循环提供。

Post-candidate CLI 当前通过 `CFLOW_PROVIDERS` 注册真实 Provider；未设置时默认仍是确定性 Fake Adapter。

```sh
export CFLOW_PROVIDERS=codex,claude

./cflow workflow-create my-change --provider codex
printf '%s\n' '描述需要完成的修改及其约束。' | \
  ./cflow discuss --provider codex
./cflow plan-generate --provider codex
./cflow plan-check --provider claude
./cflow plan-show
./cflow plan-approve
./cflow spec-generate --provider codex
./cflow compile-workflow --provider claude
./cflow execution-dry-run
./cflow execution-show
./cflow execution-approve
```

Approval 命令需要交互确认，并且默认选择“否”。Provider 执行可能访问网络并产生模型费用；批准前应检查精确 Route、预算、命令、Git Identity/Signing 事实和权限边界。

当前 Demo CLI 仍属于工程候选：`doctor` 中部分有状态检查仍会显示 `NOT_YET_AVAILABLE`，并且当前 post-candidate Provider wiring 尚未获得更新后的 Gate 3 证据。已确认的 2026-08-07 TUI 工作流任务将在此基础上加入 Bubble Tea TUI、原生 Codex/Claude 需求讨论、Foreground Runner、聚合 Workspace、workspace-aware Apply 与显式 Cleanup。

## 命令概览

| 领域 | 命令 |
|---|---|
| 读取与诊断 | `list`、`status`、`inspect`、`inspect task`、`logs`、`report`、`doctor`、`version` |
| Plan 生命周期 | `workflow-create`、`discuss`、`plan-generate`、`plan-check`、`plan-show`、`plan-approve` |
| 执行定义 | `spec-generate`、`compile-workflow`、`execution-dry-run`、`execution-show`、`execution-approve` |
| Runtime 控制 | `retry`、`pause`、`resume`、`cancel`、`policy-confirm` |
| 恢复 | `replacement-preview`、`replacement-approve` |
| 交付 | `apply`、`apply --confirm`、`apply --execute` |
| 清理 | `cleanup`、`cleanup --execute <manifest-id>` |

使用 `cflow <command> --help` 查看精确参数。如果没有传入 Workflow ID，命令会使用当前项目中适用的 Workflow 投影。

## 信任边界

CFlow 提供严格的工作流与仓库证据门禁，但它不是 OS Sandbox。

- Provider CLI 使用用户现有 Provider 配置与默认权限运行。
- Provider 和已批准的项目工具可能访问网络。
- CFlow 不添加 Danger/Bypass Flag，不复制凭据，也不承诺 Agent 无法读取 Worktree 之外的文件。
- CFlow 管理的敏感路径按 Owner-only 权限设计；持久化的 Provider/Tool 内容会经过统一脱敏流程。
- CFlow 不对本地证据提供应用层加密，操作系统备份可能包含这些数据。
- Cancel 会保留 Artifact、Session、Commit、Worktree 与证据；Cleanup 是独立的精确目标操作，并且不会删除审计历史。

## 验收与发布证据

仓库包含确定性 Gate 脚本和需要显式开启的真实 Provider 测试：

```sh
./scripts/gate1.sh <artifact-dir>
./scripts/gate2.sh <artifact-dir>
./scripts/gate3.sh <artifact-dir>
```

Gate 1 和 Gate 2 只产生内部 Candidate。Gate 3 可以产生 Demo Complete Candidate，但不会自动成为 Release。真实 Cross-Provider E2E 与 Self-Dogfood 需要单独授权，因为它们会调用 Provider CLI、可能产生费用，并且 Dogfood 流程会执行受保护 Apply。

历史证据只证明其绑定的精确二进制与源码 Commit，不会自动覆盖后续 Commit。

## 文档

- [产品需求文档](docs/cflow-prd.md)
- [技术设计](docs/cflow-demo-design.md)
- [实现 Plan](docs/cflow-demo-implementation-plan.md)
- [TUI 工作流设计（2026-08-07 已确认）](docs/superpowers/specs/2026-08-07-cflow-tui-workflow-design.md)
- [TUI 工作流实施 Plan](docs/superpowers/plans/2026-08-07-cflow-tui-workflow-implementation-plan.md)
- [Gate 3 验收报告](docs/cflow-demo-acceptance-report.md)
- [Local-first 边界](docs/cflow-local-first.md)
- [领域语言](CONTEXT.md)

## Demo 非目标

不包含 Web UI、云端服务、任意 Workflow 脚本、自动 Push/PR、跨仓库 Workflow、OpenCode Adapter 或无限自主 Retry Loop。（Provider TUI Attach 原列于非目标；2026-08-07 已确认的 TUI 工作流方向已将原生 Codex/Claude 需求讨论作为主链路的一部分，详见对应的 TUI 设计文档。）

## License

[MIT](LICENSE)
