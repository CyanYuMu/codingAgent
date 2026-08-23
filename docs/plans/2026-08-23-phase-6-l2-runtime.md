# P6-L2 派发运行时 实现计划

> **Goal:** 把子 agent 委派从「单任务、无状态、无结构化产出」升级成「批量并行、yield 显式终止、状态机、失败控制」。
>
> **Spec:** [../specs/multi-agent-orchestration.md](../specs/multi-agent-orchestration.md)（§2）。

## 文件结构

| 文件 | 职责 |
|---|---|
| `internal/subagent/spec.go` | `SubagentSpec`（加 OutputSchema/Timeout/MaxTurns）+ `Result`/`Status`/`Task` |
| `internal/subagent/manager.go` | `Run` 返回 `Result`（拦截 yield）+ `RunMany`（Semaphore 批量并行） |
| `internal/subagent/yield.go` | `yield` 工具（显式终止） |
| `internal/subagent/task.go` | `task` 工具改 `tasks[]` |
| `internal/subagent/*_test.go` | 状态机/yield/批量 单测 |

---

## Task 1: SubagentSpec 扩展 + Result/Status（`spec.go`）

- [ ] `SubagentSpec` 加 `OutputSchema map[string]any`、`Timeout time.Duration`、`MaxTurns int`。
- [ ] 定义 `Status`（pending/running/completed/failed/killed）+ `Result{ID, Status, Data, Text, Err}` + `Task{Subagent, Prompt}`。

---

## Task 2: yield 工具 + Run 返回 Result（`yield.go` + `manager.go`）

- [ ] `yieldTool`：`Name()="yield"`，`Execute` 把 `data` 写进 sink（子 agent 显式结束）。
- [ ] `Manager.Run` 改成返回 `Result`：
  - 拦截 `EventToolStart` 里 `Name=="yield"` 的调用，解析 `data` 作为结构化产出。
  - 有 yield → `StatusCompleted` + `Data`；无 yield → 用最后文本作 `Text` + `StatusCompleted`。
  - `EventError` → `StatusFailed` + `Err`。
- [ ] `RunMany(ctx, tasks)`：每个 task 一个 goroutine，Semaphore 限并发，结果按序返回 `[]Result`。

---

## Task 3: task 工具改 tasks[]（`task.go`）

- [ ] `Parameters()` 改成 `{tasks: [{subagent, prompt}]}`；`Execute` 解析 tasks → `mgr.RunMany` → 拼结果写 sink。

---

## Task 4: failure control + 接线 + 测试

- [ ] `RunMany` 里对每个 task 用 `context.WithTimeout`（`Timeout`）+ `MaxTurns` 透传给子 agent。
- [ ] 测试：状态机（unknown subagent → failed）、RunMany 批量、yield 拦截。

---

## 自检

- **spec 覆盖**：P6-L2 的 6 项 → Task 1-4 全覆盖。
- **类型一致性**：`SubagentSpec`（Task 1）被 `Run/RunMany`（Task 2）消费；`Result`（Task 1）被 task 工具（Task 3）消费。
