---
project: CFlow
document_type: design-specification
status: proposed
created: 2026-08-12
updated: 2026-08-12
scope: TUI hierarchical workflow workspace navigation
supersedes:
  - docs/superpowers/specs/2026-08-11-cflow-tui-main-page-visual-design.md
---

# CFlow TUI 层级工作区与全局命令设计

## 1. 设计目标

将 CFlow 默认全屏 TUI 从“固定 Lifecycle 列表驱动的主页面”调整为“层级式 Workflow 工作区”。

主页面保留：

- 左侧 Workflow 列表；
- 中间动态工作区；
- 右侧只读 Inspector。

变化集中在交互模型：

- 左侧负责选择上下文；
- 中间根据当前层级和 Workflow 阶段渲染内容；
- Enter 进入下一层或执行当前确认页；
- Esc 返回上一层；
- 不再用左右键跨生命周期阶段；
- 不再用 q 退出；
- 用 / 打开全局 Command 浮窗，本期只支持 /exit。

本设计由用户于 2026-08-12 确认，后续实现计划和相关视觉文档必须以本设计为准。

## 2. 与既有设计的关系

2026-08-11 的主页面视觉设计只允许视觉刷新，并明确禁止修改页面层级、按键语义和导航路径。本设计是用户后续明确指令，优先级高于该视觉设计，因此将其标记为 Superseded。

以下安全与权威不变量继续保留：

- TUI 不直接写 SQLite、Artifact、Git 或最终状态；
- Runtime/Decision Kernel 是 Workflow 状态和合法动作的唯一权威；
- Agent 自然语言不能直接决定验收或完成；
- Apply、Cleanup、Approval 仍需显式进入预览和确认；
- Runner 必须受控停止并完成 join，不得遗留后台进程；
- Target Branch 只能由显式 Apply Execute 改变；
- 不自动 Push、创建 PR 或修改远程仓库。

## 3. 页面与层级模型

TUI 的 UI 层级与 Runtime 生命周期状态分离。

~~~text
Home
├─ Workflow Menu
│  ├─ Readonly Workspace
│  ├─ Stage Workspace
│  └─ Action Preview / Confirm
│
└─ New Workflow
   ├─ Create Workspace
   └─ Create Preview

Global Command Palette
└─ /exit
~~~

### 3.1 Home

Home 是默认入口。

左侧只展示 Workflow 选择：

~~~text
WORKFLOWS

▸ NEW WORKFLOW
  calculator · paused
  auth-refactor · running
~~~

Home 不再把 Lifecycle 列表作为可交互导航区域。中间展示当前选择的 Workflow Overview：

- Workflow 名称；
- Stage；
- Runtime；
- Discuss → Plan → Define → Execute → Report → Apply → Cleanup 进度；
- 当前事实摘要；
- 主要合法动作提示。

右侧 Inspector 始终展示当前 Workflow 的只读事实：

- Target Branch；
- Workspace Head；
- Plan Revision/Status/Hash；
- Runtime；
- Health；
- 其他已有权威 Projection 字段。

选择变化只刷新 Workspace Projection，不执行 Command。

Home 的键位：

- ↑↓：选择 Workflow；
- Enter：进入选中 Workflow 的 Workflow Menu；
- Esc：不执行、不退出；
- /：打开 Global Command Palette；
- Ctrl+C：保留为受控停止信号；
- q：不再退出。

### 3.2 Workflow Menu

Workflow Menu 在中间区域展示当前 Workflow 的功能入口，采用分组平铺结构：

~~~text
WORKFLOW MENU

CONTINUE
▸ Resume / Continue
  Start Native Discussion

VIEW
  Current Stage
  Plan / Evidence
  Specs / Catalog
  Workflow DAG
  Task Graph
  Event Log
  Final Report

CONTROL
  Pause Workflow
  Cancel Workflow
  Apply Workflow
  Cleanup Workflow
~~~

具体条目由 Application Projection 提供：

- VIEW 条目只有在对应权威事实或 Artifact 存在时展示；
- CONTINUE 和 CONTROL 条目只有在 Runtime Projection 明确允许时展示；
- TUI 不根据 Stage、Runtime 字符串或 Agent 文本自行推断合法动作；
- 非法动作不展示，不提供 Ignore、Force 或 Best-effort 入口。

默认高亮规则：

1. 新创建 Workflow：Start Native Discussion；
2. 有合法主要动作：第一个主要合法动作；
3. Workflow Blocked：Inspect 或对应阻塞查看入口；
4. 没有可执行动作：Current Stage；
5. 只读工作区内部：内容列表的第一项。

Workflow Menu 的键位：

- ↑↓：选择菜单项；
- Enter：进入对应工作区或预览页；
- Esc：返回 Home；
- /：打开 Global Command Palette。

### 3.3 Stage Workspace

Stage Workspace 是中间区域的动态内容，不是固定 Lifecycle 列表。

统一结构：

~~~text
WORKSPACE

workflow: <name>
stage: <stage>
runtime: <runtime>

CURRENT CONTEXT
<事实摘要>

AVAILABLE ACTIONS
▸ <primary action>
  <other action>
~~~

阶段内容：

| 阶段 | 工作区内容 |
|---|---|
| New Workflow | 名称、Provider、Target/Git 事实、创建预览 |
| Discussion | Session、Change Set、Start/Continue/Finish/Switch/Pause/Cancel |
| Plan | Draft Plan、Checker Evidence、Revision/Hash、Plan 状态 |
| Define | Specs、Command Catalog、Dynamic Workflow/DAG |
| Execution Approval | Plan、Specs、DAG、Diff、Hash、Scope、Budget 与 Commit Policy 预览 |
| Execute | Task Graph、当前 Task、Agent、日志、Decision |
| Report | Final Report 和 Evidence 摘要 |
| Apply | Target 状态、Diff、Apply Preflight、Apply Preview |
| Cleanup | Cleanup Dry Run Manifest、待清理路径和证据 |

生命周期进度可以作为非交互信息保留，但不再用 ←→ lifecycle 跨阶段跳转。

## 4. Enter/Esc 语义

### 4.1 进入与返回

所有 UI 页面遵循同一个导航栈：

~~~text
Home
  └─ Enter → Workflow Menu
       └─ Enter → Stage Workspace / Readonly Workspace / Action Preview
            └─ Esc → Workflow Menu
                 └─ Esc → Home
~~~

- Enter 只进入当前选中的下一层，或在确认页执行；
- Esc 只返回上一层；
- Home 的 Esc 不退出；
- 页面返回不会取消已提交的 Runtime Command；
- 页面进入和返回不会改变 Runtime。

### 4.2 查看类入口

查看类入口只读：

~~~text
Workflow Menu
  └─ Enter → Readonly Workspace
       ├─ ↑↓ 浏览内容
       └─ Esc 返回 Workflow Menu
~~~

查看类入口不调用状态变化 Command，不启动 Provider，不改变 Git。

### 4.3 动作类入口

动作类入口先进入 Action Workspace 或 Preview：

~~~text
Workflow Menu
  └─ Enter → Action Workspace / Preview
       ├─ Enter → 执行 Typed Application Command
       └─ Esc   → 返回 Workflow Menu
~~~

所有需要确认的动作都只使用 Enter：

- 不显示 y/n；
- 不接受 y/Y/n/N 作为确认协议；
- Enter 在预览页中确认；
- Esc 取消并返回；
- 不存在“Enter alone never confirms”的旧行为。

高风险动作至少经过一个 Preview 层：

~~~text
Approval / Cancel / Apply / Cleanup
  └─ Enter → Preview
       └─ Enter → Execute
~~~

## 5. New Workflow 流程

New Workflow 固定在左侧 Workflow 列表最上方：

~~~text
Home
  └─ select NEW WORKFLOW
       └─ Enter
           └─ Create Workspace
               └─ 输入 Workflow Name
                   └─ Enter
                       └─ Create Preview
                           ├─ Enter → 创建 Workflow
                           └─ Esc   → 返回编辑
~~~

Create Workspace 展示：

- Workflow Name 输入框；
- 默认 Provider；
- Target Branch、HEAD、Dirty 状态和 Fingerprint；
- Workspace 隔离说明；
- 当前 Target 工作区不会被创建过程修改的事实。

创建成功后：

1. 新 Workflow 成为当前选中项；
2. 回到该 Workflow 的 Workflow Menu；
3. 默认高亮 Start Native Discussion；
4. 不自动启动 Provider 或 Native Session。

创建确认只使用 Enter，不使用 y。

文本输入状态下，/ 是普通字符，不打开 Global Command Palette。

## 6. Discussion 与后续阶段流程

### 6.1 Discussion

~~~text
Workflow Menu
  └─ Start Native Discussion
       └─ Enter
           └─ Discussion Workspace
               └─ Enter
                   └─ Native Session
~~~

Native Session 返回后进入 Discussion Return Workspace：

~~~text
Discussion Return
├─ Continue Same Session
├─ Finish Discussion
├─ Switch Agent
├─ Pause Workflow
└─ Cancel Workflow
~~~

Enter 进入所选动作的下一层；涉及状态变化时进入对应 Preview 或 Native Session。

### 6.2 Plan 与 Define

~~~text
Plan Workspace
├─ Generate Plan
├─ Check Plan
├─ View Plan
└─ Approve Plan

Define Workspace
├─ Generate Specs
├─ Compile Workflow
├─ View Catalog
└─ View DAG
~~~

Approve Plan 进入 Plan Preview，第二次 Enter 执行批准。

### 6.3 Execution

~~~text
Execution Approval
├─ Review Plan
├─ Review Specs
├─ Review DAG
├─ Review Diff
└─ Approve Execution
~~~

Approve Execution 进入完整 Preview，第二次 Enter 执行批准。

~~~text
Execute Workspace
├─ Start Runner
├─ Inspect Task Graph
├─ Pause Workflow
└─ Cancel Workflow
~~~

Foreground Runner 继续由 Application/Runtime 控制。TUI 只展示已提交事件和 Runtime Projection。

### 6.4 Report、Apply、Cleanup

~~~text
Report Workspace
└─ View Final Report

Apply Workspace
└─ Apply Preview
    └─ Enter → Apply Execute

Cleanup Workspace
└─ Cleanup Dry Run
    └─ Enter → Cleanup Execute
~~~

Apply 和 Cleanup 必须保持现有安全门：

- Apply 前重新检查 Target Branch、HEAD、Index 和工作区；
- Apply Execute 前只执行用户当前预览的精确结果；
- Cleanup 先生成 Dry Run Manifest；
- Cleanup Execute 只处理用户确认的 Manifest；
- 任何事实漂移都进入 Blocked，不自动覆盖、Stash、Force 或 Ignore。

## 7. Global Command Palette

Global Command Palette 在所有非文本输入页面可用：

~~~text
/
┌────────────────────────────────────┐
│ /exit                 Exit CFlow    │
│                                    │
│ /_                                 │
│ ↑↓ Navigate   Enter Select   Esc Close │
└────────────────────────────────────┘
~~~

本期只支持：

~~~text
/exit
~~~

规则：

- / 打开浮窗；
- 输入内容用于过滤命令；
- 当前只有 /exit；
- ↑↓ 选择命令；
- Enter 执行命令；
- Esc 关闭浮窗并恢复原页面；
- 不保存命令历史；
- 不执行任意 Shell；
- 不允许把命令文本直接交给 Shell；
- 后续命令必须加入受限、显式的命令注册表。

### 7.1 /exit 的退出行为

空闲状态：

~~~text
/exit
  └─ Enter → 退出 TUI
~~~

Runner 运行中：

~~~text
/exit
  └─ Enter
      └─ Pause and Exit Preview
          └─ Enter
              ├─ 提交受控 Pause
              ├─ 等待 Runner 完整 join
              └─ 退出 TUI
~~~

Runner 运行中按 Esc 返回原页面，不退出、不遗留进程。

q 不再作为退出入口。

## 8. Application/TUI 边界

建议新增只读 WorkflowMenuQuery 和 WorkflowMenuView，避免 TUI 自行推断菜单：

~~~text
WorkflowMenuView
├─ Workflow facts
├─ Readonly entries
├─ Legal action entries
└─ Default selected entry
~~~

菜单条目应是类型化数据，而不是自由字符串命令：

~~~text
MenuEntry
├─ kind: readonly | action
├─ label
├─ route
├─ action reference
└─ availability/evidence reference
~~~

约束：

- Application 根据权威 State、Projection、Artifact 和 LegalActions 生成菜单；
- TUI 只负责选择、渲染、进入下一层和发出既有 Typed Command；
- TUI 不直接读取 SQLite、Artifact、Git、Provider；
- Runtime 仍是最终状态和合法动作的唯一权威；
- 读操作与状态变更必须通过不同的 Typed Query/Command 区分；
- /exit 是 TUI 进程控制动作，Runner 运行时必须调用现有受控停止接口。

现有 WorkspaceView 可以继续承担 Home 概览；Workflow Menu 和各阶段工作区使用专门的只读 View，避免把所有阶段数据堆进一个聚合结构。

## 9. 错误处理与恢复

| 场景 | 行为 |
|---|---|
| Home Projection 查询失败 | 停留 Home，展示稳定错误，不执行动作 |
| 当前 Workflow 消失或迁移 | 清除陈旧选择，重新加载 Home Projection |
| Workflow Menu 查询失败 | 保留 Workflow 选择，显示菜单加载错误 |
| Action Preview 查询失败 | 不执行 Command，停留当前层 |
| Command 失败 | 停留当前层，展示错误和 Runtime 合法操作 |
| Command 成功但 Projection 未刷新 | 保持输入锁定，等待匹配的权威 Projection |
| Projection 与当前 Workflow 不匹配 | 丢弃陈旧结果，不覆盖当前 UI |
| Native Provider 非零退出 | 返回 Discussion Return，保留 Session 和 Change Set 事实 |
| Apply/Cleanup 事实漂移 | 进入 Blocked，Target Branch 和未确认文件保持不变 |
| /exit 期间 Runner 停止失败 | 不直接退出，进入可诊断的受控停止错误状态 |
| Renderer 错误 | 不改变 Runtime，保留 Headless CLI 恢复入口 |

不提供 Force、Ignore、Best-effort 或隐式状态修复入口。

## 10. 测试与验收

### 10.1 TUI 单元测试

覆盖：

- Home Workflow 选择只更新 Selection；
- Home Enter 进入 Workflow Menu；
- Workflow Menu 分组和默认高亮；
- Esc 按层返回；
- Home Esc 不退出；
- q 不退出；
- / 打开 Command Palette；
- /exit 过滤、选择和 Enter 执行；
- Command Palette Esc 关闭且恢复原页面；
- 文本输入状态下 / 作为普通字符；
- Create Name Enter 进入 Create Preview；
- Create Preview Enter 创建；
- 所有 Preview 不接受 y/n；
- Apply、Cleanup、Approval 需要第二次 Enter 执行；
- Runner 运行时 /exit 等待受控停止和 join。

### 10.2 Application Projection 测试

覆盖：

- WorkflowMenuView 只返回权威合法动作；
- Readonly 条目只在对应事实存在时出现；
- Action 条目不从 Stage/Runtime 文本推断；
- Default selected entry 与新建、Paused、Blocked、Running 状态一致；
- 菜单条目类型化映射不产生自由 Shell；
- Projection 刷新后菜单与 Runtime 一致。

### 10.3 集成与 E2E

Fake Provider E2E 覆盖：

~~~text
Home
→ New Workflow
→ Create Workspace
→ Create Preview
→ Create
→ Workflow Menu
→ Start Native Discussion
→ Plan
→ Define
→ Execution Approval
→ Execute
→ Report
→ Apply Preview
→ Apply Execute
→ Cleanup Dry Run
→ Cleanup Execute
~~~

所有确认均使用 Enter；不再依赖 y/n。

真实 Codex/Claude Native E2E 和 Self-Dogfood 仍需单独明确批准，不属于普通测试门槛。

### 10.4 响应式验收

继续覆盖：

- 160×45；
- 120×30；
- 100×24；
- 80×24；
- 60×18；
- Command Palette 在宽屏和窄屏下不越界；
- 每行不超过终端宽度；
- 总行数不超过终端高度；
- 浮窗关闭后底层页面状态保持不变；
- CJK、ANSI、长 Workflow Name 和长路径稳定截断。

## 11. 后续文档与实现顺序

后续必须：

1. 将本设计标记为用户审阅通过后，更新 PRD/AGENTS 中相关当前阶段描述；
2. 更新 2026-08-11 视觉设计和实现计划，明确其视觉约束仍保留，但原有页面层级限制已被本设计取代；
3. 重新编写以 UI Navigation、WorkflowMenuView、Command Palette 和 Enter/Esc 语义为核心的 implementation plan；
4. 按项目要求为每个实现 Task 使用独立 Implementer、规格 Review 和代码质量 Review；
5. 每个 Task 运行目标测试、全量测试、Git Commit 和 Git-visible Clean 验证；
6. 不在本设计文档提交中修改实现代码。

本设计文档不改变 Runtime、Store、GitFlow、Provider 或安全策略的事实权威，只定义新的 TUI 用户交互和所需的类型化投影边界。

