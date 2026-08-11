---
project: CFlow
document_type: design-specification
status: proposed
created: 2026-08-11
scope: TUI main workspace visual refresh
---

# CFlow TUI 主页面视觉与 MVVM 设计

> **2026-08-12 已确认变更**：TUI 主入口采用 Home → Workflow Menu → 动态 Stage Workspace 层级；Enter 进入或确认，Esc 返回；q 不再退出；/ 打开 Global Command Palette，本期只支持 /exit。权威规格见 `docs/superpowers/specs/2026-08-12-cflow-tui-workspace-navigation-design.md`。2026-08-11 的视觉约束、Lip Gloss 响应式要求和安全不变量继续保留，但其“只做视觉刷新、不得改变页面层级和按键语义”的限制已 Superseded。

## 1. 目标

本设计只优化 Bubble Tea 默认入口的主 Workspace 页面，使它成为克制、专业、可扫描的 Workflow 工作台。主页面优先回答：当前选中的 Workflow 处于哪个阶段，以及 Runtime 当前允许哪些动作。

本轮同时建立轻量文件级 MVVM 边界，使 UI 样式与布局不再混入 Bubble Tea 根 Model 或业务状态映射。

## 2. 范围

本轮允许：

- 重构主页面的布局、主题、样式和纯展示组件；
- 将现有 Workspace 映射与渲染拆成清晰的 ViewModel 和 View 文件；
- 为响应式布局把终端 `width` 和 `height` 传入纯渲染函数；
- 引入并固定与现有 Bubble Tea v2 兼容的 Lip Gloss v2 版本；
- 更新主页面的映射测试、渲染测试和响应式测试。

本轮禁止：

- 修改 Application、Runtime、Decision Kernel、Store、Projection 或 Provider；
- 给 `WorkspaceView` 或 `WorkspaceModel` 增加业务或只读数据字段；
- 修改按键语义、焦点规则、页面层级、导航路径或确认流程；
- 新增自动 Resume、Dispatch、Approval、Apply、Cleanup 或其他状态变化；
- 从 Stage、Runtime 或 Agent 文本推断 Runtime 没有明确投影的合法动作；
- 顺手改造 Discussion、Approval、Execution、Blocked、Terminal 等其他页面。

## 3. MVVM 边界

### Model

`internal/tui/app.go` 继续拥有 Bubble Tea 根 Model、消息循环、当前页面、终端尺寸和现有选择状态。它只把当前 `WorkspaceViewModel` 与尺寸交给主页面 View，不包含颜色、Panel 拼接或主页面文案布局。

### ViewModel

现有 `WorkspaceModel` 语义上收敛为 `WorkspaceViewModel`。`MapWorkspace` 保持为纯函数，只把单次权威 `app.WorkspaceView` 映射为渲染所需数据：Project、Workflow 列表、当前选择、Lifecycle、Health 和 Runtime Legal Actions。

### Workspace stale-selection recovery

Workspace 的只读投影可能在两次刷新之间遇到已删除或已迁移的当前选择。此时根 Model 可以在 `PageWorkspace` 查询路径中，仅针对明确的“workflow 不存在”错误，以无选择的聚合 Workspace 查询重新获取投影；`MapWorkspace` 再把选择规范化为投影中的第一项（或空选择）。该恢复只修复 TUI 的陈旧选择，不执行 Command、不改变 Runtime/最终状态，也不扩展 `WorkspaceView` 的权威事实来源。


ViewModel 不查询 Application、不执行 Command、不读 Git/SQLite/Provider，也不从状态字符串重新推导状态机。是否重命名导出类型由实现时根据兼容成本决定，但文件和职责边界必须清晰。

### View

主页面 View 是纯函数：输入 ViewModel、终端宽度和高度，输出字符串。建议文件边界：

```text
internal/tui/app.go                  Bubble Tea 根 Model / Update / View 路由
internal/tui/workspace_viewmodel.go  Workspace ViewModel 与纯映射
internal/tui/workspace_view.go       主页面响应式布局与纯渲染
internal/tui/theme.go                Lip Gloss token 与样式
internal/tui/components.go           Panel、Badge、KeyHint、Progress 等纯组件
```

不在本轮创建 Workflow 列表、主内容区或 Inspector 的独立 Bubble Tea 子 Model，以免提前引入新的 Focus 和消息路由语义。

## 4. 视觉层级

主页面由五层组成：

1. Header：`CFLOW` 品牌、Project/Target 上下文、现有 Git/Provider Health。
2. 左栏：Workflow 列表和生命周期。选中项同时使用前缀、背景和文字强调，不依赖颜色。
3. 中栏：选中 Workflow 的名称、Stage、Runtime、生命周期进度、现有 Legal Actions 和已有摘要事实。
4. Inspector：只展示现有 ViewModel 中有权威来源的 Target、Runtime、Workspace Head、Plan Revision/Status/Hash 与 Health；没有来源的 Route、Budget 或 Evidence 数量不得伪造或硬编码。
5. Footer：唯一的状态与键位提示行；键位必须与当前既有语义一致。

视觉风格使用深色、中性、低饱和配色。蓝色表示当前选择或焦点，绿色表示已完成或健康，黄色表示暂停或需注意，红色只用于真实错误或危险状态。通过粗体、前缀、边框和标签提供非颜色冗余。

浏览器视觉稿中的阴影、渐变、多字号和像素圆角不属于 TUI 验收项。终端实现用对比色背景、粗体、大小写、Unicode 边框和固定字符间距表达同一层级。

## 5. 响应式规则

渲染同时以终端宽度和高度为约束，并按优先级降低信息密度：

- Wide：至少 `120` 列且至少 `28` 行，显示 Workflow/Lifecycle、Main、Inspector 完整三栏。
- Medium：`88–119` 列或高度不足 `28` 行，显示 Workflow/Lifecycle 与 Main 双栏，把 Inspector 压缩为 Main 内的只读摘要。
- Compact：少于 `88` 列，使用单列优先级布局，先显示当前 Workflow、Stage/Runtime、Lifecycle 和 Legal Actions，再显示可容纳的摘要。
- Minimal：尺寸不足以安全渲染 Compact 时，显示稳定的最小上下文和键位提示，不允许负宽度、半截边框、panic 或不可见 footer。

所有断点均须满足：

- 每一可见行的显示宽度不超过终端宽度；
- 完整输出高度不超过终端高度；
- Header、主要上下文和 Footer 优先保留；
- 长 Project Root、Workflow Name、Branch、Hash 和 Provider Name 使用确定性截断；
- ANSI、Unicode、宽字符和 CJK 文本的显示宽度通过 Lip Gloss 的 width-aware API 计算，禁止用 `len()` 对齐或裁剪可见文本；
- 不因尺寸变化改变选择、执行 Command 或触发 Application Query。

## 6. 测试与验收

至少覆盖 `160×45`、`120×30`、`100×24`、`80×24` 和 `60×18`。测试必须验证：

- 不 panic；
- 每行显示宽度不越界；
- 总行数不越界；
- Wide、Medium、Compact/Minimal 分支命中预期结构；
- 长名称、空 Workflow、Blocked/Paused/Running 和缺少可选字段时保持稳定；
- 选择和导航测试继续证明只更新 UI Selection，不调用 Execute；
- 主页面只显示 Runtime 投影的 Legal Actions；
- 非颜色文本仍能辨识选择、状态和健康结果；
- 原有 TUI、CLI 与全量测试通过。

目标命令：

```bash
go test ./internal/tui -run 'Workspace|Responsive|Layout|Navigation' -count=1
go test ./internal/tui ./internal/cli ./cmd/cflow -count=1
go test ./... -count=1
```

## 7. 可直接修改的实施提示词

```text
你正在修改 CFlow 仓库的 Bubble Tea TUI。请先读取并遵守：

1. AGENTS.md
2. docs/cflow-prd.md
3. docs/superpowers/specs/2026-08-07-cflow-tui-workflow-design.md，重点是 §5 和 §6.1
4. docs/superpowers/plans/2026-08-07-cflow-tui-workflow-implementation-plan.md，重点是 Task 9–10
5. docs/superpowers/specs/2026-08-11-cflow-tui-main-page-visual-design.md
6. internal/tui/app.go、workspace.go、viewmodel.go、keys.go 及相关测试

目标：只优化默认 TUI 的主 Workspace 页面，把它改造成克制、专业、可扫描的 Workflow 工作台。主页面首先回答“当前 Workflow 在哪个阶段”和“Runtime 当前允许哪些动作”。不要改造其他页面。

架构要求：采用轻量文件级 MVVM。

- Model：internal/tui/app.go 只保留 Bubble Tea 根状态、消息循环、页面路由、尺寸和已有选择状态，不放主页面样式或 Panel 拼接。
- ViewModel：把 Workspace 的类型和 MapWorkspace 纯映射整理到 workspace_viewmodel.go。只能映射现有 app.WorkspaceView；不得查询 Application、执行 Command、读 Git/SQLite/Provider，或重新推导状态机。
- View：把主页面纯渲染整理到 workspace_view.go；输入 WorkspaceViewModel、width、height，输出 string。
- Theme：在 theme.go 集中定义 Lip Gloss 颜色、边框、间距和文字层级。
- Components：在 components.go 放置无状态的 Panel、Badge、KeyHint、Progress 等小组件。不要引入独立 Bubble Tea 子 Model，不要改变 Focus 或消息路由。

视觉要求：

- Header：CFLOW、Project/Target 上下文、现有 Git/Provider Health。
- 左栏：Workflow 列表 + Discuss → Plan → Define → Execute → Report → Apply → Cleanup 生命周期。
- 中栏：选中 Workflow 名称、Stage、Runtime、生命周期进度、现有 Runtime Legal Actions 和已有摘要事实。当前 Workflow 与下一步可执行入口是视觉焦点。
- Inspector：只显示现有 ViewModel 能权威提供的事实；禁止硬编码或推断 Route、Budget、Evidence 等不存在的数据。
- Footer：全屏唯一的状态/键位提示行，内容必须与现有键位语义一致。
- 深色、中性、低饱和；蓝色表示当前选择，绿色表示完成/健康，黄色表示暂停/注意，红色只表示真实错误/危险。选择和状态不得只靠颜色表达。
- 浏览器风格中的阴影、渐变和多字号不要模拟；使用终端可实现的背景色、粗体、Unicode 边框和字符间距。

使用与现有 charm.land/bubbletea/v2 v2.0.6 兼容的 Lip Gloss v2，并固定精确依赖版本，不使用浮动 latest。用 Lip Gloss 的 ANSI/Unicode-aware width/height 能力布局、测量、截断和对齐。删除主页面手写 ANSI、stripANSI、len/rune padding 和脆弱的 joinColumns 对齐逻辑；禁止用 len() 计算可见宽度。

响应式是硬性要求：

- Wide：width >= 120 且 height >= 28，完整三栏。
- Medium：width 为 88–119，或高度不足 28，双栏；Inspector 变为 Main 内的只读摘要。
- Compact：width < 88，单列优先级布局。
- Minimal：尺寸更小时稳定降级，不得出现负宽度、半截边框、panic、内容错位或 footer 丢失。
- 所有长文本必须确定性截断；每一行显示宽度不得超过 width，完整输出不得超过 height。
- 尺寸变化只能重排视图，不能改变选择、发 Command 或新增 Query。

严格保护业务边界：

- 不修改 Application、Runtime、Decision Kernel、Store、Projection 或 Provider。
- 不给 app.WorkspaceView / Workspace ViewModel 增加字段。
- 不修改任何按键语义、页面层级、导航路径或确认流程。
- 不新增自动 Resume、Dispatch、Approval、Apply、Cleanup。
- Legal Actions 只能来自现有 Runtime Projection；不得从 Stage、Runtime 或 Agent 文本推断。
- TUI 不直接写 SQLite、Artifact、Git 或最终状态。
- 工作区已有未提交修改。先检查 git status/diff，保留所有用户改动，不覆盖、不回退、不顺手修改无关文件。

测试驱动实施：先补失败测试，再修改实现。至少覆盖 160x45、120x30、100x24、80x24、60x18，以及长 Workflow 名称、长 Project Root/CJK、空 Workflow、Paused、Blocked、Running 和可选字段缺失。逐行使用 ANSI-aware width 断言不越界，并断言总高度不越界。保留“导航只更新 Selection、从不 Execute”和“只展示权威 Legal Actions”的测试。

完成后运行：

go test ./internal/tui -run 'Workspace|Responsive|Layout|Navigation' -count=1
go test ./internal/tui ./internal/cli ./cmd/cflow -count=1
go test ./... -count=1

最后报告：修改文件、MVVM 职责边界、三个响应式布局的行为、测试证据、git diff/status，以及任何未解决限制。不要自动 push、创建 PR，且不要运行真实 Codex/Claude E2E 或 Self-Dogfood。
```

## 8. 明确限制

由于本轮不得增加 Projection 字段，视觉稿中的“下一步动作说明”、Route、Budget、Evidence 数量等内容只有在现有 ViewModel 已有权威事实时才可显示。实现不得为了贴近视觉稿而伪造文案或扩展业务查询；缺少事实时应使用更简单的现有 Stage、Runtime 和 Legal Actions 表达。
