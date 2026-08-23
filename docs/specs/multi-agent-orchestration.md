# Multi-Agent 主动委派 + 并行编排 Spec

> 状态：**待评审** · 所属 spec：[harness-rearchitecture.md](harness-rearchitecture.md) · 前置：P6（Subagent + MCP）
> 把 P6 的「单子 agent 委派」升级成「多 agent 主动委派 + 并行编排 + 可靠完成」。参照 oh-my-pi 的 eagerTasks 委派策略 + task 运行时，但**分三层、按杠杆落地**，避免一次全上。

---

## 0. 目标与背景

### 现状问题（P6 实测暴露）

1. **主 agent 不主动委派**：有 `task` 工具 + 指令，但模型仍「自己 bash/glob 探索」，不委派。根因：指令是软提示、task 描述没枚举子 agent、主 agent 有全套工具（能自己干就不委派）。
2. **完成度不可靠**：子 agent 用「最后一个 MessageEnd 文本」当结果，无结构化产出、无 schema 校验、无显式终止。
3. **无状态/无并发控制**：无状态机、无并发上限、无超时重试。

### 三层架构（按杠杆排序）

| 层 | 解决什么 | 关键机制 |
|---|---|---|
| **Layer 1 委派策略** | 主 agent「不委派」 | 配置模式 + 触发清单 + coordinator 角色 + 能力边界 + whenToUse + 反例 |
| **Layer 2 派发运行时** | 委派「不可靠」 | SubagentSpec + tasks[] 批量 + Semaphore + yield/outputSchema + 状态机 + failure control |
| **Layer 3 通信+隔离** | 委派「不可扩展」 | mailbox bus + worktree + session 持久化 + 审计 hook（**延后**） |

---

## 1. Layer 1：委派策略（最高杠杆）

### 1.1 配置模式（`config.yaml`）

```yaml
delegation_mode: always  # conservative | preferred | always
```

| 模式 | 语义 | 能力边界 |
|---|---|---|
| `always` | MUST delegate，唯一例外「约 30 行内单文件编辑、直接回答、用户明确要求自己执行」 | **orchestrator/worker 分离**（主 agent 只挂 task/remember，硬保证） |
| `preferred` | 多文件改动/调查/验证是 strong candidate，允许主 agent 判断小任务 | 主 agent 保留全套工具（软约束） |
| `conservative` | 不委派，除非用户或 AGENTS.md 明确要求 | 主 agent 保留全套工具 |

### 1.2 system prompt 三块（按模式生成）

```go
// internal/agent 或 cmd/agent 按 delegation_mode 生成三段：
func delegationInstruction(mode string) string { ... }
```

**① 角色（coordinator）**

```
你是 codeclaw 的协调者（coordinator）。你的工作是：理解任务 → 分解 → 派发子 agent → 综合结果 → 验收。
你不是执行者，不要大量调用工具自己干活。
```

**② 触发清单**

```
- 3+ 文件或跨模块改动 → 分解并委派
- 多个互相独立的调查/验证问题 → 并行派多个子 agent
- 探索未知代码库 → 派 explorer，禁止自己逐文件读
- 非平凡实现/改动后 → 派 reviewer 验收
- 长耗时验证/测试 → 派 worker
- 只有「直接回答、约 30 行内单文件编辑、用户明确要求执行命令」才留在主线程
```

**③ 反例**

```
- Don't peek：委派后不要自己又读一遍文件
- Don't race：不要 spawn 子 agent 后自己 idle 等
- Don't duplicate：不要「派了子 agent 又自己做一遍」
- Own decomposition：顶层计划必须自己拆，不能外包给 blank-context 子 agent
- Real concurrency：必须拆成真正独立的 slice，禁止假并行
- Dependencies only：只有严格依赖才串行，否则必须并行
```

### 1.3 能力边界（orchestrator/worker，`always` 模式）

```go
// cmd/agent/main.go
workerRegistry := tool.NewRegistry()           // 子 agent 的工具
for _, t := range tool.Builtins(bash) { workerRegistry.Register(t) } // read/write/glob/grep/bash
// workerRegistry 不含 task（防递归）

orchestratorRegistry := tool.NewRegistry()     // 主 agent 的工具
orchestratorRegistry.Register(subagent.NewTaskTool(mgr))
orchestratorRegistry.Register(tool.NewRememberTool(mem))

// always 模式：主 agent 用 orchestratorRegistry，子 agent 用 workerRegistry
// preferred/conservative 模式：主 agent 用 workerRegistry + task
```

### 1.4 whenToUse 字段

```go
type SubagentSpec struct {
    ...
    WhenToUse string  // 触发场景，task 描述枚举时带上
}
// 例：reviewer.WhenToUse = "非平凡实现/改动后验收、代码审查"
```

---

## 2. Layer 2：派发运行时

### 2.1 统一 SubagentSpec

```go
// internal/subagent/spec.go
type SubagentSpec struct {
    ID           string          // 唯一 id
    AgentType    string          // reviewer/explorer/planner/verifier...
    SystemPrompt string
    WhenToUse    string          // 触发场景（Layer 1）
    OutputSchema map[string]any  // 可选 JSON Schema，校验 yield 产出
    Timeout      time.Duration   // wall-clock 超时（0 = 无）
    MaxTurns     int             // 工具循环上限（默认 50）
}
```

### 2.2 tasks[] 批量派发

```go
// task 工具参数改成 tasks[]，一次发多个 = 显式并行
func (taskTool) Parameters() map[string]any {
    return map[string]any{
        "tasks": map[string]any{"type": "array", "items": map[string]any{
            "type": "object",
            "properties": map[string]any{
                "subagent": map[string]any{"type": "string"},
                "prompt":   map[string]any{"type": "string"},
            },
        }},
    }
}

// Manager.RunMany 并行派发多个子 agent
func (m *Manager) RunMany(ctx context.Context, tasks []Task) []Result
```

### 2.3 Semaphore 并发控制

```go
type Manager struct {
    ...
    sem chan struct{} // 并发上限，可动态 resize
}

func (m *Manager) acquire(ctx context.Context) error {
    select {
    case m.sem <- struct{}{}:
        return nil
    case <-ctx.Done():
        return ctx.Err()
    }
}
```

### 2.4 yield + outputSchema（完成度保证）

- 子 agent 的工具集加一个 `yield` 工具：`yield { data: {...} }`，子 agent **必须**调它结束。
- Manager 拦截 `yield` 调用 → 提取 `data` → 按 `OutputSchema` 校验。
- 校验失败 → 有限重试（≤3）→ 强制 tool choice `yield`。
- 拿不到 yield → 返回 `{status: failed, partial: 最后文本}`。

### 2.5 状态机

```go
type Status int
const (
    StatusPending Status = iota
    StatusRunning
    StatusCompleted
    StatusFailed
    StatusKilled
    // idle/parked/aborted 是 Layer 3（复用/恢复）才加
)
```

### 2.6 failure control

| 机制 | 实现 |
|---|---|
| abort | ctx 取消，传播到子 agent 的模型调用 + 工具 |
| wall-clock timeout | `SubagentSpec.Timeout`，超时 kill |
| maxTurns | 工具循环上限 |
| 有限重试 | schema 校验失败重试 ≤3 |
| partial output | kill/fail 保留 partial findings + transcript |

### 2.7 Result

```go
type Result struct {
    ID     string
    Status Status
    Data   map[string]any // yield 的结构化产出（校验过）
    Text   string         // 失败时的 partial 文本
    Err    error
}
```

---

## 3. Layer 3：通信 + 隔离（**延后**，Layer 1/2 稳定后再做）

| 机制 | 用途 | 何时需要 |
|---|---|---|
| mailbox bus（send/wait/inbox + aside/wake/revive） | 长时间后台任务的异步通信 | 子 agent 跨 turn 后台跑、主 agent 随时追问 |
| worktree/patch 隔离 | 写密集并行不冲突 | 多个子 agent 同时改文件 |
| session 持久化 | 多轮子 agent（追问/续跑） | 子 agent 需要跨调用恢复 |
| 审计 hook | 「连续 N 次文件读取」建议委派 | P7 trace 就绪后 |

---

## 4. 与现有代码的映射（去留）

| 现有 | 处置 |
|---|---|
| `subagent.Definition` | **扩展**成 `SubagentSpec`（加 WhenToUse/OutputSchema/Timeout/MaxTurns） |
| `subagent.Manager.Run` | **保留**，作为 `RunMany` 的单任务版本 |
| `task` 工具（单 subagent） | **改**成 tasks[] 批量 |
| `tool.Concurrency()` / `ExecuteAll` | **保留**，Layer 2 的并行基础 |
| `Registry.Without("task")` 防递归 | **保留**，Layer 1 能力边界的基础 |
| `approver`（P4.5 interrupt/resume） | **保留**，子 agent 仍 headless（yolo） |

---

## 5. 实现顺序（纳入 P6）

1. **P6-L1**：`delegation_mode` 配置 + SubagentSpec 加 WhenToUse + system prompt 三块 + orchestrator/worker 能力边界。
2. **P6-L2**：SubagentSpec 扩展 + tasks[] 批量派发 + Semaphore + yield/outputSchema + 状态机 + failure control。
3. **P6-L3**：mailbox / worktree / session / 审计（按需后置）。

---

## 6. 待评审点

1. **分层顺序**（L1 委派策略 → L2 运行时 → L3 通信隔离）——认可吗？
2. **`delegation_mode` 三档**（conservative/preferred/always）+ always 触发 orchestrator/worker 能力边界——认可吗？
3. **yield 机制**（子 agent 必须调 yield 结束 + outputSchema 校验 + 重试 + 强制 tool choice）——这是 Layer 2 的核心，接受这个复杂度吗？
4. **mailbox bus + worktree 延后到 Layer 3**——认可吗？
5. **tasks[] 批量派发**（一次 task 调用带多个 item，替代「多次 task 调用」）——认可吗？
