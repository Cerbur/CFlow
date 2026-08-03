# Add divide

## 背景

The calculator currently returns a zero when a division by zero is requested.

## 目标

Division by zero must produce a typed, user-visible error instead of a silent zero.

## 范围

In scope: the division operator, its error type, and the CLI error rendering.
Out of scope: floating-point precision changes and other arithmetic operators.

## 非目标

This plan does not change subtraction, multiplication, or addition, and does not
introduce a decimal arithmetic engine.

## 约束

The change must keep the existing command-line interface, must not add external
dependencies, and must keep the module layout under `internal/`.

## 当前实现分析

`internal/calc/divide.go` computes `a / b` and returns the zero value on a zero
divisor; `internal/cli` renders the result without checking the divisor.

## 推荐技术方案

Introduce a `DivisionByZeroError` type in `internal/calc`, have `Divide` return
`(int, error)`, and render the error through the existing CLI error path.

## 关键设计决策

The divisor check happens inside `Divide` so every caller receives the typed
error; the CLI only renders it.

## 涉及模块与文件边界

`internal/calc/divide.go` (signature and error type), `internal/calc/divide_test.go`
(new tests), `internal/cli/render.go` (error rendering).

## 数据与兼容性影响

No persisted data changes. The `Divide` signature change is source-compatible
with the single existing caller after a one-line update.

## 测试与验收方案

`go test ./internal/calc` covers zero and non-zero divisors; a CLI-level test
asserts the error message; acceptance is a green `go test ./...`.

## 风险与回滚

The main risk is a caller that ignores the new error value; the single caller
is updated in the same change and the change is a small revert if needed.

## 未决问题

No blocking open questions remain.
