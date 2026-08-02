---
project: CFlow
document_type: product-requirements
status: approved
version: 0.2
created: 2026-08-02
updated: 2026-08-02
approved: 2026-08-02
---

# CFlow 产品需求文档

| 项目 | 内容 |
|---|---|
| 产品名称 | CFlow |
| 产品形态 | Local-first Coding Agent Workflow CLI |
| PRD 版本 | Demo v0.2 Approved |
| 文档日期 | 2026-08-02 |
| 目标平台 | macOS、Linux；Windows 暂通过 WSL 支持 |
| 首期 Agent | Codex CLI、Claude Code；OpenCode Adapter 为 P1，不属于 Demo 完成门槛 |
| 已确认实现栈 | Go 1.26 + SQLite；无 CGO 的单二进制分发基线 |
| 核心定位 | 将需求讨论、Plan 验收、任务拆分、动态工作流生成、并行 Coding、独立验收和失败修复固化为可恢复的本地执行流程 |

## 产品定义与关键决策

### 产品背景

CFlow 希望把目前依赖人工 Prompt、手动启动多个 Coding Agent、手动维护 Plan 和手动分配任务的开发流程，固化成一个可恢复、可验收、可切换 Agent 的本地 CLI 工作流。

完整链路为：

```text
用户描述需求
    ↓
CFlow 调度 Agent 进行需求讨论
    ↓
生成 Draft Plan
    ↓
独立 Agent 检查 Plan
    ↓
Plan Checked
    ↓
用户批准精确 Plan Revision
    ↓
Agent 将 Plan 拆成可执行 Specs
    ↓
CFlow 编译安全 DAG，独立调度 Agent 提交受限优化补丁
    ↓
用户批准 Specs、Verification Catalog、Workflow、Agent Routing、Provider 默认权限信任边界和 Budget
    ↓
CFlow Runtime 创建 Worktree 并调度 Coding Agent
    ↓
确定性检查 + 独立 Agent 验收
    ↓
失败后有限重试或重新规划
    ↓
合并到 Integration Branch
    ↓
全量确定性验证 + 独立 Final Review
    ↓
生成最终报告，Workflow Completed
    ↓ 用户后续显式选择
受保护 cflow apply 到 Target Branch
```

CFlow 的价值不在于简单封装：

```bash
codex ...
claude ...
opencode ...
```

而在于提供四项现有 Coding Agent CLI 上层缺失的能力：

| 能力 | CFlow 负责的内容 |
|---|---|
| Plan 生命周期 | Draft、Check、Checked、修改历史、来源 Session |
| 可恢复执行 | Workflow 状态、Agent Session、任务运行、Worktree、提交记录 |
| 验收闭环 | 确定性命令、语义 Review、失败重试、独立验收 Session |
| 跨 Agent 路由 | 不同阶段选择不同 Agent、模型、预算和权限 |

Codex 已支持非交互 `codex exec`、JSONL 事件流和按 Session ID 恢复；Claude Code 支持 Session Resume、`stream-json`、JSON Schema 输出和预算上限；OpenCode 也支持 Session ID、JSON 事件输出、Session 导出和 Headless Server。这意味着 CFlow 可以通过薄适配层驱动现有 CLI，而不必直接对接各家模型 API。citeturn3view0turn3view2turn3view3turn3view4turn3view5turn3view6

现有 AWS Labs CLI Agent Orchestrator 已覆盖 tmux Session、多 Provider、Supervisor/Worker 和并行 Agent 调度；GitHub Agentic Workflows 则将自然语言 Markdown 编译为带安全约束的 GitHub Actions Workflow。因此，CFlow 不应以“另一个多 Agent 进程管理器”为核心卖点，而应聚焦于 **本地 Plan-to-Done 生命周期、恢复语义和 Coding 任务验收**。citeturn0search0turn8search0turn8search2turn8search3

### 产品目标

Demo 版本必须验证以下核心假设：

> 结构化 Plan、受限 Workflow、Worktree 隔离和自动验收，能否显著减少复杂 Coding Agent 任务中的人工上下文搬运与重复操作。

#### 已确认：Demo 核心用户

> 决策日期：2026-08-02
>
> 决策状态：已确认

Demo 的首要用户是：在自己的开发机器上维护一个现有 Git 仓库、熟悉基本 CLI/Git 操作、已经安装并登录 Codex CLI 与 Claude Code，并希望把可拆成多个任务的 Coding 需求交给受控多 Agent 流程执行的开发者或仓库维护者。

该用户愿意检查 Plan、Execution Dry Run 和最终证据，但不希望手工搬运 Agent 上下文、创建多个 Worktree、维护任务依赖、追踪 Session、判断 Retry 或汇总验收证据。Demo 不以不熟悉 Git 的终端新手、共享云端控制台管理员或需要多人实时协作的团队为首要用户；团队成员可以各自在本机使用 CFlow，但 Demo 不提供共享 Workflow、远程审批或集中式权限管理。

#### 已确认：Local-first 边界

> 决策日期：2026-08-02
>
> 决策状态：已确认

- CFlow Runtime、SQLite、Artifact、Session 索引、Log、Evidence、Worktree 和 Lock 都保存在用户本机；本机文件、Git 和进程事实构成恢复与审计依据，不依赖 CFlow 云端 Control Plane。
- CFlow 不创建云账户、不上传 Workflow 状态、不提供远程团队协作，也不自动 Push、Fetch、创建 PR 或修改远程 Git Ref。
- Local-first 不等于 Offline 或 OS Sandbox。Codex/Claude 等 Provider CLI、用户批准的 Verification Command 以及其现有配置可能访问网络；CFlow 必须展示该信任边界，但 Demo 不统一拦截网络。
- Provider 自身保存的远程模型请求或账户数据受 Provider 条款与用户配置约束，不属于 CFlow 可证明的本地持久化边界。CFlow 只保证自己管理的数据按本 PRD 的权限、脱敏和不可变规则落在本机。
- `CFLOW_HOME` 必须位于可证明 Owner-only 权限与可靠 Advisory Lock 的本地文件系统；网络盘或无法验证语义的同步目录不属于 Demo 支持范围。

#### 已确认的 Demo 范围基线

> 决策日期：2026-08-02
>
> 决策状态：已确认

Demo 必须验证完整的多 Agent Plan-to-Done 闭环，而不是只验证 Planning 流程或单 Provider 的简化纵向切片。以下能力共同构成 CFlow 的核心产品价值，不得通过删除其中关键环节将 Demo 收缩为普通 Agent CLI Wrapper：

- 至少两个真实 Coding Agent Provider 的托管与跨阶段路由。
- Requirement Discussion、Plan Generation、独立 Plan Check、Spec、Workflow、Coding、Review 和 Final Verification 的完整生命周期。
- 无依赖任务的真实并行调度和独立 Git Worktree。
- Agent Session、Workflow 和运行证据的持久化与异常恢复。
- 确定性检查、独立 Agent 验收和有上限的失败重试或 Repair。
- Task Branch 向 CFlow Integration Branch 的受控合并，以及最终可审计报告。

完整闭环是 Demo 的范围基线。本版已经完成逐项决策收敛并获得用户整份批准，可以进入实现设计文档；在实现设计与实现 Plan 分别获得用户确认前仍不得编码。

具体目标如下：

| 目标 | Demo 成功标准 |
|---|---|
| 工作流持久化 | 退出 CFlow 后重新进入，可以继续原 Workflow |
| Agent Session 恢复 | CFlow 可以记录并使用 Provider Session ID 继续讨论 |
| Plan 质量门禁 | 没有通过独立 Plan Check 的 Plan 不得拆 Spec |
| 并行 Coding | 无依赖且文件边界不冲突的任务可以并行执行 |
| 失败恢复 | Agent、测试或进程失败后状态不丢失，可继续或重试 |
| 独立验收 | Coding Session 不能自己把任务标记为最终完成 |
| 成本控制 | 每个节点有 Agent、模型、轮次、重试和预算限制 |

你已有的 `plan-workflow` 约束中，“Plan 是事实来源、完成需要独立验收、任务需要真实提交证据、并行任务必须无依赖且文件边界不冲突、恢复时需要从 Git 事实重建状态”等原则，应继续作为 CFlow Workflow 设计的基础。fileciteturn0file0

### 非目标

Demo 版本明确不做以下内容：

| 非目标 | 原因 |
|---|---|
| Web UI | 会显著扩大交付范围，CLI 已足以验证核心价值 |
| 云端多租户 | CFlow 首期是单机 Local-first 工具 |
| 任意 Python、Shell 动态脚本 | 难以验证、恢复和限制权限 |
| 自动 Push 或创建 PR | 涉及远端权限和不可逆动作 |
| 跨多个 Git 仓库执行 | Worktree、依赖和验收模型复杂度过高 |
| 非 Git、无有效 HEAD 或 Detached HEAD 的新 Workflow | 无法同时固定 Worktree/Commit 证据和可供受保护 Apply 的 Target Branch |
| Agent 无限自主循环 | 必须受重试、预算和状态机约束 |
| 自动修改用户主分支历史 | 自动阶段只写 CFlow Integration Branch；Target 只能由用户在完成后显式 `cflow apply` 更新 |
| Provider API 直连 | 优先复用用户已经安装和登录的 Coding CLI |

### 关键产品决策

本节记录已经逐项讨论并确认的产品与安全决策。PRD v0.2 已获得用户整份批准；以后新增的未确认内容必须单独标为“提案”或“待决”，不得混入已确认规则。

#### 已确认：Dynamic Workflow 生成模型

> 决策日期：2026-08-02
>
> 决策状态：已确认

Dynamic Workflow 采用“确定性安全骨架 + Agent 受限调度补丁”模型，不由 Agent 直接生成完整 DAG，更不生成任意可执行代码。

CFlow Compiler 根据已静态校验的 Specs 确定性生成 `Agent Task → Verify → Merge → Final Verify` 安全骨架；Specs 与最终 Workflow 随后一起进入 Execution Approval。独立调度 Agent 只能通过受限 Patch IR 提议并行分组、Provider 路由、Checkpoint 和预算调整；Compiler 验证并应用合法补丁。Agent 不得删除验收节点、弱化 Spec 依赖、绕过 Merge、提高硬预算上限或加入任意命令。

最终产物仍是受限声明式 Workflow IR：

```yaml
nodes:
  - type: agent_task
  - type: verify
  - type: merge
  - type: checkpoint
```

CFlow 负责校验和解释执行。GitHub Agentic Workflows 同样采用“人类友好的描述文件 → 编译后的受限 Workflow”模式，并在编译阶段执行 Schema、权限和安全校验，这说明编译式 Workflow 比直接执行 Agent 生成脚本更适合作为安全边界。citeturn8search1turn8search8turn8search9

#### 已确认：Workflow-local Verification Command Catalog

> 决策日期：2026-08-02
>
> 决策状态：已确认

Spec 和 Dynamic Workflow 不得包含自由 argv。所有外部 Verification Command 必须先进入当前 Workflow 的不可变、版本化 Named Command Catalog，Spec/Workflow 仅通过 `command_id` 引用。

Catalog 生成和批准流程：

1. Plan Approved 后，CFlow 从固定 Base Commit 的仓库配置和常见 Manifest/Wrapper 中确定性发现候选命令；发现只产生 Candidate，不代表可信或可执行。
2. Spec Agent 可以引用已有 `command_id`，也可以在结构化输出的 `proposed_commands` 中提出新命令；Agent 无权把 Proposal 直接加入可执行 Catalog。
3. CFlow 对 Proposal 执行策略校验，生成新的不可变 Catalog Revision，并在 Dry Run 中把新增命令单独展示给用户。
4. Execution Approval 必须同时固定 Catalog Revision/Hash；只有被该 Approval 覆盖的条目才能执行。
5. Compiler 必须拒绝未知 `command_id`、用途不匹配的引用、自由 argv、未固定的项目 Wrapper/Manifest 来源以及 Catalog Hash 不一致。

命令策略最低要求：

- 每个条目分别保存 `executable`、`args`、仓库内 `cwd`、Purpose、Timeout、Expected Exit Codes、输出上限、环境策略和来源证据；禁止 Shell Command String、Pipe、Redirect、Command Substitution 和隐式 `shell: true`。
- Project-relative Wrapper 必须来自 Workflow Base Commit 并固定文件 Hash；PATH Executable 必须在 Approval Preview 时解析绝对路径并固定 Binary Hash。执行前再次解析，不匹配则 `COMMAND_IDENTITY_CHANGED` 并保持 `BLOCKED`。
- Demo 默认拒绝 Shell Interpreter、内联代码执行参数、破坏性 Git 操作、Package Publish/Deploy、系统管理和明显越界的绝对路径。策略不是完整 OS Sandbox，不能把“通过策略校验”描述成命令绝对安全。
- `cwd` 必须位于 CFlow 管理的 Task/Integration/Apply Worktree。环境默认最小化，`HOME` 指向 Run 隔离目录；额外环境变量只允许批准变量名，Secret-like 值不得写入 Catalog、Event 或日志。
- Verification 后任何 Tracked File 变化都失败。Catalog 声明的 `transient_write_paths` 只允许命令运行期间使用；进入 Review/Merge 前必须由命令自身清理，最终只能留下 Git Ignored Output，且必须保存前后 Git 状态证据。任何 Git-visible Untracked Output 都失败，CFlow 不自动清理。
- Runtime 自身执行的 Git/Worktree 命令和 Provider Adapter 命令使用独立的内建 Command Policy，不通过 Verification Catalog，也不得被 Agent 修改。

Catalog 是审批与审计边界，不是通用脚本插件系统。Demo 不支持用户在 Catalog 中嵌入 Shell、Python、TypeScript 或其他任意脚本文本。

#### 已确认：显式受保护 Apply

> 决策日期：2026-08-02
>
> 决策状态：已确认

工作分支和自动 Runtime 不直接合并到用户当前分支。

所有任务最终先汇总到：

```text
cflow/<workflow-id>/integration
```

完整验收通过后，用户执行：

```bash
cflow apply
```

才将 Integration Branch 合并到最初的 Target Branch。这样可以保证执行过程中用户分支稳定，也可以在最终合并前统一查看 Diff。

执行 `cflow apply` 前必须重新检查用户目标工作区：工作区必须干净、当前分支必须仍是记录的 Target Branch。CFlow 不得自动 Stash、创建 WIP Commit 或覆盖用户未提交内容。

`cflow apply` 是 Workflow 已完成后的显式交付操作，不是 Dynamic Workflow Node，也不重新打开已终止的 Workflow 状态。每次 Apply 必须单独记录 Intent、Attempt、输入 Commit、验证证据和结果；未经用户命令不得自动执行。

Target Branch Drift 采用隔离式 Apply Staging：

1. 记录 Apply 开始时的 Target HEAD 和已验收 Integration HEAD。
2. 从该 Target HEAD 创建独立 Apply Branch 和 Worktree，不在用户工作区中预演合并。
3. 在 Apply Worktree 重验 Git Commit Identity/Signing Policy；通过后合并 Integration Branch，文本冲突可复用一次受限 Merge Resolution Attempt。Apply Merge Commit 创建后必须匹配该 Preflight。
4. 对 Commit Policy 合法的组合结果重新运行全量确定性检查，并创建独立 Apply Verification Session 执行语义 Review。
5. 全部通过后再次检查用户工作区仍干净、仍位于 Target Branch，且 Target HEAD 等于 Apply 开始时记录的 Commit。
6. 只有上述条件仍成立，才在用户工作区执行 `--ff-only`，将 Target Branch 更新到已验证的 Apply Commit。
7. 合并、验证、冲突修复或最终 Compare-and-Swap 任一步失败时，Target Branch 保持不变；保留 Apply Branch、Worktree、日志和验证证据供检查或重试。

Target HEAD 在 Staging 期间再次前进时，当前 Apply Attempt 进入 `BLOCKED`；新的用户显式重试必须从新的 Target HEAD 创建新 Attempt，不得复用旧验证结论或强制更新分支。

Apply Verification 只能执行已获 Execution Approval 且 Purpose 包含 `apply_verify` 的 `command_id`。每次 Apply Attempt 在运行验证前必须重新校验 Catalog Revision/Hash，并在 Apply Worktree 中重验 Project Wrapper 的来源 Hash 和 PATH Executable 的绝对路径/Binary Hash。

如果 Target Drift 改变了 Wrapper、Manifest 或 Executable Identity，CFlow 不得静默执行漂移后的工具，也不得沿用旧 Catalog：当前 Apply Attempt 以 `COMMAND_IDENTITY_CHANGED` 进入 `BLOCKED`，Target Branch 保持不变。用户检查差异后，可以显式创建新的 Apply Attempt，并批准由新 Target HEAD 重新发现、校验和固定的 Apply Verification Catalog Revision。这个批准属于独立 Apply 操作的安全确认，不新增 Workflow 主链路的第三个 Approval Gate，也不改写已完成 Workflow 的历史 Execution Approval。

**Plan Checker 必须使用独立 Session。**

可以使用相同 Provider，但不能复用生成 Plan 的 Session。Coding Agent 与最终 Reviewer 也必须是不同 Session。

**状态机不只用一个枚举。**

CFlow 使用：

```text
stage  = 当前处于哪个业务阶段
status = 当前阶段处于什么运行状态
```

避免产生大量类似 `PLAN_CHECK_FAILED_RETRYING` 的组合状态。

#### 已确认：Agent 交互主协议

> 决策日期：2026-08-02
>
> 决策状态：已确认

CFlow 采用“托管主链路 + 原生 Session Attach”的非对称交互模型：

- CFlow 托管模式是 Workflow Runtime 的唯一权威执行协议。CFlow 通过 Provider 的非交互结构化模式发送输入、接收事件流，并持久化 Session ID、输出、成本、错误和状态转换证据。
- `cflow session attach` 是显式的人工介入和逃生入口，可进入 Provider 原生 TUI，但不形成第二套 Workflow Runtime。
- 进入 Attach 前，CFlow 必须将相关自动执行节点置于安全暂停点，避免 CFlow 与原生 TUI 同时驱动同一 Session 或 Worktree。
- 原生 TUI 中的 Agent 不得直接修改 CFlow 最终状态。Attach 结束后，CFlow 必须根据 Provider Session、Artifact、Git、Worktree 和数据库事实执行 Reconcile，再决定下一步状态。
- 自动调度、结构化 Plan/Spec/Review 输出、重试和恢复必须始终能够仅通过托管协议完成，不依赖原生 TUI。

CFlow 自己接收用户输入，通过 Provider 的非交互结构化模式发送给 Agent，再把输出流展示给用户。这样可以可靠捕获 Session ID、事件、成本和错误。

原生全屏 TUI Attach 作为人工介入能力：

```bash
cflow session attach
```

但不作为 Demo 主链路的强依赖，因为不同 Provider 对交互 Session 的创建、发现和跨模式恢复语义并不完全一致。

#### 已确认：未知 Provider CLI 协议 Fail-closed

> 决策日期：2026-08-02
>
> 决策状态：已确认

CFlow Demo 只允许已被当前内建 Provider Protocol Registry 明确认可的 CLI Executable Identity、Version Range、Event Dialect 和 Purpose Capability 执行托管主链路。未知版本、已知不兼容版本或无法证明结构化协议能力时，不以 Warning + Best Effort 继续：

1. Protocol Registry 随 CFlow 二进制版本化并记录 Revision/Hash；每条记录至少固定 Provider、Executable Name、允许的 Version Range、Dialect ID、Session ID/Resume/Structured Events/Output Schema/Budget/Cancel 能力和事件 Schema。Demo 不支持用户配置“忽略版本”或自行声明兼容。
2. `detect()` 必须返回 `MISSING`、`SUPPORTED`、`UNKNOWN_VERSION` 或 `INCOMPATIBLE_PROTOCOL`，并记录解析到的绝对 Executable Path、Binary Hash、CLI Version、Registry Revision/Hash、Dialect ID 和能力集合。仅“命令存在”不等于可执行 Workflow。
3. Execution Approval、Session Start/Resume、Fallback 切换和每次 Provider 子进程启动前都必须重验 Protocol Binding。当前 Route 或 Fallback 引用的 Provider 不是 `SUPPORTED` 时，以 `PROVIDER_PROTOCOL_UNSUPPORTED` 阻塞且不创建 Node Attempt、不扣 Retry；机器上存在但未被该 Workflow 引用的未知 Provider 不构成阻塞。
4. Approval 后 Executable Path/Binary Hash/Version/Dialect 或 Registry Binding 漂移时，以 `PROVIDER_PROTOCOL_BINDING_CHANGED` 关闭 Dispatch Gate 并返回 `BLOCKED`，要求重新生成 Dry Run/Execution Approval；不得把一次批准泛化到另一个 Provider Binary。已经运行的进程继续受事件流校验和两阶段停止规则约束。
5. 已识别版本在运行中产生未知 Event Type、非法字段顺序、重复冲突 Session ID、缺少强制 `session_started`、未验证的 Completion Payload 或无法安全解析的 Stream 时，保存完整 Frame Boundary 和脱敏后的原始证据，记录不可自动重试的 `PROVIDER_PROTOCOL_VIOLATION` 或 `PROVIDER_SESSION_ID_MISSING`。受影响进程按两阶段协议停止，Attempt 结果不得被视为成功；Workflow 按普通不可重试失败的 Quiescing/Blocked 规则收敛。
6. Protocol Compatibility 与 Authentication 分开。`Authentication Unknown` 不伪装成 Protocol Unsupported；但托管非交互启动确认缺少登录或凭据时，以 `PROVIDER_AUTHENTICATION_REQUIRED` 在 Agent 产生可信结果前阻塞且不自动重试、不读取或复制凭据。
7. 不兼容时仍允许 `help`、`list`、`status`、`inspect`、`logs` 和 `doctor` 等纯只读诊断；不得创建/恢复 Session、生成或修改 Artifact、批准 Execution、Retry、启动 Merge/Apply 或推进 Scheduler。为确保可以止损，已有 Run 的 `pause`/`cancel` 仍可执行受控停止，终态 Workflow 的受保护 `cleanup` 仍按既有事实门禁运行，二者都不得启动 Provider。
8. 支持新 Provider 版本必须通过升级 CFlow 的 Registry/Adapter、固定 Dialect Fixture 和真实 Provider Compatibility Test 完成；在新版 CFlow 验证前，保存未知 Stream 不等于信任其语义。

因此，Agent 的自然语言“已经完成”、进程退出码为零或看似可解析的 JSON，都不能覆盖 Protocol Registry 与事件状态机的判断。

#### 已确认：Provider 默认权限与 Commit/Clean Worktree Gate

> 决策日期：2026-08-02
>
> 决策状态：已确认

CFlow Demo 使用用户已经安装和配置的 Provider CLI 默认权限模型，不建立跨 Provider Permission Profile、Capability Gate 或通用 OS Sandbox。CFlow 不覆盖用户的全局 Provider 配置，也不声称能够阻止 Agent 在运行期间访问网络、读取 Worktree 外文件或产生其他 Provider 默认允许的副作用。

这一选择把本地机器、Provider CLI 及其现有用户配置放入信任边界。CFlow 必须在首次运行、Dry Run 和最终报告中明确说明这一限制；不得展示 `sandboxed=true`、`read_only_enforced=true` 等无法由 Runtime 证明的结论。CFlow 自己不得主动加入 Danger/Bypass/Skip-Permissions 参数，但如果用户的 Provider 默认配置已经放宽权限，Demo 不负责重新收紧或统一它。

CFlow 仍负责自身可以确定性控制和验证的边界：

- Provider 进程的 `cwd` 指向 CFlow 管理的固定 Snapshot 或 Task Worktree，并记录 Provider、CLI Version、调用 argv（脱敏后）、cwd、Session ID、开始/结束时间和事件证据。
- Requirement、Plan、Check、Spec、Workflow Optimization 和 Reviewer 等非编码 Session 返回后，输入 Snapshot 的 HEAD 与 Git-visible 状态必须与启动前一致；否则拒绝该输出并记录 `UNEXPECTED_AGENT_MUTATION`。这只是仓库内事后检查，不能证明 Worktree 外没有副作用。
- Task Implementation/Repair Agent 必须在自己的 Task Worktree 中工作；Task 最终必须包含从不可变 `task_base_commit` 开始的一个或多个实现 Commit。CFlow 不替 Agent 执行 `git add`、`git commit`、`git stash`、`git reset`、`git clean` 或 `git commit --amend`。
- Agent 返回后，Task Branch HEAD 必须是 `task_base_commit` 的后代且不等于它，并且 `git status --porcelain=v2 -z --untracked-files=all` 必须为空。这里的 Git-clean 包含无 Staged、Unstaged 和非 Ignored Untracked 文件；Ignored 文件不计入 Git-visible Dirty 状态。
- 缺少 Task 实现 Commit 时当前 Attempt 以 `MISSING_IMPLEMENTATION_COMMIT` 失败；Worktree 不干净时以 `DIRTY_TASK_WORKTREE` 失败。两种情况都不得进入 Verification、Review 或 Merge，且不得由 Runtime 丢弃未提交内容来制造“干净”状态。
- `DIRTY_TASK_WORKTREE` 采用原地 Repair：失败 Attempt 先不可变落盘其起止 HEAD、起止 Dirty Fingerprint、Porcelain Status、Tracked/Staged Diff、Untracked Path/Hash 和 Session 证据；随后保留原 Task Branch 与 Worktree。Retry Budget 尚有余额时，Scheduler 在同一 Execute Node 下创建新的编号 Attempt 和独立 Repair Session，继续操作该 Worktree，而不是创建下游 Repair Node 或新 Worktree。
- 启动原地 Repair 前，Runtime 必须重新计算当前 HEAD、Git Status 和 Dirty Fingerprint，并与失败 Attempt 的结束证据精确比较。任何外部变化都以 `DIRTY_WORKTREE_DRIFTED` 进入 `BLOCKED`，不得把未知修改交给 Agent，也不得自动覆盖。
- Repair Attempt 可以提交需要保留的 Dirty 修改，也可以由 Agent 自己移除不需要的未提交残留；如果 Task 已经存在合法实现 Commit，不要求为了“本 Attempt 有提交”而制造空 Commit。Repair 返回时仍必须满足 Task 级 Commit/Clean Gate。再次失败会生成新的不可变 Attempt 并继续消耗同一 Retry Budget，预算耗尽后 Node `FAILED`、Workflow `BLOCKED`，Worktree 原样保留。
- Commit Gate 通过后，CFlow 始终对从不可变 `task_base_commit` 到当前 HEAD 的完整 Commit Range 执行 `write_scope`、禁止路径和 Git 事实检查，不能只检查最后一次 Attempt 或最后一个 Commit。只有 Task Commit 存在、Worktree Git-clean、完整 Commit Range 合法三项同时成立，Task 才能进入 Verification。
- Task Branch 采用 append-only 多 Commit 历史。一个 Task 可以由多个实现、Fix 或 Revert Commit 组成；不要求 Squash，也不允许 Agent 通过 amend、rebase、reset、force-update 或替换 Branch 历史让已被 CFlow 记录的 Commit 消失。修复已提交内容必须追加新 Fix/Revert Commit。
- 每个 Attempt 结束时，CFlow 记录 `end_head_commit`，并使用独立审计 Ref `refs/cflow/<workflow-id>/tasks/<task-id>/attempts/<attempt-number>` 固定该 Commit，防止后续 GC 使历史证据不可读取。创建或校验审计 Ref 是 Runtime 内建 Git 操作，不修改 Task Branch、Integration Branch 或用户 Target Branch。
- 新 Attempt 返回后的 HEAD 必须等于上一个已记录 HEAD，或是它的后代；等于只允许用于清理未提交残留且 Task 已有合法实现 Commit。若上一个已记录 HEAD 不再是当前 HEAD 的祖先，则当前 Attempt 以不可自动重试的 `TASK_HISTORY_REWRITTEN` 失败，Node 进入 `FAILED`，Workflow 按“无其他 In-flight 立即 Blocked / 有 In-flight 先 Quiescing”规则收敛。CFlow 保留所有审计 Ref 和证据，但不自动 Reset、Merge 或 Force-update 来修复历史。
- Integration 使用 `--no-ff` 合并最终 Task Branch，保留 Task 的 append-only Commit 序列和独立 Merge Commit；Final Report 同时列出完整 Task Commit Range 与 Merge Commit。
- Verification 执行后必须再次确认 HEAD 等于待验收 Commit，且 Worktree 恢复 Git-clean。Catalog 声明的临时输出可以在命令运行期间存在，但进入 Review/Merge 前必须已被命令自身清理，或仅剩 Git Ignored Output；任何 Git-visible 残留都使 Verification 失败，CFlow 不自动清理。
- Merge 前再次比较已验收 Commit、Task Branch HEAD 和 Git-clean 状态，防止验收后修改或补提交绕过证据。

角色名称和 Prompt 中的“只读”“仅修改 Write Scope”等仍是 Agent 行为要求，但在 Demo 的 A 方案下不是 Provider 沙箱保证；最终推进权只来自上述 Git、Commit、Catalog、Review 和 Runtime 状态机证据。

#### 已确认：本地严格权限与写盘前统一脱敏

> 决策日期：2026-08-02
>
> 决策状态：已确认

CFlow Demo 不实现应用层加密、系统 Keychain/Keystore 或自有密钥恢复。它依赖当前本机用户账户的文件系统隔离，并只持久化完整的“已脱敏对话与结构化事件”，不保存 Provider 原始字节流或未脱敏 Secret：

1. CFlow 创建的 `CFLOW_HOME`、Project/Workflow/Run/Session/Log/Evidence/Scratch 目录必须为当前用户所有并使用 `0700`；SQLite 主文件、WAL/SHM、Artifact、Session、Log、Export、Lock 元数据和其他敏感文件创建为 `0600`。Task/Integration/Apply Worktree 根目录为 `0700`，仓库内版本控制文件继续保留 Git Mode，CFlow 不批量改写源码文件权限。
2. 路径必须通过 Canonical Path、Owner、Type 和 Symlink Boundary 校验。既有 `CFLOW_HOME`、父目录或敏感文件存在 Group/Other 权限、Owner 不符、Symlink Escape，或文件系统不能可靠证明 POSIX 权限时，`doctor` 报 `INSECURE_CFLOW_HOME_PERMISSIONS`，Provider 驱动的 Mutating Workflow Fail-closed。CFlow 不静默 chmod 用户既有目录；由 CFlow 本次新建的路径必须从创建时即满足权限。
3. Provider stdout/stderr Frame、结构化事件、Prompt、Tool Input/Output、argv、环境、异常、Git/Verification 输出和用户输入必须先经过统一 Redaction Pipeline，再进入任何终端展示、SQLite、Artifact、Log、Event、`events.jsonl`、Context Bundle、Final Report 或 Export。不得存在先展示/写入 Raw Log、稍后再清洗的旁路。
4. Redaction Pipeline 结合结构化字段策略、CFlow 已知 Secret 值精确替换、Secret-like 环境变量名、Credential/Token/Private Key 模式和 Provider Dialect 专用规则。Secret 值替换为稳定类别占位符，例如 `[REDACTED:provider_token]`；不得保存原值、可逆编码、原值 Hash、长度或足以离线猜测 Secret 的派生信息。
5. 原始 Frame 只允许在有上限的进程内存 Buffer 中短暂存在，完成解析与脱敏后立即释放并尽力清零；Crash、Swap、Core Dump 和同用户恶意进程不在 Demo 能证明消除的威胁模型内。产品必须明确“不加密 at rest”和这些剩余风险。
6. 如果 Frame 无法按已批准 Dialect 解析、字段类型未知、二进制/超限内容无法安全检查，或 Redactor 自身失败，CFlow 不持久化该内容，也不继续把它解释为可信事件。Runtime 只记录不含原内容的 `SENSITIVE_DATA_REDACTION_FAILED` Finding（Provider、Session/Process ID、Frame Ordinal、类别和时间），两阶段停止受影响进程，Attempt 不成功且不扣 Retry，Workflow 按不可重试失败收敛为 `BLOCKED`。
7. `status`、`inspect`、`logs`、Context Bundle、Session Resume、Final Report 和所有 Export 只能读取相同的已脱敏 Artifact；Demo 不提供 `--show-secrets`、Raw Log、Raw Event Export 或绕过 Redactor 的 Debug Mode。Export 文件同样使用 `0600` 和原子写入。
8. 完整已脱敏 Transcript/Event Artifact 是不可变证据，保存 Frame 顺序、事件语义、占位符类别、Redactor/Rule Revision 和内容 Hash。它可以支持审计与恢复，但不得被描述为 Provider 原始字节的法证副本。
9. CFlow 不设置自动 TTL，现有 Cancel/Cleanup 仍不删除 Transcript、Log、SQLite 或 Evidence。`doctor`、首次运行、Dry Run 和 Final Report 必须提示长期本地保留、未加密 at rest、可能被 OS Backup 同步，以及用户负责保护本机账户和磁盘加密。
10. 所有敏感文件采用同目录临时文件、`0600` 创建、Flush/Sync、原子 Rename 的写入模式；临时文件在成功 Rename 前同样不可放宽权限。Crash Recovery 只接受权限正确、Hash 完整且具有完成 Intent/Result 的 Artifact，残留临时文件不得被当成有效证据或自动导出。

这一策略不承诺防御 Root、同账户恶意程序、被攻破的 Provider CLI、内存取证或未加密磁盘被离线读取；Demo 的安全目标是避免 CFlow 自己把常见凭据持久化，并阻止其他本机账户通过宽松权限直接读取工作流证据。

#### 已确认：Git Commit Identity 与 Signing Preflight

> 决策日期：2026-08-02
>
> 决策状态：已确认

CFlow 继承目标仓库当前有效的 Git Author、Committer 和 Commit Signing 配置，但在任何可能创建 Commit 的 Agent Session 或 Runtime Git 操作之前先执行 Preflight。CFlow 不写入 Local/Global/System Git Config，不注入 Bot Identity，不关闭用户已经启用的签名，也不读取、复制或保存私钥和口令。

Preflight 最低流程：

1. 在目标 Repository Context 中使用当前 Git Executable 和继承环境解析 `GIT_AUTHOR_IDENT`、`GIT_COMMITTER_IDENT`；缺少或非法的 Name/Email 时以 `GIT_IDENTITY_NOT_CONFIGURED` 阻塞。
2. 读取当前生效的 Commit Signing Mode、Format、Signing Key 和 Signer Program 等必要配置，生成规范化 `commit_policy_fingerprint`；时间戳、路径中的临时值和 Secret 不得进入 Fingerprint。
3. Signing 未启用时，Identity 校验成功即可通过。Signing 已启用时，CFlow 必须在 CFlow 管理的临时 Git Repository 中执行一次不更新目标仓库 Ref/Worktree 的签名 Probe；Probe 使用同一 Git Executable、继承环境和解析后的有效 Identity/Signing 配置，关闭 stdin/TTY 交互并设置硬 Timeout。
4. Probe 失败、超时或无法证明当前 Signer 可非交互使用时，以 `GIT_SIGNING_PREFLIGHT_FAILED` 阻塞。CFlow 不回退为 Unsigned Commit，也不通过弹出交互式凭据输入来自动推进。
5. Preflight 结果保存为不可变 `git/commit-preflight-<revision>.json`，记录 Repository、Git Version、规范化 Identity、Signing Mode/Format、Key Fingerprint 或公开 Key ID、Policy Fingerprint、Probe 结果和时间；不得保存私钥、Passphrase、Credential Helper 输出或未脱敏环境值。

Preflight 执行时机与失效规则：

- `cflow doctor` 在 Git Repository 中提供只读 Identity/Signing 诊断，但不得创建 Commit 或改变配置。
- Execution Approval 前必须存在成功的 Preflight Report，Execution Approval 同时确认该 Report 的精确 Revision、Hash 和 `commit_policy_fingerprint`。Coding/Repair/Merge Resolution Session、Integration Merge 和 Apply Staging Merge 启动前重新计算 Fingerprint；未变化时可以复用成功 Probe，变化时必须生成新的 Preflight Revision 并重新验证。
- Preflight 失败发生在 Provider 或 Git Merge 子进程启动前，不消耗 Node Retry Budget；主执行链路的 Workflow 进入 `BLOCKED`，由用户在仓库或自己的 Git 配置中修复后执行 Resume。Workflow 已完成后的 Apply 则只阻塞当前 Apply Attempt，Workflow 继续保持 `COMPLETED/SUCCEEDED`。CFlow 只显示诊断和修复建议，不代替用户修改配置。
- Preflight 降低“执行到一半才发现无法 Commit”的概率，但不能保证外部 Signing Agent、硬件密钥或凭据在后续运行期间始终可用；实际 Commit 失败仍按对应 Attempt/Runtime Failure 留证。

#### 已确认：执行期间 Commit Policy 漂移确认

> 决策日期：2026-08-02
>
> 决策状态：已确认

执行期间重新计算出的 `commit_policy_fingerprint` 与当前已确认值不同时，CFlow 不静默接受，也不使 Plan、Specs、Verification Catalog、Dynamic Workflow 或原 Execution Approval 失效。它采用一次只绑定精确新策略的异常安全确认：

1. CFlow 检测到新 Fingerprint 后先关闭 Dispatch Gate，并在存在活动 Attempt 时完成 Policy Safety Stop；随后才为新 Fingerprint 生成不可变 Preflight Revision 并完成全量验证。新 Preflight 失败时沿用 `GIT_IDENTITY_NOT_CONFIGURED` 或 `GIT_SIGNING_PREFLIGHT_FAILED` 阻塞，不展示可批准确认。
2. 新 Preflight 成功后，在任何 Commit-capable Provider 或 Git Merge 子进程启动前建立安全 Checkpoint。若存在活动 Attempt，必须先完成下述 Policy Safety Stop；未发现窗口 Commit 时，主 Workflow 才以 `COMMIT_POLICY_CONFIRMATION_REQUIRED` 进入 `EXECUTION/PAUSED`。尚未创建 Node Attempt 时不得为此创建 Attempt，已有 Attempt 也不消耗 Retry Budget。
3. 界面必须并列展示旧值与新值的规范化差异、Preflight Revision/Hash/Fingerprint、Repository Context 和下一项受影响动作。Secret、私钥和 Passphrase 仍不得展示或落盘。
4. 未发现窗口 Commit、因而没有 Replacement Execution Gate 时，用户批准 append-only 写入 `COMMIT_POLICY` Approval，精确绑定新 Preflight Revision/Hash/Fingerprint。该确认在同一 Workflow 内对完全相同的 Fingerprint 有效，后续动作不得重复询问；它不是主链路的第三个常规批准门，也不重新批准或替换原 Execution Approval。
5. Approval 写入前以及启动受影响动作前都必须 Compare-and-Swap 重算 Fingerprint。再次漂移时返回 `COMMIT_POLICY_INPUT_CHANGED`、保持暂停并展示最新差异，不得把一次确认泛化到其他 Identity、Signing Key、Format 或 Signer Program。
6. 用户拒绝时记录不可变拒绝事实并保持 `PAUSED`，不启动受影响动作；用户可以自行恢复原 Git 配置、确认另一个有效 Preflight 或取消 Workflow。若配置恢复到已确认的精确 Fingerprint，且当前 Probe 仍满足复用规则，可以继续而不重复确认。
7. `cflow resume` 必须恢复同一待确认 Preflight 与旧/新差异，并先重验当前 Fingerprint；不得因为进程重启自动越过确认。

Workflow 已经 `COMPLETED/SUCCEEDED` 后发生的 Apply Policy 漂移只影响当前 Apply Attempt：新 Preflight 成功后，该 Attempt 以 `COMMIT_POLICY_CONFIRMATION_REQUIRED` 进入 `BLOCKED`，Target Branch 保持不变。用户确认必须同时绑定 Apply Attempt、Target HEAD、Integration HEAD 和新 Preflight Revision/Hash/Fingerprint；已完成 Workflow 的状态及历史 Execution Approval 不改变。确认前任一 HEAD 或 Fingerprint 再次变化时，该确认输入作废。

#### 已确认：Commit Policy 漂移立即安全停止

> 决策日期：2026-08-02
>
> 决策状态：已确认

Commit Policy 漂移是全局安全暂停信号，不适用“允许当前 Attempt 完成”的普通 Failure Quiescing：

1. 每个既有强制重验点发现 Fingerprint 漂移时，Runtime 必须在同一序列化边界关闭 Dispatch Gate、记录 `COMMIT_POLICY_SAFETY_STOP_REQUESTED`，并把 Run 置为 `STOPPING`，`stop_reason = COMMIT_POLICY_DRIFT`。从该时刻起不得启动任何新外部动作。
2. CFlow 对该 Workflow 的全部活动 Provider、Verification 和 Git Attempt 执行 Ctrl+C 章节定义的两阶段有限停止，不只停止触发漂移的 Node，也不允许不相关兄弟 Attempt 继续完成。该安全停止优先于 Run `QUIESCING`；原有 Blocking Finding 仍保留。
3. 被停止的 Attempt/Session 以 `INTERRUPTED` 和完整 Checkpoint 留证，`retry_budget_charged = false`。Worktree、Index、未提交修改和已创建 Commit 原样保留，不执行自动 Commit、Stash、Reset、Clean、Rebase 或历史改写。
4. 没有其他 Blocking Finding、孤儿进程或漂移窗口 Commit 时，停止完成后 Workflow 进入 `EXECUTION/PAUSED`，展示精确新 Preflight 确认；存在其他 Blocking Finding 时保持 `BLOCKED`，但任何后续 Commit-capable 动作仍必须先解决 Policy 确认。
5. 为缩小运行中漂移窗口，只要存在 Commit-capable 受管进程，Runtime 必须以不超过 1 秒的固定周期重新计算规范化 Fingerprint；平台文件通知只能作为提前唤醒，不能替代可移植轮询。监控只读取公开有效配置，不读取 Signer Secret，也不为每次轮询执行签名 Probe。
6. CFlow 不能原子观察或阻止 Provider 内部的 `git commit`。安全停止后必须对每个活动 Task/Integration Worktree 比较 Stop Request 前固定的 HEAD 与最终 HEAD，并扫描新 Commit。检测到窗口内 Commit 时记录 `COMMIT_DURING_POLICY_DRIFT_WINDOW`、关联旧/新 Fingerprint 和实际 Identity/Signature Evidence，并使 Workflow `BLOCKED`。
7. 后续用户确认新 Policy 不得追溯授权漂移窗口 Commit，也不得使其自动通过 Commit/Clean Gate。该 Commit 及 Audit Ref 保留，不能通过 amend、reset、rebase 或 force-update 擦除；如何显式处置该 Commit 需要走受审计 Repair/Revision 决策。
8. Agent 使用临时 `-c`、环境变量、`--author` 或 `--no-gpg-sign` 不一定改变 Repository Fingerprint，因此仍由提交后 `COMMIT_POLICY_MISMATCH` 门禁负责；Periodic Monitor 不能被描述成 OS Sandbox 或 Git Hook 强制。

如果 Policy 漂移发生在 Apply Attempt，安全停止只作用于该 Apply 的全部活动子进程；Target Branch 保持不变，Apply Attempt `BLOCKED`，已完成 Workflow 状态不改变。若强制停止后仍有匹配进程，则沿用 Project Mutation Quarantine，不能展示可继续执行的 Policy 确认。

#### 已确认：漂移窗口 Commit 的隔离与替代执行

> 决策日期：2026-08-02
>
> 决策状态：已确认

`COMMIT_DURING_POLICY_DRIFT_WINDOW` 不能通过原 Branch 上的追加 Revert、事后批准或历史改写恢复可信链。CFlow 采用“永久隔离旧路径、从最后可信基线创建替代路径”的恢复方式：

1. Runtime 必须为包含窗口 Commit 的 Task、Integration 或 Apply Branch 创建不可变 Branch Quarantine Record，并以唯一 `refs/cflow/<workflow-id>/quarantine/<quarantine-id>` 固定发现时的 HEAD。该 Branch、其中全部 Commit、Worktree 状态、日志和证据永久可检查，但从此不得作为 Verify、Review、Merge、Final Verify 或 Apply 的输入。
2. CFlow 不自动 Cherry-pick、Revert、Reset、Rebase、Merge 或复制窗口 Commit 到可信路径。替代 Agent 可以把已脱敏的失败 Diff 和 Evidence 作为只读上下文，但必须依据已批准需求在新 Branch 上重新产生 Commit，并重新通过完整 Commit Policy、Scope、Verification 和独立 Review 门禁。
3. 窗口 Commit 位于 Task Branch 时，旧 Attempt 保持 `INTERRUPTED`，不得改写成普通执行失败；Runtime 依据不可重试的窗口 Commit Finding 将旧 Node 置为 `FAILED`，Task 投影不得成为 `VERIFIED/MERGED/COMPLETED`。CFlow 生成带新 Spec ID、`replaces_task_id` 和隔离证据引用的 Repair Spec，以及新的 Dynamic Workflow Revision；只有新的 Execution Approval 生效后，才从当时最后已验收的 Integration HEAD 创建新 Task Branch/Worktree 和全新的 Task/Node/Attempt。新 Task 不继承旧 Task 的 Attempt Number 或 Retry 消耗。
4. 窗口 Commit 位于 Integration Branch 时，旧 Integration Branch 整体隔离。CFlow 从 Stop Request 前记录的最后已验收 Integration HEAD 创建唯一命名的 Replacement Integration Branch，生成新的 Dynamic Workflow Revision，并在新的 Execution Approval 后只重放仍可信 Task Commit 的 Merge/Verify 路径；不得把窗口 Merge Commit 直接带入新 Branch。
5. 窗口 Commit 位于 Apply Staging Branch 时，原 Apply Attempt 永久保持 `BLOCKED`，旧 Staging Branch/Worktree 隔离，Target Branch 不变。用户显式创建的新 Apply Attempt 通过 `supersedes_apply_attempt_id` 指向旧 Attempt，并从当前 Target HEAD 和已验收 Integration HEAD 重新开始，重新执行适用的 Catalog、Commit Policy、Merge、全量验证、Review 和最终 Compare-and-Swap 门禁。
6. 只有实际包含窗口 Commit 的 Branch 被隔离。其他因 Policy Safety Stop 而 `INTERRUPTED`、但 HEAD 不包含窗口 Commit 的 Task Worktree 可以保留；它们必须等待当前批准条件全部满足，并在 Resume 前通过既有 HEAD/Status/Dirty Fingerprint 一致性检查。
7. `COMMIT_DURING_POLICY_DRIFT_WINDOW` Finding 只有在替代 Spec/Workflow Revision 获批并建立全新可信执行路径，或 Workflow 被取消时，才能由 Runtime 标记为 `RESOLVED/CONTAINED`；原 Finding、Quarantine Record、Audit Ref 和 Commit Evidence 永不删除。任何未完成隔离副作用都保持 Workflow `BLOCKED`。

“最后已验收的 Integration HEAD”是 Runtime 在启动 Commit-capable Integration 动作前固定、且其完整历史已经通过 Commit Evidence 和 Integration 验收的 Commit；不能用安全停止后的当前 Branch HEAD 倒推或猜测。

#### 已确认：Replacement Execution Approval 吸收 Policy 确认

> 决策日期：2026-08-02
>
> 决策状态：已确认

当漂移窗口 Commit 迫使 CFlow 创建新 Repair Spec 或 Dynamic Workflow Revision 时，原 Execution Approval 已因执行 Artifact 变化而失效。新的 Replacement Execution Approval 同时固定当前成功 Preflight 的 Revision、Hash 和 Fingerprint，因此它直接承担本次 Commit Policy 确认，不再要求用户对同一 Fingerprint 额外提交 `COMMIT_POLICY` Approval：

1. Replacement Execution Approval 页面必须在同一预览中展示 Quarantine Finding/Branch/Audit Ref、旧与新执行 Revision、Replacement 基线、Routing/Budget、旧/新 Commit Policy 差异和当前成功 Preflight。用户的一次批准精确绑定全部输入。
2. 写入的仍是 `gate_type = EXECUTION` 的 append-only Approval，不新增 `RECOVERY` Gate，也不额外写一条同 Fingerprint 的 `COMMIT_POLICY` Approval。`decision_context_json` 必须记录 `reason = COMMIT_POLICY_DRIFT_REPLACEMENT`、被替代的 Execution Approval ID、Quarantine ID 集合、Reconciliation Manifest Revision/Hash 和 `absorbs_commit_policy_confirmation = true`。
3. 展示批准页前、写入 Approval 前以及启动每个受影响 Commit-capable 动作前，都必须重新计算 Fingerprint 并核对 Preflight Hash。Approval 写入前漂移或任一 Artifact/基线变化时返回 `APPROVAL_INPUT_CHANGED`，重新生成完整预览；Approval 生效后再次漂移则作为新的运行期漂移，重新执行 Policy Safety Stop。
4. `latestConfirmedCommitPolicy` 必须把绑定精确 Preflight 的有效 `EXECUTION` Approval 与独立 `COMMIT_POLICY` Approval 都视为可审计 Policy 确认来源，并返回具体 Approval ID/类型；不得仅查询最后一条 `COMMIT_POLICY` Row。
5. 没有窗口 Commit、且 Plan/Spec/Catalog/Workflow/Policy 等执行输入均未变化的普通运行期 Policy 漂移，仍使用独立 `COMMIT_POLICY` 确认，不强迫用户重做 Execution Approval。
6. Workflow 已经 `COMPLETED/SUCCEEDED` 后的 Apply 不创建 Replacement Execution Approval。Apply Policy 漂移仍通过独立 `COMMIT_POLICY` Approval 精确绑定新 Apply Attempt、Target HEAD、Integration HEAD 和 Preflight；不得借已完成 Workflow 的历史 Execution Approval 越过确认。
7. Recovery 发现 Replacement Execution Gate 尚未决定时，只恢复这一张统一预览并重新校验全部引用；不得同时恢复第二个 Policy Gate。已经存在 Hash 完全匹配的 Replacement Execution Approval 时恢复其 Policy 确认来源，继续前仍执行动作级 Fingerprint CAS。

#### 已确认：未污染兄弟 Task 增量恢复

> 决策日期：2026-08-02
>
> 决策状态：已确认

Replacement Execution Approval 生效后，CFlow 不重做整个 Execution，也不丢弃被 Policy Safety Stop 中断但仍可信的兄弟 Task。它只替换污染路径，并以确定性证据恢复其余节点：

1. 旧 Attempt 永久保持 `INTERRUPTED`，`retry_budget_charged = false`。恢复时在同一 Task/Node 下创建连续编号的新 Attempt；不得复活、覆盖或改写旧 Attempt，也不得把安全中断计为 Provider Failure。
2. 同一 Task Branch/Worktree 只有同时满足以下条件才可复用：没有 Branch Quarantine；完整祖先链不包含任何窗口 Commit；HEAD、Porcelain Status 和 Dirty Fingerprint 与 Stop Checkpoint 完全一致；Task Base、Spec 内容、Scope、Acceptance、Route/Budget 和依赖语义未改变；其依赖 Commit 仍存在于当前可信 Integration 基线。
3. 新 Dynamic Workflow Revision 可以引用旧 Node 作为同一逻辑节点，但必须验证 Node ID、规范化 Definition Hash 和全部依赖边完全一致。任一语义变化都必须创建带新 ID 的 Node，不得借“恢复”绕过新 Execution Approval 或旧图不可变约束。
4. 已经 `SUCCEEDED` 的 Node 只有在其结果证据仍完整、对应 Commit 已包含在最后已验收 Integration HEAD 或仍可从可信 Task Branch 重新验证、且新图定义完全一致时保持成功。否则它不得静默回退或伪装复用，必须在 Replacement 预览中显式列为新 Verify/Merge 路径。
5. 对可复用的 Coding Task，新 Attempt 优先 Resume 原 Provider Session；Resume 失败时按 LOST/Context Bundle/Successor Session 规则处理。新 Attempt 创建本身不扣 Retry，之后真实 Provider/Verification Failure 才按当前批准预算计费。
6. 旧 Commit 继续关联创建时的旧 Preflight Evidence；恢复后产生的新 Commit 使用 Replacement Execution Approval 固定的新 Preflight。完整 Commit Range 必须逐 Commit 具有有效 Evidence，不能要求旧合法 Commit 被重签或改写。
7. Replacement Execution Approval 页面必须列出 `reuse_succeeded`、`resume_interrupted`、`replace_contaminated` 和 `rerun_verification` 四类 Node 清单及理由。用户批准后 Scheduler 只调度后面三类中尚未满足的节点；已成功且可复用节点不得重复执行。
8. Recovery 必须重算上述分类并与 Approval 固定的 Reconciliation Manifest Hash 比较。任一 Branch、HEAD、Definition、Dependency 或 Evidence 漂移时返回 `REPLACEMENT_RECONCILIATION_CHANGED` 并重新进入统一 Execution Gate，不得自行扩大复用集合。

因此，增量恢复的边界由 Runtime 的 Git/Artifact/Evidence 比较决定，不由 Agent 声称“这个 Task 没受影响”决定。

Commit 创建后的确定性校验：

- 对每个新增 Task Commit、Integration Merge Commit 和 Apply Staging Merge Commit，CFlow 必须读取实际 Author/Committer Identity 与签名状态，并关联创建前的 Preflight Revision。
- 实际 Identity 必须符合该 Preflight 解析出的 Identity；若 Signing 已启用，每个新 Commit 都必须通过 `git verify-commit`，且签名者必须与 Preflight 固定的公开 Key/Principal 一致。
- Agent 通过临时 `-c`、环境变量、`--author`、`--no-gpg-sign` 或其他方式绕过继承策略时，以不可自动重试的 `COMMIT_POLICY_MISMATCH` 阻塞。Append-only 历史下 CFlow 不通过改写 Commit 修复，所有审计 Ref 和失败证据保留。
- Signing 未启用时不强制 Commit 必须无签名；若 Agent 主动签名，CFlow 记录并验证可验证的签名，但仍以 Preflight Identity 作为提交身份门禁。

#### 已确认：Session Resume 失败与跨 Provider 上下文交接

> 决策日期：2026-08-02
>
> 决策状态：已确认

Provider Session Resume 不可恢复时，CFlow 不伪装成原 Session 已延续，也不因 Provider 故障直接丢弃已完成的讨论或执行证据：

- CFlow 先尝试恢复原 Provider Session；确认不可恢复后，将原 Session 标记为 `LOST`，保留其完整已脱敏事件、对话、摘要、错误和关联证据；不保留未脱敏 Raw Stream。
- CFlow 生成新的、不可变且版本化的 Context Bundle，再创建继任 Session。继任记录必须通过 `supersedes_session_id` 指向原 Session，并记录所使用 Context Bundle 的 Revision、路径和 Hash。
- Context Bundle 至少包含需求、当前有效 Plan/Spec/Verification Catalog/Workflow Revision 与 Hash、仓库基线、阶段摘要、已确认决策、相关失败证据、未决问题和权限边界。它表示可审计的上下文交接，不声称恢复了原模型的隐藏状态。
- 完整已脱敏对话在本地保留，但默认只向继任 Agent 注入结构化摘要和完成当前 Purpose 所需的最小证据摘录，避免无界上下文增长和跨 Provider 提示污染。
- 继任 Session 可以继续使用原 Provider，也可以切换 Provider；切换前必须重新检查结构化事件、Session、Schema 和预算等协议能力。Provider 工具权限继续采用目标 Provider 的用户默认配置，不把权限差异当成 CFlow 已统一或已强制；跨 Provider 切换必须向用户展示这一信任边界。
- 因 Provider 故障而为自动执行节点创建继任 Session 时，必须创建新的 Node Attempt 并消耗对应 Retry/Budget；Requirement Discussion 等人工交互阶段允许继续，但必须明确告知用户 Session 已降级恢复以及当前 Provider。用户主动 Ctrl+C 产生的受控中断不计作 Provider 故障，适用下述独立规则。

任何 Context Bundle 更新都必须创建新 Revision；不得原地修改已被 Session 引用的 Bundle。

#### 已确认：Ctrl+C 两阶段有限停止

> 决策日期：2026-08-02
>
> 决策状态：已确认

Ctrl+C 是用户请求的受控暂停，不是 Runtime Crash，也不是 Task Failure。CFlow 不让已启动的自动化子进程脱离前台继续无限运行：

1. 第一次 Ctrl+C 立即停止调度新 Node 和启动任何新的 Provider、Verification 或 Git 子进程，在事务中追加 `CONTROLLED_STOP_REQUESTED`，并将当前 Run 置为 `STOPPING`。
2. CFlow 对所有活动子进程并发调用对应 Adapter Cancel 或 Context Cancel，同时继续排空 stdout/stderr 和结构化事件。Demo 的 Grace Period 固定为 10 秒，并在 CLI 显示剩余时间。
3. Grace Period 到期仍存活的 Process Group 先收到终止信号；再等待固定 2 秒仍未退出时，进入强制终止阶段。第二次 Ctrl+C 跳过剩余 Grace Period，立即进入强制终止阶段。平台实现必须针对整个受管 Process Group，而不是只终止父 PID。
4. CFlow 使用 PID + Process Start Token 确认进程已经退出，随后保存最后完整事件、Session ID、Git HEAD、Porcelain Status、Dirty Fingerprint 和未完成外部副作用的 Intent。不得把截断的 JSONL 尾部伪装成有效事件。
5. 活动 Node Attempt 以不可变 `INTERRUPTED` 结束，`retry_budget_charged = false`；Node 在事实协调后回到 `READY`。普通运行中的 Workflow 保持 `PAUSED`，Scheduler 不得在本次 Run 中重新启动它；如果 Ctrl+C 发生在已有 Blocking Finding 的 Quiescing 中，则 Workflow 收敛为 `BLOCKED`。已经 `SUCCEEDED` 的 Node 不回退。
6. 有可恢复 Provider Session ID 的 Session 进入 `INTERRUPTED`；Resume 时优先尝试原 Session。若 Provider 明确无法恢复，再按 Session LOST/Context Bundle/继任 Session 规则处理，此时由 Provider 恢复失败触发的新 Attempt 才按原规则计入 Retry/Budget。
7. 被中断的 Coding Worktree 和未提交修改全部原样保留。Resume 前必须精确核对中断时的 HEAD、Status 和 Dirty Fingerprint；一致时可在同一 Branch/Worktree 中创建后继 Attempt，漂移时以 `INTERRUPTED_WORKTREE_DRIFTED` 阻塞。中断本身不得触发自动 Commit、Stash、Reset、Clean 或下游调度。
8. 正在进行的 Git/Worktree 外部副作用仍按 Intent/Result 协调；CFlow 不假设信号终止具有事务性。Resume 必须先判断 Ref、Index、Worktree 和 Commit Object 的实际结果，再决定补记完成、重试或阻塞。
9. 只有所有受管子进程已确认退出，且 Run `INTERRUPTED`、Workflow 的目标 `PAUSED/BLOCKED` 状态、Attempt/Session/Checkpoint/Event 已持久化后，才能释放 Workflow Owner 和 Project Writer。强制终止后仍存在匹配身份的进程时，记录 `ORPHAN_CHILD_PROCESS`、Quarantine Project Mutation，并将 Workflow 置为 `BLOCKED` 后再退出协调 Runtime。
10. 在无活动子进程的输入提示或 Approval Gate 按 Ctrl+C 时，直接保存当前门和引用 Hash 后暂停。`/pause` 与 `cflow pause` 复用同一受控停止协议；`/cancel` 与 `cflow cancel` 复用进程停止部分，但最终进入 `CANCELLED`，并适用下述保留策略。

如果协调 Runtime 在 `STOPPING` 中自身崩溃，后续 Recovery 仍按 OS Lock、Process Identity、外部 Git 事实和 Intent/Result 事件处理；不得因为曾经记录 Stop Intent 就假设子进程已经终止。

#### 已确认：Cancel 逻辑终止与证据保留

> 决策日期：2026-08-02
>
> 决策状态：已确认

`cflow cancel` 是用户明确要求结束一个未完成 Workflow 的终态操作，但不隐含资源删除：

1. Cancel 前必须展示 Workflow ID、当前 Stage、活动 Session/Node、所有受管 Worktree/Branch、Dirty 状态、尚未合并 Commit 和预计保留路径，并要求默认否定的显式确认。Agent 无权发起或确认 Cancel。
2. 用户确认后，Runtime 先以 append-only `WORKFLOW_CANCEL_REQUESTED` Event 固定 Actor、原因和当前事实摘要，再复用 Ctrl+C 的两阶段有限停止协议；从记录 Intent 起不得再调度新 Node、Retry、Repair、Merge 或 Verification。
3. 所有子进程确认退出并完成 Checkpoint 后，在同一事务中将活动 Session、Node Attempt 和未完成 Node 标记为 `CANCELLED`，Run 标记为 `CANCELLED`，Workflow `runtime_status` 标记为终态 `CANCELLED`，并追加 `WORKFLOW_CANCELLED`。已经 `SUCCEEDED` 的 Node 和已产生的 Commit/Review/Verification 事实保持原样。
4. 如果强制终止后仍存在匹配身份的受管进程，或外部 Git 副作用尚不能协调，Workflow 不得抢先进入 `CANCELLED`。它保留 Cancel Intent，以 `CANCEL_PENDING_ORPHAN_PROCESS` 或对应 Reconciliation Finding 进入 `BLOCKED` 并 Quarantine Project Mutation；进程退出且事实协调完成后，Recovery 自动完成原 Cancel Intent，而不是恢复执行。
5. Cancel 不自动删除、移动、压缩或改写任何 Workflow Artifact、SQLite Row、Event、Session 对话、Log、Verification Evidence、Context Bundle、Task/Integration/Apply Worktree、Task/Integration/Apply Branch、Commit Object 或 `refs/cflow/...` Audit Ref。Dirty Worktree 与未提交内容尤其必须原样保留。
6. CFlow 不设置自动 TTL、磁盘配额淘汰或后台 Garbage Collection。`status`、`inspect`、`logs` 和报告导出仍可读取 Cancelled Workflow；`resume`、`retry` 和 `apply` 必须拒绝该终态。
7. 用户可以基于当前仓库事实创建新的 Workflow，但它拥有新的 Workflow ID、Base Commit、Integration Branch 和审批历史，不得复活、覆盖或继续写入已 Cancelled Workflow。
8. 删除保留资源只能由独立、显式的 `cflow cleanup [workflow-id]` 操作完成。Cleanup 必须先提供只读 Dry Run 和逐项目标清单，并遵守下述已确认删除门禁。

重复执行 `cflow cancel` 时，如果 Workflow 已经 `CANCELLED`，命令只返回现有终态和保留资源清单，不追加伪造的新取消历史；对 `COMPLETED/SUCCEEDED` 或 `FAILED` 终态执行 Cancel 也必须拒绝，不能借 Cancel 改写既有终态。

#### 已确认：Cleanup 仅删除安全干净的衍生目录

> 决策日期：2026-08-02
>
> 决策状态：已确认

Demo 的 Cleanup 只回收可以确定性重建、且删除不会破坏审计链的目录；它不是 Workflow Purge：

1. 只有 `CANCELLED`、`FAILED` 或 `COMPLETED/SUCCEEDED` 终态 Workflow 可以 Cleanup。`PAUSED`、`BLOCKED`、`RUNNING`、存在未完成 Cancel Intent 或存在非终态 Apply Attempt 时必须拒绝。
2. `cflow cleanup <workflow-id>` 默认只生成不可变 Cleanup Plan 和 Dry Run，不删除任何内容。Plan 逐项记录 Target Type、Canonical Path、Repository/Workflow、预期 Worktree Branch/HEAD、目录 Fingerprint、可删除原因和 Plan Hash。
3. 用户只能通过 `cflow cleanup <workflow-id> --execute <cleanup-plan-id>` 执行刚才检查过的精确 Plan，并再次确认完整目标清单。Demo 不提供 `--force`、通配 Workflow、按年龄批量删除或后台自动 Cleanup。
4. 可删除目标仅限：SQLite 中属于该 Workflow 的 CFlow-managed Task/Integration/Apply Worktree，以及 Run 隔离目录中明确标记为 Scratch 的 `tmp/`、临时 `HOME` 和 Cache 子目录。`runs/<run-id>/` 本身、Log、Verification、Final Report、Session、Context Bundle 和其他 Evidence 目录绝不属于临时目录。
5. Cleanup 的“安全干净”比 Task Gate 更严格：Worktree 不得有 Staged、Unstaged、任何 Untracked 文件（包括 Ignored）、未完成 Merge/Rebase/Cherry-pick/Revert/Bisect、锁文件或匹配身份的活动进程。Git 命令自身拒绝移除时 CFlow 也必须拒绝，不能升级为 Force。
6. 每个 Worktree 的 Canonical Path 必须位于记录的 `CFLOW_HOME/worktrees/<project-key>/<workflow-id>/` 下，并与 SQLite、`git worktree list --porcelain -z`、预期 Branch 和 HEAD 完全一致。Symlink Escape、路径越界、Owner 不匹配或事实漂移返回 `CLEANUP_FACT_MISMATCH`，不得删除该目标或尚未处理的后续目标。
7. 执行前必须持有 Project Writer 与 Workflow Owner，并重新确认没有受管进程、Project Mutation Quarantine 或活动 Apply。每个删除分别写入 `CLEANUP_ITEM_REQUESTED` 与 `CLEANUP_ITEM_COMPLETED/FAILED`；调用 `git worktree remove <exact-absolute-path>` 时不得传 `--force`，也不得调用影响其他 Worktree 的全局 `git worktree prune`。
8. Cleanup 过程中发生 Drift、命令失败或 Runtime Crash 时允许出现已记录的部分完成结果。Runtime 停止处理后续项目，将 Cleanup Attempt 标记为 `BLOCKED`；Recovery 根据精确路径、Git Worktree Registry 和 Intent/Result 协调，已删除项目不得伪装成仍存在，未删除项目不得自动跳过门禁。
9. Cleanup 永远保留所有 Task/Integration/Apply Branch、`refs/cflow/...` Audit Ref、Commit、SQLite Row、Event、Approval、Artifact、Log、Session 和 Evidence。它不改变 Workflow 终态；`status`/`inspect` 必须区分“Worktree 已清理”与“Branch/证据已删除”。
10. Scratch Directory 删除也必须以记录的 Canonical Path 为精确目标，拒绝空路径、Workspace Root、Repository Root、`CFLOW_HOME` Root、`~`、`/`、未解析变量和 Symlink Escape。CFlow 不跟随目录内指向目标外部的 Symlink。

Cleanup Failure Code 至少包括 `CLEANUP_WORKFLOW_NOT_TERMINAL`、`CLEANUP_ACTIVE_PROCESS`、`CLEANUP_TARGET_DIRTY`、`CLEANUP_FACT_MISMATCH` 和 `CLEANUP_ITEM_FAILED`。这些错误不改变原 Workflow 终态，也不触发 Agent Repair 或 Node Retry。

## 用户交互与功能需求

### 启动与项目识别

> 决策日期：2026-08-02
>
> 决策状态：已确认

Demo 只允许在具有有效 `HEAD`、且当前 `HEAD` 通过 `git symbolic-ref` 附着于一个本地 Branch 的 Git 仓库中创建 Workflow。非 Git 或无有效 HEAD 的目录只提供 `cflow doctor`、帮助和错误说明；Detached HEAD 仓库不得创建新 Workflow，但可以按已保存的 Target/Base 继续或查看该 Project 的既有 Workflow，Apply 仍必须等待用户工作区回到记录的 Target Branch。CFlow 不猜测 Target Branch，不自动执行 `git init`、创建初始 Commit、Checkout Branch 或提供 Planning-only Workflow。

用户在任意项目子目录执行：

```bash
cflow
```

CFlow 首先执行项目发现：

```text
当前目录
  ↓
尝试 git rev-parse --show-toplevel
  ├─ 失败 → 仅允许 doctor/help；禁止创建 Workflow
  └─ 成功 → 检查 HEAD 是否有效
               ├─ 否 → 仅 doctor/help
               └─ 是 → 检查 HEAD 是否附着于本地 Branch
                            ├─ 否 → Detached HEAD；禁止新建，但可查看/恢复既有 Workflow
                            └─ 是 → 使用 Git Repository Root 与当前 Branch 创建上下文
```

随后生成 Project Key。

用户设想的：

```text
/Users/yuancheng/Documents/Code/Resume
→ Users-yuancheng-Documents-Code-Resume
```

可读性很好，但存在路径碰撞：

```text
/a-b/c
/a/b-c
```

二者都会映射成：

```text
a-b-c
```

因此正式规则为：

```text
<可读路径 Slug>--<Canonical Path SHA-256 前十位>
```

例如：

```text
Users-yuancheng-Documents-Code-Resume--6f9e13a8c2
```

Project Key 生成规则：

```text
canonicalPath = realpath(gitRoot)
normalizedPath = Unicode NFC(canonicalPath)
slug = removeLeadingSlash(normalizedPath)
slug = replacePathSeparatorsWithDash(slug)
slug = replaceUnsafeChars(slug, "_")
slug = truncate(slug, 80)
hash = sha256(normalizedPath).substring(0, 10)
projectKey = slug + "--" + hash
```

项目移动后默认会被识别为新项目。用户可以使用：

```bash
cflow project relink <old-project-key>
```

迁移历史 Workflow。

### 首次进入交互

推荐交互如下：

```text
$ cflow

CFlow
Project: /Users/yuancheng/Documents/Code/Resume
Project Key: Users-yuancheng-Documents-Code-Resume--6f9e13a8c2

发现 3 个历史工作流：

  1. [RUNNING] resume-agent-project
     Stage: EXECUTION
     Progress: 7 / 12
     Updated: 2026-08-01 23:41

  2. [PAUSED] add-cflow-plan-check
     Stage: PLAN_CHECK
     Updated: 2026-07-30 18:10

  3. [COMPLETED] refactor-readme
     Completed: 2026-07-28

请选择：
  › 继续历史工作流
    创建新工作流
    查看项目状态
    退出
```

### 历史工作流交互

选择历史 Workflow 后，CFlow 显示：

```text
Workflow: resume-agent-project
Stage: EXECUTION
Status: PAUSED
Plan: CHECKED
Specs: 12
Completed Tasks: 7
Running Tasks: 0
Failed Tasks: 1
Last Event: verification_failed
Last Agent: codex / session 0198...
Integration Branch: cflow/wf-20260801-001/integration
```

可执行操作：

```text
› 继续执行
  查看 Plan
  查看 Specs
  查看 Dynamic Workflow
  查看任务状态
  查看日志
  调整需求或 Plan
  重试失败任务
  从某个阶段重新生成
  取消工作流
```

“调整部分内容”必须明确影响范围：

| 调整类型 | 系统行为 |
|---|---|
| 仅修改说明文字 | 创建新 Plan Revision；Specs 可暂不标记 Stale，但新 Plan 仍须重新 Check/Approval，并通过与现有 Specs 的兼容性校验 |
| 修改目标、范围或验收标准 | Plan 回到 `DRAFT`，Specs 和 Dynamic Workflow 标记为 Stale |
| 修改某个 Spec | 该 Spec 及依赖它的节点标记为 Stale |
| 修改 Agent 或预算 | 不改变业务阶段，只生成新的 Workflow Revision |
| 修改已完成任务 | 不回退历史任务，新增 Repair Spec |

### 新建工作流交互

```text
$ cflow

请选择：
  › 创建新工作流

Workflow 名称：
> add-coupon-exclusion

请描述需求，输入 /done 结束，/cancel 取消：

> 给优惠券系统增加互斥规则。
> 要兼容历史活动，不能明显增加主链路延迟。
> /done

选择需求讨论 Agent：
  › Codex
    Claude Code
    OpenCode（P1 Adapter 可用时）

选择模型配置：
  › 使用 CFlow 默认配置
    使用 Provider 默认模型
    手动指定
```

创建后立即持久化：

```text
stage  = REQUIREMENT_DISCUSSION
status = RUNNING
```

即使 Agent 尚未成功启动，也必须已经存在 Workflow 记录，以便异常后恢复。

### 需求讨论交互

CFlow 使用自己维护的对话循环：

```text
[Codex]
我需要确认以下问题：
1. 互斥是券与券之间，还是券与活动之间？
2. 互斥规则由运营配置还是代码固定？
3. 历史活动是默认不互斥，还是按迁移规则处理？

[You]
券与券、券与活动都可能互斥，由运营配置。
历史活动默认不互斥。

[Codex]
还需要确认配置生效时间和缓存一致性策略……

[You]
/finish
```

特殊命令：

| 命令 | 行为 |
|---|---|
| `/finish` | 要求 Agent 产出最终 Plan |
| `/pause` | 使用两阶段有限停止协议保存 Session、Attempt 与 Git Checkpoint，并将 Workflow 暂停 |
| `/status` | 展示 Workflow 和 Session 状态 |
| `/agent` | 查看或切换 Agent；经协议能力检查后创建继任 Session，并注入版本化 Context Bundle；权限采用目标 Provider 默认配置 |
| `/context` | 查看已注入 Agent 的文件和约束 |
| `/cancel` | 显式确认后逻辑终止当前 Workflow；完整保留 Worktree、Branch、Audit Ref 和证据 |

`/finish` 不等于 Plan 已通过，只表示要求 Discussion Agent 生成 Draft Plan。

### 已确认：两个用户批准门

> 决策日期：2026-08-02
>
> 决策状态：已确认

CFlow 主链路只设置两个显式用户批准门：

1. **Plan Approval**：独立 Plan Checker 返回 `pass` 后，Plan Revision 进入 `CHECKED`，Workflow 暂停。用户检查 Plan 与 Checker 证据后，选择批准、要求修订、返回讨论或取消；只有用户批准后 Plan 才进入 `APPROVED` 并开始 Spec Generation。
2. **Execution Approval**：Specs、Verification Command Catalog 和 Dynamic Workflow 生成、静态验证、Git Commit Preflight 成功并完成 Dry Run 后，Workflow 再次暂停。用户一次性批准固定的 Plan/Spec/Catalog/Dynamic Workflow Revision 与 Hash、Agent Routing/Fallback、Retry/Cost Budget、执行边界以及当前 Commit Preflight Revision/Hash/Fingerprint，然后才进入 `EXECUTION`。界面必须同时展示 Commit Identity/Signing Preflight 摘要，并提示 Agent 使用 Provider 默认权限，CFlow 不提供统一沙箱保证。

Specs 和 Verification Catalog 不设置额外独立批准门，但在 Execution Approval 页面必须完整可检查和可调整。任何调整都会创建新的不可变 Spec/Catalog/Dynamic Workflow Revision，并重新生成 Dry Run；旧 Execution Approval 不得复用。

批准是对精确输入集合的 Compare-and-Swap 决策，不是“以后都同意”：

- Plan 内容、Revision 或 Hash 变化时，Plan Approval 和依赖它的 Execution Approval 均失效，必须重新 Check 和批准。
- Spec、Verification Catalog、Dynamic Workflow、Routing/Fallback 或 Budget 变化时，只使 Execution Approval 失效，必须重新展示 Dry Run 并批准。
- 同一已批准 Spec、Scope、Routing/Fallback Policy 和 Budget 内的新 Attempt、Session Resume、Repair Attempt 及一次受限 Merge Resolution 可以自动执行，不重复打断用户。
- 创建 Repair Spec、新 Dynamic Workflow Revision、扩大 Write Scope、改变 Acceptance、使用未批准 Provider 或突破预算时，Workflow 必须进入 `BLOCKED`/`PAUSED` 并重新经过 Execution Approval。
- 执行期间 Commit Policy 合法漂移时，只暂停并确认新 Preflight 的精确 Revision/Hash/Fingerprint；不使 Plan 或 Execution Approval 失效。该异常确认不是第三个常规主链路批准门。
- 漂移窗口 Commit 导致新 Repair Spec 或 Dynamic Workflow Revision 时，新的 Replacement Execution Approval 同时确认其精确 Commit Preflight；不得紧接着对同一 Fingerprint 再展示 `COMMIT_POLICY` Gate。普通漂移与 Apply 不适用该合并规则。
- Ctrl+C 或正常退出停留在批准门时，Workflow 保存为 `PAUSED`；`cflow resume` 必须返回同一门并重新校验引用 Hash，不能自动越过。

所有批准与拒绝均是 append-only 用户决策事实，由 CFlow Runtime 写入 SQLite 并追加 Event；Agent 无权批准自身产物。

### Plan Check 交互

Plan 生成成功后：

```text
Plan 已生成：
~/.cflow/projects/.../workflows/wf-.../plan/plan-001.md

Plan Status: DRAFT

请选择 Plan Check Agent：
  › Claude Code
    Codex
    OpenCode（P1 Adapter 可用时）
```

Checker 必须输出结构化结果：

```json
{
  "decision": "pass",
  "summary": "Plan 已包含范围、非目标、文件边界、任务方向和验收方法。",
  "blockingGaps": [],
  "nonBlockingSuggestions": [
    "建议进一步明确缓存灰度指标"
  ],
  "confidence": 0.91
}
```

可用决策：

| Decision | 行为 |
|---|---|
| `pass` | Plan 从 `DRAFT` 转为 `CHECKED`，Workflow 暂停等待用户批准 |
| `needs_discussion` | 返回需求讨论阶段，并将 Gap 注入原 Discussion Session |
| `needs_revision` | 创建独立 Plan Revision Session |
| `reject` | 标记 Plan Check Failed，要求人工选择下一步 |

Checker `pass` 后展示：

```text
Plan Check: PASS
Plan Revision: 1
Plan SHA-256: 8a60...
Checker: claude / session c680...

› 批准 Plan 并生成 Specs
  查看 Plan
  查看 Checker 证据
  要求修订
  返回需求讨论
  暂停
  取消 Workflow
```

只有“批准 Plan 并生成 Specs”会写入 Plan Approval 并将 Plan Status 转为 `APPROVED`。

### Spec、Workflow 与执行交互

Plan Approved 后自动生成 Specs：

```text
Plan 已由用户批准，正在拆分 Specs…
```

Spec 生成后展示：

```text
共生成 12 个 Specs：

Parallel Group A:
  S01 analyze-domain-model
  S02 analyze-cache-impact
  S03 design-test-fixtures
  S04 analyze-admin-api

Sequential:
  S05 define-exclusion-model   depends on S01,S04
  S06 implement-rule-engine    depends on S05
  S07 implement-cache          depends on S02,S05
  ...
```

Specs 静态校验通过后，CFlow 自动编译 Dynamic Workflow；此处不增加单独批准门。用户仍可查看或调整 Specs，任何调整都会生成新 Revision 并重新编译。

Workflow 生成并验证通过后：

```text
Dynamic Workflow Ready

Plan: revision 1 / 8a60...
Specs: revision 1 / 51bd...
Verification Catalog: revision 1 / a913...
Dynamic Workflow: revision 1 / f092...
Tasks: 12
Max Parallelism: 4
Estimated Agent Runs: 18
Retry Budget: 8
Agent Routing: codex implementation; claude review
Fallback Providers: codex, claude
Agent Permission Mode: provider defaults (trusted; not sandboxed by CFlow)
Git Commit Identity: Yuan Cheng <redacted@example.com>
Commit Signing: enabled / ssh / SHA256:ab12... / preflight passed
Target Branch: main
Integration Branch: cflow/wf-20260802-001/integration

› 批准以上 Revision、命令、路由、预算和 Provider 默认权限信任边界并开始执行
  Dry Run
  查看 Specs
  查看 Verification Commands
  查看 Provider 版本与默认权限风险说明
  查看 Git Commit Identity/Signing Preflight
  查看 Workflow
  调整 Agent 路由
  调整预算
  暂停
```

## 状态机与持久化模型

### Workflow 双层状态

Workflow 使用两个字段：

```text
type WorkflowStage =
  | "REQUIREMENT_DISCUSSION"
  | "PLAN_GENERATION"
  | "PLAN_CHECK"
  | "SPEC_GENERATION"
  | "WORKFLOW_GENERATION"
  | "EXECUTION"
  | "FINAL_VERIFICATION"
  | "COMPLETED";

type RuntimeStatus =
  | "PENDING"
  | "RUNNING"
  | "PAUSED"
  | "BLOCKED"
  | "FAILED"
  | "SUCCEEDED"
  | "CANCELLED";
```

典型组合：

| Stage | Status | 含义 |
|---|---|---|
| `REQUIREMENT_DISCUSSION` | `RUNNING` | 正在与 Agent 讨论 |
| `PLAN_CHECK` | `PAUSED` | 等待选择 Checker，或 Checker 已通过后等待用户批准固定 Plan Revision |
| `WORKFLOW_GENERATION` | `PAUSED` | Specs 与 Dynamic Workflow 已验证，等待统一 Execution Approval |
| `EXECUTION` | `RUNNING` | Dynamic Workflow 正在执行 |
| `EXECUTION` | `PAUSED` | 用户主动暂停，或 Commit Policy 漂移后等待确认精确新 Preflight |
| `EXECUTION` | `BLOCKED` | Worktree、依赖、Agent 执行事实或人工决策阻塞 |
| `FINAL_VERIFICATION` | `BLOCKED` | 最终验收失败且无法自动继续，等待用户确认 Repair |
| `COMPLETED` | `SUCCEEDED` | Workflow 完成 |
| 任意未完成 Stage | `CANCELLED` | 用户取消已安全完成；原 Stage 保留用于说明取消发生位置 |

### 主状态转换

```text
REQUIREMENT_DISCUSSION
    ↓ finish
PLAN_GENERATION
    ↓ plan written as DRAFT
PLAN_CHECK
    ├─ needs_discussion → REQUIREMENT_DISCUSSION
    ├─ needs_revision   → PLAN_GENERATION
    └─ pass → PAUSED / Plan CHECKED
         ↓ user approves exact Plan Revision + Hash
SPEC_GENERATION
    ↓ specs validated
WORKFLOW_GENERATION
    ↓ workflow validated → PAUSED / Execution Approval
    ↓ user approves exact Plan/Spec/Catalog/Workflow Revisions + Routing + Budget
      and acknowledges Provider-default permission trust boundary
EXECUTION
    ├─ task failure and retry available → EXECUTION
    ├─ commit policy drift → PAUSED / exact Preflight confirmation
    ├─ recoverable external issue       → BLOCKED
    └─ all tasks merged
         ↓
FINAL_VERIFICATION
    ├─ verification failed → EXECUTION with Repair Specs
    └─ passed
         ↓
COMPLETED
```

普通运行控制只允许：

```text
RUNNING → PAUSED
RUNNING → BLOCKED
PAUSED  → RUNNING
BLOCKED → RUNNING
```

用户 Cancel 不属于普通暂停/恢复转换：任意非终态只能在 `WORKFLOW_CANCEL_REQUESTED` 已落盘、受管进程全部退出且外部副作用已协调后转为 `CANCELLED`；Cancel Intent 暂时无法完成时先进入 `BLOCKED`，Recovery 最终只能完成取消。

任意非终态只有在 Runtime 无法安全判断或恢复权威事实时才能进入 `FAILED`。`FAILED`、`CANCELLED` 和 `COMPLETED/SUCCEEDED` 均为 Workflow 终态，不能直接恢复；需要继续时创建新的 Workflow Revision 或 Repair Workflow。

### 已确认：Retry 耗尽与失败终态语义

> 决策日期：2026-08-02
>
> 决策状态：已确认

CFlow 将“某次执行失败”“业务节点无法自动继续”和“Runtime 自身不可安全恢复”严格分开：

1. 每次 Node Attempt 都是不可变的审计事实。Attempt 失败后不得清空错误、覆盖结果或重新打开同一 Attempt。
2. Failure 可自动重试且预算仍充足时，Node 返回 `READY`；Scheduler 启动重试时创建新的、连续编号的 Attempt。Workflow 保持 `RUNNING`。
3. Failure 不可自动重试，或 Node/Workflow Retry Budget 已耗尽时，当前 Node 进入 `FAILED`，同时创建可解释的 Blocking Finding。若没有其他运行中 Node，Workflow 立即进入 `BLOCKED`；若存在并行运行中的 Node，则先按下述 Quiescing 策略收敛，再进入 `BLOCKED`，等待用户选择 Repair Spec、新 Workflow Revision 或取消。
4. 从 `BLOCKED` 继续时不得复活已经失败的 Node 或 Attempt。Repair 必须生成新的不可变 Spec Revision 和 Dynamic Workflow Revision，以新 Repair/Verify/Merge Node 替代活动图中的失败路径；下游依赖只能由新路径的成功证据满足。需求或方案发生变化时生成新的 Workflow Revision。旧图、旧 Node 和旧失败证据始终保留。
5. Workflow `FAILED` 只用于 Runtime 级不可恢复故障，例如无法协调的权威 Artifact Hash 冲突、数据库完整性损坏、状态机不变量破坏，或关键事实缺失到无法安全判断下一步。Provider 退出、测试失败、Review 不通过、Scope Violation、Merge Conflict 和 Retry 耗尽本身都不得把 Workflow 置为 `FAILED`。

`DIRTY_TASK_WORKTREE` 在预算内属于 Execute Node 的可重试失败：新 Attempt 使用同一个 Task Branch/Worktree 和新的独立 Repair Session，不能据此提前创建 Repair Spec、修改 DAG 或满足任何下游依赖。只有预算耗尽进入 `BLOCKED` 后，才适用第 4 条的新 Repair Spec/Workflow Revision 流程。

#### 已确认：并行失败后的 Quiescing

> 决策日期：2026-08-02
>
> 决策状态：已确认

当一个 Node 发生不可自动重试失败或 Retry Budget 耗尽，而其他 Node 的 Attempt 已经在并行运行时，CFlow 采用“停止新调度、等待当前 Attempt 收敛”：

1. Runtime 在记录失败 Node、Blocking Finding 和 `WORKFLOW_QUIESCE_REQUESTED` 的同一事务中，将当前 Run 置为 `QUIESCING`，固定当时所有 In-flight Node/Attempt ID，并关闭该 Run 的 Dispatch Gate。Workflow `runtime_status` 暂时保持 `RUNNING`，但 CLI 必须显示 `Quiescing due to <finding>`，不能伪装成正常推进。
2. Dispatch Gate 关闭后，不得启动任何新 Node、Node Retry、Repair Attempt、Provider Session、Verification、Review、Merge、Checkpoint Agent 或后继 DAG 节点。已经 Ready/Pending 的 Node 保持原状态；依赖满足也不能越过 Gate。
3. 只有 Quiesce Snapshot 中已经 `RUNNING` 的 Attempt 可以继续到自身终点，并继续受原 Timeout、Budget、Commit/Clean/Scope、Verification 和副作用门禁约束。该 Attempt 成功时可以把自己的 Node 标记为 `SUCCEEDED`；失败时保留不可变失败证据，并按正常规则投影为 `READY` 或 `FAILED`，但本 Run 不启动 Retry。
4. “完成当前 Attempt”不包含启动后继节点。Coding Attempt 即使产生合法 Commit，也不会在本 Run 新启动 Verify/Review/Merge；已经运行的 Verify 或 Merge Attempt 则可以完成并记录实际结果。由此产生的所有 Commit 和 Integration 变化继续受已有 Preflight、Append-only 和验收规则约束。
5. 所有 Snapshot Attempt 收敛后，Runtime 在同一事务将 Run 置为 `BLOCKED`、Workflow 置为 `BLOCKED`，追加 `WORKFLOW_QUIESCED`，保存已完成、待验收、Ready、Failed 和未启动 Node 清单，然后释放 Workflow Owner 与 Project Writer。
6. Quiescing 不新增全局等待时限；每个 In-flight Attempt 已有的硬 Timeout 构成上限。用户仍可执行 Ctrl+C 终止等待或执行 Cancel。Ctrl+C 不得清除原 Blocking Finding；停止完成后 Workflow 直接进入 `BLOCKED`，而不是可继续调度的普通 `PAUSED`。
7. Quiescing 中发生新的 Node Failure 时追加独立 Finding，但不重复创建 Quiesce 流程。其自动 Retry 即使预算充足也延后到后续用户处理阻塞之后；不会因为兄弟任务收敛而自动恢复 Scheduler。
8. Quiescing 期间禁止生成或自动批准 Repair Spec/Workflow Revision。用户只有在 Workflow 已经稳定 `BLOCKED` 后，才能检查全部收敛证据并选择受限 Repair、新 Revision 或 Cancel。

如果触发失败时没有其他 In-flight Attempt，Runtime 可以在同一事务直接将 Run/Workflow 置为 `BLOCKED` 并追加等价摘要，无需经过空的 `QUIESCING` 状态。可重试且预算充足的普通 Attempt Failure 不触发 Quiescing，其他并行 Node 继续正常执行。

因此，Workflow `FAILED` 不存在 `FAILED → RUNNING` 转换，只允许查看、导出诊断证据、显式清理衍生资源，或以已验证事实为基线创建新的 Workflow Revision；清理不改变终态，任何修复都不得改写原 Workflow 的终态历史。

### Plan 状态

```text
type PlanStatus =
  | "DRAFT"
  | "CHECKING"
  | "CHECKED"
  | "APPROVED"
  | "STALE"
  | "REJECTED";
```

“Draw”统一修正为“Draft”。

Plan 状态约束：

```text
DRAFT → CHECKING → CHECKED → APPROVED
CHECKING → DRAFT
CHECKING → REJECTED
CHECKED → STALE
APPROVED → STALE
STALE → DRAFT
REJECTED → DRAFT（创建新 Plan Revision，旧 Revision 保持 REJECTED）
```

`CHECKED` 只证明独立 Checker 认为 Plan 可执行，`APPROVED` 才证明用户批准了该精确 Plan Revision 与 Hash。两者不得合并，也不得由同一个 Agent 决定。

Plan Checker 通过前必须满足：

| 检查项 | 要求 |
|---|---|
| 背景与目标 | 明确要解决什么问题 |
| 范围与非目标 | 明确做什么和不做什么 |
| 约束 | 兼容性、性能、权限、部署等边界 |
| 仓库分析 | 涉及模块、现状和关键调用链 |
| 技术方案 | 至少有推荐方案及关键取舍 |
| 文件边界 | 可定位到模块或目录 |
| 验收标准 | 可通过命令、测试或明确 Review 判断 |
| 风险与回滚 | 不能只写“无” |
| 未决问题 | 不得存在阻断执行的问题 |
| Plan 元信息 | Workflow、Session、版本、Git Base 完整 |

### 已确认的状态职责模型

> 决策日期：2026-08-02
>
> 决策状态：已确认

CFlow 采用“最小权威状态机 + 派生投影”，避免 Workflow、Task 和 Node 同时声明互相矛盾的结果。

权威状态机只有以下四类：

1. **Workflow**：`stage` 描述业务所处阶段，`runtime_status` 描述运行、暂停、阻塞、失败、成功或取消等正交状态。
2. **Artifact Revision**：Plan 等需要门禁的 Artifact Revision 拥有 `DRAFT`、`CHECKING`、`CHECKED`、`APPROVED`、`STALE`、`REJECTED` 等审核状态；Artifact 文件本身保持不可变，状态保存在 SQLite。`CHECKED` 属于独立 Agent 结论，`APPROVED` 属于用户决定。
3. **Node**：Scheduler 的实际执行状态，是 Task 执行进度的权威来源。
4. **Session / Run**：分别描述 Provider Session 和一次 Runtime Run 的进程、退出与恢复状态。

Node 状态：

```text
type NodeStatus =
  | "PENDING"
  | "READY"
  | "RUNNING"
  | "SUCCEEDED"
  | "FAILED"
  | "CANCELLED"
  | "SKIPPED";
```

Node 的 Retry 不通过 `RETRY_WAIT` 复用业务状态。失败 Attempt 保持终态；预算内重试时 Node 返回 `READY`，Scheduler 再创建新的有编号 Attempt。预算耗尽或 Failure 不可自动重试时 Node 进入 `FAILED`；无其他 In-flight Attempt 时 Workflow 直接 `BLOCKED`，否则 Run 先 `QUIESCING` 再使 Workflow `BLOCKED`。Node 只有在当前 Attempt 成功并具备该节点类型要求的证据后才能进入 `SUCCEEDED`。

Session 与 Run 状态：

```text
type SessionStatus =
  | "STARTING"
  | "ACTIVE"
  | "INTERRUPTED"
  | "PAUSED"
  | "COMPLETED"
  | "FAILED"
  | "CANCELLED"
  | "LOST";

type RunStatus =
  | "STARTING"
  | "RUNNING"
  | "QUIESCING"
  | "STOPPING"
  | "INTERRUPTED"
  | "BLOCKED"
  | "SUCCEEDED"
  | "FAILED"
  | "CANCELLED";
```

`QUIESCING` 表示因确定性 Blocking Failure 停止新调度、仅等待快照中的当前 Attempt 收敛；`STOPPING` 表示受控停止正在终止子进程。两者都不表示子进程已经退出。Run `BLOCKED` 表示收敛完成且 Workflow 已稳定阻塞；`INTERRUPTED` 是不可变 Attempt/Run 结果，不等于 `FAILED` 或 `CANCELLED`。Node 本身不增加 `PAUSED` 状态：普通中断协调完成后 Node 回到 `READY`，由 Workflow 的 `PAUSED` 阻止调度。

### Task 进度投影

Task 不拥有独立的权威状态机。CLI 展示的 Task 进度由 Node、Git 和 Verification 证据派生：

| 投影 | 派生条件 |
|---|---|
| `PENDING` | Execute Node 尚未 Ready |
| `READY` | 依赖满足且 Execute Node 为 Ready |
| `RUNNING` | 任一当前 Task Node 正在运行 |
| `IMPLEMENTED` | Execute Node 成功，存在合法 Commit，Worktree 干净且 Scope 检查通过 |
| `VERIFYING` | Verify Node 正在运行 |
| `VERIFIED` | 确定性检查和独立 Reviewer 均通过 |
| `MERGED` / `COMPLETED` | Merge Node 成功，Task Commit 已存在于 Integration Branch |
| `BLOCKED` | Workflow 或相关 Node 存在需要人工处理的 Finding |
| `FAILED` | 当前 Node 失败且 Retry Budget 已耗尽 |
| `CANCELLED` | Workflow 或相关 Node 已取消 |

Task 投影可缓存到 SQLite 以加速查询，但必须可从权威事实重建，不能作为状态转换输入。历史 `COMPLETED` Task 不回退；发现缺陷时生成新的 Repair Spec 和 Nodes。

### 全局目录结构

```text
~/.cflow/
├── config.yaml
├── cflow.db
├── backups/
│   └── db/
│       └── <migration-id>/
│           ├── cflow.db
│           └── backup-manifest.json
├── logs/
├── locks/
│   ├── db-schema.lock
│   ├── projects/
│   │   └── <project-key>.writer.lock
│   └── workflows/
│       └── <workflow-id>.owner.lock
├── prompts/
│   ├── requirement-discussion.md
│   ├── plan-finalize.md
│   ├── plan-check.md
│   ├── spec-generate.md
│   ├── workflow-optimize.md
│   ├── task-implement.md
│   ├── task-review.md
│   └── merge-conflict.md
├── projects/
│   └── Users-yuancheng-Documents-Code-Resume--6f9e13a8c2/
│       ├── project.yaml
│       └── workflows/
│           └── wf-20260802-001-add-coupon-exclusion/
│               ├── workflow.yaml
│               ├── events.jsonl              # 由 SQLite Events 可重建的审计导出
│               ├── plan/
│               │   ├── plan-001.md
│               │   └── checks/
│               │       └── check-001.json
│               ├── specs/
│               │   └── 001/
│               │       ├── index.yaml
│               │       ├── S01.md
│               │       └── S02.md
│               ├── dynamic/
│               │   ├── workflow-001.yaml
│               │   └── compile-report.json
│               ├── verification/
│               │   ├── catalog-001.yaml
│               │   └── policy-report-001.json
│               ├── routing/
│               │   └── routing-policy-001.yaml
│               ├── git/
│               │   ├── commit-preflight-001.json
│               │   └── commit-preflight-002.json
│               ├── cleanup/
│               │   └── cleanup-plan-001.json
│               ├── sessions/
│               │   ├── discussion.json
│               │   ├── plan-check.json
│               │   ├── task-S01-attempt-1.json
│               │   └── context/
│               │       └── context-001.json
│               ├── runs/
│               │   └── run-001/
│               │       ├── state.snapshot.json # 非权威诊断快照
│               │       ├── logs/
│               │       ├── verification/
│               │       ├── tmp/                # 明确标记的可清理 Scratch
│               │       └── final-report.md
│               └── artifacts/
└── worktrees/
    └── <project-key>/
        └── <workflow-id>/
            ├── planning/
            ├── integration/
            ├── S01/
            └── S02/
```

### 事实来源边界

> 决策日期：2026-08-02
>
> 决策状态：已确认

CFlow 按事实类型分治，不设一个覆盖所有领域的“万能事实来源”：

| 数据 | 事实来源 |
|---|---|
| Workflow、Plan、Task、Node、Session、Run 的当前运行状态、Blocking Finding 与 Lease 元数据 | SQLite |
| 当前数据库 Schema Version 与已应用 Migration 链 | SQLite `schema_migrations`；`PRAGMA user_version` 仅作一致性 Guard |
| Migration 前数据库备份与校验元数据 | `backups/db/<migration-id>/cflow.db` + 不可变 `backup-manifest.json`；SQLite Migration Row 保存 Manifest Path/Hash，二者必须一致 |
| Artifact Reader 支持范围 | 当前 CFlow 二进制内嵌的 Artifact Compatibility Registry；Artifact 自身的 `schema_version` 只声明格式，不自行证明兼容 |
| 活进程互斥事实 | `locks/` 下由 OS 管理生命周期的 Advisory Lock；SQLite Lease 不能单独证明进程仍持有锁 |
| 权威状态变化序列 | SQLite `events` 表 |
| 需求与技术方案 | 不可变且版本化的 `plan/plan-<revision>.md` |
| Task 定义和依赖 | 不可变且版本化的 `specs/<revision>/` Artifact |
| 动态执行图 | 不可变且版本化的 `dynamic/workflow-<revision>.yaml` |
| 可执行 Verification Command 定义 | 不可变且版本化的 `verification/catalog-<revision>.yaml`；SQLite 保存活动 Revision、Hash 和路径 |
| Agent 路由与 Fallback 策略 | 不可变且版本化的 `routing/routing-policy-<revision>.yaml`；Execution Approval 固定对应 Hash |
| Git Commit Identity/Signing Preflight | 不可变且版本化的 `git/commit-preflight-<revision>.json`；SQLite 保存活动 Revision、Hash 和关联 Commit/Run |
| Cleanup 目标清单与执行结果 | 不可变 `cleanup/cleanup-plan-<id>.json` + SQLite `cleanup_attempts/cleanup_items` + Intent/Result Events；目录和 Git Worktree Registry 是删除完成与否的外部事实 |
| 用户 Plan/Execution、Apply Catalog、Commit Policy Approval 与拒绝 | SQLite append-only `approvals` 表；`events` 表保存同事务状态变化序列 |
| Agent Session ID、状态、关联关系、Provider/CLI Version、脱敏 argv、cwd 和默认权限风险标记 | SQLite `sessions` 表及其 `metadata_json` |
| 受管 Provider/Verification/Git 子进程身份 | SQLite `managed_processes` 表保存 PID、Process Start Token、Process Group 和状态；操作系统进程事实用于恢复校验 |
| Agent 完整已脱敏事件、对话和摘要证据 | `sessions/*.json` Artifact，由 SQLite 保存 Hash、位置与 Redactor/Rule Revision；不是 Raw Provider Byte Stream |
| Session 降级恢复和跨 Provider 交接上下文 | 不可变且版本化的 `sessions/context/context-<revision>.json`，由 SQLite Session 记录 Revision、Hash 和位置 |
| 实际代码事实 | Git Commit、Task/Integration Branch、Diff、Worktree，以及 `refs/cflow/.../attempts/...` 下的 Attempt 审计 Ref |
| 验收原始证据 | `runs/<run-id>/verification/`，由 SQLite 保存摘要、Hash 和位置 |
| 用户可读最终结果 | `final-report.md`，由权威状态和证据生成 |
| 审计导出 | `events.jsonl`，由 SQLite `events` 表按 Sequence 重建 |

约束如下：

- SQLite 不复制 Plan、Spec、Workflow、Verification Catalog、Routing Policy、Commit Preflight 或验证证据正文，只保存当前活动 Revision、内容 Hash、Artifact 路径和运行态。
- 版本化 Artifact 写入后不可原地修改；调整内容必须生成新 Revision。生命周期状态不写回 Artifact 正文。
- `events.jsonl` 不是第二事实来源，不参与状态恢复判断；导出中断或损坏时可由 SQLite 重新生成。
- 进入任一事实来源前先执行统一 Redaction；SQLite、Artifact、Log、Export 和重建后的 `events.jsonl` 都只能包含已脱敏内容。任何声称“Raw”的文件不属于允许的数据模型。
- CFlow 自建目录为 `0700`、敏感文件为 `0600`。权限、Owner、Symlink 或文件系统语义不能证明安全时，不得启动 Provider 驱动的 Mutating Workflow；纯只读诊断与止损命令仍可运行。
- `workflow.yaml` 仅作为 Workflow 身份、仓库基线和 Artifact 引用清单，不保存 `stage`、`runtime_status`、`plan_status` 或 `active_run_id` 等可变运行态。
- Recovery 必须分别检查 SQLite、Artifact Hash、Git/Worktree 和验证证据，再由 Runtime 记录 Reconciliation Finding；任何单一来源都不能自行覆盖其他事实。

### 已确认：Forward-only SQLite Migration 与不可变 Artifact Schema

> 决策日期：2026-08-02
>
> 决策状态：已确认

CFlow Demo 必须支持已有本地状态跨版本升级，但不实现 Down Migration，也不通过重建或原地改写历史 Artifact 来伪造升级。迁移边界如下：

1. CFlow 二进制内嵌按版本连续编号的 Forward-only SQLite Migration，以及每条 Migration 的稳定 ID 和 SHA-256。已应用 Migration 的内容和校验值不可在后续版本中修改。
2. `schema_migrations` 是数据库 Schema Version 的权威记录；`PRAGMA user_version` 只作为一致性 Guard。两者不一致、已应用 Migration 校验值不匹配或 Migration 链中断时必须 Fail-closed，不得猜测或跳版本。
3. 普通数据库使用者在整个数据库使用期持有共享 DB Schema Lock；Migration 在打开可写数据库前先取得同一路径的排他 DB Schema Lock。这样新二进制不得在旧 Runtime 仍使用数据库时升级 Schema。共享 Lock 之间不互斥，因此不同 Project 的正常运行仍可并行。
4. 已知且连续的待执行 Migration 由首个需要可写状态的前台命令在启动时自动执行；所有只读命令都不得触发 Migration。当前二进制仍有对应旧 Schema Reader 时，`list`、`status`、`inspect`、`logs` 和 `export` 可以继续只读；没有安全 Reader 时只允许 `help` 和 `doctor` 报告版本问题。Migration 前必须展示 From/To Version 和备份位置。没有连续迁移路径时阻塞，不允许创建空库覆盖旧库。
5. Migration 必须先使用 SQLite 一致性备份机制生成 `0600` 的数据库备份，不能直接复制处于 WAL 状态的文件。备份目录为 `0700`，Manifest 以 `0600` 原子写入，并固定源/目标 Schema Version、CFlow Build Version、数据库 Hash/Size、Migration ID/Checksum 链、创建时间和备份路径。
6. 只有源数据库和备份都通过完整性校验、且 Backup Manifest 可回读并校验后，才允许开始迁移。Demo 不自动删除迁移备份，也不因迁移失败自动覆盖现有数据库；恢复路径和备份位置必须明确报告给用户。
7. 所有待执行 SQLite DDL/DML 和对应 `schema_migrations` Row 在一个 `BEGIN IMMEDIATE` 事务中提交。Migration 事务不得修改 Artifact、Git、Worktree 或启动 Provider；提交前必须再次运行外键与数据库完整性检查。
8. 崩溃发生在 Commit 前时 SQLite 回滚整个 Migration，下一次启动验证备份与版本后幂等重试；Commit 后以 `schema_migrations` 事实识别完成，不重复执行。若 DB、Version、Checksum 或 Manifest 无法确定地归入任一状态，则以 `DATABASE_MIGRATION_INCOMPLETE` 阻塞，不自动恢复备份。
9. 数据库 Schema Version 高于当前二进制支持的最大版本时返回 `DATABASE_SCHEMA_TOO_NEW`。旧二进制不得执行 Mutating Command、不得 Down Migrate；`doctor` 仍可只读报告版本和兼容性，但不得按未知 Schema 解释或修改业务表。
10. Plan、Spec、Dynamic Workflow、Verification Catalog、Routing Policy、Session、Evidence 等版本化 Artifact 必须包含 `schema_version`，写入后永久不可变。每种 Artifact 由内嵌 Compatibility Registry 显式列出当前 Reader 支持的历史版本；Reader 在反序列化正文前先验证 Type、Version、Hash 和路径边界。
11. SQLite Migration 不得原地升级 Artifact。新格式确实需要新语义时，只能通过正常的 Workflow Revision 与既有 Approval Gate 生成新的派生 Artifact，旧 Revision 继续保留。发布新 CFlow 版本前，必须证明其 Reader 能读取所有仍受支持的历史版本；不支持时以 `ARTIFACT_SCHEMA_UNSUPPORTED` 阻塞相关 Workflow，禁止 Best-effort 解析。
12. Migration 与 Compatibility Check 在普通 Workflow Recovery/Reconciliation 之前完成；未证明数据库和所引用 Artifact 可兼容读取前，不得恢复 Scheduler、Provider、Retry、Merge 或 Apply。

最低数据库记录：

```sql
CREATE TABLE schema_migrations (
    version INTEGER PRIMARY KEY,
    migration_id TEXT NOT NULL UNIQUE,
    migration_sha256 TEXT NOT NULL,
    cflow_version TEXT NOT NULL,
    backup_manifest_path TEXT,
    backup_manifest_sha256 TEXT,
    applied_at TEXT NOT NULL
);
```

全新数据库的 Baseline Row 不需要备份，两个 Backup 字段为空；从任何既有数据库升级时两者都必须存在且可验证。

Migration Failure Code 至少包括 `DATABASE_SCHEMA_TOO_NEW`、`DATABASE_MIGRATION_PATH_MISSING`、`MIGRATION_CHECKSUM_MISMATCH`、`DATABASE_MIGRATION_FAILED`、`DATABASE_MIGRATION_INCOMPLETE` 和 `ARTIFACT_SCHEMA_UNSUPPORTED`。这些错误不消耗 Node Retry Budget，也不得触发 Agent Repair；用户修复环境或安装兼容二进制后重新进入，Runtime 再从数据库、备份 Manifest 和 Artifact Hash 协调事实。

### 已确认：Project Writer Lease 与 Workflow Owner

> 决策日期：2026-08-02
>
> 决策状态：已确认

CFlow Demo 允许不同 Git Project 并行运行，但同一个 Project 同时只允许一个 Mutating Runtime：

- `list`、`status`、`inspect`、`doctor` 等只读操作不获取 Project Writer Lease，即使项目正在执行也必须可用。
- 创建/修改 Workflow、驱动 Agent Session、生成 Artifact、创建或删除 Worktree、调度 Node、写 Integration Ref、执行 Recovery 或 Apply 都必须持有 Project Writer Lease。
- 一个 Mutating Runtime 获得 Project Writer 后，还必须获得目标 Workflow Owner；创建 Workflow 时先持有 Project Writer，Workflow ID 落库后再获得 Owner。
- Project Writer 由协调 Runtime 持有。其并发 Task Agent、Verification 和子进程共享这一 Owner，不各自竞争 Project Lease；Workflow 内仍按 DAG 和 Resource Lock 并行。
- Workflow 进入 `PAUSED`、`BLOCKED`、终态或当前前台命令正常结束时，必须先按两阶段有限停止协议终止所有自动化受管子进程并持久化 Checkpoint，随后才释放 Workflow Owner 和 Project Writer。Demo 不把仍在执行的自动化子进程作为后台 Job 安全脱离；原生交互 Attach 的终端脱离语义属于 P1。另一个 Workflow 随后可以在同一项目运行；固定 Base Commit、独立 Integration Branch 和 Apply Target Drift 规则仍然有效。
- 不同 Project 的 Writer 可以并行；它们只会在短 SQLite 事务上由数据库正常串行化，不使用全局 CFlow Writer Lock。

Lease 采用 OS Advisory Lock + SQLite 元数据双层机制：

1. OS Lock 是活进程互斥依据，进程退出或崩溃时由操作系统释放。
2. SQLite `leases` 保存 Owner、PID、Process Start Token、Run、Heartbeat 和诊断信息，用于 CLI 展示、审计和恢复，不得仅因 Heartbeat 超时就强占仍被 OS 持有的锁。
3. 新 Runtime 只有先成功获得 OS Lock，且确认旧 PID/Process Start Token 已不存在或不匹配，才能把残留 Lease 标记为 Stale 并开始 Reconcile。PID 必须与 Process Start Token 一起比较，避免 PID 重用。
4. OS Lock 仍被持有时，即使 Heartbeat 超时也只报告 `PROJECT_BUSY_OR_HUNG`，不得自动 Steal、Kill 或提供 Demo 级 `--force` 绕过。
5. `CFLOW_HOME` 必须位于支持可靠 Advisory Lock 的本地文件系统；检测到不支持或无法验证的网络文件系统时 `doctor` 报错，Demo 不承诺其锁语义。
6. Provider、Verification 和 Git 子进程必须记录 PID、Process Start Token 与 Process Group；Lock 文件描述符不得继承给子进程。协调 Runtime 崩溃后，即使 OS Lock 已释放，只要仍存在匹配身份的受管孤儿子进程，新 Runtime 就必须记录 Project 级 `ORPHAN_CHILD_PROCESS` Finding，将 Project Mutation 置为 Quarantined 并保持 Workflow `BLOCKED`，不得开始任何 Workflow 的新 Mutating Action 或自动 Kill。后续 Writer Acquisition 必须重新检查进程事实，确认孤儿进程已退出并完成 Reconcile 后才能解除 Quarantine。

固定锁顺序：

```text
DB Schema Lock（普通数据库使用者共享持有；Migration 排他持有）
→ Project Writer
→ Workflow Owner
→ Integration / Apply Lock
→ 按名称排序的 Node Resource Locks
```

任何代码路径不得逆序获取。Apply 使用同一个 Project Writer，并通过 Target HEAD Compare-and-Swap 防止释放 Lease 后的外部 Git Drift。

Project Busy 时，CLI 展示持有者的 Workflow、Run、PID、开始时间和最近 Heartbeat，并允许用户进入只读 Status/Inspect 或退出；不得静默等待或夺锁。

SQLite 适合保存运行态和索引，因为单个事务具有 ACID 属性，即使发生程序、操作系统或电源中断，也能保证事务内修改全部生效或全部不生效。WAL 模式还能允许读写并发，但仍需正确处理 `SQLITE_BUSY`。citeturn6view0turn6view2

推荐初始化：

```sql
PRAGMA journal_mode = WAL;
PRAGMA foreign_keys = ON;
PRAGMA busy_timeout = 5000;
PRAGMA synchronous = NORMAL;
```

### 核心数据库表

```sql
CREATE TABLE projects (
    id TEXT PRIMARY KEY,
    project_key TEXT NOT NULL UNIQUE,
    canonical_path TEXT NOT NULL UNIQUE,
    display_name TEXT NOT NULL,
    git_root TEXT NOT NULL,
    git_remote TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    last_opened_at TEXT NOT NULL
);

CREATE TABLE workflows (
    id TEXT PRIMARY KEY,
    project_id TEXT NOT NULL,
    name TEXT NOT NULL,
    stage TEXT NOT NULL,
    runtime_status TEXT NOT NULL,
    plan_status TEXT,
    active_run_id TEXT,
    target_branch TEXT,
    base_commit TEXT,
    initial_worktree_dirty INTEGER NOT NULL DEFAULT 0,
    initial_dirty_fingerprint TEXT,
    integration_branch TEXT,
    cancel_requested_at TEXT,
    cancelled_at TEXT,
    cancelled_by TEXT,
    cancel_reason TEXT,
    revision INTEGER NOT NULL DEFAULT 1,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    FOREIGN KEY(project_id) REFERENCES projects(id)
);

CREATE TABLE workflow_artifact_refs (
    workflow_id TEXT NOT NULL,
    artifact_type TEXT NOT NULL,
    active_revision INTEGER NOT NULL,
    artifact_path TEXT NOT NULL,
    artifact_sha256 TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY(workflow_id, artifact_type),
    FOREIGN KEY(workflow_id) REFERENCES workflows(id)
);

CREATE TABLE approvals (
    id TEXT PRIMARY KEY,
    workflow_id TEXT NOT NULL,
    gate_type TEXT NOT NULL,
    decision TEXT NOT NULL,
    actor TEXT NOT NULL,
    plan_revision INTEGER NOT NULL,
    plan_sha256 TEXT NOT NULL,
    specs_revision INTEGER,
    specs_sha256 TEXT,
    verification_catalog_revision INTEGER,
    verification_catalog_sha256 TEXT,
    dynamic_workflow_revision INTEGER,
    dynamic_workflow_sha256 TEXT,
    routing_policy_sha256 TEXT,
    budget_policy_sha256 TEXT,
    git_commit_preflight_revision INTEGER,
    git_commit_preflight_sha256 TEXT,
    git_commit_policy_fingerprint TEXT,
    apply_attempt_id TEXT,
    target_head_commit TEXT,
    integration_head_commit TEXT,
    decision_context_json TEXT NOT NULL DEFAULT '{}',
    created_at TEXT NOT NULL,
    FOREIGN KEY(workflow_id) REFERENCES workflows(id),
    FOREIGN KEY(apply_attempt_id) REFERENCES apply_attempts(id)
);

CREATE TABLE findings (
    id TEXT PRIMARY KEY,
    project_id TEXT NOT NULL,
    workflow_id TEXT,
    task_id TEXT,
    code TEXT NOT NULL,
    severity TEXT NOT NULL,
    status TEXT NOT NULL,
    evidence_json TEXT NOT NULL DEFAULT '{}',
    created_at TEXT NOT NULL,
    resolved_at TEXT,
    resolution_json TEXT,
    FOREIGN KEY(project_id) REFERENCES projects(id),
    FOREIGN KEY(workflow_id) REFERENCES workflows(id),
    FOREIGN KEY(task_id) REFERENCES tasks(id)
);

CREATE TABLE sessions (
    id TEXT PRIMARY KEY,
    workflow_id TEXT NOT NULL,
    task_id TEXT,
    supersedes_session_id TEXT,
    purpose TEXT NOT NULL,
    provider TEXT NOT NULL,
    model TEXT,
    provider_session_id TEXT,
    context_bundle_revision INTEGER,
    context_bundle_path TEXT,
    context_bundle_sha256 TEXT,
    status TEXT NOT NULL,
    started_at TEXT NOT NULL,
    ended_at TEXT,
    metadata_json TEXT NOT NULL,
    FOREIGN KEY(workflow_id) REFERENCES workflows(id),
    FOREIGN KEY(supersedes_session_id) REFERENCES sessions(id)
);

CREATE TABLE managed_processes (
    id TEXT PRIMARY KEY,
    run_id TEXT NOT NULL,
    session_id TEXT,
    process_type TEXT NOT NULL,
    pid INTEGER NOT NULL,
    process_start_token TEXT NOT NULL,
    process_group_id TEXT,
    status TEXT NOT NULL,
    started_at TEXT NOT NULL,
    ended_at TEXT,
    metadata_json TEXT NOT NULL DEFAULT '{}',
    FOREIGN KEY(run_id) REFERENCES runs(id),
    FOREIGN KEY(session_id) REFERENCES sessions(id)
);

CREATE TABLE tasks (
    id TEXT PRIMARY KEY,
    workflow_id TEXT NOT NULL,
    spec_id TEXT NOT NULL,
    replaces_task_id TEXT,
    title TEXT NOT NULL,
    cached_projection_status TEXT,
    worktree_path TEXT,
    branch_name TEXT,
    task_base_commit TEXT,
    implementation_commits_json TEXT NOT NULL DEFAULT '[]',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE(workflow_id, spec_id),
    FOREIGN KEY(workflow_id) REFERENCES workflows(id),
    FOREIGN KEY(replaces_task_id) REFERENCES tasks(id)
);

CREATE TABLE nodes (
    id TEXT PRIMARY KEY,
    workflow_id TEXT NOT NULL,
    task_id TEXT,
    supersedes_node_id TEXT,
    node_type TEXT NOT NULL,
    definition_sha256 TEXT NOT NULL,
    status TEXT NOT NULL,
    current_attempt_number INTEGER NOT NULL DEFAULT 0,
    retry_budget_consumed INTEGER NOT NULL DEFAULT 0,
    max_retry_budget INTEGER NOT NULL,
    last_error_code TEXT,
    last_error_message TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    FOREIGN KEY(workflow_id) REFERENCES workflows(id),
    FOREIGN KEY(task_id) REFERENCES tasks(id),
    FOREIGN KEY(supersedes_node_id) REFERENCES nodes(id)
);

CREATE TABLE node_attempts (
    id TEXT PRIMARY KEY,
    node_id TEXT NOT NULL,
    attempt_number INTEGER NOT NULL,
    status TEXT NOT NULL,
    session_id TEXT,
    start_head_commit TEXT,
    start_dirty_fingerprint TEXT,
    end_head_commit TEXT,
    end_dirty_fingerprint TEXT,
    end_head_audit_ref TEXT,
    started_at TEXT NOT NULL,
    ended_at TEXT,
    evidence_manifest_json TEXT NOT NULL DEFAULT '{}',
    retry_budget_charged INTEGER NOT NULL DEFAULT 1,
    interruption_reason TEXT,
    error_code TEXT,
    error_message TEXT,
    UNIQUE(node_id, attempt_number),
    FOREIGN KEY(node_id) REFERENCES nodes(id),
    FOREIGN KEY(session_id) REFERENCES sessions(id)
);

CREATE TABLE leases (
    scope_type TEXT NOT NULL,
    scope_key TEXT NOT NULL,
    owner_id TEXT NOT NULL,
    workflow_id TEXT,
    run_id TEXT,
    pid INTEGER NOT NULL,
    process_start_token TEXT NOT NULL,
    acquired_at TEXT NOT NULL,
    heartbeat_at TEXT NOT NULL,
    metadata_json TEXT NOT NULL DEFAULT '{}',
    PRIMARY KEY(scope_type, scope_key),
    FOREIGN KEY(workflow_id) REFERENCES workflows(id)
);

CREATE TABLE runs (
    id TEXT PRIMARY KEY,
    workflow_id TEXT NOT NULL,
    status TEXT NOT NULL,
    pid INTEGER,
    heartbeat_at TEXT,
    stop_requested_at TEXT,
    stop_reason TEXT,
    force_stop_requested_at TEXT,
    quiesce_requested_at TEXT,
    blocking_finding_id TEXT,
    quiesce_snapshot_json TEXT NOT NULL DEFAULT '{}',
    started_at TEXT NOT NULL,
    ended_at TEXT,
    error_code TEXT,
    error_message TEXT,
    FOREIGN KEY(workflow_id) REFERENCES workflows(id),
    FOREIGN KEY(blocking_finding_id) REFERENCES findings(id)
);

CREATE TABLE cleanup_attempts (
    id TEXT PRIMARY KEY,
    workflow_id TEXT NOT NULL,
    status TEXT NOT NULL,
    plan_path TEXT NOT NULL,
    plan_sha256 TEXT NOT NULL,
    requested_by TEXT NOT NULL,
    started_at TEXT NOT NULL,
    ended_at TEXT,
    error_code TEXT,
    error_message TEXT,
    UNIQUE(workflow_id, id),
    FOREIGN KEY(workflow_id) REFERENCES workflows(id)
);

CREATE TABLE cleanup_items (
    id TEXT PRIMARY KEY,
    cleanup_attempt_id TEXT NOT NULL,
    ordinal INTEGER NOT NULL,
    target_type TEXT NOT NULL,
    canonical_path TEXT NOT NULL,
    expected_branch TEXT,
    expected_head_commit TEXT,
    expected_fingerprint TEXT NOT NULL,
    status TEXT NOT NULL,
    started_at TEXT,
    ended_at TEXT,
    error_code TEXT,
    error_message TEXT,
    UNIQUE(cleanup_attempt_id, ordinal),
    FOREIGN KEY(cleanup_attempt_id) REFERENCES cleanup_attempts(id)
);

CREATE TABLE apply_attempts (
    id TEXT PRIMARY KEY,
    workflow_id TEXT NOT NULL,
    attempt_number INTEGER NOT NULL,
    supersedes_apply_attempt_id TEXT,
    status TEXT NOT NULL,
    target_head_at_start TEXT NOT NULL,
    integration_head TEXT NOT NULL,
    apply_branch TEXT NOT NULL,
    apply_worktree_path TEXT NOT NULL,
    verification_catalog_revision INTEGER NOT NULL,
    verification_catalog_sha256 TEXT NOT NULL,
    git_commit_preflight_revision INTEGER,
    git_commit_preflight_sha256 TEXT,
    git_commit_policy_fingerprint TEXT,
    staged_apply_commit TEXT,
    applied_target_commit TEXT,
    verification_manifest_json TEXT NOT NULL DEFAULT '{}',
    started_at TEXT NOT NULL,
    ended_at TEXT,
    error_code TEXT,
    error_message TEXT,
    UNIQUE(workflow_id, attempt_number),
    FOREIGN KEY(workflow_id) REFERENCES workflows(id),
    FOREIGN KEY(supersedes_apply_attempt_id) REFERENCES apply_attempts(id)
);

CREATE TABLE branch_quarantines (
    id TEXT PRIMARY KEY,
    workflow_id TEXT NOT NULL,
    task_id TEXT,
    apply_attempt_id TEXT,
    branch_kind TEXT NOT NULL,
    branch_name TEXT NOT NULL,
    worktree_path TEXT NOT NULL,
    head_commit TEXT NOT NULL,
    audit_ref TEXT NOT NULL UNIQUE,
    reason_code TEXT NOT NULL,
    evidence_manifest_json TEXT NOT NULL,
    created_at TEXT NOT NULL,
    UNIQUE(workflow_id, branch_name),
    FOREIGN KEY(workflow_id) REFERENCES workflows(id),
    FOREIGN KEY(task_id) REFERENCES tasks(id),
    FOREIGN KEY(apply_attempt_id) REFERENCES apply_attempts(id)
);

CREATE TABLE git_commit_preflights (
    id TEXT PRIMARY KEY,
    workflow_id TEXT NOT NULL,
    revision INTEGER NOT NULL,
    repository_context TEXT NOT NULL,
    git_version TEXT NOT NULL,
    commit_policy_fingerprint TEXT NOT NULL,
    identity_json TEXT NOT NULL,
    signing_policy_json TEXT NOT NULL,
    probe_status TEXT NOT NULL,
    artifact_path TEXT NOT NULL,
    artifact_sha256 TEXT NOT NULL,
    created_at TEXT NOT NULL,
    UNIQUE(workflow_id, revision),
    FOREIGN KEY(workflow_id) REFERENCES workflows(id)
);

CREATE TABLE git_commit_evidence (
    id TEXT PRIMARY KEY,
    workflow_id TEXT NOT NULL,
    task_id TEXT,
    node_attempt_id TEXT,
    apply_attempt_id TEXT,
    commit_hash TEXT NOT NULL,
    commit_kind TEXT NOT NULL,
    preflight_id TEXT NOT NULL,
    author_identity_json TEXT NOT NULL,
    committer_identity_json TEXT NOT NULL,
    signature_status TEXT NOT NULL,
    signer_identity TEXT,
    evidence_json TEXT NOT NULL DEFAULT '{}',
    created_at TEXT NOT NULL,
    UNIQUE(workflow_id, commit_hash),
    FOREIGN KEY(workflow_id) REFERENCES workflows(id),
    FOREIGN KEY(task_id) REFERENCES tasks(id),
    FOREIGN KEY(node_attempt_id) REFERENCES node_attempts(id),
    FOREIGN KEY(apply_attempt_id) REFERENCES apply_attempts(id),
    FOREIGN KEY(preflight_id) REFERENCES git_commit_preflights(id)
);

CREATE TABLE events (
    sequence INTEGER PRIMARY KEY AUTOINCREMENT,
    event_id TEXT NOT NULL UNIQUE,
    project_id TEXT NOT NULL,
    workflow_id TEXT,
    run_id TEXT,
    task_id TEXT,
    apply_attempt_id TEXT,
    event_type TEXT NOT NULL,
    payload_json TEXT NOT NULL,
    created_at TEXT NOT NULL,
    FOREIGN KEY(project_id) REFERENCES projects(id),
    FOREIGN KEY(workflow_id) REFERENCES workflows(id)
);
```

`events.project_id` 固定事件所属 Project；`workflow_id` 对 Workflow/Run/Task/Apply 事件必填，但对 Workflow 创建前的 Project Writer、Project Quarantine 等 Project 级事件可为空。Runtime 必须按 Event Type 校验所需 Scope，禁止用虚假 Workflow ID 填充。数据库 Migration 的权威历史由 `schema_migrations` 和 Backup Manifest 表达，不为了记录迁移而在未知 Schema 上抢先写普通 Event。

`workflow_artifact_refs` 只保存每类 Artifact 的当前活动 Revision、Path 和 Hash；历史正文仍由不可变文件保留。切换活动 Revision 必须与 Event Append 在同一事务中完成。

`git_commit_preflights` 保存每次 Identity/Signing Preflight 的索引与规范化 Policy Fingerprint，正文保存在不可变 Artifact；`git_commit_evidence` 将每个 Task、Integration 或 Apply Commit 绑定到创建前的 Preflight，并保存实际 Identity 与签名验证结果。私钥、口令和 Credential Helper 输出不得进入任一表。

`branch_quarantines` 是 append-only 安全索引，固定不再允许进入可信执行链的 Branch、HEAD、Worktree、Audit Ref 和证据。替代 Task 通过 `replaces_task_id` 指向旧 Task，新 Apply Attempt 通过 `supersedes_apply_attempt_id` 指向旧 Attempt；旧 Row、Branch 和 Ref 不更新为“成功”或被覆盖。Replacement Integration Branch 的切换必须通过新 Workflow Revision、Execution Approval 和 Intent/Result Event 审计。

`nodes.definition_sha256` 固定规范化节点定义。Replacement Revision 只有在 Node ID、Definition Hash 和依赖边均未改变时才能复用原 Node Row；语义改变时创建新 Node，并通过 `supersedes_node_id` 关联旧 Node。增量恢复分类保存为不可变 Reconciliation Manifest，Revision/Path/Hash 由 Workflow Artifact Ref 和 Replacement Execution Approval 的 `decision_context_json` 固定。

`approvals` 只追加、不更新或删除。`PLAN` Gate 只允许 Plan 字段有值；`EXECUTION` Gate 必须同时固定 Plan、Specs、Verification Catalog、Dynamic Workflow、Routing Policy、Budget Hash 和当前 Git Commit Preflight Revision/Hash/Fingerprint。执行期间 Policy 合法漂移且执行 Artifact 未变化时，额外写入 `COMMIT_POLICY` 决策，只固定新的 Preflight 和原 Execution Approval 上下文；漂移窗口 Commit 导致 Replacement Spec/Workflow Revision 时，新的 `EXECUTION` Approval 在 `decision_context_json` 记录 Quarantine/被替代 Approval 和 `absorbs_commit_policy_confirmation = true`，不得再写重复的 `COMMIT_POLICY` Row。Target Drift 导致 Apply Catalog 身份变化时，额外写入 `APPLY_CATALOG` 决策；Apply Commit Policy 漂移时写入同时固定 Apply Attempt、Target/Integration HEAD 和新 Preflight 的 `COMMIT_POLICY` 决策；二者都不改变 Workflow 的已完成状态。Runtime 只有在活动引用、HEAD、Fingerprint 与相应 Approval 完全匹配时才能越过门；判断与状态更新、Approval Insert 和 Event Append 必须在同一事务中完成。

`cleanup_attempts` 固定用户已检查的 Cleanup Plan Artifact 与执行结果；`cleanup_items` 保存每个精确目录目标的 Intent/Result 索引。Cleanup Plan 和历史 Row 不可修改或删除，重复执行必须创建新 Attempt；已完成项目通过外部事实与 Result Event 幂等协调，不能靠覆盖旧 Row 重来。

`leases` 只保存当前 Owner 的可变诊断元数据；只有已持有对应 OS Lock 的进程可以创建、刷新或释放记录。Lease 历史通过 `LEASE_ACQUIRED`、`LEASE_RELEASED`、`LEASE_FOUND_STALE` Event 审计，不能把数据库行本身当作活进程证明。

`findings` 保存需要 Runtime 或用户处理的结构化问题。存在状态为 `OPEN` 的 Project 级 `ORPHAN_CHILD_PROCESS` 或其他 Mutation Quarantine Finding 时，Project Writer 即使取得 OS Lock 也只能执行只读诊断和 Reconcile，不能启动普通 Mutating Action；Finding 只能在外部事实重新校验通过后由 Runtime 解决。

### Workflow 元信息

`workflow.yaml` 是静态身份与 Artifact Manifest，不是运行状态快照：

```yaml
schema_version: 1
workflow_id: wf-20260802-001
project_id: prj-6f9e13a8c2
name: add-coupon-exclusion

repository:
  canonical_path: /Users/yuancheng/Documents/Code/Resume
  target_branch: main
  base_commit: a32f1fbc
  initial_worktree_dirty: true
  initial_dirty_fingerprint: sha256:2c71...
  integration_branch: cflow/wf-20260802-001/integration

artifacts:
  active_plan:
    revision: 1
    path: plan/plan-001.md
    sha256: 8a60...
  active_specs: null
  active_verification_catalog: null
  active_dynamic_workflow: null
  active_routing_policy: null

created_at: 2026-08-02T10:00:00+09:00
```

### Plan 文件格式

```markdown
---
schema_version: 1
workflow_id: wf-20260802-001
document_type: plan
revision: 1
planner_provider: codex
planner_session_id: 0198abc
repository_base_commit: a32f1fbc
created_at: 2026-08-02T10:00:00+09:00
content_sha256: 8a60...
---

# 需求名称

## 背景

## 目标

## 范围

## 非目标

## 约束

## 当前实现分析

## 推荐技术方案

## 关键设计决策

## 涉及模块与文件边界

## 数据与兼容性影响

## 测试与验收方案

## 风险与回滚

## 未决问题
```

Plan Artifact 写入后不可原地修改。Plan Check 结果写入独立且不可变的 Check Artifact：

```yaml
schema_version: 1
plan_revision: 1
plan_sha256: 8a60...
decision: pass
checker_provider: claude
checker_session_id: c680...
created_at: 2026-08-02T10:31:00+09:00
```

随后由 CFlow Runtime 在同一 SQLite 事务中将权威 `plan_status` 转为 `CHECKED`，记录 Check Artifact 的 Hash 和事件，并将 Workflow 置为 `PAUSED` 等待用户 Plan Approval。只有用户批准精确 Plan Revision/Hash 后，Runtime 才将 `plan_status` 转为 `APPROVED`。Agent 不直接拥有修改状态或批准产物的权限。

## Agent 调度与动态工作流设计

### Agent Adapter

统一接口：

```text
interface AgentAdapter {
  readonly provider: AgentProvider;
  readonly capabilities: AgentCapabilities;

  detect(): Promise<AgentInstallation>;
  start(request: AgentStartRequest): AsyncIterable<AgentEvent>;
  resume(request: AgentResumeRequest): AsyncIterable<AgentEvent>;
  cancel(handle: AgentRunHandle): Promise<void>;
  inspectSession(sessionId: string): Promise<AgentSessionInfo | null>;
}

type ProtocolCompatibility =
  | "MISSING"
  | "SUPPORTED"
  | "UNKNOWN_VERSION"
  | "INCOMPATIBLE_PROTOCOL";

interface AgentInstallation {
  compatibility: ProtocolCompatibility;
  executablePath?: string;
  executableSha256?: string;
  cliVersion?: string;
  registryRevision: number;
  registrySha256: string;
  dialectId?: string;
  capabilities: AgentCapabilities;
}

interface AgentProtocolBinding {
  provider: AgentProvider;
  executablePath: string;
  executableSha256: string;
  cliVersion: string;
  registryRevision: number;
  registrySha256: string;
  dialectId: string;
  requiredCapabilities: AgentCapabilities;
}

interface AgentCapabilities {
  structuredEvents: boolean;
  resumableSession: boolean;
  sessionIdInEventStream: boolean;
  nativeInteractiveResume: boolean;
  structuredOutputSchemaOnStart: boolean;
  structuredOutputSchemaOnResume: boolean;
  budgetLimit: boolean;
}

interface AgentStartRequest {
  cwd: string;
  prompt: string;
  contextBundle?: ContextBundleRef;
  model?: string;
  purpose: AgentPurpose;
  outputSchema?: object;
  timeoutMs: number;
  budget?: AgentBudget;
  environment: Record<string, string>;
  expectedProtocolBinding: AgentProtocolBinding;
}

interface ContextBundleRef {
  revision: number;
  path: string;
  sha256: string;
}

interface AgentResumeRequest {
  sessionId: string;
  cwd: string;
  prompt: string;
  contextBundle?: ContextBundleRef;
  purpose: AgentPurpose;
  outputSchema?: object;
  timeoutMs: number;
  budget?: AgentBudget;
  environment: Record<string, string>;
  expectedProtocolBinding: AgentProtocolBinding;
}

interface AgentRunHandle {
  runId: string;
  processId: string;
  processStartToken: string;
  processGroupId?: string;
  providerSessionId?: string;
}

interface AgentSessionInfo {
  providerSessionId: string;
  status: "ACTIVE" | "COMPLETED" | "FAILED" | "LOST";
  protocolBinding: AgentProtocolBinding;
}
```

Adapter 只统一 Session、结构化事件、Schema、Budget、Cancel 和 Resume 协议。Demo 启动 Agent 时沿用 Provider CLI 及用户现有配置的默认权限行为，不生成跨 Provider 权限策略，也不注入 Danger/Bypass/Skip-Permissions 参数。每次调用必须记录检测到的 CLI Version、脱敏 argv、cwd 和相关环境变量名，供用户理解实际信任边界；这些记录不是沙箱证明。

`AgentProtocolBinding` 固定 Provider、Executable Path/Binary Hash、CLI Version、Registry Revision/Hash、Dialect ID 和本次 Purpose 所需能力。`start`/`resume` 必须 Compare-and-Swap 当前检测结果；不匹配时在创建子进程前返回 `PROVIDER_PROTOCOL_BINDING_CHANGED`。Session `metadata_json` 保存该 Binding 和实际事件 Schema Revision，以支持恢复时重验。

Capability 必须按具体操作建模，不能因为 Provider 的新 Session 支持某个 Flag 就推断 Resume 也支持。例如结构化 Final Output 在部分 Codex CLI 历史版本中曾只由 `exec` 支持、后续版本才扩展到 `exec resume`；Protocol Registry 必须分别验证 `structuredOutputSchemaOnStart` 与 `structuredOutputSchemaOnResume`。某 Purpose 需要结构化结果而当前操作不具备对应能力时，必须使用已批准且受支持的独立新 Session/Context Bundle 路径，或 Fail-closed，不得退回自由文本并声称 Schema 已验证。

统一事件模型：

```text
type AgentEvent =
  | { type: "session_started"; sessionId: string }
  | { type: "assistant_delta"; text: string }
  | { type: "assistant_message"; text: string }
  | { type: "tool_started"; tool: string; input: unknown }
  | { type: "tool_finished"; tool: string; output: unknown }
  | { type: "usage"; inputTokens?: number; outputTokens?: number; costUsd?: number }
  | { type: "completed"; result: unknown }
  | { type: "failed"; code: string; message: string };
```

Provider 能力映射：

| Provider | Demo 调用方式 | Session 恢复 | 结构化输出 | 权限边界 |
|---|---|---|---|---|
| Codex | `codex exec --json` | `codex exec resume <session>` | JSONL Events | 沿用用户安装版本及其默认配置；CFlow 不声称统一其 Sandbox 行为 |
| Claude Code | `claude -p --output-format stream-json` | `claude --resume <session>` | Stream JSON、JSON Schema | 沿用用户安装版本及其默认配置；CFlow 不声称 Tool 权限等同于 OS Sandbox |
| OpenCode | `opencode run --format json` | `opencode run --session <session>` | Raw JSON Events | P1 Adapter；沿用 Provider 默认权限模型 |

这些能力均由当前官方 CLI 文档提供。citeturn3view2turn3view3turn3view4turn3view5turn3view6

### Agent 角色

| Purpose | 责任 | 是否可写代码 |
|---|---|---:|
| `REQUIREMENT_DISCUSSION` | 澄清需求、分析仓库、生成 Plan | 否 |
| `PLAN_CHECK` | 检查 Plan 是否可执行 | 否 |
| `SPEC_GENERATION` | 将 Plan 拆成 Specs | 否 |
| `WORKFLOW_OPTIMIZATION` | 对确定性 Workflow 骨架提出受限调度补丁 | 否 |
| `TASK_IMPLEMENTATION` | 在 Task Worktree 编码和自测 | 是 |
| `TASK_REVIEW` | 独立检查 Task Diff 和测试 | 否 |
| `TASK_REPAIR` | 根据失败证据修复 | 是 |
| `MERGE_RESOLUTION` | 只解决 Integration Merge Conflict | 是 |
| `FINAL_VERIFICATION` | 验收完整 Integration Branch | 否 |

这些角色描述的是职责、Prompt 和后置验收要求，不代表 CFlow 已对 Provider 工具权限实施统一强制。非编码角色如果修改了受管 Snapshot，其输出无效；编码角色只有通过 Commit/Clean Worktree Gate 和后续独立验收才能推进。

`routing/routing-policy-001.yaml` 记录各 Purpose 使用的 Provider、Model、Fallback、预算和精确 Agent Protocol Binding。Dry Run 必须同时提示所有 Route 均使用对应 Provider 的默认权限与用户现有配置，并展示 CLI Version、Dialect 与 Registry Revision/Hash；Execution Approval 通过 Routing Policy Hash 固定这些 Binding。Routing Approval 不是对 Provider 行为安全性的证明。

### Verification Command Catalog 数据格式

`verification/catalog-001.yaml`：

```yaml
schema_version: 1
workflow_id: wf-20260802-001
revision: 1
repository_base_commit: a32f1fbc

commands:
  - id: coupon-domain-test
    source:
      kind: discovered_wrapper
      path: ./mvnw
      sha256: 41ef...
    purposes: [task_verify, final_verify, apply_verify]
    executable:
      kind: project_relative
      value: ./mvnw
      sha256: 41ef...
    args: [-pl, coupon-domain, test]
    cwd: .
    timeout_seconds: 600
    expected_exit_codes: [0]
    output_limit_bytes: 10485760
    environment:
      inherit: [PATH, TMPDIR, LANG, LC_ALL]
      fixed:
        CI: "true"
      isolated_home: true
    transient_write_paths:
      - coupon-domain/target/**
```

Catalog 必须使用 Canonical Serialization 计算整体 Hash；Command ID 在同一 Catalog Revision 中唯一。Entry 的 Source、Executable Identity 或任何参数变化都必须创建新 Catalog Revision，不能原地更新。

### Spec 数据格式

`specs/index.yaml`：

```yaml
schema_version: 1
workflow_id: wf-20260802-001
plan_revision: 1

tasks:
  - id: S01
    title: analyze-domain-model
    goal: 梳理现有优惠券与活动关系模型
    depends_on: []
    parallel_group: analysis-a

    write_scope:
      - docs/cflow-analysis/S01/**
    read_scope:
      - src/main/java/**
      - plan/plan-001.md

    resource_locks: []

    acceptance:
      deterministic:
        - type: file_exists
          path: docs/cflow-analysis/S01/domain-model.md
      semantic:
        - 说明现有券、活动和规则之间的关系
        - 标出潜在兼容风险

    execution:
      provider: codex
      model: default
      max_attempts: 2
      timeout_minutes: 30

  - id: S05
    title: implement-exclusion-model
    goal: 实现互斥规则领域模型
    depends_on: [S01, S04]
    parallel_group: null

    write_scope:
      - src/main/java/com/example/coupon/domain/**
      - src/test/java/com/example/coupon/domain/**

    resource_locks:
      - coupon-domain-model

    acceptance:
      deterministic:
        - type: command_ref
          command_id: coupon-domain-test
      semantic:
        - 历史规则默认行为保持不变
        - 新互斥规则可以由配置驱动
```

### 并行安全判断

只有同时满足以下条件才允许两个 Task 并行：

```text
A 不依赖 B
B 不依赖 A
A.write_scope 与 B.write_scope 不相交
A.resource_locks 与 B.resource_locks 不相交
二者可以独立执行验收
```

判定分为两层：

```text
静态判定
  - depends_on
  - write_scope glob
  - resource_locks
  - target module

Agent 辅助判定
  - 是否存在隐含共享文件
  - 是否依赖同一个架构决策
  - 是否会共同修改配置或 Schema
```

Agent 只能将任务从“可并行”降级为“串行”，不能覆盖静态冲突强行并行。

### Dynamic Workflow IR

生成管线已经确认如下：

```text
Approved Plan + Validated Specs
    ↓
CFlow Compiler 确定性生成安全骨架
    ↓
独立 Workflow Optimization Agent 提交 Patch IR
    ↓
Compiler 校验补丁没有弱化依赖、验收、安全和预算约束
    ↓
应用合法补丁并输出最终 Dynamic Workflow IR
```

Patch IR 只允许以下类型的建议：

- 在静态安全范围内降低或调整并行度。
- 为任务选择已配置且协议能力匹配的 Provider、模型和预算档位。
- 插入受支持的非审批 Checkpoint。Patch 不得新增第三个常规用户 Approval Gate；只有 Runtime 内建且已在本 PRD 定义的异常安全确认可以暂停用户。
- 在不超过 Workflow 硬上限的前提下收紧节点 Timeout 或 Retry Budget。

任何补丁校验失败时，CFlow 保留确定性骨架并生成 Compile Finding，不得让 Agent 输出替代 Compiler 的最终解释权。

```yaml
schema_version: 1
workflow_id: wf-20260802-001
spec_revision: 1

settings:
  max_parallelism: 4
  blocking_failure_policy: quiesce
  max_total_agent_runs: 24
  auto_merge_to_integration: true
  auto_merge_to_target: false

nodes:
  - id: execute-S01
    type: agent_task
    spec_id: S01
    depends_on: []

  - id: verify-S01
    type: verify
    spec_id: S01
    depends_on: [execute-S01]

  - id: merge-S01
    type: merge
    spec_id: S01
    depends_on: [verify-S01]

  - id: execute-S05
    type: agent_task
    spec_id: S05
    depends_on:
      - merge-S01
      - merge-S04

  - id: verify-S05
    type: verify
    spec_id: S05
    depends_on: [execute-S05]

  - id: merge-S05
    type: merge
    spec_id: S05
    depends_on: [verify-S05]

  - id: final-verification
    type: final_verify
    depends_on:
      - merge-S11
      - merge-S12
```

CFlow Compiler 必须校验：

| 校验 | 规则 |
|---|---|
| Schema | 所有字段类型和枚举合法 |
| DAG | 不存在依赖环 |
| Spec 覆盖 | 每个有效 Spec 恰好有一个执行节点 |
| 验收覆盖 | 每个 Coding Spec 必须有 Verify 节点 |
| 合并覆盖 | 每个 Coding Spec 必须有 Merge 节点 |
| 依赖保持 | Workflow 不能弱化 Spec 依赖 |
| 权限 | 不得包含未支持节点或任意 Shell |
| Verification Catalog | 所有 Command Ref 存在、Purpose 匹配、无自由 argv，Catalog Revision/Hash 与 Execution Approval 一致 |
| Agent Route | 每个 Purpose 的 Provider/Model/Fallback 存在且协议能力满足结构化事件、Session、Schema 与预算要求；权限沿用 Provider 默认配置并在 Dry Run 明示 |
| 并发上限 | 不超过用户配置 |
| 成本上限 | Agent Run 总数、Retry 总数可计算 |
| 最终验收 | 必须存在且只能有一个 Final Verify |

JSON Schema 可以用于校验 Workflow IR 的结构和约束；JSON Schema 的 Assertion 模型本身就是对文档是否满足约束进行通过或失败判断。citeturn6view3

### Worktree 策略

> 决策日期：2026-08-02
>
> 决策状态：已确认

CFlow 采用 Integration Head 作为依赖任务的代码基线：

- Workflow 创建时记录不可变 `base_commit` 和确定性的 Integration Branch Name，但此时只创建固定在 Base Commit 的 Planning Snapshot Worktree，不创建 Integration Git Ref。
- Planning Snapshot Worktree 供 Requirement Discussion、Planner、Checker、Spec 与 Workflow Optimization Session 使用。CFlow 不声称 Provider 无法写入；每次非编码 Session 前后都必须比较 HEAD/Index/Tracked/Untracked Snapshot，任何变化都使该 Session 输出无效并阻塞，不能进入 Artifact 或 Approval。
- 只有 Execution Approval 已提交、Project Writer/Workflow Owner 已取得且 Execution Run 即将开始时，Runtime 才以 Intent/Result 从 `base_commit` 创建 Integration Branch 与 Worktree。Recovery 若发现 Ref/Worktree 已存在，必须验证名称、路径和 HEAD 精确匹配 Base/已记录进度，不能覆盖或另选基线。
- 无依赖 Task 在首次 Ready 时从当时的 Integration HEAD 创建 Worktree；初始情况下该值等于 Workflow Base Commit。
- 有依赖 Task 只有在所有声明依赖对应的 Merge Node 都成功后才能 Ready。
- Task 变为 Ready 时读取并记录当时的 Integration HEAD 为 `task_base_commit`，然后从该 Commit 创建 Task Branch 和 Worktree。
- `task_base_commit` 可以包含已经合并的非直接依赖 Task；这是 Integration Branch 的一致快照，但不得据此自动增加 Spec 依赖。
- Task Agent 启动后不得自动 Rebase 或切换基线。Integration Branch 后续前进不影响正在运行的 Task。
- Integration Merge 必须通过 Workflow 级互斥锁串行执行；并行发生在 Coding 和 Verification，不并行写 Integration Worktree。
- Compiler 必须为每个 Merge Node 注入同一个 `integration:<workflow-id>` Resource Lock。
- Recovery 必须核对记录的 `task_base_commit`、Task Branch HEAD、依赖 Merge Commit 和当前 Integration 历史。
- Recovery 还必须核对每个 Attempt 的 `end_head_commit` 与审计 Ref。Ref 缺失但 Commit Object 仍存在时可以通过带 Expected-Absent 的 `git update-ref` 确定性重建并记录 Event；Ref 指向其他 Commit 时以 `AUDIT_REF_MISMATCH` 阻塞且不得覆盖，Commit Object 已缺失时属于无法恢复的 `ATTEMPT_COMMIT_EVIDENCE_MISSING`。
- Branch Quarantine 后旧 Branch 永久退出活动基线计算。Replacement Task 从当时已验收 Integration HEAD 创建；Replacement Integration Branch 只能从安全停止前已固定的最后已验收 Integration HEAD 创建。两者都必须使用新名称和新 Worktree，不能移动或复用被隔离 Branch。

#### 已确认：用户当前工作区隔离

> 决策日期：2026-08-02
>
> 决策状态：已确认

用户当前工作区允许存在未提交修改，但必须与 Workflow 严格隔离：

- 创建 Workflow 时记录 `target_branch`、`base_commit = HEAD`、Dirty 状态和 Dirty Fingerprint。
- Dirty Fingerprint 至少覆盖暂存区 Diff、工作区 Diff、未跟踪文件路径清单及其内容 Hash；它用于审计和安全检查，不把内容复制进 Workflow。
- CLI 必须明确提示：未提交修改不会进入 Workflow 的 Plan、Task、Verification 或 Integration Branch，并要求用户确认后继续。
- CFlow 不自动执行 Stash、WIP Commit、Reset、Checkout 或其他会改变用户当前工作区的操作。
- Planner、Checker、Implementer 和 Reviewer 只能读取 CFlow 管理的、固定在 `base_commit` 或后续 Integration Commit 的 Worktree，不把用户当前脏工作区作为仓库分析基线。
- 自动执行阶段不要求用户当前工作区持续保持原状；Dirty Fingerprint 的后续变化只记录审计 Finding，不影响已固定的 Workflow Base。
- `cflow apply` 前必须要求目标工作区干净且位于预期 Target Branch；不满足时进入 `BLOCKED`，由用户自行处理。

Git 官方支持一个 Repository 同时关联多个 Worktree，使不同分支可以在独立工作目录中被检出；`git worktree list --porcelain -z` 还提供稳定、适合程序解析的输出格式。citeturn5view0

CFlow 创建：

```text
Target Branch: main
Base Commit: abc1234

Integration:
  branch: cflow/wf-20260802-001/integration
  path: ~/.cflow/worktrees/<project>/<workflow>/integration

Task S01:
  branch: cflow/wf-20260802-001/S01
  path: ~/.cflow/worktrees/<project>/<workflow>/S01
  base: abc1234

Task S05 after dependencies merged:
  branch: cflow/wf-20260802-001/S05
  path: ~/.cflow/worktrees/<project>/<workflow>/S05
  base: def5678  # recorded Integration HEAD
```

命令概念：

```bash
git worktree add \
  -b cflow/wf-20260802-001/integration \
  <integration-path> \
  abc1234

git worktree add \
  -b cflow/wf-20260802-001/S01 \
  <task-path> \
  abc1234

git worktree add \
  -b cflow/wf-20260802-001/S05 \
  <task-path> \
  def5678
```

任务完成后：

```text
Task Worktree
  ↓ Git Identity/Signing Preflight
  ↓ Agent 提交实现 Commit
Commit/Clean Gate
  ↓ HEAD 前进 + Git-clean + Identity/Signing + 完整 Commit Range Scope 通过
Deterministic Verification
  ↓ HEAD 未变化 + Git-clean
Task Reviewer 验收 Commit 和 Diff
  ↓
Integration Worktree
  ↓ git merge --no-ff task-branch
Integration Verification
```

#### 已确认：Merge Conflict 处理

> 决策日期：2026-08-02
>
> 决策状态：已确认

如发生 Merge Conflict：

```text
merge node 检测文本冲突
           ↓
记录 Pre-Merge HEAD、冲突文件和双方 Commit
           ↓
启动独立 MERGE_RESOLUTION Session（最多一次 Attempt）
           ↓
写范围限制为冲突文件与相关 Specs write_scope 并集
           ↓
Git Commit Preflight + Scope Check + 受影响测试
    ├─ 通过 → 提交 Merge Commit
    └─ 失败 → 恢复 Pre-Merge HEAD，保留证据并 BLOCKED
```

Merge Resolution 与普通 Integration Merge 必须在创建 Merge Commit 前通过当前 Repository Context 的 Identity/Signing Preflight，并在创建后验证实际 Merge Commit。Identity 或签名不符合 Preflight 时恢复受管 Integration Worktree 到 Pre-Merge HEAD、保留失败 Commit 证据并 `BLOCKED`；不得改写失败 Commit 后伪装为同一次成功。

没有文本冲突但合并后验证失败属于语义冲突：CFlow 恢复 Pre-Merge HEAD 并创建 Repair Spec，不允许 Merge Resolution Agent 顺手修改功能。

禁止 Coding Agent 直接在用户当前工作区修改代码。

### 验收顺序

每个 Task 使用如下门禁：

```text
检查 Agent 是否产生 Commit
    ↓
检查 Commit 是否属于 Task Branch
    ↓
检查 Worktree Git-clean、Task 历史 append-only
    ↓
检查实际 Commit Identity/Signing 符合 Preflight
    ↓
检查修改文件是否在 write_scope 内
    ↓
运行格式化、编译、静态检查
    ↓
运行单元测试
    ↓
运行 Spec 自定义验收命令
    ↓
独立 Reviewer 语义检查
    ↓
Verify Node 成功，Task 投影为 VERIFIED
    ↓
Merge Node 成功并确认 Commit 位于 Integration Branch
    ↓
Task 投影为 COMPLETED
```

最终验收：

```text
Integration Branch 无未提交改动
    ↓
所有 Task Commit 已合并
    ↓
全量构建与测试
    ↓
Plan 验收标准逐项检查
    ↓
独立 Final Reviewer
    ↓
生成 final-report.md
```

确定性检查应优先于 Agent Review。GitHub Agentic Workflows 的成本指导同样建议能由确定性工具完成的工作优先使用确定性工具，并只在必要时运行 Agent。citeturn8search10

### 重试策略

```yaml
retry_policy:
  implementation:
    max_attempts: 2
  verification_repair:
    max_attempts: 2
  merge_resolution:
    max_attempts: 1
  workflow_replan:
    max_attempts: 1
```

失败分类：

| Failure Code | 是否自动重试 | 行为 |
|---|---:|---|
| `AGENT_PROCESS_CRASHED` | 是 | 恢复 Session 或创建新 Attempt |
| `AGENT_TIMEOUT` | 是 | 保存日志，增加 Attempt |
| `PROVIDER_PROTOCOL_UNSUPPORTED` | 否，且不扣失败重试预算 | 当前 Route/Fallback 的 CLI 缺失、版本未知或协议不兼容；不启动 Provider/Attempt，Workflow Blocked |
| `PROVIDER_PROTOCOL_BINDING_CHANGED` | 否，且不扣失败重试预算 | Executable/Version/Registry/Dialect 与 Approval 不一致；关闭 Dispatch 并重新生成 Dry Run/Execution Approval |
| `PROVIDER_PROTOCOL_VIOLATION` | 否，且不扣失败重试预算 | 事件流违反已批准 Dialect；停止受影响进程并按 Quiescing/Blocked 收敛，不能信任退出码或自然语言结果 |
| `PROVIDER_SESSION_ID_MISSING` | 否，且不扣失败重试预算 | 需要恢复的 Purpose 未获得唯一 Session ID；保存流证据并 Blocked |
| `PROVIDER_AUTHENTICATION_REQUIRED` | 否，且不扣失败重试预算 | 非交互启动确认缺少登录/凭据；不读取凭据，用户修复后再 Resume |
| `INSECURE_CFLOW_HOME_PERMISSIONS` | 否，且不扣失败重试预算 | CFLOW_HOME/敏感路径的 Owner、Mode、Symlink 或文件系统权限语义不安全；不启动 Provider，用户显式修复后重验 |
| `SENSITIVE_DATA_REDACTION_FAILED` | 否，且不扣失败重试预算 | 当前 Frame 无法安全解析/脱敏；不持久化原内容，停止受影响进程并 Blocked |
| `DATABASE_SCHEMA_TOO_NEW` | 否，且不扣失败重试预算 | 数据库版本高于当前二进制支持上限；只允许 Doctor 诊断，不执行 Mutating Command 或 Down Migration |
| `DATABASE_MIGRATION_PATH_MISSING` | 否，且不扣失败重试预算 | 当前版本到目标版本不存在连续的内嵌 Forward Migration；保留数据库并阻塞 |
| `MIGRATION_CHECKSUM_MISMATCH` | 否，且不扣失败重试预算 | 已应用 Migration 的 ID/Checksum 与当前内嵌 Registry 不一致；不得继续或重写历史记录 |
| `DATABASE_MIGRATION_FAILED` / `DATABASE_MIGRATION_INCOMPLETE` | 否，且不扣失败重试预算 | 事务失败或数据库、Manifest、Version 事实不能确定协调；报告已验证备份，禁止自动覆盖恢复 |
| `ARTIFACT_SCHEMA_UNSUPPORTED` | 否，且不扣失败重试预算 | 所引用 Artifact Version 不在对应 Reader Compatibility Registry；不进行 Best-effort 解析或原地改写 |
| `USER_INTERRUPTED` | 否，且不扣失败重试预算 | Attempt/Run 记为 Interrupted，Node 协调后 Ready，Workflow Paused |
| `COMMAND_FAILED` | 是 | 将失败日志注入 Repair Agent |
| `SEMANTIC_REVIEW_FAILED` | 是 | 创建 Repair Attempt |
| `SCOPE_VIOLATION` | 否 | Blocked，等待人工决定 |
| `MERGE_CONFLICT` | 一次 | 启动独立 Merge Resolution Attempt；失败恢复 Integration 并 Blocked |
| `INTEGRATION_VERIFICATION_FAILED` | 否 | 恢复 Pre-Merge HEAD，创建 Repair Spec |
| `MISSING_CREDENTIALS` | 否 | Blocked |
| `PLAN_DRIFT` | 否 | Plan 标记 Stale |
| `BUDGET_EXCEEDED` | 否 | Paused |
| `RETRY_EXHAUSTED` | 否 | Node Failed；无 In-flight 时 Workflow 直接 Blocked，有 In-flight 时先 Quiescing，随后等待 Repair Spec、新 Workflow Revision 或取消 |
| `COMMIT_DURING_POLICY_DRIFT_WINDOW` | 否，且不扣失败重试预算 | 永久隔离包含该 Commit 的 Branch；Task/Integration 通过新 Repair Spec 或 Workflow Revision 与新 Execution Approval 建立替代路径，Apply 通过用户显式新 Attempt 重做；旧 Commit 不得进入可信链 |
| `REPLACEMENT_RECONCILIATION_CHANGED` | 否，且不扣失败重试预算 | Replacement 分类所依据的 Branch/HEAD/Definition/Dependency/Evidence 已漂移；重新生成 Manifest 和统一 Execution Approval 预览，不自动扩大复用集合 |

## 技术栈与系统架构

### 技术栈方案比较

| 方案 | 优点 | 缺点 | 适合阶段 |
|---|---|---|---|
| TypeScript + Node.js | CLI、JSONL、Prompt 和 Schema 迭代最快，Provider CLI 生态接近 | 运行时依赖 Node；包含 SQLite/PTY 原生模块时单文件分发更复杂 | 快速原型，但未被选择 |
| Go | 单二进制、进程控制、并发、崩溃恢复和跨平台构建路径清晰；标准库 `os/exec` 默认不经过 Shell | 复杂 Schema、Prompt 模板和快速协议迭代代码量高于 TypeScript | **已确认用于 Demo 和长期 Runtime** |
| Rust | 类型与资源控制最强，适合高可靠基础设施 | 开发和测试成本最高，跨平台 PTY 及 Agent 后续维护门槛更高 | 若未来出现 Go 无法满足的可靠性瓶颈再评估 |
| Java | `ProcessBuilder`、并发和 SQLite 生态成熟，可生成自包含平台包 | 包体、启动形态和逐平台打包相对不利，作为本地 CLI 没有超过 Go 的明确收益 | JVM 组织环境，Demo 不采用 |

按 CFlow 关注维度比较：

| 维度 | TypeScript + Node.js | Go | Rust | Java |
|---|---|---|---|---|
| CLI 开发效率 | 最高 | 高 | 中 | 中 |
| 子进程与流式 IO | 成熟，但需约束 Shell API | 标准库直接支持 argv、Pipe 和 Context 取消 | 控制力强，代码更繁 | 成熟，`ProcessBuilder` 原生接收参数列表 |
| PTY | 依赖原生 Addon | 需要平台 Adapter | 需要平台 Adapter | 需要平台 Adapter |
| JSONL、YAML、Schema | 动态数据和 Schema 迭代最快 | Struct + 严格 Decoder，代码更多但边界清晰 | Serde 强，类型建模成本高 | Jackson 等生态成熟，配置较重 |
| SQLite | 常见驱动含原生 Addon | 可使用 `database/sql` + 无 CGO Driver | 可静态链接 Driver | JDBC 成熟，但随 JVM/应用包分发 |
| Git Worktree | 调用 Git CLI，能力充分 | 调用 Git CLI，进程边界清晰 | 调用 Git CLI，能力充分 | 调用 Git CLI，能力充分 |
| DAG 调度与取消 | Event Loop 易实现，CPU/阻塞边界需谨慎 | Goroutine、Channel、Context 适合有界并发 | 强类型并发，复杂度最高 | Executor/Virtual Thread 能力成熟 |
| 崩溃恢复 | 主要依赖应用协议 | 显式错误、事务与紧凑 Runtime 适合守护式 CLI | 最强静态约束，但实现成本高 | 能力充分，部署更重 |
| 单二进制分发 | SEA 仍有依赖与平台限制 | 原生强项；无 CGO 时最直接 | 原生强项 | `jpackage` 是逐平台自包含应用包 |
| 跨平台兼容 | Node Runtime 一致，原生 Addon 增加矩阵 | 无 CGO 时构建矩阵简单 | Cross Compile 和系统依赖配置较繁 | JVM 一致，平台安装包需分别构建 |
| 测试成本 | 低 | 低，标准库支持充分 | 高 | 中 |
| Codex 后续维护 | 最容易生成和修改 | 结构直接、显式错误，维护难度低 | 生命周期和类型错误修复成本高 | 生态熟悉但样板较多 |
| 长期 Runtime 成本 | 迭代快，但分发和原生依赖需持续治理 | **可靠性、开发速度、分发的最佳平衡** | 可靠性上限最高，投入也最高 | 企业生态稳健，但不匹配本地 CLI 优先级 |

Go 可直接构建可执行文件；`os/exec` 接收程序与 argv 且不会隐式调用 Shell，符合 CFlow 的命令安全约束。Go 1.26 是 2026 年 2 月发布的当前稳定主版本。[Go 1.26 发布说明](https://go.dev/doc/go1.26)、[`os/exec` 文档](https://pkg.go.dev/os/exec)

### 已确认：Go 技术基线

> 决策日期：2026-08-02
>
> 决策状态：已确认

CFlow Demo 从第一天使用 Go，并以无需用户安装 Go、Node.js、JVM 或其他语言 Runtime 的单二进制作为分发基线。Demo 与长期 Runtime 共享同一语言和领域模型，不预设产品化阶段重写。

Release Build 默认使用 `CGO_ENABLED=0`，为 macOS 和 Linux 的 amd64/arm64 分别产出二进制；Windows Demo 仍以 WSL 为支持边界。若未来某项依赖必须启用 CGO，必须先提交设计变更并说明对交叉构建、签名和安装体验的影响。

技术基线：

```text
Go 1.26.x
Cobra：子命令、Flag、Help 和 Completion
bufio + golang.org/x/term：行式交互和终端能力检测
os/exec：所有外部命令使用 program + argv，禁止隐式 Shell
encoding/json + go.yaml.in/yaml/v3：结构化事件与 Artifact
database/sql + 无 CGO SQLite Driver：本地运行状态
embed：内嵌 Migration、Schema 和默认 Prompt
testing：单元和集成测试
log/slog：结构化日志
```

SQLite Driver 的首选候选是 `modernc.org/sqlite`，因为它实现 `database/sql` 且不依赖 CGO，符合跨平台单二进制目标；最终版本号必须在实现设计中固定并由 `go.sum` 锁定，不得在构建时使用浮动 `latest`。[Driver 文档](https://pkg.go.dev/modernc.org/sqlite)

不推荐 Demo 一开始引入 Bubble Tea 全屏 TUI，原因是：

```text
CFlow 全屏 TUI
    +
Codex / Claude / OpenCode 子进程输出
    +
用户输入转发
```

会增加终端重绘、stdin 所有权、异常恢复和 PTY 兼容问题。

Demo 使用行式交互：

```text
Cobra + bufio/x/term + Streaming stdout
```

能够满足开箱即用，并更容易测试。Native Session Attach 仍为 P1，PTY 必须放在独立平台 Adapter 中，不得把 PTY 依赖扩散到核心 Runtime。

本 PRD 后续保留的 TypeScript 风格代码块仅是既有的行为伪代码，不代表实现语言或最终 API。实现设计必须将其翻译为 Go 接口、结构体、显式错误返回和 `context.Context` 取消语义；不得机械逐行翻译。

### 逻辑架构

```text
┌────────────────────────────────────────────┐
│                  CLI Layer                 │
│ start / status / resume / inspect / apply │
└─────────────────────┬──────────────────────┘
                      │
┌─────────────────────▼──────────────────────┐
│             Application Services           │
│ WorkflowService / PlanService / RunService │
└───────────┬─────────────────────┬──────────┘
            │                     │
┌───────────▼──────────┐ ┌────────▼───────────┐
│    Domain Model      │ │ Workflow Runtime   │
│ State Machine        │ │ DAG Scheduler      │
│ Plan / Spec / Task   │ │ Retry / Checkpoint │
└───────────┬──────────┘ └────────┬───────────┘
            │                     │
┌───────────▼─────────────────────▼───────────┐
│             Infrastructure                  │
│ SQLite / Files / Git / Worktree / Logging  │
└───────────┬─────────────────────┬───────────┘
            │                     │
┌───────────▼──────────┐ ┌────────▼───────────┐
│   Agent Adapters     │ │ Verification       │
│ Codex / Claude / OC  │ │ Commands / Review  │
└──────────────────────┘ └────────────────────┘
```

### 代码目录

```text
cflow/
├── go.mod
├── go.sum
├── Makefile
├── README.md
├── cmd/
│   └── cflow/
│       └── main.go
├── internal/
│   ├── cli/
│   │   ├── root.go
│   │   └── commands/
│   │       ├── start.go
│   │       ├── status.go
│   │       ├── resume.go
│   │       ├── inspect.go
│   │       ├── retry.go
│   │       ├── apply.go
│   │       └── doctor.go
│   ├── domain/
│   │   ├── project.go
│   │   ├── workflow.go
│   │   ├── artifact.go
│   │   ├── node.go
│   │   ├── session.go
│   │   ├── event.go
│   │   └── state_machine.go
│   ├── app/
│   │   ├── project_service.go
│   │   ├── workflow_service.go
│   │   ├── discussion_service.go
│   │   ├── plan_service.go
│   │   ├── spec_service.go
│   │   ├── compiler_service.go
│   │   └── execution_service.go
│   ├── agent/
│   │   ├── adapter.go
│   │   ├── registry.go
│   │   ├── codex.go
│   │   ├── claude.go
│   │   ├── opencode.go
│   │   └── fake.go
│   ├── runtime/
│   │   ├── schema.go
│   │   ├── compiler.go
│   │   ├── dag.go
│   │   ├── scheduler.go
│   │   ├── executors.go
│   │   └── recovery.go
│   ├── gitx/
│   │   ├── repository.go
│   │   ├── worktree.go
│   │   ├── branch.go
│   │   ├── commit_policy.go
│   │   └── diff.go
│   ├── verification/
│   │   ├── command.go
│   │   ├── scope.go
│   │   ├── task_review.go
│   │   └── final_review.go
│   ├── persistence/
│   │   ├── db.go
│   │   ├── migration.go
│   │   ├── migration_backup.go
│   │   ├── repositories/
│   │   ├── artifact_store.go
│   │   ├── artifact_compatibility.go
│   │   └── event_store.go
│   ├── prompt/
│   ├── config/
│   └── platform/
│       └── pty/
├── migrations/
├── prompts/
├── schemas/
│   ├── workflow.schema.json
│   ├── specs.schema.json
│   ├── verification-catalog.schema.json
│   ├── routing-policy.schema.json
│   ├── git-commit-preflight.schema.json
│   ├── migration-backup-manifest.schema.json
│   ├── artifact-envelope.schema.json
│   └── plan-check.schema.json
└── tests/
    ├── integration/
    ├── e2e/
    ├── dogfood/
    └── testdata/
```

Go 单元测试与被测 Package 同目录并使用 `*_test.go`；跨 Package 的完整流程测试放在 `tests/integration` 和 `tests/e2e`。`internal/` 防止 Runtime 内部 Package 被仓库外部误当成稳定 SDK，Demo 不承诺公共 Go Library API。

### Prompt 版本化

每个 Session 必须记录：

```json
{
  "purpose": "PLAN_CHECK",
  "promptTemplate": "plan-check.md",
  "promptVersion": "1.0.0",
  "provider": "claude",
  "providerSessionId": "c680...",
  "inputArtifactHashes": {
    "plan.md": "sha256:..."
  }
}
```

这样修改 Prompt 后，历史 Workflow 仍然可审计。

### 安全边界

CFlow 必须实现以下限制：

| 风险 | 控制 |
|---|---|
| Agent 生成任意 Shell | Spec/Workflow 只允许引用获批的 Verification `command_id`；Catalog 与 Runtime 内建命令均使用 argv 数组且禁止字符串 Shell |
| Agent Session 越权 | Demo 信任 Provider 默认权限和用户现有配置；CFlow 只记录调用事实并明确不提供 OS Sandbox 保证 |
| Prompt Injection | Prompt 声明角色边界；仓库内结果必须经过 Commit、Clean Worktree、Diff、Catalog 和独立 Review 门禁 |
| Provider 配置扩权 | CFlow 不覆盖用户 Provider 配置，也不把现有配置视为安全证明；首次运行、Dry Run 与报告必须提示此信任边界 |
| 网络访问 | Demo 不统一禁止 Agent 网络访问；是否允许由 Provider 默认配置决定，CFlow 不得声称已禁网 |
| Secret 泄露 | 日志对常见 Token、Key 和环境变量值脱敏 |
| 无限循环 | 每个 Node、Task、Workflow 的自动执行与失败重试都有预算和上限；用户主动中断不扣失败 Retry，但也不会被 Runtime 自动循环触发 |
| 越界修改 | Agent 必须提交并留下 Git-clean Worktree；完整 Commit Range 必须匹配 `write_scope`。这是仓库内事后门禁，不是 OS Sandbox |
| 删除测试绕过验收 | Reviewer 检查测试文件删除和验收命令变化 |
| 修改 Workflow 状态 | Agent 只能输出建议，状态由 CFlow 转换 |
| 并发写同一文件 | DAG 静态冲突检测与 Resource Lock |
| 主分支污染 | Coding 只发生在 Worktree Branch |
| 不可恢复执行 | 每次状态转换与外部副作用前后记录事件 |

## 核心伪代码与接口契约

### 启动流程

```text
async function main(): Promise<void> {
  const cwd = process.cwd();
  const discovery = await discoverGitRepository(cwd);

  if (!discovery.isGitRepository || !discovery.hasValidHead) {
    await ui.showNonGitActions({
      allowedCommands: ["doctor", "help"],
      message: "CFlow Workflow requires a Git repository with a valid HEAD",
    });
    return;
  }

  const projectIdentity = await buildProjectIdentity(discovery.gitRoot);

  await storage.initialize({
    schemaLock: "locks/db-schema.lock",
  });

  let project = await projectService.findByIdentity(projectIdentity);
  const workflows = project
    ? await workflowService.listByProject(project.id)
    : [];

  const action = await ui.selectStartupAction(workflows);

  switch (action.type) {
    case "CREATE":
      await leaseManager.withProjectWriter(projectIdentity.projectKey, async () => {
        project = await projectService.findOrCreate(projectIdentity);
        await projectService.markOpened(project.id);
        await recoverInterruptedRuns(project.id);
        await recoverInterruptedApplyAttempts(project.id);
        await createWorkflowFlow(project);
      });
      break;
    case "OPEN":
      // 打开和查看不取 Writer；只有继续、重试、调整、取消等 Mutating Action
      // 通过 runMutatingWorkflowAction 获取 Project Writer + Workflow Owner。
      await openWorkflowFlow(action.workflowId);
      break;
    case "STATUS":
      await showProjectStatus(project?.id);
      break;
    case "EXIT":
      return;
  }
}

async function runMutatingWorkflowAction(workflowId, action) {
  const workflow = await workflowRepository.get(workflowId);

  return leaseManager.withProjectWriter(workflow.projectId, async () => {
    await recoverInterruptedRuns(workflow.projectId);
    await recoverInterruptedApplyAttempts(workflow.projectId);

    return leaseManager.withWorkflowOwner(workflowId, action);
  });
}
```

### 状态转换

所有状态变化必须经过统一服务，禁止直接写表：

```text
async function transitionWorkflow(
  workflowId: string,
  command: WorkflowCommand,
): Promise<Workflow> {
  return db.transaction(async (tx) => {
    const workflow = await workflowRepository.getForUpdate(tx, workflowId);

    const decision = workflowStateMachine.decide(workflow, command);

    if (!decision.allowed) {
      throw new InvalidTransitionError({
        workflowId,
        stage: workflow.stage,
        status: workflow.runtimeStatus,
        command,
        reason: decision.reason,
      });
    }

    const updated = decision.apply(workflow);

    await workflowRepository.save(tx, updated);
    await eventStore.append(tx, {
      eventId: randomUUID(),
      workflowId,
      type: decision.eventType,
      payload: decision.eventPayload,
      createdAt: clock.now(),
    });

    return updated;
  });
}
```

### 新建 Workflow

```text
async function createWorkflowFlow(project: Project): Promise<void> {
  const name = await ui.askWorkflowName();
  const requirement = await ui.readMultilineRequirement();

  const gitState = await git.inspect(project.canonicalPath);
  const dirtyFingerprint = gitState.dirty
    ? await git.computeDirtyFingerprint(project.canonicalPath)
    : null;

  if (gitState.dirty) {
    const confirmed = await ui.confirmDirtyWorkspaceIsolation({
      targetBranch: gitState.currentBranch,
      baseCommit: gitState.headCommit,
      dirtyFingerprint,
      message: "未提交修改不会进入本 Workflow",
    });
    if (!confirmed) return;
  }

  const workflow = await workflowService.create({
    projectId: project.id,
    name,
    targetBranch: gitState.currentBranch,
    baseCommit: gitState.headCommit,
    initialWorktreeDirty: gitState.dirty,
    initialDirtyFingerprint: dirtyFingerprint,
    initialStage: "REQUIREMENT_DISCUSSION",
    initialStatus: "PENDING",
  });

  await artifactStore.writeWorkflowMetadata(workflow);

  await worktreeManager.ensureBaseSnapshotWorktree({
    workflowId: workflow.id,
    baseCommit: workflow.baseCommit,
    purpose: "PLANNING_SNAPSHOT",
    requireUnchangedAfterNonCodingSession: true,
  });

  const provider = await ui.selectAgent(
    await agentRegistry.detectProtocolCompatibleProviders({
      purpose: "REQUIREMENT_DISCUSSION",
    }),
  );

  await transitionWorkflow(workflow.id, {
    type: "START_REQUIREMENT_DISCUSSION",
    provider,
  });

  await runDiscussion(workflow.id, requirement, provider);
}
```

### 需求讨论与 Session 恢复

```text
async function runDiscussion(
  workflowId: string,
  initialRequirement: string,
  provider: AgentProvider,
): Promise<void> {
  let session = await sessionRepository.findActive(
    workflowId,
    "REQUIREMENT_DISCUSSION",
  );

  if (!session) {
    session = await sessionService.create({
      workflowId,
      purpose: "REQUIREMENT_DISCUSSION",
      provider,
      status: "STARTING",
    });
  }

  let nextInput = initialRequirement;

  while (true) {
    const request = buildDiscussionRequest({
      workflowId,
      session,
      userInput: nextInput,
    });

    const { activeSession, events, degradedRecovery } =
      await sessionRunner.openEventStream({
        session,
        request,
        // Resume 不可恢复时：原 Session -> LOST，生成不可变 Context Bundle，
        // 创建带 supersedes_session_id 的继任 Session；跨 Provider 前重查协议能力，
        // 工具权限仍采用目标 Provider 默认配置。
        resumeFallback: "SUCCESSOR_WITH_CONTEXT_BUNDLE",
      });

    session = activeSession;

    if (degradedRecovery) {
      ui.notifySessionRecovery({
        lostSessionId: degradedRecovery.lostSessionId,
        successorSessionId: session.id,
        provider: session.provider,
        contextBundleRevision: degradedRecovery.contextBundleRevision,
      });
    }

    for await (const event of events) {
      await agentEventStore.persist(session.id, event);
      ui.renderAgentEvent(event);

      if (event.type === "session_started") {
        session = await sessionService.bindProviderSession(
          session.id,
          event.sessionId,
        );
      }

      if (event.type === "failed") {
        await sessionService.fail(session.id, event);
        throw new AgentExecutionError(event);
      }
    }

    const input = await ui.readDiscussionInput();

    if (input === "/pause") {
      await transitionWorkflow(workflowId, {
        type: "PAUSE",
        reason: "USER_REQUEST",
      });
      return;
    }

    if (input === "/finish") {
      await finalizePlan(workflowId, session);
      return;
    }

    nextInput = input;
  }
}
```

### Plan 生成

```text
async function finalizePlan(
  workflowId: string,
  discussionSession: Session,
): Promise<void> {
  await transitionWorkflow(workflowId, {
    type: "START_PLAN_GENERATION",
  });

  const outputSchema = planOutputSchema();

  const result = await agentRunner.resumeForStructuredResult({
    session: discussionSession,
    prompt: promptRegistry.render("plan-finalize", {
      requiredSections: PLAN_REQUIRED_SECTIONS,
      outputFormat: "markdown",
    }),
    outputSchema,
  });

  const plan = planParser.parse(result.planMarkdown);

  const validation = planStaticValidator.validate(plan);

  if (!validation.ok) {
    await transitionWorkflow(workflowId, {
      type: "PLAN_GENERATION_FAILED",
      errors: validation.errors,
    });
    return;
  }

  const planArtifact = await artifactStore.writeImmutablePlanRevision(
    workflowId,
    plan,
  );

  await transitionWorkflow(workflowId, {
    type: "PLAN_DRAFT_CREATED",
    planRevision: planArtifact.revision,
    planHash: planArtifact.sha256,
    planPath: planArtifact.path,
  });
}
```

### Plan Check

```text
async function checkPlan(
  workflowId: string,
  checkerProvider: AgentProvider,
): Promise<void> {
  const workflow = await workflowRepository.get(workflowId);
  const plan = await artifactStore.readPlan(workflowId);
  const discussionSession = await sessionRepository.findByPurpose(
    workflowId,
    "REQUIREMENT_DISCUSSION",
  );

  await assertPlanStatus(workflow, "DRAFT");

  const checkerSession = await sessionService.create({
    workflowId,
    purpose: "PLAN_CHECK",
    provider: checkerProvider,
    status: "STARTING",
  });

  if (
    checkerSession.providerSessionId &&
    checkerSession.providerSessionId === discussionSession.providerSessionId
  ) {
    throw new InvariantViolation("Plan checker must use an independent session");
  }

  await transitionWorkflow(workflowId, {
    type: "START_PLAN_CHECK",
  });

  const result = await agentRunner.runForStructuredResult({
    session: checkerSession,
    prompt: promptRegistry.render("plan-check", {
      plan,
      repositoryPath: workflow.repositoryPath,
      baseCommit: workflow.baseCommit,
    }),
    outputSchema: planCheckSchema,
  });

  const checkArtifact = await artifactStore.writeImmutablePlanCheck(
    workflowId,
    {
      planRevision: workflow.activePlanRevision,
      planHash: workflow.activePlanHash,
      checkerProvider,
      checkerSessionId: checkerSession.providerSessionId,
      result,
    },
  );

  switch (result.decision) {
    case "pass":
      await transitionWorkflow(workflowId, {
        type: "PLAN_CHECK_PASSED",
        checkArtifactPath: checkArtifact.path,
        checkArtifactHash: checkArtifact.sha256,
      });
      return;

    case "needs_discussion":
      await transitionWorkflow(workflowId, {
        type: "PLAN_CHECK_NEEDS_DISCUSSION",
        gaps: result.blockingGaps,
      });
      return;

    case "needs_revision":
      await transitionWorkflow(workflowId, {
        type: "PLAN_CHECK_NEEDS_REVISION",
        gaps: result.blockingGaps,
      });
      return;

    case "reject":
      await transitionWorkflow(workflowId, {
        type: "PLAN_CHECK_REJECTED",
        reason: result.summary,
      });
  }
}
```

Plan Approval 伪代码：

```text
async function approvePlan(workflowId, expectedPlanRevision, expectedPlanHash) {
  await storage.transaction(async (tx) => {
    const workflow = await workflowRepository.getForUpdate(tx, workflowId);

    assertGate(workflow, "PLAN_CHECK", "PAUSED", "CHECKED");
    assertExactRef(workflow.activePlan, {
      revision: expectedPlanRevision,
      sha256: expectedPlanHash,
    });

    await approvalRepository.append(tx, {
      gateType: "PLAN",
      decision: "APPROVED",
      actor: "USER",
      planRevision: expectedPlanRevision,
      planSha256: expectedPlanHash,
    });

    await transitionWorkflowInTransaction(tx, workflow, {
      type: "PLAN_APPROVED_BY_USER",
    });
  });
}
```

### Spec 生成

```text
async function generateSpecs(workflowId: string): Promise<void> {
  const plan = await artifactStore.readApprovedPlan(workflowId);

  await transitionWorkflow(workflowId, {
    type: "START_SPEC_GENERATION",
  });

  const result = await agentRunner.runForStructuredResult({
    purpose: "SPEC_GENERATION",
    prompt: promptRegistry.render("spec-generate", {
      plan,
      requirements: {
        stableTaskIds: true,
        explicitDependencies: true,
        explicitWriteScopes: true,
        explicitAcceptance: true,
        explicitResourceLocks: true,
      },
    }),
    outputSchema: specsSchema,
  });

  const staticResult = specValidator.validate(result);

  if (!staticResult.ok) {
    await transitionWorkflow(workflowId, {
      type: "SPEC_GENERATION_FAILED",
      errors: staticResult.errors,
    });
    return;
  }

  const graph = DependencyGraph.fromSpecs(result.tasks);

  if (graph.hasCycle()) {
    throw new SpecValidationError("Task dependency graph contains a cycle");
  }

  const conflicts = scopeConflictDetector.find(result.tasks);

  const normalizedSpecs = serializeConflictingTasks(result.tasks, conflicts);

  await artifactStore.writeSpecs(workflowId, normalizedSpecs);

  const catalogResult = await verificationCatalogCompiler.compile({
    baseCommit: plan.repositoryBaseCommit,
    discoveredCandidates: await verificationDiscovery.scanBaseCommit(
      plan.repositoryBaseCommit,
    ),
    proposedCommands: result.proposedCommands,
    commandRefs: collectCommandRefs(normalizedSpecs),
  });

  await artifactStore.writeVerificationPolicyReport(
    workflowId,
    catalogResult.policyReport,
  );

  if (!catalogResult.ok) {
    await transitionWorkflow(workflowId, {
      type: "VERIFICATION_CATALOG_FAILED",
      errors: catalogResult.errors,
    });
    return;
  }

  const catalog = await artifactStore.writeVerificationCatalog(
    workflowId,
    catalogResult.catalog,
  );

  await transitionWorkflow(workflowId, {
    type: "SPECS_GENERATED",
    taskCount: normalizedSpecs.tasks.length,
    verificationCatalogRevision: catalog.revision,
    verificationCatalogHash: catalog.sha256,
  });
}
```

### Dynamic Workflow 编译

```text
async function generateAndCompileWorkflow(
  workflowId: string,
): Promise<void> {
  const plan = await artifactStore.readApprovedPlan(workflowId);
  const specs = await artifactStore.readSpecs(workflowId);
  const verificationCatalog =
    await artifactStore.readVerificationCatalog(workflowId);

  await transitionWorkflow(workflowId, {
    type: "START_WORKFLOW_GENERATION",
  });

  const baselineWorkflow = workflowCompiler.compileBaseline({
    plan,
    specs,
    verificationCatalog,
  });

  const proposedPatch = await agentRunner.runForStructuredResult({
    purpose: "WORKFLOW_OPTIMIZATION",
    prompt: promptRegistry.render("workflow-optimize", {
      plan,
      specs,
      verificationCatalog,
      baselineWorkflow,
      supportedPatchOperations: SUPPORTED_WORKFLOW_PATCH_OPERATIONS,
    }),
    outputSchema: workflowPatchSchema,
  });

  const report = workflowCompiler.applyAndValidatePatch({
    plan,
    specs,
    verificationCatalog,
    baselineWorkflow,
    proposedPatch,
  });

  await artifactStore.writeCompileReport(workflowId, report);

  if (!report.ok) {
    await transitionWorkflow(workflowId, {
      type: "WORKFLOW_COMPILE_FAILED",
      errors: report.errors,
    });
    return;
  }

  await artifactStore.writeDynamicWorkflow(
    workflowId,
    report.normalizedWorkflow,
  );

  await transitionWorkflow(workflowId, {
    type: "WORKFLOW_READY",
    nodeCount: report.normalizedWorkflow.nodes.length,
  });
}
```

`WORKFLOW_READY` 只将 Workflow 置为 `WORKFLOW_GENERATION/PAUSED` 并生成可审计 Dry Run，不得启动 Scheduler 或创建 Coding Task Worktree。

Execution Approval 伪代码：

```text
async function approveExecution(workflowId, expected) {
  await compiler.revalidateDryRun(expected.dynamicWorkflow);
  await verificationPolicy.revalidateCatalog(expected.verificationCatalog);
  const currentPreflight = await gitCommitPolicy.assertSuccessfulPreflight({
    workflowId,
    repositoryContext: "TARGET_REPOSITORY",
  });
  await gitCommitPolicy.compareAndSwapCurrentFingerprint(
    expected.gitCommitPreflight.fingerprint,
  );
  assertExactRef(currentPreflight, expected.gitCommitPreflight);
  await agentRegistry.revalidateRoutingProtocolCapabilities(
    expected.routingPolicy,
    expected.fallbackProviders,
  );

  await storage.transaction(async (tx) => {
    const workflow = await workflowRepository.getForUpdate(tx, workflowId);

    assertGate(workflow, "WORKFLOW_GENERATION", "PAUSED", "APPROVED");
    assertExactRef(workflow.activePlan, expected.plan);
    assertExactRef(workflow.activeSpecs, expected.specs);
    assertExactRef(
      workflow.activeVerificationCatalog,
      expected.verificationCatalog,
    );
    assertExactRef(workflow.activeDynamicWorkflow, expected.dynamicWorkflow);
    assertExactHash(workflow.routingPolicy, expected.routingPolicySha256);
    assertExactHash(workflow.budgetPolicy, expected.budgetPolicySha256);

    await approvalRepository.append(tx, {
      gateType: "EXECUTION",
      decision: "APPROVED",
      actor: "USER",
      gitCommitPreflight: expected.gitCommitPreflight,
      ...expected,
      decisionContext: expected.replacementContext
        ? {
            reason: "COMMIT_POLICY_DRIFT_REPLACEMENT",
            supersededExecutionApprovalId:
              expected.replacementContext.supersededExecutionApprovalId,
            quarantineIds: expected.replacementContext.quarantineIds,
            reconciliationManifest:
              expected.replacementContext.reconciliationManifest,
            absorbsCommitPolicyConfirmation: true,
          }
        : {},
    });

    await transitionWorkflowInTransaction(tx, workflow, {
      type: "EXECUTION_APPROVED_BY_USER",
    });
  });
}
```

Dry Run 和 Git Fingerprint 重校验发生在事务外，但 Approval Insert 前必须在事务内再次比较所有活动引用、Preflight Revision/Hash/Fingerprint 和 Replacement Context。任何不匹配都返回 `APPROVAL_INPUT_CHANGED`，保持 `PAUSED` 并要求重新展示完整预览。动作启动前仍须再次 CAS，不能把批准事务与外部 Git 配置变化误称为原子操作。

Commit Policy 漂移确认伪代码：

```text
async function requireConfirmedCommitPolicy(context) {
  const current = await gitCommitPolicy.recomputeFingerprint(context);
  const confirmed = await approvalRepository.latestConfirmedCommitPolicy(context);

  if (current.fingerprint === confirmed.fingerprint) {
    return gitCommitPolicy.reuseSuccessfulPreflight(confirmed.preflight);
  }

  await commitPolicySafetyStop.stopAllActiveAttempts({
    ...context,
    previousFingerprint: confirmed.fingerprint,
    detectedFingerprint: current.fingerprint,
    retryBudgetCharged: false,
  });

  const next = await gitCommitPolicy.runAndPersistPreflight(current);
  if (!next.success) throw blockWithPreflightFailure(next);

  const approval = await approvalRepository.findExactCommitPolicyApproval({
    workflowId: context.workflowId,
    applyAttemptId: context.applyAttemptId,
    targetHead: context.targetHead,
    integrationHead: context.integrationHead,
    preflightRevision: next.revision,
    preflightSha256: next.sha256,
    fingerprint: next.fingerprint,
  });

  if (!approval) {
    await checkpointBeforeCommitCapableAction(context);
    await pauseWorkflowOrBlockApply(
      context,
      "COMMIT_POLICY_CONFIRMATION_REQUIRED",
    );
    throw confirmationRequired(diff(confirmed.preflight, next));
  }

  await gitCommitPolicy.compareAndSwapCurrentFingerprint(next.fingerprint);
  await gitCommitPolicy.assertPreflightHash(next);
  return next;
}
```

运行中监控伪代码：

```text
async function monitorCommitPolicy(runId) {
  while (await processRegistry.hasCommitCapableProcess(runId)) {
    await clock.waitAtMost("1s");
    const current = await gitCommitPolicy.recomputeFingerprintForRun(runId);
    const confirmed = await approvalRepository.latestConfirmedCommitPolicyForRun(runId);

    if (current.fingerprint !== confirmed.fingerprint) {
      await commitPolicySafetyStop.stopAllActiveAttempts({
        runId,
        previousFingerprint: confirmed.fingerprint,
        detectedFingerprint: current.fingerprint,
        retryBudgetCharged: false,
      });
      return;
    }
  }
}
```

`stopAllActiveAttempts` 必须与 Scheduler Submit Gate 共用序列化边界，并在发出 Cancel 前固定各 Worktree 的 HEAD/Status/Fingerprint。停止完成后扫描 Drift Window 新 Commit；存在 `COMMIT_DURING_POLICY_DRIFT_WINDOW` 时不得调用普通 Policy Confirmation 路径自动恢复。

用户提交 `COMMIT_POLICY` Approval 时，Runtime 必须在同一事务写入 Approval、Event 和状态转换前重算 Fingerprint 并核对活动 HEAD；不匹配返回 `COMMIT_POLICY_INPUT_CHANGED`。初始 Execution Approval 已承担对当时 Preflight 的确认，因此不会紧接着再展示一次异常确认。

Replacement Execution Approval 同样承担其精确 Preflight 的确认。`latestConfirmedCommitPolicy` 必须统一读取有效 `EXECUTION` 与 `COMMIT_POLICY` Approval，并返回 Approval 来源；如果活动 Replacement Spec/Workflow Revision 尚未获批，则不得把旧 `COMMIT_POLICY` Approval 当成对新执行 Artifact 的授权。Apply 没有 Replacement Execution Gate，始终沿用独立 Apply `COMMIT_POLICY` 规则。

### DAG Scheduler

```text
async function executeWorkflow(workflowId: string): Promise<void> {
  const workflow = await artifactStore.readDynamicWorkflow(workflowId);
  const run = await runService.start(workflowId);

  await worktreeManager.ensureIntegrationWorktree(workflowId);

  await transitionWorkflow(workflowId, {
    type: "START_EXECUTION",
    runId: run.id,
  });

  const graph = Dag.from(workflow.nodes);
  const scheduler = new Scheduler({
    maxParallelism: workflow.settings.maxParallelism,
  });

  while (!graph.isTerminal()) {
    await runService.heartbeat(run.id);

    const runState = await runService.get(run.id);
    if (runState.status === "QUIESCING") {
      const results = await scheduler.collectCompletedSnapshotAttempts(
        runState.quiesceSnapshot,
      );
      await persistQuiescingResultsWithoutDispatch(results);

      if (await scheduler.snapshotAttemptsSettled(runState.quiesceSnapshot)) {
        await finalizeWorkflowBlockedAfterQuiesce(run.id);
        return;
      }

      continue;
    }

    const readyNodes = [];
    for (const node of graph.readyNodes()) {
      if (!lockManager.canAcquire(node.resourceLocks)) continue;
      if (!await dependencyGate.requiredMergesSucceeded(node)) continue;
      readyNodes.push(node);
    }

    if (readyNodes.length === 0) {
      if (graph.hasRunningNodes()) {
        await scheduler.waitForAnyCompletion();
        continue;
      }

      throw new WorkflowDeadlockError(graph.explainBlockedNodes());
    }

    for (const node of readyNodes) {
      scheduler.submit(async () => {
        await lockManager.withLocks(node.resourceLocks, async () => {
          await executeNode({
            workflowId,
            runId: run.id,
            node,
          });
        });
      });
    }

    const results = await scheduler.collectCompleted();

    for (const result of results) {
      graph.applyResult(result);

      await eventStore.append({
        workflowId,
        runId: run.id,
        eventType: "NODE_COMPLETED",
        payload: result,
      });

      if (result.status === "FAILED") {
        const disposition = await handleNodeFailure(workflowId, result);
        if (disposition.requiresUserAction) {
          await runService.beginQuiescingOrBlockImmediately({
            workflowId,
            runId: run.id,
            blockingFindingId: disposition.findingId,
            inFlightAttemptIds: scheduler.inFlightAttemptIds(),
          });
        }
      }
    }
  }

  await transitionWorkflow(workflowId, {
    type: "EXECUTION_GRAPH_COMPLETED",
  });

  await runFinalVerification(workflowId, run.id);
}
```

`beginQuiescingOrBlockImmediately` 必须与 Scheduler 的 Submit/Dispatch Gate 使用同一个序列化边界：Quiesce Snapshot 固定后，不得存在“已经不在 Snapshot、却刚好被提交”的竞态 Node。Snapshot 只包含事务开始时已经具有持久化 `RUNNING` Attempt 的 Node；内存中的 Queue 不足以证明已启动。

### Task 执行

```text
async function resolveTaskBaseCommit(
  workflowId: string,
  spec: Spec,
): Promise<string> {
  await dependencyGate.assertRequiredMergesSucceeded(
    workflowId,
    spec.dependsOn,
  );

  const recorded = await taskRepository.getRecordedBaseCommit(
    workflowId,
    spec.id,
  );
  if (recorded) return recorded;

  const integrationHead = await git.resolveRef(
    await branchManager.integrationBranch(workflowId),
  );

  await taskRepository.recordBaseCommitOnce(
    workflowId,
    spec.id,
    integrationHead,
  );

  return integrationHead;
}
```

```text
async function executeAgentTask(
  context: NodeExecutionContext,
): Promise<NodeResult> {
  const spec = await artifactStore.readSpec(
    context.workflowId,
    context.node.specId,
  );

  const task = await taskRepository.getBySpec(context.workflowId, spec.id);

  const worktree = await worktreeManager.ensureTaskWorktree({
    workflowId: context.workflowId,
    taskId: spec.id,
    baseCommit: await resolveTaskBaseCommit(context.workflowId, spec),
  });

  const baseline = await git.snapshot(worktree.path);
  const previousAttempt = await nodeAttemptService.previousAttempt(
    context.node.id,
  );

  if (previousAttempt?.errorCode === "DIRTY_TASK_WORKTREE") {
    await dirtyRepairGate.assertExactFailedAttemptEndState({
      previousAttempt,
      currentState: baseline,
      failureCode: "DIRTY_WORKTREE_DRIFTED",
    });
  }

  const commitPreflight = await gitCommitPolicy.ensureCurrentPreflight({
    workflowId: context.workflowId,
    repositoryContext: worktree.path,
    beforeCommitCapableAction: true,
    consumeRetryBudgetOnFailure: false,
  });

  const attempt = await nodeAttemptService.start(context.node.id);
  await nodeAttemptService.recordGitStart(attempt.id, baseline);

  const session = await sessionService.create({
    workflowId: context.workflowId,
    taskId: spec.id,
    purpose: attempt.attemptNumber === 1
      ? "TASK_IMPLEMENTATION"
      : "TASK_REPAIR",
    provider: spec.execution.provider,
    status: "STARTING",
  });

  const prompt = promptRegistry.render("task-implement", {
    spec,
    planPath: artifactStore.planPath(context.workflowId),
    previousFailures: await nodeAttemptService.previousFailureEvidence(
      context.node.id,
    ),
    requirements: {
      mustCommit: true,
      branch: worktree.branch,
      writeScope: spec.writeScope,
      appendOnlyHistory: true,
      previousRecordedHead: previousAttempt?.endHeadCommit,
    },
  });

  const agentResult = await agentRunner.run({
    session,
    cwd: worktree.path,
    prompt,
    timeoutMs: minutes(spec.execution.timeoutMinutes),
  });

  if (!agentResult.success) {
    return failure("AGENT_EXECUTION_FAILED", agentResult.error);
  }

  const after = await git.snapshot(worktree.path);

  await nodeAttemptService.recordGitEnd(attempt.id, after);
  const auditRef = await git.createAttemptAuditRef({
    workflowId: context.workflowId,
    taskId: spec.id,
    attemptNumber: attempt.attemptNumber,
    targetCommit: after.headCommit,
    requireRefAbsent: true,
  });
  await nodeAttemptService.recordEndHeadAuditRef(attempt.id, auditRef);

  if (
    previousAttempt?.endHeadCommit &&
    !await git.isAncestor(
      worktree.path,
      previousAttempt.endHeadCommit,
      after.headCommit,
    )
  ) {
    return failure("TASK_HISTORY_REWRITTEN", {
      previousHead: previousAttempt.endHeadCommit,
      currentHead: after.headCommit,
      previousAuditRef: previousAttempt.endHeadAuditRef,
      currentAuditRef: auditRef,
      retryable: false,
    });
  }

  if (
    after.headCommit === task.taskBaseCommit ||
    !await git.isAncestor(
      worktree.path,
      task.taskBaseCommit,
      after.headCommit,
    )
  ) {
    return failure("MISSING_IMPLEMENTATION_COMMIT");
  }

  if (!after.gitVisibleClean) {
    return failure("DIRTY_TASK_WORKTREE", {
      statusPorcelainV2: after.statusPorcelainV2,
      dirtyFingerprint: after.dirtyFingerprint,
    });
  }

  const changedFiles = await git.changedFiles(
    worktree.path,
    task.taskBaseCommit,
    after.headCommit,
  );
  const commits = await git.commitsBetween(
    worktree.path,
    task.taskBaseCommit,
    after.headCommit,
  );
  const newCommits = await git.commitsBetween(
    worktree.path,
    previousAttempt?.endHeadCommit ?? task.taskBaseCommit,
    after.headCommit,
  );

  const commitPolicyResult = await gitCommitPolicy.verifyCommits({
    commits: newCommits,
    preflight: commitPreflight,
    commitKind: "TASK_IMPLEMENTATION",
    taskId: task.id,
    nodeAttemptId: attempt.id,
  });
  if (!commitPolicyResult.ok) {
    return failure("COMMIT_POLICY_MISMATCH", {
      retryable: false,
      evidence: commitPolicyResult.evidence,
    });
  }
  await gitCommitPolicy.assertValidEvidenceForFullRange(commits);

  const scopeResult = scopeChecker.check(
    changedFiles,
    spec.writeScope,
  );

  if (!scopeResult.ok) {
    return failure("SCOPE_VIOLATION", scopeResult.violations);
  }

  return success({
    headCommit: after.headCommit,
    changedFiles,
    commits,
    commitPreflightRevision: commitPreflight.revision,
  });
}
```

### Task 验收与修复

```text
async function verifyTask(
  workflowId: string,
  specId: string,
): Promise<NodeResult> {
  const spec = await artifactStore.readSpec(workflowId, specId);
  const task = await taskRepository.getBySpec(workflowId, specId);
  const catalog = await artifactStore.readApprovedVerificationCatalog(
    workflowId,
  );
  const verifiedCommit = task.implementationCommits.at(-1);

  for (const check of spec.acceptance.deterministic) {
    const result = check.type === "command_ref"
      ? await deterministicVerifier.runCatalogCommand({
          entry: catalog.resolve(check.commandId),
          requiredPurpose: "task_verify",
          cwdRoot: task.worktreePath,
          revalidateExecutableIdentity: true,
          shell: false,
        })
      : await deterministicVerifier.runBuiltinCheck(check, {
          cwdRoot: task.worktreePath,
        });

    await verificationStore.record(task.id, check, result);

    if (!result.success) {
      return await scheduleRepairOrFail({
        task,
        failureCode: "COMMAND_FAILED",
        evidence: result,
      });
    }
  }

  const postVerification = await git.inspect(task.worktreePath);
  if (
    postVerification.headCommit !== verifiedCommit ||
    !postVerification.gitVisibleClean
  ) {
    return await scheduleRepairOrFail({
      task,
      failureCode: "VERIFICATION_DIRTIED_WORKTREE",
      evidence: postVerification,
    });
  }

  const reviewerSession = await sessionService.create({
    workflowId,
    taskId: task.id,
    purpose: "TASK_REVIEW",
    provider: selectReviewerProvider(task),
    status: "STARTING",
  });

  assertDifferentSession(
    reviewerSession,
    await sessionRepository.findImplementationSession(task.id),
  );

  const review = await agentRunner.runForStructuredResult({
    session: reviewerSession,
    cwd: task.worktreePath,
    prompt: promptRegistry.render("task-review", {
      spec,
      commits: task.implementationCommits,
      diff: await git.diffForCommits(
        task.worktreePath,
        task.implementationCommits,
      ),
      verificationEvidence: await verificationStore.list(task.id),
    }),
    outputSchema: taskReviewSchema,
  });

  const postReview = await git.inspect(task.worktreePath);
  if (
    postReview.headCommit !== verifiedCommit ||
    !postReview.gitVisibleClean
  ) {
    return await scheduleRepairOrFail({
      task,
      failureCode: "UNEXPECTED_AGENT_MUTATION",
      evidence: postReview,
    });
  }

  if (review.decision !== "pass") {
    return await scheduleRepairOrFail({
      task,
      failureCode: "SEMANTIC_REVIEW_FAILED",
      evidence: review,
    });
  }

  return success({
    commits: task.implementationCommits,
    reviewerSessionId: reviewerSession.providerSessionId,
    reviewEvidenceHash: review.evidenceHash,
  });
}
```

### Merge

```text
async function mergeTask(
  workflowId: string,
  specId: string,
): Promise<NodeResult> {
  const task = await taskRepository.getBySpec(workflowId, specId);
  const integration = await worktreeManager.getIntegration(workflowId);

  await lockManager.assertHeld(`integration:${workflowId}`);
  await git.assertHeadAndClean(
    task.worktreePath,
    task.implementationCommits.at(-1),
  );
  await git.assertClean(integration.path);

  const preMergeHead = await git.resolveHead(integration.path);
  const commitPreflight = await gitCommitPolicy.ensureCurrentPreflight({
    workflowId,
    repositoryContext: integration.path,
    beforeCommitCapableAction: true,
    consumeRetryBudgetOnFailure: false,
  });

  const merge = await git.mergeNoFastForward({
    cwd: integration.path,
    branch: task.branchName,
    message: `merge(cflow/${workflowId}/${specId}): integrate task`,
  });

  if (merge.conflicted) {
    return await resolveMergeConflict({
      workflowId,
      specId,
      integration,
      preMergeHead,
      conflictFiles: merge.conflictFiles,
      allowedWriteScope: await specRepository.unionWriteScopesForMerge(
        workflowId,
        specId,
      ),
      commitPreflight,
      maxAttempts: 1,
    });
  }

  const mergeCommitPolicy = await gitCommitPolicy.verifyCommits({
    commits: [merge.commit],
    preflight: commitPreflight,
    commitKind: "INTEGRATION_MERGE",
    taskId: task.id,
  });
  if (!mergeCommitPolicy.ok) {
    await git.restoreManagedIntegrationWorktree({
      cwd: integration.path,
      expectedCurrentHead: merge.commit,
      targetHead: preMergeHead,
    });
    return failure("COMMIT_POLICY_MISMATCH", {
      retryable: false,
      evidence: mergeCommitPolicy.evidence,
    });
  }

  const verification = await runAffectedVerification({
    workflowId,
    specId,
    cwd: integration.path,
  });

  if (!verification.success) {
    await git.restoreManagedIntegrationWorktree({
      cwd: integration.path,
      expectedCurrentHead: merge.commit,
      targetHead: preMergeHead,
    });

    const repairSpec = await repairService.createFromIntegrationFailure({
      workflowId,
      sourceSpecId: specId,
      evidence: verification,
      preMergeHead,
    });

    return failure("INTEGRATION_VERIFICATION_FAILED", {
      verification,
      repairSpecId: repairSpec.id,
      restoredHead: preMergeHead,
    });
  }

  return success({
    mergeCommit: merge.commit,
  });
}
```

### 显式 Apply 与 Target Drift

```text
async function applyCompletedWorkflow(workflowId: string): Promise<void> {
  const workflow = await workflowRepository.get(workflowId);
  await assertWorkflowCompletedWithFinalEvidence(workflow);

  const target = await git.inspect(workflow.repositoryPath);
  await assertCleanExpectedTargetWorktree(target, workflow.targetBranch);

  const integrationHead = await git.resolveRef(workflow.integrationBranch);
  const attempt = await applyAttemptService.start({
    workflowId,
    targetHeadAtStart: target.headCommit,
    integrationHead,
  });

  const staging = await worktreeManager.createApplyStagingWorktree({
    workflowId,
    attemptNumber: attempt.attemptNumber,
    baseCommit: target.headCommit,
  });

  const commitPreflight = await gitCommitPolicy.ensureCurrentPreflight({
    workflowId,
    repositoryContext: staging.path,
    beforeCommitCapableAction: true,
    consumeRetryBudgetOnFailure: false,
  });

  const merge = await git.mergeNoFastForward({
    cwd: staging.path,
    branch: workflow.integrationBranch,
    message: `apply(cflow/${workflowId}): stage verified integration`,
  });

  if (merge.conflicted) {
    await resolveApplyConflictOnce({
      workflowId,
      attempt,
      staging,
      conflictFiles: merge.conflictFiles,
      commitPreflight,
    });
  }

  const stagedHead = await git.resolveHead(staging.path);
  const applyCommitPolicy = await gitCommitPolicy.verifyCommits({
    commits: [stagedHead],
    preflight: commitPreflight,
    commitKind: "APPLY_STAGING_MERGE",
    applyAttemptId: attempt.id,
  });
  if (!applyCommitPolicy.ok) {
    await applyAttemptService.block(attempt.id, "COMMIT_POLICY_MISMATCH");
    return;
  }

  const verification = await runFullApplyVerification({
    workflowId,
    cwd: staging.path,
    verificationCatalog: await resolveApplyVerificationCatalog({
      workflowId,
      applyAttemptId: attempt.id,
      requiredPurpose: "apply_verify",
      revalidateSourceAndExecutableIdentity: true,
    }),
    reviewerPurpose: "APPLY_VERIFICATION",
    requireIndependentSession: true,
  });

  if (!verification.success) {
    await applyAttemptService.fail(attempt.id, verification);
    return;
  }

  const stagedApplyCommit = await git.resolveHead(staging.path);
  await applyAttemptService.markReadyToApply({
    attemptId: attempt.id,
    stagedApplyCommit,
    verification,
  });

  const latestTarget = await git.inspect(workflow.repositoryPath);
  await assertCleanExpectedTargetWorktree(
    latestTarget,
    workflow.targetBranch,
  );

  if (latestTarget.headCommit !== attempt.targetHeadAtStart) {
    await applyAttemptService.block(attempt.id, "TARGET_HEAD_DRIFTED");
    return;
  }

  await git.fastForwardOnly({
    cwd: workflow.repositoryPath,
    expectedHead: attempt.targetHeadAtStart,
    targetCommit: stagedApplyCommit,
  });

  await applyAttemptService.complete({
    attemptId: attempt.id,
    appliedTargetCommit: stagedApplyCommit,
  });
}
```

### 受控停止

```text
async function stopRunOnInterrupt(runId, signalCount) {
  if (signalCount === 1) {
    await storage.transaction(async (tx) => {
      await runRepository.markStopping(tx, runId, "USER_SIGINT");
      await scheduler.stopDispatching(tx, runId);
      await eventStore.append(tx, {
        runId,
        eventType: "CONTROLLED_STOP_REQUESTED",
      });
    });

    await processSupervisor.cancelAllAndDrain({
      runId,
      gracePeriod: "10s",
      terminationPeriod: "2s",
    });
  } else {
    await processSupervisor.forceTerminateAllProcessGroups(runId);
  }

  const facts = await checkpointInterruptedWork(runId);
  if (facts.liveManagedProcesses.length > 0) {
    await quarantineProjectMutation(runId, "ORPHAN_CHILD_PROCESS");
    await blockWorkflow(runId);
    return;
  }

  await storage.transaction(async (tx) => {
    await attemptRepository.interruptActive(tx, runId, {
      retryBudgetCharged: false,
      evidence: facts,
    });
    await sessionRepository.interruptActive(tx, runId);
    await nodeRepository.readyInterruptedNodes(tx, runId);
    await runRepository.markInterrupted(tx, runId);
    await workflowRepository.pause(tx, runId);
    await eventStore.append(tx, {
      runId,
      eventType: "CONTROLLED_STOP_COMPLETED",
    });
  });
}
```

Signal Handler 本身只设置并发安全的停止标记并唤醒协调循环，不直接执行 SQLite、日志或 Git 操作。进程 Supervisor 必须保证 Cancel/Terminate/Kill 幂等；第二次 Ctrl+C 只升级终止阶段，不创建第二个 Stop 流程。

### 崩溃恢复

```text
async function recoverInterruptedRuns(projectId: string): Promise<void> {
  await leaseManager.assertProjectWriterHeld(projectId);
  const runs = await runRepository.findRunningByProject(projectId);

  for (const run of runs) {
    const oldOwner = await leaseRepository.findWorkflowOwner(run.workflowId);
    const identityAlive = oldOwner
      ? await processInspector.matchesStartToken(
          oldOwner.pid,
          oldOwner.processStartToken,
        )
      : false;
    const liveChildren = await processRegistry.findLiveChildren(run.id);

    if (await osLockInspector.isHeldByAnotherProcess(run.workflowId)) {
      await reconciliationService.recordFinding(run.workflowId, {
        type: "PROJECT_BUSY_OR_HUNG",
        owner: oldOwner,
      });
      continue;
    }

    if (identityAlive) {
      // OS Lock 与 Process Identity 矛盾，不能仅凭 Heartbeat 推断旧 Runtime 已死。
      await reconciliationService.recordFinding(run.workflowId, {
        type: "LEASE_PROCESS_INCONSISTENT",
        owner: oldOwner,
      });
      continue;
    }

    if (liveChildren.length > 0) {
      await reconciliationService.recordFinding(run.workflowId, {
        type: "ORPHAN_CHILD_PROCESS",
        processes: liveChildren,
      });
      await projectService.quarantineMutation(projectId, {
        reason: "ORPHAN_CHILD_PROCESS",
        runId: run.id,
      });
      await workflowRepository.block(run.workflowId);
      continue;
    }

    await db.transaction(async (tx) => {
      await leaseRepository.markStale(tx, oldOwner);
      await runRepository.markInterrupted(tx, run.id);

      await workflowRepository.setRuntimeStatus(
        tx,
        run.workflowId,
        "PAUSED",
      );

      await eventStore.append(tx, {
        workflowId: run.workflowId,
        runId: run.id,
        eventType: "RUN_INTERRUPTED",
        payload: {
          previousOwner: oldOwner,
          reason: "OS_LOCK_FREE_AND_PROCESS_IDENTITY_GONE",
        },
      });
    });

    await reconcileWorkflowFromExternalFacts(run.workflowId);
  }
}

async function reconcileWorkflowFromExternalFacts(
  workflowId: string,
): Promise<void> {
  const tasks = await taskRepository.list(workflowId);
  const worktrees = await git.listWorktreesPorcelain();

  for (const task of tasks) {
    const tree = worktrees.find((it) => it.path === task.worktreePath);

    if (!tree) {
      await reconciliationService.recordTaskFinding(task.id, {
        type: "WORKTREE_MISSING",
      });
      continue;
    }

    const gitState = await git.inspect(tree.path);

    if (gitState.headCommit !== task.recordedHeadCommit) {
      await reconciliationService.recordTaskFinding(task.id, {
        type: "HEAD_MISMATCH",
        actual: gitState.headCommit,
        expected: task.recordedHeadCommit,
      });
    }

    if (!gitState.gitVisibleClean) {
      await reconciliationService.recordTaskFinding(task.id, {
        type: "DIRTY_TASK_WORKTREE",
        statusPorcelainV2: gitState.statusPorcelainV2,
        dirtyFingerprint: gitState.dirtyFingerprint,
      });
    }
  }
}
```

Recovery 遇到最后一个失败原因为 `DIRTY_TASK_WORKTREE` 的 Task 时，只有当前 HEAD、Porcelain Status 和 Dirty Fingerprint 与该 Attempt 的结束证据完全一致，才能恢复为同一 Execute Node 的可重试状态；不一致时记录 `DIRTY_WORKTREE_DRIFTED` Finding 并保持 `BLOCKED`。Recovery 不得自动清理、提交或复制 Dirty 内容到新 Worktree。

Recovery 遇到受控 `INTERRUPTED` Attempt 时，必须先验证已记录的 Process Group 不再存活，并比较当前 HEAD、Porcelain Status、Dirty Fingerprint 与中断 Checkpoint。事实一致时 Node 保持 `READY`、Workflow 保持 `PAUSED`，直到用户显式 Resume；事实漂移时记录 `INTERRUPTED_WORKTREE_DRIFTED` 并 `BLOCKED`。Verification 被中断时必须从头运行对应 `command_id`，不得把部分输出当作通过证据。

Recovery 在任何 Provider Session Resume 或 Successor/Fallback 创建前必须重验 Approval 固定的 Agent Protocol Binding。当前 Executable/Version/Dialect/Registry 不受支持时记录 `PROVIDER_PROTOCOL_UNSUPPORTED` 或 `PROVIDER_PROTOCOL_BINDING_CHANGED` 并保持 `BLOCKED`，不得尝试 Best-effort Resume、解析未知事件或消耗 Retry。只有原 Execution Approval 已固定且当前仍为 `SUPPORTED` 的 Fallback 才可按既有路由规则使用；否则必须重新生成 Routing Policy/Dry Run 并经过 Execution Approval。

Recovery 遇到 Run `QUIESCING` 或 `WORKFLOW_QUIESCE_REQUESTED` 无 Result 时，绝不能重新打开 Dispatch Gate。存在活的受管子进程时沿用 Orphan Process Quarantine；所有进程已退出后，按 Quiesce Snapshot 和外部事实协调每个 Attempt 的结果，随后将 Run/Workflow 置为 `BLOCKED` 并补记 `WORKFLOW_QUIESCED`。即使某些兄弟 Node 已成功，也不能因此自动恢复 Scheduler 或启动其后继节点。

Recovery 遇到 `COMMIT_POLICY_SAFETY_STOP_REQUESTED` 而没有完成 Result 时，必须保持 Dispatch Gate 关闭，且不得按普通 Quiescing 让活动 Attempt 继续。仍有匹配进程时进入 Project Mutation Quarantine；进程已退出后，依据 Stop Request 前后的 HEAD/Status/Fingerprint 协调全部 Interrupted Attempt，并扫描 Drift Window Commit。没有窗口 Commit 和其他 Finding 时恢复到 Policy Confirmation 的 `PAUSED`；存在窗口 Commit、孤儿进程或原有 Blocking Finding 时保持 `BLOCKED`。

Recovery 遇到 `COMMIT_DURING_POLICY_DRIFT_WINDOW` 时必须先验证 Quarantine Audit Ref、Branch HEAD、Worktree Path 和不可变 Evidence Manifest。Quarantine Record 或 Audit Ref 尚未创建但窗口 Commit 与 Stop Checkpoint 可唯一证明时，只能以 Expected-Absent 语义补齐同一隔离 Intent/Result；Ref 指向冲突 Commit、旧 Branch 已被移动、窗口 Commit 缺失或事实不能唯一解释时保持 `BLOCKED`，不得自动选择可信 HEAD。已经创建 Replacement Task、Integration Branch 或 Apply Attempt 时，Recovery 必须核对其 `replaces/supersedes` 链、新基线和对应 Approval，不得恢复旧 Branch 的调度资格，也不得重复创建另一条替代路径。

如果活动 Repair Spec/Dynamic Workflow Revision 因窗口 Commit 等待新的 Execution Approval，Recovery 必须恢复包含 Quarantine、Replacement 和 Commit Policy 差异的统一 Execution Gate。不存在匹配的 `EXECUTION` Approval 时不得额外展示或接受独立 `COMMIT_POLICY` Gate；存在完全匹配且 `absorbs_commit_policy_confirmation = true` 的 Approval 时，将其作为当前 Policy 确认来源。任一 Artifact、Replacement Base、Quarantine Set、Preflight 或 Fingerprint 不匹配时返回 `APPROVAL_INPUT_CHANGED`，不能只恢复其中的 Policy 部分。

Replacement Execution Approval 已存在时，Recovery 还必须重算 Node Definition Hash、依赖边、可信 Integration Base、各复用 Task 的 HEAD/Status/Dirty Fingerprint 和 Commit Evidence，并与 Reconciliation Manifest Hash 比较。完全一致时，旧 `INTERRUPTED` Attempt 保持终态，在同一可信 Task/Node 下创建后继 Attempt；不一致时记录 `REPLACEMENT_RECONCILIATION_CHANGED`、关闭 Dispatch Gate 并返回统一 Execution Gate。已经成功且分类为 `reuse_succeeded` 的 Node 不重复运行，污染或语义变化节点不得借旧 Node ID 恢复。

Recovery 发现 `WORKFLOW_CANCEL_REQUESTED` 而没有匹配的 `WORKFLOW_CANCELLED` 时，必须优先完成取消而不是恢复 Scheduler。仍有匹配受管进程或未协调副作用时保持 `BLOCKED`/Project Quarantine；所有进程退出、Git/Worktree/Intent 事实可解释且 Checkpoint 完整后，原子写入各活动实体的 `CANCELLED` 终态与 Result Event。该过程不得删除保留资源，也不得把 Interrupted Attempt 重新置为 Running。

Cleanup Recovery 只协调已经存在 `CLEANUP_ITEM_REQUESTED` 的精确目标：Worktree 已从 Git Registry 消失且目录不存在时补记 Completed；两者仍存在且与 Plan Fingerprint 一致时保持可显式重试；Registry 与目录只有一方存在、路径被其他 Worktree 占用或事实漂移时记录 `CLEANUP_FACT_MISMATCH` 并 Block Cleanup Attempt。Recovery 不得自动开始 Dry Run 中尚未 Request 的后续删除。

Recovery 必须验证当前 Task Branch HEAD 等于最后记录的 Attempt HEAD，或是其后代；否则记录 `TASK_HISTORY_REWRITTEN` 并保持 `BLOCKED`。Attempt 审计 Ref 缺失时只有在对应 Commit Object 仍存在且目标值可由 SQLite 证据唯一确定的情况下才能重建；Ref 值冲突不得自动覆盖。

Recovery 还必须确保每个已记录 Commit 都能关联到 Hash 匹配的 Preflight Artifact 和 Commit Evidence。Evidence Row 缺失但 Commit 与 Preflight Artifact 均完整时可以重新执行只读 Identity/Signature Verification 并补记 Event；Preflight Artifact 缺失或 Hash 冲突时不得相信 Commit 的旧通过状态。

Recovery 遇到 `COMMIT_POLICY_CONFIRMATION_REQUIRED` 时，必须恢复待确认 Preflight、上一个已确认 Preflight、差异摘要和受影响动作，并重新计算当前 Fingerprint。仍与待确认 Fingerprint 相同时返回同一确认点；再次变化时记录 `COMMIT_POLICY_INPUT_CHANGED` 并生成或复用对应的新 Preflight，绝不能把旧确认应用到新策略。主 Workflow 的等待状态恢复为 `EXECUTION/PAUSED`；Apply Attempt 的等待状态恢复为 `BLOCKED`，且 Workflow 仍保持 `COMPLETED/SUCCEEDED`。

Apply Recovery 必须根据 Target Ref、Apply Branch、Staging Worktree、验证 Manifest 和 Intent/Result Events 协调：Target 已指向已验证 Apply Commit 时补记成功；Target 仍为 `target_head_at_start` 时保留 Staging 并将 Attempt 标记为 `INTERRUPTED`；出现其他 Ref 变化时标记 `BLOCKED`，不得强制覆盖。

被 Branch Quarantine 标记的 Apply Staging 不适用上述普通 Interrupted 恢复：旧 Attempt 永久保持 `BLOCKED`，Target 必须仍未包含窗口 Commit；只有用户显式创建且事实校验通过、并以 `supersedes_apply_attempt_id` 指向旧 Attempt 的新 Apply Attempt，才能从当前 Target/Integration HEAD 重新开始。

### 原子副作用模式

涉及 Git 或文件系统操作时，SQLite 事务不能覆盖外部副作用，所以采用 Intent/Result 事件：

```text
记录 WORKTREE_CREATE_REQUESTED
    ↓ commit DB transaction
执行 git worktree add
    ↓
成功：记录 WORKTREE_CREATED
失败：记录 WORKTREE_CREATE_FAILED
```

恢复时若只有 `REQUESTED` 没有结果，则检查外部事实：

```text
Worktree 已存在 → 补记 CREATED
Worktree 不存在 → 重新执行或标记 FAILED
```

该模式同样适用于：

```text
Agent 启动
Git Commit
Git Merge
Verification Command
Artifact 写入
```

## Demo 交付计划与验收标准

### Demo 功能范围

本节采用已确认的“完整多 Agent Plan-to-Done 闭环”作为 Demo 范围基线。Codex 与 Claude Adapter、跨阶段路由、并行 Worktree 执行、独立验收、有限重试、恢复和 Integration Merge 均属于价值验证的一部分，不降级为单 Provider 或仅 Planning 的 Demo。

| 优先级 | 功能 | Demo 要求 |
|---|---|---|
| P0 | Project 识别 | 有效 Git Root、有效 HEAD、附着的本地 Target Branch、Project Key、历史 Workflow；Detached HEAD 禁止新建但可操作既有 Workflow，非 Git/无 HEAD 仅 doctor/help |
| P0 | Workflow 新建与恢复 | 创建、暂停、重新进入 |
| P0 | Workflow Cancel | 显式确认、受控停止、终态落盘与完整资源/证据保留 |
| P0 | Safe Cleanup | 只删除精确匹配且完全干净的受管 Worktree/Scratch Directory；Branch、Ref 和证据永久保留 |
| P0 | SQLite 状态机 | 合法转换、事件记录、异常恢复 |
| P0 | Forward-only Migration | 共享/排他 Schema Lock、一致性备份、事务迁移、崩溃恢复与 Artifact 兼容检查 |
| P0 | Local Data Protection | CFLOW_HOME 0700/0600、统一 Redactor、原子敏感 Artifact、失败关闭 |
| P0 | Provider Protocol Registry | 版本/Executable/Dialect/Capability Binding、未知协议 Fail-closed |
| P0 | Codex Adapter | JSONL、Session ID、Resume |
| P0 | Claude Adapter | Stream JSON、Resume、Schema Result |
| P0 | Requirement Discussion | CFlow 托管多轮对话 |
| P0 | Plan 生成与静态校验 | 生成 Draft Plan |
| P0 | 独立 Plan Check | Pass、Needs Revision |
| P0 | Spec 生成 | DAG、文件边界、验收条件 |
| P0 | Workflow Compiler | Schema、无环、覆盖检查 |
| P0 | Worktree Manager | Integration 与 Task Worktree |
| P0 | Git Commit Policy | Identity/Signing Preflight 与提交后验证 |
| P0 | Task Scheduler | 串行、并行、失败重试 |
| P0 | Task Verification | 已批准 Command Catalog Ref + 独立 Reviewer |
| P0 | Integration Merge | 自动合并与冲突状态 |
| P0 | 最终报告 | Plan、任务、Commit、测试结果 |
| P0 | `cflow apply` | Workflow 完成后由用户显式触发，经过安全门禁后交付到 Target Branch |
| P1 | OpenCode Adapter | JSON Events、Session Resume |
| P1 | Native Session Attach | 进入 Provider 原生 UI |
| P1 | Agent 成本统计 | Token、费用、耗时 |
| P2 | CAO Backend | 可选使用 CAO 管理复杂终端 Session |
| P2 | Web UI | Workflow 可视化 |

### 已确认：三层内部交付 Gate

> 决策日期：2026-08-02
>
> 决策状态：已确认

完整 Demo 采用三个可执行内部 Gate 逐层构建和验收。每个 Gate 都必须产出可运行的候选二进制、固定输入、自动化测试和证据报告，但 Gate 1、Gate 2 都只是内部工程里程碑；只有 Gate 3 全部通过，才能宣称 CFlow Demo 完成。

| Gate | 目标与范围 | 必须通过的退出标准 | 对外状态 |
|---|---|---|---|
| Gate 1：Deterministic Core | Project Discovery、CFLOW_HOME 基础安全边界、SQLite/Migration 基线、Artifact Store、Lock/Lease、状态机、基础 CLI、Plan/Spec/Workflow Schema、Compiler、FakeAgentAdapter、Git/Worktree、确定性 Verification 与 Integration 核心 | 使用 Fake Provider 从需求输入运行到 Integration Branch；状态转换、Approval Hash、Commit/Clean/Scope Gate、Intent/Result 和基础 Crash Recovery 均有确定性测试证据 | Internal Core Candidate；不得称为 Demo |
| Gate 2：Real Multi-Agent Runtime | Provider Protocol Registry、Codex/Claude Adapter、独立 Session、跨 Provider 路由、真实并行 DAG、Session Resume/Fallback、Retry/Repair、Reviewer、Merge、Final Verification、Ctrl+C/Cancel 和完整 Recovery | 固定 Fixture 中至少两个并行 Task 经不同真实或协议等价 Provider Dialect 完成 Integration；失败、恢复、Quiescing、权限/协议漂移等关键路径可审计；真实 Codex/Claude E2E 至少成功一次 | Internal Runtime Candidate；不得称为 Demo |
| Gate 3：Release Acceptance | 受保护 `cflow apply`、Safe Cleanup、全部安全/迁移/协议/漂移故障 Fixture、最终报告、跨平台构建检查、真实 CFlow Self-Dogfood | 全部 P0 Fixture Gate 通过；真实 Cross-Provider E2E 证据完整；候选二进制从仓库外对 CFlow 自身完成有边界的 Plan-to-Done Workflow，并经显式 Apply 安全更新 Target Branch | Demo Complete Candidate；仍需发布验收签字 |

Gate 规则：

1. Gate 只控制内部交付与证据成熟度，不改变本 PRD 已确认的完整 Demo P0 范围，也不新增产品级用户 Approval Gate。
2. 后一 Gate 可以修复前一 Gate，但不得绕过或降低前一 Gate 的退出标准；所有历史测试和失败证据保留。
3. Gate 1 必须先用 `FakeAgentAdapter` 打通确定性核心，再进入真实 Provider。Gate 2 不得用 Mock 成功替代真实 Protocol Binding 与至少一次真实 Codex/Claude E2E。
4. 安全机制按依赖随功能一起实现。Gate 3 的“安全与迁移故障 Fixture”表示完成全量故障注入和发布证明，不表示前两 Gate 可以先执行不安全的 Provider、Git、Migration 或外部命令。
5. Gate 1/2 的候选二进制可以供开发验证，但 README、Release Note 和 Final Report 必须明确其未达到 Demo Definition of Done；不得把 Planning-only、单 Provider 或缺少 Apply/Dogfood 的版本包装为 Demo。
6. 任一 Gate 退出都要求工作区中的实现变更已 Commit 且 Git-visible Clean，并附测试命令、结果、Binary Hash 和已知限制；自然语言声明或“测试大多通过”不能替代证据。

### 必须提供的 CLI

```bash
cflow
cflow list
cflow status [workflow-id]
cflow resume [workflow-id]
cflow inspect [workflow-id]
cflow inspect task <task-id>
cflow logs [workflow-id]
cflow retry <task-id>
cflow pause [workflow-id]
cflow cancel [workflow-id]
cflow cleanup [workflow-id]
cflow cleanup [workflow-id] --execute <cleanup-plan-id>
cflow dry-run [workflow-id]
cflow doctor
cflow apply [workflow-id]
```

Demo 中至少完整实现：

```text
cflow
cflow status
cflow resume
cflow inspect
cflow retry
cflow pause
cflow cancel
cflow cleanup
cflow dry-run
cflow doctor
cflow apply
```

### `cflow doctor`

检查内容：

```text
✓ CFlow build version、目标 OS/Architecture 和内嵌 Schema/Migration 版本
✓ Git installed
✓ Git Author/Committer identity configured for current repository
✓ Effective commit signing policy detected
○ Commit signing enabled; Workflow execution preflight probe required
✓ Current directory writable
✓ ~/.cflow writable
✓ CFLOW_HOME owner/modes/symlink boundary secure (directories 0700, sensitive files 0600)
✓ SQLite database healthy
✓ SQLite Schema Version supported; Migration chain/checksums and latest verified backup reported
✓ Referenced Artifact Schema Versions supported by embedded Compatibility Registry
✓ Codex installed and authenticated
✓ Claude installed and authenticated
✓ Codex/Claude CLI Version recognized by Adapter protocol registry
○ Agent permissions use Provider defaults; CFlow does not provide a unified OS sandbox
○ OpenCode not installed
✓ Repository has a valid HEAD
✓ HEAD is attached to local Target Branch
○ Current worktree is dirty; allowed for Workflow creation, changes will be isolated
✓ Advisory Lock supported by local CFLOW_HOME filesystem
✓ No conflicting Project Writer; or show current Workflow/Run/PID/Heartbeat as read-only diagnostics
```

Provider 登录状态的检测应尽量使用 CLI 提供的只读命令；无法可靠判断时，只展示“Installed / Authentication Unknown”，不要尝试读取或复制 Provider 凭据。`doctor` 只检查当前 Provider/CLI Version 是否满足 Adapter 的结构化事件、Session、Schema 和预算协议；它不评估或证明 Provider 默认权限安全。

`doctor` 对每个 Provider 明确输出 `MISSING/SUPPORTED/UNKNOWN_VERSION/INCOMPATIBLE_PROTOCOL`、Executable Path/Binary Hash、CLI Version、Dialect 和 Registry Revision。未知或不兼容时必须列出受影响 Route/Purpose、允许的只读/止损命令和“升级 CFlow 或安装已支持 Provider CLI”的修复建议；不得提供 `--force`、`--ignore-version` 或 Best-effort 开关。Authentication Unknown 单独显示，不能覆盖 Protocol Compatibility。

`doctor` 默认只解析 Identity/Signing 配置并报告是否需要执行签名 Probe；真正会调用 Signer 的 Probe 必须在 Workflow Execution Preflight 中运行，使用隔离临时 Repository、关闭交互并受硬 Timeout 约束。无论成功失败，CFlow 都不得输出私钥位置、Passphrase、Credential Helper 内容或未脱敏环境值。

`doctor` 必须递归检查 CFlow 管理的敏感目录和索引文件的 Owner/Mode、Canonical Path、Symlink Boundary，以及目标文件系统能否可靠执行 Owner-only 权限。发现既有不安全路径时只报告精确路径、当前/期望 Mode 和修复建议，不自动 chmod，也不读取文件内容；Provider 驱动的 Mutating Workflow 以 `INSECURE_CFLOW_HOME_PERMISSIONS` Fail-closed。首次运行和报告同时明确 Demo 不提供应用层加密，OS Backup/磁盘保护属于用户环境责任。

`doctor` 必须只读报告数据库当前/支持的 Schema Version、内嵌 Migration 链与 Checksum 状态、最近一次可验证备份，以及被活动 Workflow 引用的 Artifact Type/Version 兼容性。它不得获取排他 Schema Lock、不得自动执行 Migration，也不得把“存在备份”等同于“允许自动覆盖恢复”。

### 已确认：双层 Demo 验收

> 决策日期：2026-08-02
>
> 决策状态：已确认

CFlow Demo 同时使用确定性 Fixture Gate 和真实 CFlow 自举 Dogfood。两者职责不同，缺一不可：

1. **Fixture Gate** 是可重复的发布门禁。CI 使用固定响应和故障注入，不依赖真实 Provider 安装、认证、网络或模型随机性；任何核心状态机、证据、Git 隔离或恢复行为失败都阻止发布。
2. **Dogfood Validation** 使用真实 Codex 与 Claude，让候选 CFlow 二进制在 CFlow 自身 Git 仓库上完成一项有边界的真实改动。它是 Demo 价值验收的必需证据，但不是每次 Commit 都运行的确定性 CI Test。

真实 Provider 的自然语言声明不构成通过。Fixture 和 Dogfood 最终都必须由 CFlow Runtime 根据 Artifact Hash、Git Commit、确定性测试、独立 Review 和验证 Manifest 判定结果。

### 第一层：确定性 Fixture Gate

准备 Fixture Repository：

```text
calculator-demo/
├── package.json
├── src/
│   ├── add.ts
│   └── subtract.ts
└── test/
```

输入需求：

```text
增加 multiply 和 divide。
divide 遇到除数为零时抛出明确异常。
增加单元测试并更新 README。
```

预期拆分：

```text
S01 implement-multiply
S02 implement-divide
S03 update-readme
S04 integration-tests

S01、S02、S03 可并行
S04 依赖 S01、S02
```

验收：

| 场景 | 通过标准 |
|---|---|
| 新建 Workflow | `~/.cflow` 出现项目和 Workflow |
| 脏工作区隔离 | 未提交修改保持原样且不出现在 Base Snapshot、Task Diff 或 Integration Branch |
| 讨论 | Session ID 被持久化 |
| Session 降级恢复 | Resume 不可恢复时原 Session 为 `LOST`；继任 Session、Context Bundle Revision/Hash 和跨 Provider 能力检查均可审计 |
| Ctrl+C | 第一次停止调度并有限等待 Cancel/事件排空，超时终止 Process Group；第二次立即强制终止。Attempt/Run 为 Interrupted、Retry 不扣减，Checkpoint 后 Workflow Paused，重新执行 `cflow resume` 可恢复 |
| Cancel | 显式确认后停止全部受管进程并进入终态；Worktree、Branch、Audit Ref、Dirty 内容和全部证据保持原样且可 Inspect；残留进程或未协调副作用使 Cancel Intent 保持 Blocked，Recovery 只完成取消而不恢复执行 |
| Cleanup | 默认只生成精确 Plan；Execute 只删除终态 Workflow 中无任何 Tracked/Untracked/Ignored 内容、无进行中 Git 操作且事实匹配的受管 Worktree/Scratch Directory，Branch、Audit Ref 与证据保持可读 |
| Project Writer | 同一项目的第二个 Mutating Runtime 被拒绝，但 Status/Inspect 仍可读；不同项目可并行 |
| Stale Lease | 进程崩溃后 OS Lock 自动释放，残留 SQLite Lease 经 PID + Process Start Token 校验后被协调 |
| Schema Migration | 旧数据库先产生可验证的一致性备份，再在排他 Schema Lock 下事务升级；故障注入后只能确定回滚或完成，旧 Artifact Hash/内容不变 |
| Version Compatibility | 数据库过新、Migration 缺链/Checksum 不符或 Artifact Version 不受支持时 Fail-closed，不执行 Down Migration、Best-effort 解析或 Provider 驱动动作 |
| Plan | 生成不可变 Plan Revision，SQLite `plan_status` 为 `DRAFT` |
| Plan Check | 独立 Session 通过后 SQLite 改为 `CHECKED` 并暂停，Plan Artifact Hash 不变 |
| Plan Approval | 用户批准精确 Revision/Hash 后 SQLite 改为 `APPROVED`；Hash 变化时旧 Approval 不可复用 |
| Spec | 依赖关系和 Write Scope 正确 |
| Dry Run | 展示正确并行分组 |
| Execution Approval | Git Commit Preflight 成功后，用户一次批准 Plan/Spec/Verification Catalog/Workflow Revision、Routing/Fallback、Budget 与当前 Preflight Revision/Hash/Fingerprint，并确认 Provider 默认权限信任边界；Replacement 场景同时展示 Quarantine/Policy 差异并吸收精确 Policy 确认，不重复询问；任一引用变化时保持暂停 |
| Git Commit Preflight | 缺少 Identity 或非交互 Signing Probe 失败时，在 Agent/Merge 启动前阻塞且不消耗 Retry；实际 Commit Identity/Signing 必须与对应 Preflight 一致 |
| Commit Policy 漂移 | 检测后立即关闭调度并安全停止全部活动 Attempt，Interrupted 不扣 Retry；无窗口 Commit 时再确认精确新 Preflight；窗口 Commit 的旧 Branch 永久隔离且不能追溯批准，只能经新 Repair/Workflow Revision 和 Execution Approval 建立替代路径，Apply 必须显式新 Attempt |
| Replacement 增量恢复 | Reconciliation Manifest 将节点分类；未污染兄弟 Task 复用原 Branch/Worktree 并创建不扣 Retry 的后继 Attempt，可信成功 Node 不重做，任一事实漂移返回统一 Execution Gate |
| 并行失败收敛 | 一个 Node 不可重试失败或预算耗尽后停止新调度；只允许快照中已运行 Attempt 收敛，不启动后继 Verify/Merge/Retry，全部结束后 Workflow Blocked |
| Verification Catalog | 只执行已批准且 Purpose 匹配的 `command_id`；自由 argv、未知 ID、身份漂移或越界写入均阻塞 |
| Provider Protocol | 当前 Route/Fallback 的 Executable/Version/Registry/Dialect 必须为 SUPPORTED 并被 Execution Approval 固定；未知/不兼容时不启动 Provider，纯只读诊断与 stop/cancel 仍可用 |
| 本地数据保护 | CFlow 自建目录/敏感文件为 0700/0600；完整对话与事件只有在统一脱敏后落盘，无法安全脱敏时不保存内容并 Blocked；所有读取和 Export 无 Raw/Show-secret 旁路 |
| Provider Routing | Planner/Implementer/Reviewer 可按 Fixture 路由到不同已支持 Provider Event Dialect，且 Session 独立；Binding 漂移或未知事件流必须 Fail-closed |
| Execution | 创建四个 Task Branch 或所需 Worktree |
| Commit/Clean Gate | Coding Agent 返回后必须存在新 Commit，且 Task Worktree 无 Staged、Unstaged 或非 Ignored Untracked 文件；否则不得进入 Verification |
| Dirty Worktree Repair | 当前 Attempt 失败并留证；预算内用同一 Branch/Worktree、新 Attempt 和独立 Repair Session 原地处理，外部漂移或预算耗尽时阻塞 |
| Append-only Task History | Task 可有多个 Commit；Repair 只能追加 Fix/Revert Commit，历史重写会被审计 Ref 与祖先检查发现并阻塞 |
| Scope | S01 不允许修改 divide 文件 |
| Verification | 每个 Task 执行测试 |
| Repair | 注入一次测试失败后创建新 Repair Attempt；旧 Attempt 和证据保持不变 |
| Merge Conflict | 注入文本冲突后最多执行一次受限 Merge Resolution；失败时恢复 Pre-Merge HEAD |
| Merge | 合并到 Integration Branch |
| Final Review | 全量测试通过 |
| Report | 包含 Session、Commit、测试、重试、耗时 |
| Target Branch | 自动执行阶段保持不变 |
| Apply | 只有用户显式调用且全部安全门禁通过后才更新 Target Branch |
| Target Drift | 在独立 Apply Worktree 合并并重新验收；验证期间 Target 再次前进则阻塞且保持不变 |
| Apply Command Identity Drift | Target Drift 改变 Wrapper、Manifest 或 Executable Identity 时，不运行漂移命令、不更新 Target；显式新 Apply Attempt 固定新 Catalog 后才能继续 |

同一个 Fixture Repository 运行相互隔离的场景集，不把所有故障串进一个难以诊断的超长测试：

```text
fixture_happy_path
fixture_ctrl_c_and_resume
fixture_ctrl_c_during_parallel_tasks
fixture_cancel_preserves_all_evidence
fixture_safe_cleanup_and_recovery
fixture_parallel_failure_quiescing
fixture_project_lease_and_stale_recovery
fixture_forward_only_migration_and_recovery
fixture_session_lost_and_context_handoff
fixture_provider_protocol_fail_closed
fixture_sensitive_data_redaction_fail_closed
fixture_missing_commit_and_dirty_worktree
fixture_task_history_rewrite
fixture_git_identity_and_signing_preflight
fixture_git_commit_policy_drift_confirmation
fixture_commit_policy_drift_safety_stop
fixture_commit_policy_drift_replacement_path
fixture_verification_failure_and_repair
fixture_merge_conflict_and_rollback
fixture_apply_target_drift
fixture_verification_catalog_identity_drift
```

每个场景从固定 Git Commit 和独立临时 `CFLOW_HOME` 开始，使用确定的 Clock、ID Generator、Fake Provider Event Stream 和 Fault Injector；不得读取开发者真实的 `~/.cflow`。

### 单元测试要求

至少覆盖：

```text
project_key_test.go
  - 路径映射稳定
  - 路径碰撞由 Hash 区分
  - Symlink 使用 Canonical Path
  - 非 Git 目录、无有效 HEAD 和 Detached HEAD 均不得创建新 Workflow，也不得由 CFlow 猜测或 Checkout Target Branch；Detached HEAD 仍可按已保存事实恢复既有 Workflow

workspace_isolation_test.go
  - Dirty Fingerprint 稳定且覆盖 staged、unstaged 和 untracked 内容
  - 创建 Workflow 不修改用户工作区
  - Base Snapshot 不包含未提交修改
  - Workflow 创建后只有 Planning Snapshot Worktree，Integration Ref/Worktree 在 Execution Approval 前不存在
  - 非编码 Session 改变 Planning Snapshot 的 HEAD、Index、Tracked 或 Untracked 内容时输出无效且不得生成受信 Artifact
  - Execution Approval 后创建 Integration Ref/Worktree；Intent/Result 崩溃恢复只接受精确 Base/Path/HEAD
  - Apply 在脏工作区或错误 Target Branch 上被阻塞

migration_test.go
  - 普通数据库使用者持共享 Schema Lock；Migration 需要排他 Lock，旧 Runtime 活跃时不得升级
  - 备份通过 SQLite 一致性机制创建，目录/文件为 0700/0600，Manifest 的版本、Hash、Size 和 Migration Checksum 可验证
  - 源数据库或备份完整性校验失败时不得开始 Migration
  - 在每个 Migration Fault Point 崩溃时，数据库只能处于完整旧版本或完整新版本，不出现部分 DDL/DML
  - Commit 前崩溃可幂等重试，Commit 后崩溃由 schema_migrations 识别完成且不重复执行
  - 已应用 Checksum 不匹配、Migration 链中断和数据库版本过新时 Fail-closed；不得 Down Migrate 或覆盖数据库
  - doctor 只读报告版本、链和备份状态，不取得排他 Lock 或执行 Migration
  - SQLite Migration 前后所有既有 Artifact 字节和 Hash 不变
  - Artifact Compatibility Registry 显式接受支持版本；未知版本以 ARTIFACT_SCHEMA_UNSUPPORTED 阻塞且不反序列化正文
  - Migration/Compatibility Check 完成前 Recovery 不启动 Scheduler、Provider、Retry、Merge 或 Apply

apply_staging_test.go
  - Target 已前进时从最新 Target HEAD 创建隔离 Staging
  - Staging 合并后重新运行全量验证和独立 Review
  - Target HEAD 未变化时只允许 fast-forward 更新
  - Target 在验证期间再次前进时 Attempt Blocked
  - 合并或验证失败时 Target Branch 保持不变
  - 中断后根据 Target Ref 和 Staging 证据恢复 Apply Attempt
  - Wrapper、Manifest 或 Executable Identity 漂移时以 COMMAND_IDENTITY_CHANGED 阻塞且 Target Branch 不变
  - Apply Staging Merge 前重验 Commit Policy，Merge Commit Identity/Signing 不符时 Target Branch 不变
  - Apply 漂移窗口 Commit 隔离旧 Staging Branch/Worktree，旧 Attempt 保持 Blocked，显式新 Attempt 以 supersedes_apply_attempt_id 关联后从当前 Target/Integration HEAD 重做

verification_catalog_test.go
  - 确定性发现只生成 Candidate，Agent Proposal 未进入批准 Catalog 前不可执行
  - 未知 Command ID、Purpose 不匹配、自由 argv 和 Catalog Hash 不一致均被 Compiler 拒绝
  - Project Wrapper Hash 或 PATH Executable 的绝对路径/Binary Hash 变化时阻塞
  - Shell Interpreter、内联代码、破坏性 Git、Publish/Deploy 和越界绝对路径被策略拒绝
  - 命令只能在受管 Worktree 和批准 cwd 中运行，额外环境变量受 allowlist 限制且 Secret-like 值不写日志
  - Verification 产生 Tracked File 变化时失败；transient_write_paths 在进入 Review/Merge 前必须已清理，最终 Git-visible Untracked Output 失败
  - Catalog Revision/Hash 变化使旧 Execution Approval 失效
  - Apply 只接受 apply_verify Purpose；身份漂移时必须显式创建并确认新 Apply Attempt

git_commit_policy_test.go
  - 缺少或非法 Author/Committer Identity 时 GIT_IDENTITY_NOT_CONFIGURED，且 Provider/Merge 子进程未启动
  - Signing 关闭时 Identity 校验即可通过，不要求签名
  - Signing 开启时在隔离临时 Repository 运行无目标 Ref/Worktree 副作用的有限时 Probe
  - Signing Probe 失败、超时或需要交互时 GIT_SIGNING_PREFLIGHT_FAILED，且不回退为 unsigned
  - CFlow 不修改 Local/Global/System Git Config，不注入 Bot Identity 或关闭签名
  - Policy Fingerprint 变化时生成新 Preflight Revision；未变化时可复用成功 Probe
  - 初始 Execution Approval 精确确认当时 Preflight，不紧接着重复询问
  - 无窗口 Commit 的合法 Policy 漂移在新 Preflight 成功后以 COMMIT_POLICY_CONFIRMATION_REQUIRED 暂停，且 Provider/Merge 未启动、Retry 未消耗
  - Policy 漂移在同一序列化边界关闭 Dispatch Gate、记录 COMMIT_POLICY_SAFETY_STOP_REQUESTED，并停止全部活动兄弟 Attempt
  - Policy Safety Stop 优先于 Failure Quiescing；已有 Blocking Finding 保留且最终 Workflow Blocked
  - 存在 Commit-capable 进程时不慢于每 1 秒重算 Fingerprint；文件通知只提前唤醒，轮询不运行 Signing Probe
  - 被停止 Attempt/Session 为 INTERRUPTED、Retry 不扣减，所有 Worktree、Index、Dirty 内容和既有 Commit 原样保留
  - Stop Request 固定各 Worktree HEAD；停止后扫描新增 Commit，窗口 Commit 生成 COMMIT_DURING_POLICY_DRIFT_WINDOW 并阻塞
  - 后续 COMMIT_POLICY Approval 不得追溯授权窗口 Commit，也不得使其通过 Commit/Clean Gate
  - 窗口 Commit 通过唯一 Quarantine Audit Ref 固定；旧 Branch/Commit/Worktree/Evidence 可读但永不进入 Verify、Merge 或 Apply
  - Task 窗口 Commit 的旧 Attempt 保持 INTERRUPTED、Retry 不扣减，旧 Node 因不可重试 Finding 进入 FAILED 且 Task 不得投影为完成
  - Task 窗口 Commit 生成带 replaces_task_id 的新 Repair Spec/Workflow Revision，并从最后已验收 Integration HEAD 创建新 Branch
  - Integration 窗口 Commit 隔离旧 Integration Branch，从停止前固定的最后已验收 HEAD 创建 Replacement Integration Branch
  - 替代路径不自动 Cherry-pick、Revert、Reset、Rebase、Merge 或复制窗口 Commit，且必须重新通过完整门禁
  - 只有包含窗口 Commit 的 Branch 被隔离；未污染兄弟 Task 在 Approval 与 Resume 一致性检查后仍可继续
  - Quarantine Intent 中断后 Recovery 幂等补齐同一记录和 Audit Ref，不重复创建替代路径
  - 临时 config、环境、--author 或 --no-gpg-sign 绕过仍由提交后 COMMIT_POLICY_MISMATCH 捕获
  - 强制停止后存在孤儿进程时进入 Project Mutation Quarantine，不能展示可继续执行的 Policy Gate
  - Apply Policy Safety Stop 不改变 Target Branch 或已完成 Workflow 状态
  - 漂移确认绑定精确 Revision/Hash/Fingerprint；再次漂移返回 COMMIT_POLICY_INPUT_CHANGED
  - 已确认 Fingerprint 在同一 Workflow 内不重复询问，拒绝时保持暂停且不启动 Commit-capable 动作
  - 漂移确认不使 Plan/Spec/Workflow 或 Execution Approval 失效
  - latestConfirmedCommitPolicy 同时识别绑定精确 Preflight 的 EXECUTION 与 COMMIT_POLICY Approval，并返回来源 ID/类型
  - Apply 漂移确认同时绑定 Apply Attempt、Target/Integration HEAD；确认前 Target Branch 不变且 Workflow 保持 Completed
  - Task、Integration 和 Apply Commit 都关联创建前 Preflight，并验证实际 Author/Committer 与签名
  - Repair 前 Policy 合法变化时旧 Commit 保留原 Preflight Evidence，只有新增 Commit 使用新 Revision，完整 Range 必须全部有有效证据
  - Signing 已启用但 Commit 未签名、签名无效或 Signer 不匹配时 COMMIT_POLICY_MISMATCH
  - Agent 用临时 config、环境、--author 或 --no-gpg-sign 绕过时 COMMIT_POLICY_MISMATCH 且不可自动重试
  - Preflight Artifact、Event 和日志不包含私钥、Passphrase、Credential Helper 输出或未脱敏环境值

task_commit_gate_test.go
  - 最终 HEAD 等于 task_base_commit 或不以它为祖先时返回 MISSING_IMPLEMENTATION_COMMIT
  - Staged、Unstaged 或非 Ignored Untracked 任一存在时返回 DIRTY_TASK_WORKTREE
  - Ignored Output 不计入 Git-visible Dirty 状态
  - CFlow 不通过 add、commit、stash、reset、clean 或 amend 修复失败 Gate
  - Gate 失败时不启动 Verification、Review 或 Merge，并保存 Status/Diff/Fingerprint 证据
  - DIRTY_TASK_WORKTREE 在预算内复用同一 Branch/Worktree，但创建新的编号 Attempt 和独立 Repair Session
  - Repair 启动前 HEAD、Status 或 Dirty Fingerprint 漂移时返回 DIRTY_WORKTREE_DRIFTED 并阻塞
  - Task 已有合法 Commit 时，Repair 仅清理由 Agent 自己产生的未提交残留不要求空 Commit
  - Dirty Repair 再次失败会消耗同一 Retry Budget；预算耗尽后 Worktree 原样保留且 Workflow Blocked
  - Task 允许多个 Commit，Repair 通过追加 Fix/Revert Commit 修复已提交内容
  - 每个 Attempt End HEAD 都由唯一且不可覆盖的 refs/cflow/.../attempts/... 审计 Ref 固定
  - 新 HEAD 不包含上一 Attempt End HEAD 时返回不可自动重试的 TASK_HISTORY_REWRITTEN 并阻塞
  - 清理未提交残留时 HEAD 可等于上一 Attempt End HEAD，但 Task HEAD 仍不得等于 task_base_commit
  - 完整 Commit Range 越出 write_scope 时失败，不能只检查最后一个 Commit
  - Verification 后 HEAD 变化或 Git-visible Dirty 时失败
  - Merge 前 HEAD、已验收 Commit 与 Git-clean 状态必须再次一致

provider_default_permissions_test.go
  - Adapter 启动参数不包含由 CFlow 添加的 Danger/Bypass/Skip-Permissions 开关
  - Session 记录 Provider、CLI Version、脱敏 argv、cwd 和默认权限风险标记
  - 非编码 Session 修改受管 Snapshot 时返回 UNEXPECTED_AGENT_MUTATION
  - Dry Run 和 Final Report 不输出 sandboxed 或 read_only_enforced 等未经证明的结论

provider_protocol_registry_test.go
  - MISSING、SUPPORTED、UNKNOWN_VERSION 和 INCOMPATIBLE_PROTOCOL 确定性映射到 Registry Revision/Hash
  - 当前 Route/Fallback 未全部 Supported 时不展示可批准 Execution Gate，也不创建 Provider Process/Node Attempt
  - 未被 Workflow 引用的未知 Provider 不阻塞其他受支持 Route
  - Approval 后 Executable Path/Binary Hash/Version/Dialect/Registry 漂移时 PROVIDER_PROTOCOL_BINDING_CHANGED 且 Dispatch 关闭
  - 未知 Event、冲突 Session ID、缺失 session_started 或非法 Completion 产生 Protocol Finding，退出码 0 也不得成功
  - Protocol Failure 不扣 Retry，受影响 Attempt 不可成功，兄弟 Attempt 按 Quiescing 规则收敛
  - Authentication Unknown 与 Protocol Unsupported 分开；确认缺少凭据时 PROVIDER_AUTHENTICATION_REQUIRED
  - 不兼容时只读诊断和已有 Run 的 pause/cancel 可用，但 create/resume/retry/approve/apply 不启动 Provider
  - 不存在 ignore-version/force/best-effort 配置或 CLI 开关

data_protection_test.go
  - 新建 CFLOW_HOME/Project/Workflow/Run/Session/Log/Evidence 目录为 0700，SQLite/WAL/SHM/Artifact/Log/Export 为 0600
  - 既有路径 Owner/Mode 不安全、Symlink Escape 或权限语义不可证明时 INSECURE_CFLOW_HOME_PERMISSIONS，且不自动 chmod
  - Provider Frame、Prompt、Tool IO、argv/env、Error、Git/Verification Output 在任一持久化 Sink 前经过同一 Redactor Revision
  - 已知 Secret 值和 Private Key/Token/Credential 模式替换为类别占位符，数据库和全部 Artifact 中不存在原值、原值 Hash 或长度
  - 无法解析、二进制/超限或 Redactor 失败时不落原内容，只写 SENSITIVE_DATA_REDACTION_FAILED 元数据并停止受影响进程
  - status/inspect/logs/Context Bundle/Resume/Final Report/Export 只使用已脱敏 Artifact，且不存在 show-secrets/raw-debug 旁路
  - 完整已脱敏 Transcript 保持 Frame 顺序、Rule Revision 和 Hash，可恢复但不声称为 Raw Byte Stream
  - 敏感文件以同目录 0600 临时文件原子替换；Crash 残留临时文件不提升为有效 Artifact
  - Worktree 根目录为 0700，但受版本控制文件模式不被批量改写

state_machine_test.go
  - 所有合法状态转换
  - 非法转换被拒绝
  - Completed 不得恢复
  - Retry 耗尽且无其他 In-flight Node 时 Node/Run/Workflow 直接 Blocked
  - Retry 耗尽且存在 In-flight Node 时 Run 先 Quiescing，全部收敛后 Run/Workflow Blocked
  - Workflow Failed 仅接受 Runtime 不可恢复故障且不得恢复为 Running
  - 任意非终态只有在 Cancel Intent、安全停机和事实协调完成后才能转为 CANCELLED
  - Cancel Intent 因孤儿进程阻塞后，Recovery 只能完成取消而不能恢复执行
  - 从 Blocked 创建 Repair 时生成新 Spec/Dynamic Workflow Revision 和新 Node，不改写旧图或旧 Attempt

approval_gate_test.go
  - Checker Pass 只进入 CHECKED/PAUSED，不自动生成 Specs
  - Plan Approval 只接受当前 Plan Revision 和 Hash，并转为 APPROVED
  - Execution Approval 原子固定 Plan/Spec/Verification Catalog/Workflow、Routing 和 Budget Hash，并记录用户已看到 Provider 默认权限风险说明
  - Git Commit Preflight 未成功时不能展示可批准的 Execution Gate
  - Execution Approval 同时固定 Git Commit Preflight Revision/Hash/Fingerprint
  - 任一引用变化时返回 APPROVAL_INPUT_CHANGED 且不得开始执行
  - 预算内 Attempt 和已批准 Fallback Provider 不重复请求批准
  - 新 Repair Spec、扩大 Scope 或未批准 Provider 必须重新经过 Execution Approval
  - 漂移窗口 Replacement Execution Approval 同时固定 Quarantine、被替代 Approval 和当前 Preflight，只写一条 EXECUTION Row
  - Replacement Execution Approval 的 decision_context 标记 absorbs_commit_policy_confirmation，且同一 Fingerprint 不再展示 COMMIT_POLICY Gate
  - Replacement Artifact/Base/Quarantine Set/Preflight/Fingerprint 任一变化时 APPROVAL_INPUT_CHANGED，Reconciliation Manifest 变化时 REPLACEMENT_RECONCILIATION_CHANGED，不能只批准 Policy 部分
  - 普通运行期漂移仍写独立 COMMIT_POLICY；Apply 漂移仍绑定新 Apply Attempt、Target/Integration HEAD，不被 Replacement 合并规则越过
  - Apply 身份漂移后的新 Catalog 只能通过 append-only APPLY_CATALOG 决策固定到精确 Apply Attempt 与 Target/Integration HEAD
  - append-only COMMIT_POLICY 决策只批准精确新 Preflight，不替换原 Execution Approval，也不构成常规第三主门
  - Ctrl+C 后 Resume 返回原批准门

controlled_stop_test.go
  - 第一次 Ctrl+C 原子记录 Stop Intent、停止调度，并并发调用所有活动 Adapter Cancel
  - Grace Period 内持续排空完整事件；截断 JSONL 尾部不进入权威事件
  - 10 秒后升级 Process Group Terminate，再过 2 秒升级 Force Kill
  - 第二次 Ctrl+C 跳过 Grace Period，且不会创建重复 Stop 流程
  - Attempt/Run 进入 INTERRUPTED、Node 回到 READY；普通 Workflow PAUSED，已有 Quiescing Blocker 时 Workflow BLOCKED，Retry Budget 不扣减
  - 有 Session ID 时 Resume 优先恢复原 Session；恢复失败再按 LOST/Successor 规则处理
  - Coding Worktree 的未提交修改原样保留；HEAD/Status/Fingerprint 漂移时 INTERRUPTED_WORKTREE_DRIFTED
  - Verification 中断后从头执行 command_id，部分输出不能作为通过证据
  - 所有子进程退出和 Checkpoint 落盘前不得释放 Workflow Owner/Project Writer
  - 强制终止后仍有匹配进程时 Project Mutation Quarantined，且不得启动其他 Workflow
  - COMMIT_POLICY_DRIFT 复用同一两阶段有限停止；无窗口 Commit/其他 Finding 时进入 Policy PAUSED，否则 BLOCKED

cancel_workflow_test.go
  - Cancel 默认否定并展示 Workflow、活动执行、Dirty Worktree、Branch、Commit 和保留路径摘要
  - 用户确认后先写 WORKFLOW_CANCEL_REQUESTED，再停止调度并复用两阶段进程停止
  - 安全停止后活动 Session/Attempt/Node、Run 和 Workflow 原子进入 CANCELLED，已成功 Node 不回退
  - Cancel 不删除或改写任何 Artifact、DB Row、Event、Log、Session、Evidence、Worktree、Branch、Commit 或 Audit Ref
  - Dirty Worktree 和未提交内容在 Cancel 前后 Fingerprint 完全一致
  - 受管进程残留时保存 Cancel Intent、Workflow Blocked、Project Quarantined，不抢先写 CANCELLED
  - Recovery 对未完成 Cancel Intent 只完成取消，不重启 Scheduler、Provider 或 Retry
  - Cancelled Workflow 允许 Status/Inspect/Logs/Export，拒绝 Resume/Retry/Apply
  - 重复 Cancel 幂等；Completed/Failed Workflow 不得被改写为 Cancelled
  - 创建新 Workflow 不复用或覆盖旧 Workflow 的 Branch、Artifact 和审批历史

cleanup_test.go
  - 默认命令只生成不可变 Cleanup Plan/Dry Run，不执行删除
  - Execute 必须绑定精确 cleanup-plan-id 和 Hash，并在删除前再次确认目标清单
  - 非终态 Workflow、未完成 Cancel Intent、活动 Apply/Process 或 Project Quarantine 均拒绝 Cleanup
  - Staged、Unstaged、普通 Untracked、Ignored 文件或进行中 Git 操作任一存在时 CLEANUP_TARGET_DIRTY
  - Canonical Path、SQLite、Git Worktree Registry、Branch、HEAD 或 Fingerprint 不一致时 CLEANUP_FACT_MISMATCH
  - 只以无 --force 的 git worktree remove 删除精确路径，且不调用 git worktree prune
  - Scratch 删除拒绝 Root、CFLOW_HOME/Repository/Workspace Root、未解析变量与 Symlink Escape
  - 中途失败保留已完成 Result，停止后续删除；Recovery 只协调已 Request 项目
  - Cleanup 后所有 Branch、Audit Ref、Commit、SQLite、Event、Approval、Artifact、Log、Session 和 Evidence 保持不变
  - Status/Inspect 将 Worktree 已清理显示为预期生命周期结果，不误报证据丢失

lease_test.go
  - 同一 Project 同时只允许一个 Mutating Runtime
  - Busy 时只读 Status/Inspect 不被阻塞
  - 不同 Project Writer 可以并行
  - Workflow Paused/Blocked/终态时释放 Owner 与 Project Writer
  - OS Lock 仍被持有时不得因 Heartbeat 超时而 Steal
  - OS Lock 已释放且 PID/Start Token 失效时协调 Stale Lease
  - PID 重用不会被误判为旧 Owner 仍存活
  - 协调进程死亡但受管子进程仍存活时 Project Mutation Quarantined、Workflow Blocked，且不自动 Kill 或运行其他 Workflow
  - 锁获取顺序逆序时测试失败

plan_validator_test.go
  - 缺少范围失败
  - 缺少验收失败
  - Blocking TODO 失败

dag_test.go
  - 正常拓扑排序
  - 环检测
  - 依赖缺失检测

scheduler_quiesce_test.go
  - 可自动重试且预算充足的 Failure 不触发 Quiescing，兄弟 Node 正常执行
  - 不可重试或预算耗尽失败原子固定 Blocking Finding、Quiesce Snapshot 并关闭 Dispatch Gate
  - Snapshot 只包含已经持久化 RUNNING Attempt，不包含内存排队 Node
  - Quiescing 后 Ready/Pending/Retry/Repair/Verify/Review/Merge 均不得新启动
  - Snapshot 中的当前 Attempt 可以按原 Timeout 和门禁成功或失败，结果不可变留证
  - Coding Attempt 成功后不自动启动 Verify；已运行 Verify/Merge 可以完成并记录实际结果
  - 兄弟 Attempt 的可重试失败在本 Run 不启动 Retry，Node 保持 Ready 等待用户处理阻塞
  - 所有 Snapshot Attempt 收敛后 Run/Workflow Blocked，并保存完成/待验收/失败/未启动清单
  - Ctrl+C during Quiescing 不清除 Finding，停止后进入 Blocked 而非普通 Paused
  - Crash Recovery 不重开 Dispatch Gate；协调 Snapshot 后只完成 WORKFLOW_QUIESCED

scope_conflict_test.go
  - 文件范围重叠
  - Resource Lock 冲突
  - 无冲突并行

workflow_compiler_test.go
  - Spec 覆盖
  - Verify 覆盖
  - Unsupported Node
  - Budget 超限
  - Route/Fallback 缺少 Purpose 所需的结构化事件、Session、Schema 或预算协议能力时失败

retry_test.go
  - Attempt 上限
  - Repair Session 独立
  - Merge Resolution 最多一次 Attempt

branch_quarantine_test.go
  - Quarantine Record、Branch/HEAD/Worktree、Evidence Manifest 与唯一 Audit Ref 原子关联且 append-only
  - 被隔离 Branch 永不成为 Verify、Review、Merge、Final Verify 或 Apply 输入
  - Replacement Task 使用新 Spec/Task/Node/Attempt ID 和新 Branch，replaces_task_id 指向旧 Task，Retry 不从旧 Task 隐式继承
  - Replacement Integration Branch 从停止前固定的最后已验收 Integration HEAD 创建，旧 Integration Branch 不移动
  - 新 Repair/Workflow Revision 未经 Execution Approval 时替代路径不可调度
  - 未污染兄弟 Task 的 Branch/Worktree 保留，旧 Attempt 为 INTERRUPTED 且 Retry 不扣减，Approval 后在同一 Task/Node 创建后继 Attempt
  - 只有 Node ID、Definition Hash、依赖边、Spec/Scope/Acceptance/Route/Budget 均不变时才复用旧 Node
  - 已成功 Node 只有证据完整且属于可信 Integration Base 时保持成功；其他节点显式进入 rerun_verification 或替代路径
  - 旧合法 Commit 保留旧 Preflight Evidence，新 Commit 使用 Replacement Preflight，完整 Range 混合 Evidence 仍可验证
  - Recovery 对未完成 Quarantine Intent 幂等协调，事实冲突时 Blocked 而不猜测可信 HEAD

session_recovery_test.go
  - Resume 不可恢复时原 Session 标记为 LOST 且证据仍可读取
  - 继任 Session 通过 supersedes_session_id 关联原 Session
  - Context Bundle 不可变、版本化且 Hash 可校验
  - 默认只注入摘要与必要证据摘录，不盲目回灌完整对话
  - 跨 Provider 前重新检查 Purpose 所需协议能力，并展示目标 Provider 默认权限信任边界
  - 自动节点的继任 Session 创建新 Attempt 并消耗预算

merge_recovery_test.go
  - 文本冲突修复成功后生成 Merge Commit
  - --no-ff Merge 保留 Task 的 append-only Commit 序列和独立 Merge Commit
  - Integration Merge Commit 在创建后通过对应 Preflight Identity/Signing 校验；失败时恢复 Pre-Merge HEAD
  - 越界修改被 Scope Check 拒绝
  - 修复失败后 Integration 恢复到 Pre-Merge HEAD
  - 无文本冲突但验证失败时创建 Repair Spec

recovery_test.go
  - 进程中断
  - Intent 无 Result
  - Worktree 丢失
  - Git HEAD 不一致
  - Dirty Attempt 的 HEAD/Status/Fingerprint 一致时可创建继任 Attempt，不一致时 DIRTY_WORKTREE_DRIFTED 并阻塞
  - 审计 Ref 缺失但 Commit 存在时可重建；Ref 值冲突时 AUDIT_REF_MISMATCH 且不覆盖
  - Attempt Commit Object 缺失时 ATTEMPT_COMMIT_EVIDENCE_MISSING
  - 当前 Task HEAD 不包含最后记录 Attempt HEAD 时 TASK_HISTORY_REWRITTEN
  - Commit Evidence 缺失但 Commit/Preflight 完整时只读重验并补记；Preflight Artifact 缺失或 Hash 冲突时阻塞
  - 已建立替代路径时核对 replaces/supersedes、基线和 Approval，不恢复旧 Branch 或重复创建替代路径
  - Reconciliation Manifest 完全匹配时只恢复未满足节点；任一 Branch/HEAD/Definition/Dependency/Evidence 漂移时关闭 Dispatch 并返回统一 Gate
```

### 集成测试要求

使用 Fake Agent 输出固定响应：

```text
fakeAgent.when("REQUIREMENT_DISCUSSION").returns(planDraft);
fakeAgent.when("PLAN_CHECK").returns({ decision: "pass" });
fakeAgent.when("SPEC_GENERATION").returns(specFixture);
fakeAgent.when("WORKFLOW_OPTIMIZATION").returns(workflowPatchFixture);
fakeAgent.whenTask("S01").commits(changeFixture);
fakeAgent.when("TASK_REVIEW").returns({ decision: "pass" });
```

测试必须能在不安装任何真实 Agent 的 CI 环境中完整执行。

真实 Provider E2E 通过环境变量开启：

```bash
CFLOW_E2E_CODEX=1 go test ./tests/e2e -run TestCodex
CFLOW_E2E_CLAUDE=1 go test ./tests/e2e -run TestClaude
CFLOW_E2E_REAL=1 go test ./tests/e2e -run TestCrossProviderWorkflow
```

真实 Provider Fixture E2E 在发布 Demo 候选版本前至少成功一次，并保存 Provider CLI 版本、模型、Session ID、输入 Artifact Hash、Commit 和验证证据；它不进入默认 CI，也不得使用无限重试掩盖模型波动。

### 第二层：CFlow 自举 Dogfood

CFlow 明确允许在自身源码仓库运行自己，但必须遵守与普通目标仓库完全相同的隔离和验收规则，并增加以下自举门禁：

- 运行中的候选二进制必须先复制到仓库外的不可变位置，例如 `~/.cflow/bin/<build-id>/cflow`；记录二进制 SHA-256、Go Build Info 和源码 Commit。Agent 不得覆盖或替换正在运行的二进制。
- Dogfood Target 使用具有有效 HEAD 的 CFlow 仓库，Workflow Base Commit 固定；候选二进制的源码 Commit 与 Dogfood Base Commit 必须一致。用户当前工作区即使有未提交修改，也只能按既定 Dirty Workspace 隔离策略处理，不能进入 Task 或 Integration Branch。
- Dogfood Requirement 在运行前保存为版本化输入，必须是一项真实但有边界的改动，至少能拆成两个可并行 Coding Task，并同时涉及代码、确定性测试和用户文档；不得使用“改进 CFlow”一类无界需求。
- Requirement Discussion、Plan Check、Implementation 和 Final Review 必须实际跨 Codex/Claude 路由，且 Planner、Implementer、Reviewer Session 相互独立。
- 自动阶段不得修改用户 Target Branch。只有 Workflow 完成后，用户显式执行 `cflow apply` 且全部 Apply Staging 门禁通过，才能更新目标分支。
- Dogfood 必须生成完整 Final Report，至少包含运行二进制 Hash、Base/Integration/Apply Commit、Plan/Spec/Verification Catalog/Workflow Revision、Provider Session、Provider CLI Executable/Version/Dialect/Registry Binding、默认权限信任边界、本地权限/Redactor Revision、测试、Review、Retry/Repair、Branch Quarantine/Replacement 和恢复证据。

Dogfood 不是演示录像或人工口头确认。如果它没有安全完成完整 Plan-to-Done 链路，Demo 的价值验收即未完成；可以保留失败证据并修复 CFlow 后重新创建新的 Dogfood Workflow，但不得改写失败历史。

### 可观测性

每个日志事件使用结构化格式：

```json
{
  "timestamp": "2026-08-02T12:00:00+09:00",
  "level": "info",
  "event": "task.verification.failed",
  "projectId": "prj-6f9e13a8c2",
  "workflowId": "wf-20260802-001",
  "runId": "run-001",
  "taskId": "S05",
  "attempt": 1,
  "commandId": "coupon-domain-test",
  "exitCode": 1,
  "durationMs": 18342
}
```

最终报告示例：

```markdown
# CFlow Execution Report

Workflow: wf-20260802-001
Plan Revision: 1
Result: PASSED
Target Branch: main
Integration Branch: cflow/wf-20260802-001/integration

## Summary

Tasks: 12
Completed: 12
Retries: 3
Agent Sessions: 19
Duration: 1h 42m

## Commits

| Task | Implementation Commits | Merge |
| S01 | abc1234, bcd2345 | def4567 |

## Git Commit Policy

Preflight Revision: 2
Identity: Yuan Cheng <redacted@example.com>
Signing: enabled / ssh / SHA256:ab12...
Policy Confirmation: execution-approval / approval-id
Verified Commits: 3
Policy Mismatches: 0
Quarantined Branches: 0

## Verification

| Command ID / Check | Result |
| coupon-domain-test | Passed |
| coupon-project-verify | Passed |
| Scope Validation | Passed |
| Final Semantic Review | Passed |

## Local Data Protection

At-rest Encryption: none
Directory/File Modes: 0700 / 0600
Redactor Revision: r4 / sha256:...
Persisted Raw Provider Frames: no
Retention: indefinite; user-controlled account/disk/backup protection

## State Compatibility

Database Schema: 4 / supported
Migration Registry: r4 / checksums verified
Latest Migration Backup: verified / ~/.cflow/backups/db/...
Referenced Artifact Schemas: supported

## Agent Runtime Evidence

| Purpose | Provider / CLI / Dialect / Registry | cwd | Permission Boundary | Result Gate |
| TASK_IMPLEMENTATION | codex / 0.141.0 / codex-jsonl-v1 / r3 | .../S01 | Provider defaults; not sandboxed by CFlow | Commit + clean + scope passed |
| TASK_REVIEW | claude / 2.1.185 / claude-stream-json-v1 / r3 | .../S01 | Provider defaults; independent session | Snapshot unchanged |

## Remaining Risks

- 性能压测未在生产规模数据执行。
```

### Demo Definition of Done

Demo 只有同时满足以下条件才算完成：

```text
一条真实的小型需求可以从讨论执行到 Integration Branch
中途 Ctrl+C 采用两阶段有限停止：不再调度新工作，活动 Process Group 有界终止，Attempt/Run 可审计为 Interrupted，失败 Retry 不扣减，Checkpoint 后能够显式恢复
用户 Cancel 后 Workflow 终态可审计，所有 Worktree、Branch、Audit Ref、Dirty 内容和证据完整保留；未完成 Cancel Intent 在 Recovery 中只继续取消，不恢复执行
显式 Cleanup 只能删除终态 Workflow 中事实完全匹配且无 Tracked/Untracked/Ignored 内容的受管 Worktree 和 Scratch Directory；无 Force、无批量、无自动 GC，Branch、Ref、数据库与全部证据永久保留
Plan Checker 与 Planner 使用不同 Session
Task Reviewer 与 Coding Session 不同
至少两个 Task 可以并行执行
每个 Coding Task 在独立 Worktree 中完成
Task 必须存在新 Commit，且 Commit 后 Worktree Git-clean，才能进入验收
缺少 Commit 或存在 Staged、Unstaged、非 Ignored Untracked 文件时不得启动 Verification、Review 或 Merge
Dirty Worktree 在预算内由同一 Branch/Worktree 上的新 Repair Attempt 和独立 Session 原地处理；失败历史不可变，外部漂移或预算耗尽时阻塞
Task Branch 允许多个 Commit 但历史 append-only；已记录 Commit 被 amend、rebase、reset 或替换时阻塞
任何 Commit-capable Agent/Merge 前 Git Identity/Signing Preflight 成功；实际 Task/Integration/Apply Commit 均与对应 Preflight 一致
执行期间 Commit Policy 合法漂移且没有窗口 Commit 时，只暂停确认精确新 Preflight；不重做 Execution Approval、不消耗 Retry，确认或恢复前不启动 Commit-capable 动作
Commit Policy 漂移检测后立即停止全部活动 Attempt，而非等待其收敛；Interrupted 不扣 Retry，漂移窗口 Commit 留证并 Blocked，后续确认不得追溯授权
包含漂移窗口 Commit 的 Branch 永久退出可信执行链；Task/Integration 必须从最后已验收 Integration HEAD 经新 Repair/Workflow Revision 与 Execution Approval 建立替代 Branch，Apply 必须显式新 Attempt，旧 Branch、Commit、Audit Ref 和证据永久保留
Replacement Execution Approval 在同一页面固定 Quarantine、Replacement Artifact/Base、Budget 和当前 Preflight，并吸收该 Fingerprint 的 Policy 确认；不得重复展示 COMMIT_POLICY Gate，普通漂移和 Apply 仍使用独立 Policy 确认
未污染兄弟 Task 在 Reconciliation Manifest 证明 Branch/HEAD/Definition/Dependency/Evidence 未变后复用原 Branch/Worktree，并以新后继 Attempt 恢复且不扣 Retry；已成功可信 Node 不重做，只有污染或语义变化路径被替换
越界文件修改会阻断任务
测试失败能够触发 Repair Attempt
超过 Retry Budget 后 Node Failed、Workflow Blocked，不会无限循环或复活旧 Attempt
并行 Node 发生不可重试失败或 Retry 耗尽后停止一切新调度，只让已运行 Attempt 有界收敛；不启动后继 Verify/Merge/Retry，收敛后稳定 Blocked
Dynamic Workflow 经过 Schema 和 DAG 校验
独立 Plan Check 后必须由用户批准精确 Plan Revision/Hash，不能自动越过
执行前必须由用户一次批准精确 Spec/Verification Catalog/Workflow Revision、Routing/Fallback 和 Budget，并确认 Provider 默认权限信任边界
Workflow 创建时只建立固定 Base Commit 的 Planning Snapshot；非编码 Session 修改该 Snapshot 时输出无效，Integration Ref/Worktree 只有在 Execution Approval 后、执行开始时才能创建
Spec/Workflow 只引用已批准且 Purpose 匹配的 command_id，不包含自由 argv
Verification Command 的来源、可执行文件身份、cwd、环境和写入副作用均经过校验并留证
Agent 使用 Provider CLI 与用户现有配置的默认权限；CFlow 不添加 Danger/Bypass 参数，也不声称提供统一 OS Sandbox、禁网或 Worktree 外访问防护
当前 Route/Fallback 只有在 Executable/Version/Registry/Dialect 被识别为 Supported 并由 Execution Approval 固定后才能运行；未知协议 Fail-closed、无 Best-effort/Force 开关，纯只读诊断与 stop/cancel 仍可用
CFLOW_HOME 自建目录和敏感文件满足 0700/0600；完整 Session/事件只保存统一脱敏版本，不存在 Raw/Show-secret 旁路，权限不安全或脱敏失败时 Provider 驱动流程 Fail-closed 且不扣 Retry
非编码 Session 修改受管 Snapshot 时输出无效；编码结果必须通过 Commit/Clean Worktree/Commit Range 门禁
预算内 Repair Attempt 可自动执行；新 Repair Spec 或扩大 Scope 必须重新批准
同一 Project 只有一个 Mutating Runtime，只读命令始终可用，不同 Project 可并行
崩溃后依据 OS Lock、PID/Start Token、SQLite Lease 和外部事实恢复，不因 Heartbeat 超时强占
已有本地状态升级前生成可验证一致性备份，并在排他 Schema Lock 下执行 Forward-only 事务迁移；崩溃后可确定重试或识别完成，数据库过新、迁移链/Checksum 异常或 Artifact Schema 不支持时 Fail-closed，历史 Artifact 不被原地改写
最终不自动修改用户 Target Branch
用户可显式执行 cflow apply，安全门禁失败时 Target Branch 保持不变
Apply Verification 只运行 apply_verify Command；命令身份漂移时 Target Branch 保持不变并要求显式新 Apply Attempt
所有核心流程有 Fake Agent 集成测试
真实 Codex/Claude Cross-Provider Fixture E2E 已保存一次完整成功证据
CFlow 候选二进制已在自身仓库完成一次有边界的真实 Dogfood Workflow
Dogfood 使用仓库外不可变二进制，记录 Build Commit 与 Binary Hash，且安全 Apply 后才修改 Target Branch
```

### PRD 实现就绪 Gate Review

> Review 日期：2026-08-02
>
> Review 结论：通过；核心用户画像和整份 PRD 已获用户最终批准

| 进入实现前门槛 | Review 结果 | PRD 证据 |
|---|---|---|
| 产品目标明确 | 通过 | 核心假设、Plan-to-Done 差异化与成功标准已定义 |
| Demo 范围明确 | 通过 | 完整 Codex/Claude 多 Agent 闭环为 P0；三层 Gate 不缩减范围 |
| 非目标明确 | 通过 | 无 Web/Cloud/跨仓库/自动 Push/PR/任意脚本/无限自主循环 |
| CLI 主流程明确 | 通过 | 根命令为 `cflow`；创建、选择、暂停、恢复、重试、取消、清理、检查与 Apply 均有语义 |
| 状态机明确 | 通过 | Workflow Stage/Runtime、Plan Revision、Node、Session/Run 分责；Task 仅为派生投影 |
| Plan/Spec/Workflow Schema 明确 | 通过 | 三类 Artifact 边界、版本/Hash、Catalog Ref、Compiler 验证与 Approval 绑定已定义 |
| Agent Adapter 边界明确 | 通过 | Start/Resume/Cancel/Inspect、Session ID、Context Bundle、Protocol Binding 与按操作 Capability 已定义 |
| 持久化事实来源明确 | 通过 | SQLite、不可变 Artifact、Git/Worktree、OS Lock 和 Evidence 按事实类型分治，Events Export 非第二事实源 |
| Worktree 与 Merge 策略明确 | 通过 | Integration HEAD 基线、串行 Merge、Commit/Clean/Scope Gate、Quarantine/Replacement 与受保护 Apply 已定义 |
| Retry 和 Recovery 策略明确 | 通过 | 不可变 Attempt、有界预算、Quiescing、Ctrl+C、Cancel、Intent/Result 与多事实 Reconcile 已定义 |
| 技术栈已确认 | 通过 | Go 1.26.x、无 CGO SQLite、单二进制和 argv 子进程模型已确认 |
| 端到端验收场景明确 | 通过 | Calculator Fixture、故障矩阵、真实 Cross-Provider E2E 和 CFlow Self-Dogfood 均有证据要求 |
| 不存在阻塞实现的产品/架构未决问题 | 通过 | 核心用户画像与 PRD 已确认；下一门禁是实现设计文档审阅，而不是补充产品范围 |

Review 同时确认以下边界：

- 本 PRD 中的 TypeScript 风格接口、SQL 和伪代码用于固定行为与不变量，不是可以直接复制的实现。下一份 `docs/cflow-demo-design.md` 必须将其收敛为 Go 模块、接口、迁移顺序和错误契约。
- Provider CLI 能力是版本化外部依赖。当前环境与官方资料能证明所讨论能力存在，但实现只能信任 Protocol Registry 与运行时 `--help`/Fixture 检测，不能把 PRD 示例命令当成永久兼容保证。
- 文档保留了研究导出产生的内部 Citation Marker；这些引用不参与 Runtime 判定。正式发布文档前应整理成可访问的 Source Appendix，但这不是实现技术阻塞项。
- 当前仓库尚无代码或 Commit；本 Review 不授权创建实现文件。PRD 已获批准，下一步只能先编写并审阅 `docs/cflow-demo-design.md`，再编写实现 Plan。

### 可直接交给 Codex 的开工约束

```text
实现一个名为 CFlow 的 Go CLI Demo，并构建为无需额外语言 Runtime 的单二进制。

技术栈：
- Go 1.26.x
- Cobra
- bufio + golang.org/x/term
- os/exec
- encoding/json + go.yaml.in/yaml/v3
- database/sql + 无 CGO SQLite Driver
- embed
- testing
- log/slog

核心约束：
1. 所有运行数据存储在 ~/.cflow。
2. 从当前目录查找 Git Root；只有具有有效 HEAD 且 HEAD 附着于本地 Target Branch 的 Git 仓库可以创建 Workflow。非 Git 或无有效 HEAD 仅允许 doctor/help；Detached HEAD 禁止新建但可按已保存 Target/Base 查看或恢复既有 Workflow，Apply 必须等用户回到记录的 Target Branch。禁止自动 git init、创建初始 Commit、猜测或 Checkout Branch。
3. Project Key 使用可读路径 Slug + canonical path SHA-256 前十位。
4. Workflow 使用 stage + runtimeStatus 双层状态。
5. 所有状态转换必须通过 WorkflowStateMachine。
6. 每次状态转换必须在同一 SQLite 事务中更新状态并追加权威 Events 记录；events.jsonl 仅作为可重建审计导出。
7. Plan 为不可变、版本化的 Markdown + YAML front matter Artifact；生命周期状态只保存在 SQLite。
8. Plan 必须由独立 Agent Session 检查；Check 结果保存为独立不可变 Artifact，随后由 Runtime 将 SQLite `plan_status` 转为 `CHECKED` 并暂停；只有用户批准精确 Revision/Hash 后才能转为 `APPROVED` 并生成 Specs。
9. Spec 使用结构化 YAML，必须包含 dependsOn、writeScope、acceptance；确定性命令只能用 `command_id` 引用，禁止自由 argv。
10. Dynamic Workflow 是受限 YAML DAG，由 Compiler 根据 Specs 确定性生成安全骨架，再选择性应用独立 Agent 的受限调度补丁；不允许 Agent 生成完整 DAG、自由 argv 或任意可执行代码。
11. Workflow Compiler 必须检查 Schema、环、Spec 覆盖、Verify 覆盖、Merge 覆盖、依赖完整性、补丁权限、预算上限，以及 Verification Catalog 引用、Purpose 和 Approval Hash 一致性。
12. Coding Task 使用独立 Git Worktree。
13. Task 最终必须从不可变 `task_base_commit` 产生至少一个实现 Commit，且进入验收前 Task Worktree 必须 Git-clean：无 Staged、Unstaged 或非 Ignored Untracked 文件。CFlow 记录并检查从 `task_base_commit` 到最终 HEAD 的完整 Commit Range；缺少 Commit 或 Worktree Dirty 时不得进入 Verification、Review 或 Merge，且不得由 Runtime 自动 add、commit、stash、reset、clean 或 amend 来越过门禁。
14. Task Reviewer 必须是未参与实现的新 Session。
15. Diff 超出 writeScope 时任务失败并进入 BLOCKED。
16. 通过验收的 Task 合并到 CFlow Integration Branch。
17. 不自动合并到用户 Target Branch；Demo 必须提供显式 `cflow apply`，并将其作为 Workflow 完成后的独立受保护交付操作。
18. Agent、命令和测试失败采用有限 Retry；失败 Attempt 不可变，预算内重试创建新 Attempt，预算耗尽时 Node Failed 且 Workflow Blocked，不得无限循环或直接复活旧 Node。
19. 先实现 FakeAgentAdapter 并完成端到端测试，再实现 CodexAdapter。
20. 外部副作用使用 REQUESTED/COMPLETED/FAILED 事件，以支持崩溃恢复。
21. 允许用户当前工作区存在未提交修改，但必须记录 Dirty Fingerprint；Workflow 只读取固定 Base Commit 的 CFlow Worktree，禁止自动 Stash 或 WIP Commit。
22. `cflow apply` 前必须验证目标工作区干净且仍位于预期 Target Branch，否则进入 BLOCKED。
23. Target Branch 已前进时，`cflow apply` 必须在独立 Apply Branch/Worktree 合并并重新执行全量验证；最终仅在 Target HEAD 未再次变化时执行 fast-forward-only，禁止 Force Update。
24. Provider Session Resume 不可恢复时，原 Session 必须保留并标记为 LOST；只能通过不可变 Context Bundle 创建带 supersedes_session_id 的继任 Session，跨 Provider 前必须重查结构化事件、Session、Schema 和预算等协议能力，并展示目标 Provider 默认权限信任边界；由 Provider 故障触发的自动节点继任必须计入新 Attempt 和预算，用户主动 Ctrl+C 产生的 Interrupted Attempt 本身除外。
25. Workflow FAILED 只表示 Runtime 无法安全恢复权威事实，是不可恢复为 RUNNING 的终态；普通 Provider、测试、Review、Scope、Merge 和 Retry 耗尽失败必须通过 Node FAILED、Workflow BLOCKED 与新的 Repair/Revision 处理。
26. Demo 验收必须同时包含可重复的 Fake Provider Fixture Gate 和一次真实 Codex/Claude CFlow 自举 Dogfood；Dogfood 二进制必须位于目标仓库外且不可变，记录 Binary Hash/Build Commit，并遵守完整 Worktree、Review、Verification 和 Apply 门禁。
27. 主链路只设置 Plan Approval 与 Execution Approval 两个用户批准门；所有 Approval 必须 append-only 并绑定精确 Artifact/Policy Hash。Checker Pass 不等于用户批准，预算内 Attempt 不重复询问，新 Repair Spec、Scope/Acceptance 扩大、未批准 Provider 或预算变化必须重新批准。
28. 不同 Project 可并行，但同一 Project 同时只允许一个 Mutating Runtime，并同时持有 Project Writer 与 Workflow Owner；只读命令不得被阻塞。OS Advisory Lock 是协调进程互斥依据，SQLite 只存 Lease 元数据，Heartbeat 超时不得自动 Steal；存在受管孤儿子进程时必须 Quarantine 整个 Project 的 Mutation。
29. 所有外部 Verification Command 必须来自 Workflow-local、不可变且被 Execution Approval 固定的 Named Catalog。CFlow 执行前必须重验 Command Purpose、Catalog Hash、Wrapper/Executable Identity、受管 Worktree cwd、最小环境和文件写入副作用；身份漂移必须以 `COMMAND_IDENTITY_CHANGED` 阻塞。Apply Drift 导致 Catalog 身份变化时，只有用户显式创建并确认新的 Apply Attempt 才能使用新 Catalog，Target Branch 在此之前保持不变。
30. Agent Session 沿用 Provider CLI 和用户现有配置的默认权限。CFlow 不建立跨 Provider Permission Profile 或 OS Sandbox，不统一禁网，也不得主动添加 Danger/Bypass/Skip-Permissions 参数；首次运行、Dry Run、Execution Approval 和最终报告必须明确这一信任边界。非编码 Session 必须通过 Snapshot 未变化检查，编码 Session 必须通过 Commit、Git-clean 和完整 Commit Range `write_scope` 检查。
31. `DIRTY_TASK_WORKTREE` 在 Retry Budget 内必须保留并复用原 Task Branch/Worktree，同时创建新的不可变 Attempt 和独立 Repair Session；Repair 前当前 HEAD/Status/Dirty Fingerprint 必须与失败 Attempt 的结束证据一致，否则以 `DIRTY_WORKTREE_DRIFTED` 阻塞。Repair 不得满足下游依赖，只有 Task 级 Commit/Clean/Scope Gate 全部通过后才能进入 Verification；预算耗尽时 Worktree 原样保留、Node Failed、Workflow Blocked。
32. Task Branch 允许多个 Commit，但历史必须 append-only。每个 Attempt End HEAD 必须由唯一 `refs/cflow/<workflow-id>/tasks/<task-id>/attempts/<attempt-number>` 审计 Ref 固定；新 Attempt 的最终 HEAD 必须等于上一记录 HEAD 或是其后代，修复已提交内容只能追加 Fix/Revert Commit。历史重写以不可自动重试的 `TASK_HISTORY_REWRITTEN` 阻塞，CFlow 不自动 Reset、Merge 或 Force-update；Integration 必须使用 `--no-ff` 保留 Task Commit 序列和 Merge Commit。
33. CFlow 必须继承目标仓库当前有效的 Git Author/Committer 与 Signing 配置，但不得写 Git Config、注入 Bot Identity 或关闭签名。任何 Coding/Repair/Merge Resolution Agent、Integration Merge 或 Apply Staging Merge 启动前必须有成功且 Policy Fingerprint 未漂移的 Commit Preflight；缺少 Identity 或非交互 Signing Probe 失败时，在创建 Commit 前阻塞且不消耗 Node Retry。每个新 Commit 必须关联 Preflight 并校验实际 Identity/Signing；绕过或不匹配以不可自动重试的 `COMMIT_POLICY_MISMATCH` 阻塞，Append-only 历史不得被重写修复。
34. Execution Approval 必须确认当时 Git Commit Preflight 的精确 Revision/Hash/Fingerprint。执行期间 Policy 合法漂移且没有窗口 Commit 时，新 Preflight 成功后必须在 Commit-capable 动作前以 `COMMIT_POLICY_CONFIRMATION_REQUIRED` 暂停，并由用户 append-only 确认精确新策略；不得使 Plan/Spec/Workflow 或原 Execution Approval 失效，也不得消耗 Retry。确认前和动作前必须 CAS 重验 Fingerprint；Apply 确认还必须绑定 Apply Attempt 与 Target/Integration HEAD，且不得改变已完成 Workflow 状态。窗口 Commit 适用约束 40 和 41。
35. Ctrl+C 必须采用两阶段有限停止：第一次停止调度、记录 Stop Intent、调用 Adapter/Context Cancel 并排空事件，固定 10 秒后终止 Process Group、再过 2 秒强制终止；第二次 Ctrl+C 立即升级强制阶段。活动 Attempt/Run 必须不可变记为 `INTERRUPTED`，Node 协调后回到 `READY`，且不扣失败 Retry；普通 Workflow 进入 `PAUSED`，已有 Quiescing Blocking Finding 时进入 `BLOCKED`。Coding Worktree 原样保留并在 Resume 前核对 HEAD/Status/Dirty Fingerprint；任何受管子进程未确认退出或 Checkpoint 未持久化前不得正常释放 Owner/Writer，残留进程必须触发 Project Mutation Quarantine。
36. `cflow cancel` 必须是显式确认的逻辑终止：先 append `WORKFLOW_CANCEL_REQUESTED`，停止调度并复用两阶段进程停止，确认无活动子进程且外部事实已协调后才原子写入 `CANCELLED` 与 Result Event。Cancel 不得自动删除、移动、压缩或改写任何 Artifact、DB/Event、Session、Log、Evidence、Worktree、Branch、Commit 或 Audit Ref，Dirty 内容必须原样保留；未完成 Cancel Intent 的 Recovery 只能继续取消。资源删除只能通过独立 `cflow cleanup` Dry Run 与约束 37 的显式安全操作。
37. Demo Cleanup 仅允许用户对终态 Workflow 执行：默认生成不可变 Cleanup Plan；只有 `--execute <cleanup-plan-id>` 经再次确认并重验 Canonical Path、Git Registry、Branch、HEAD、Fingerprint 和无活动进程后，才能删除完全安全干净的 CFlow-managed Worktree 与明确 Scratch Directory。安全干净要求无 Staged、Unstaged、任何 Untracked/Ignored 内容和进行中 Git 操作；不得使用 `--force`、全局 `git worktree prune`、批量/TTL/后台 GC，也不得删除 Branch、Audit Ref、Commit、SQLite、Event、Artifact、Log、Session 或 Evidence。每项删除必须有 Intent/Result，部分失败与 Recovery 不得扩大已批准目标集。
38. Node 出现不可重试失败或 Retry Budget 耗尽且仍有并行 Attempt 运行时，Run 必须原子进入 `QUIESCING`、固定持久化 RUNNING Attempt Snapshot 并关闭 Dispatch Gate。只允许 Snapshot Attempt 在原 Timeout/预算/验收门禁内收敛；不得启动任何新 Node、Retry、Repair、Verify、Review 或 Merge。全部收敛后 Run/Workflow 才进入 `BLOCKED`；Crash Recovery 不得重开 Dispatch Gate。可自动重试且预算充足的普通失败不触发该策略。
39. Commit Policy 漂移不适用 Quiescing：检测后必须原子关闭 Dispatch Gate、记录 `COMMIT_POLICY_SAFETY_STOP_REQUESTED`，并对全部活动 Attempt 执行两阶段有限停止；Interrupted Attempt 不扣 Retry。存在 Commit-capable 子进程时必须不慢于每 1 秒重算 Fingerprint。停止前后必须扫描 HEAD 和新 Commit；`COMMIT_DURING_POLICY_DRIFT_WINDOW` 不得被后续 Policy Approval 追溯授权，必须留证并 Blocked。Recovery 不得让 Safety Stop 中的 Attempt 继续，Apply Safety Stop 不得修改 Target Branch 或 Completed Workflow 状态。
40. 包含 `COMMIT_DURING_POLICY_DRIFT_WINDOW` 的 Task、Integration 或 Apply Branch 必须以不可变 Quarantine Record 和唯一 Audit Ref 永久隔离，绝不能进入 Verify、Review、Merge、Final Verify 或 Apply。CFlow 不自动 Cherry-pick、Revert、Reset、Rebase、Merge 或复制窗口 Commit。Task/Integration 只能通过新 Repair Spec 或 Workflow Revision、全新 Branch/Worktree 和新的 Execution Approval 从最后已验收 Integration HEAD 建立替代路径；Apply 只能由用户显式创建新 Attempt 从当前 Target/Integration HEAD 重做。Recovery 必须保持旧路径不可调度并幂等协调唯一替代链。
41. 漂移窗口 Commit 导致新 Repair Spec 或 Dynamic Workflow Revision 时，Replacement Execution Approval 必须在同一 append-only `EXECUTION` Row 中固定 Quarantine Set、被替代 Approval、全部执行 Artifact/Policy Hash 和当前 Preflight Revision/Hash/Fingerprint，并以 Context 标记吸收本次 Policy 确认；不得再写或展示重复 `COMMIT_POLICY` Gate。普通运行期漂移和 Completed Workflow 的 Apply 不适用该合并。Approval 前任一输入变化必须返回 `APPROVAL_INPUT_CHANGED`；Recovery 只能恢复统一 Gate，动作启动前仍需 Fingerprint CAS。
42. Replacement Revision 必须通过不可变 Reconciliation Manifest 把 Node 分类为 `reuse_succeeded`、`resume_interrupted`、`replace_contaminated` 或 `rerun_verification`。只有未被 Quarantine、祖先链无窗口 Commit、Checkpoint HEAD/Status/Dirty Fingerprint、Node Definition Hash/依赖、Spec/Scope/Acceptance/Route/Budget 和 Commit Evidence 全部一致的 Task 才能复用原 Branch/Worktree；旧 Attempt 保持 `INTERRUPTED` 且不扣 Retry，新执行创建后继 Attempt。任一事实漂移必须以 `REPLACEMENT_RECONCILIATION_CHANGED` 返回统一 Execution Gate，Runtime 不得相信 Agent 的“未受影响”声明或重做已成功可信 Node。
43. Provider 托管执行必须 Fail-closed：内建 Protocol Registry 以 Revision/Hash 固定 Provider Executable Identity、Version Range、Dialect、事件 Schema 和 Purpose Capability；当前 Route/Fallback 不是 `SUPPORTED` 或 Approval 后 Binding 漂移时，不得启动/恢复 Provider、创建 Attempt 或消耗 Retry。未知 Event、冲突/缺失 Session ID 和非法 Completion 必须产生不可自动重试的 Protocol Finding，退出码或自然语言不能覆盖。不得提供 Ignore-version/Force/Best-effort 开关；只读诊断及不启动 Provider 的 pause/cancel/终态 cleanup 保持可用，Authentication Unknown 必须与 Protocol Unsupported 分开。
44. Demo 不实现应用层加密；CFlow 自建敏感目录/文件必须从创建时使用当前 Owner 的 `0700/0600`，既有权限、Owner、Symlink 或文件系统语义不安全时以 `INSECURE_CFLOW_HOME_PERMISSIONS` 阻塞且不自动 chmod。所有 Provider/User/Tool/Command 内容必须在任何终端展示、持久化或导出前经过统一版本化 Redactor；只保存完整已脱敏 Transcript/Event，不保存 Raw Byte、Secret 原值/Hash/长度，也不提供 Show-secret/Raw Debug。无法安全解析或脱敏时不得落原内容，以 `SENSITIVE_DATA_REDACTION_FAILED` 两阶段停止并 Blocked、不扣 Retry。敏感 Artifact 必须以 `0600` 临时文件原子写入；长期保留、未加密 at rest 和 OS Backup 风险必须明确展示。
45. SQLite Schema 只允许使用内嵌、连续、校验值不可变的 Forward-only Migration 升级。普通数据库使用者全程持共享 DB Schema Lock，迁移持排他 Lock；升级前必须创建并验证 0600 一致性备份与 Manifest，全部 DDL/DML 和 schema_migrations Row 在单一事务提交。崩溃后只能依据数据库、Version、Checksum 和 Manifest 幂等重试或识别完成，不自动覆盖恢复或 Down Migrate。版本过新、Migration 缺链/Checksum 不符或 Artifact Compatibility Registry 不支持时 Fail-closed 且不扣 Retry；历史 Artifact 永不原地改写，Migration/兼容检查完成前不得进入 Workflow Recovery 或启动 Provider。
46. 完整 Demo 采用三个内部交付 Gate：Gate 1 用 Fake Provider 验证 Deterministic Core，Gate 2 验证真实 Codex/Claude、多 Agent 并行与 Recovery，Gate 3 完成受保护 Apply、全量故障 Fixture、真实 Cross-Provider E2E 和 CFlow Self-Dogfood。每层都必须产出可运行候选、固定测试输入、测试结果和 Binary Hash；Gate 1/2 不得被称为 Demo，只有 Gate 3 满足全部 Definition of Done 并通过发布验收后才是 Demo Complete Candidate。安全门禁必须随其保护的能力在前置 Gate 同步实现，不能推迟到 Gate 3 才补。

Gate 1（Deterministic Core）需要实现：
- cflow
- cflow status
- cflow resume
- cflow inspect
- cflow dry-run
- cflow doctor
- Project Discovery
- SQLite Migration
- Project/Workflow Lease Manager
- Workflow State Machine
- FakeAgentAdapter
- Plan Generation
- Plan Check
- Spec Generation
- Workflow Compiler
- Git/Worktree Core
- Deterministic Verification
- Fake Provider Integration-to-Integration Fixture

Gate 2（Real Multi-Agent Runtime）实现：
- CodexAdapter
- ClaudeAdapter
- Provider Protocol Registry
- WorktreeManager
- Scheduler
- Task Verification
- Retry
- Integration Merge
- Session Resume/Fallback
- Ctrl+C/Cancel/Recovery
- Final Report

Gate 3（Release Acceptance）实现并完成发布证据：
- Protected cflow apply
- Safe Cleanup
- 全部安全、迁移、协议、漂移与崩溃 Fixture
- Real Cross-Provider E2E
- CFlow Self-Dogfood
- Cross-platform Build Check

Gate 1 和 Gate 2 只是内部候选，不得称为 CFlow Demo 完成；只有 Gate 3 满足全部 Definition of Done 并经过发布验收，才是 Demo Complete Candidate。

不要实现 Web UI、云端服务、自动 Push、自动 PR、跨仓库 Workflow 和任意 Shell Workflow。
所有命令执行必须使用 argv 数组，禁止 shell: true。
每个模块必须提供单元测试。
先输出目录结构和实施 Plan，确认内部依赖后再开始编码。
```
