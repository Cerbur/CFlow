# SDD ledger — plan: docs/superpowers/plans/2026-08-12-cflow-tui-workspace-navigation-plan.md

Execution mode: subagent-driven-development
Workspace: current checkout fallback; isolated worktree creation was blocked by sandbox write restrictions on .git/refs.
Setup commit: b818723 chore: ignore local implementation worktrees
Plan commit: 05ba8a4 docs: plan hierarchical tui workspace navigation
Spec commit: 46f0629 docs: define hierarchical tui workspace navigation

Pre-flight:
- Plan/spec conflict scan: clean.
- Current checkout baseline: clean except no implementation changes.
- Worktree fallback: active because git worktree add could not create .git/refs/heads/codex/cflow-tui-workspace-navigation.
- Required follow-up: keep every implementation Task commit scoped and prove git status before each next Task.
- Baseline gate: go test ./... -p 1 -count=1 -timeout=3m failed in internal/app after 180.803s at TestNativeDiscussionReturnRejectsBindingDrift; the stack showed database/sql connectionOpener goroutines. Other packages, including internal/tui, passed in the same run.
- Task 1: complete (commit 92ad705, review clean; implementer reported go test ./... -count=1 passed in 246.335s).
- Task 2: fix round 1/5 (Important Final Report shadowing addressed; Minor ordering/fact-gating coverage addressed; commits 13367d2..09e5e6c).
- Task 2: complete (commits 13367d2..09e5e6c, review clean; focused WorkflowMenu/MenuProjection tests passed in 6.010s; full internal/app baseline remains unresolved).
- Task 3: fix round 1/5 (stack/page invariant, inert action-preview routing, and q/Ctrl+C test semantics addressed; commits cba31ec..55f6c20).
- Task 3: minor (deferred): internal/tui/pages_terminal.go still contains a stale q-to-quit hint; Task 8 confirmation/footer cleanup owns that file.
- Task 3: complete (commits cba31ec..55f6c20, scoped re-review clean; focused TUI tests and TUI package gate passed; full internal/app timeout remains unresolved).
- Task 4: fix round 1/5 (Home left/right made inert, Create Esc made stack-aware, and immediate UI-only row highlighting added; commits 8ff810b..cc71946).
- Task 4: fix round 2/5 (Create Preview Esc returns to editing and preserves the name; stale lifecycle test comment refreshed; commit c0edfb4).
- Task 4: complete (commits 8ff810b..c0edfb4; final scoped re-review approved in task-4-rereview-2.md; focused gates and diff checks passed; historical note: the legacy E2E concern about the superseded n-based Home flow was later migrated by Task 11).
- Task 5: complete (commits ed6114c..6353ae3; independent review approved with no Critical/Important/Minor findings; focused tests, go vet, and diff checks passed; legacy E2E/full-suite baseline remains unresolved and Task 6 is untouched).
- Task 6: fix round 1/5 (restored clean-target ConfirmDirty=false and missing-Discovery fail-closed test coverage; commit 8626f5f).
- Task 6: complete (commits 709fa66..8626f5f; scoped re-review approved in task-6-rereview.md; focused TUI/cross-package tests passed; repository-wide 30s baseline timed out in TestRestrictedSafetyPathStopsManagedProcesses; Task 7 untouched).
- Task 7: fix round 1/5 (removed stale/misleading Cancel/Migration y/n confirmation copy without entering Task 8 handlers, and added root overlay integration coverage; commit 6e0fefb).
- Task 7: complete (commits 0f5faec..6e0fefb; scoped re-review approved in task-7-rereview.md; focused palette/exit tests and diff checks passed; package/full suite remain timeout baselines; Task 8 untouched).
- Task 8: fix round 1/5 (completed menu route mapping, section-scoped Terminal preview invalidation, and stale focused assertions/comments; commit 1e799b6).
- Task 8: fix round 2/5 (renamed/reworded active approval and migration confirmation tests/comments; commit fdf7eb4).
- Task 8: fix round 3/5 (renamed remaining migration DefaultNo test terminology; commit c7c024d).
- Task 8: complete (commits 8194fec..c7c024d; final scoped re-review approved in task-8-rereview-3.md; focused stage gates and diff checks passed; package/full suite remains unrun/timeout baseline; Task 9 untouched).
- Task 9: fix round 1/5 (fixed readonly navigation gate assertions, classified Execution Preview as a typed action, and omitted artifact routes when references cannot resolve; commits 5479c66..d256a68).
- Task 9: complete (commits b251a1e..d256a68; scoped re-review approved in task-9-rereview.md; focused TUI/Application tests passed; full suite remains incomplete; Task 10/11 untouched).
- Task 10: complete (commit 64e0968; independent review approved with no findings in task-10-review.md; focused responsive tests and diff checks passed; package/full suite remains incomplete; Task 11/12 untouched).
- Task 11: fix round 1/5 (bound Action Preview Resume/Pause to Workspace acknowledgements, added the exact Fake Discussion return journey, strengthened typed-command/ack assertions, and refreshed stale comments; commit c18c561).
- Task 11: complete (commits 367c5f1..c18c561; scoped re-review approved in task-11-rereview.md; focused E2E/trace/ack tests, script syntax, and diff checks passed; package/full suite remains incomplete; Task 12 untouched).
- Task 12: bounded review fixes complete (Home mutation shortcuts removed; Workflow Menu non-not-found errors propagate; `/exit` command/stop gates and pause-failure recovery hardened; README/spec/ledger docs refreshed; Apply exact preview identity binding explicitly deferred because the existing typed ExecuteApplyCommand contract carries only Workflow). Focused app/TUI tests, script syntax, and diff check passed; full suite/package gate intentionally not run per owner instruction; awaiting final commit.
- Task 12: final fix round complete (Decision-bound Apply now fails closed when preflight is incomplete; Apply/Cleanup/Adoption/Discussion action-preview confirmation traces are covered; unused menu action removed; commit 1edb571).
- Task 12: complete (final independent review approved Spec/Quality/Safety with one non-blocking Minor coverage suggestion; fresh focused TUI/Application/Decision gates passed; `bash -n scripts/gate-tui.sh`, `git diff --check`, and clean Git status passed). Bounded package/full gates remain non-green because `TestTUIPlanToApplyAndCleanup`, `TestApplyDeliversWorkingTreeFiles`, and `TestReportQueryRendersCompletedWorkflow` did not finish within their timeouts; real provider E2E/self-dogfood and remote operations were not run.

需求纠偏（2026-08-12，用户确认）：
- 原实现把 Workflow Menu 当成独立全屏页面，偏离目标视觉。
- 正确模型是固定三栏 `WORKFLOWS | WORKSPACE | INSPECTOR` 工作台；Enter 后图二的 Workflow Menu 只渲染在图一中间的 WORKSPACE 区域。
- 后续 Readonly Workspace、Stage Workspace、Action Preview、Create Workspace/Preview 也只替换中间内容；左栏 Workflow 列表和右栏 Inspector 持续存在（窄屏沿用既有响应式折叠规则）。
- Application Projection、Typed Command、Preview/Confirm、Runner 停止与安全不变量保持不变；本次只纠正 TUI 外壳渲染与中间内容路由。
- 纠偏依据已写入 `docs/superpowers/specs/2026-08-12-cflow-tui-workspace-navigation-design.md` 和对应实现计划。
- 纠偏实现：根渲染器新增 persistent workbench，`PageWorkflowMenu`、Readonly、Stage、Action Preview、Create 等内部 UI 状态只决定中间 `WORKSPACE` 面板内容；`WORKFLOWS` 与 `INSPECTOR` 在宽屏持续保留，既有窄屏折叠规则继续生效。
- 新增回归覆盖：`TestWorkflowMenuRendersInsideThePersistentWorkbench`、`TestHomeEnterChangesOnlyTheCenterWorkspaceContent`、`TestNestedWorkspaceContentUsesTheResponsiveShellAtSmallSizes`。
- 验证：上述聚焦测试、Workspace/WorkflowMenu 渲染组、`git diff --check` 通过；`go vet ./internal/tui` 与完整包门禁需在最终交接前继续记录。
- 最终验证补充：`go test ./internal/tui ./internal/app ./internal/cli ./cmd/cflow -run 'TestWorkflowMenu|TestHomeEnterChangesOnlyTheCenterWorkspaceContent|TestNestedWorkspaceContentUsesTheResponsiveShellAtSmallSizes|TestRenderWorkspace|TestWorkspaceStatus|TestReadonly|TestQueryWorkflowMenu' -count=1` 通过；`go vet ./internal/tui` 通过；全仓测试编译门禁需以 `go test ./... -run '^$' -count=1` 结果为准。
- 完整 `go test ./internal/tui -count=1 -timeout=3m` 仍未全绿：已有的旧 Action Preview/Native Discussion 测试仍期待一次 Enter 直接 Execute，而当前已确认规格要求 Preview → 第二次 Enter Confirm；`TestTUIPlanToApplyAndCleanup` 另有既有长 E2E 等待超时。此次固定三栏改动未改变 Typed Command、Preview/Confirm 或 Runner 语义。
