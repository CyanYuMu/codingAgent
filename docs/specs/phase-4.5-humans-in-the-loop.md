# Phase 4.5 详细设计：权限审批 Human-in-the-loop

> 状态：**待评审** · 所属 spec：[harness-rearchitecture.md](harness-rearchitecture.md) · 前置：P4（审批纯策略）
> 本阶段把审批从「纯策略 Resolve + 拒绝降级」升级成「人机交互」：`Prompt` 时**暂停 turn → 弹窗 → 人决定 → 恢复**。这是 interrupt/resume 机制的第一次落地，后续 subagent/advisor 复用。

---

## 0. 目标与边界

### 本阶段产出（P4.5 完成时）

1. `internal/tool/executor.go` —— `Approver` 接口 + Executor 在 `Prompt` 时调 approver（而非直接拒绝）。
2. `internal/agent/agent.go` —— `New` 注入 approver。
3. `internal/tui/approval.go` —— 审批弹窗（`approvalRequestMsg` + `pendingApproval` 状态 + 渲染）。
4. `cmd/agent/main.go` —— 建 approver 传给 agent。

### 本阶段不做（defer）

- 审批「记住决定」（allowlist/denylist 持久化）——后续。
- 子 agent 的 yolo 无头审批（P6）。
- 计划/预览类审批（`xd://resolve` 那套）——P6+。

### 验收标准

- `env -u GOROOT go build ./...` + `go vet ./...` + `go test ./...` 通过。
- `approval_mode: write` 下，让 agent 跑 bash → 弹窗「允许执行这个命令？」→ 点允许继续执行、点拒绝工具被拒；agent 继续后续工具循环。

---

## 1. 参照 oh-my-pi 的设计（2 个点）

### 1.1 interrupt/resume：暂停点 = 阻塞回调

oh-my-pi 的审批本质是「在工具执行前插一个**暂停点**」：循环阻塞等决定，决定回来再继续。P4.5 用**阻塞回调**实现这个暂停点——`Approver.Approve` 阻塞 agent goroutine，TUI 弹窗后经 channel 把决定送回来，循环恢复。这就是 interrupt（暂停）/ resume（恢复）。

### 1.2 审批是「运行时询问」，不是「配置时拒绝」

P4 的 `Resolve` 只是**算出**「该不该问」。P4.5 补上「真的问」：`Prompt` 时不再降级拒绝，而是弹窗让人决定。yolo 模式永远 `Allow`，不触发弹窗；`write` 模式 exec 询问；`always-ask` 模式 write+exec 询问。

---

## 2. Approver 接口（`internal/tool/executor.go`）

```go
package tool

// Approver 是审批的中断点：blocking 等待用户决定。
// true = 允许执行，false = 拒绝。ctx 取消时返回 error（中断）。
type Approver interface {
	Approve(ctx context.Context, call message.ToolCall) (bool, error)
}
```

`Executor` 增 `approver Approver`（nil = 无 HITL，退化为 P4 的「拒绝+说明」）：

```go
type Executor struct {
	registry *Registry
	mode     permission.Mode
	approver Approver
}

func NewExecutor(r *Registry, mode permission.Mode, approver Approver) *Executor
```

`Execute` 的 Prompt 分支改为：

```go
decision := permission.Resolve(t.Tier(), e.mode)
if decision == permission.DecisionPrompt {
	if e.approver == nil {
		return "tool denied: requires approval (tier=" + string(t.Tier()) + ")"
	}
	approved, err := e.approver.Approve(ctx, call)
	if err != nil {
		return "tool approval interrupted: " + err.Error()
	}
	if !approved {
		return "tool denied by user"
	}
	// 批准：继续执行
}
```

---

## 3. Agent 注入 approver（`internal/agent/agent.go`）

```go
func New(name, instruction string, m model.Model, tools *tool.Registry, mode permission.Mode, approver tool.Approver) *Agent {
	return &Agent{
		...
		executor: tool.NewExecutor(tools, mode, approver),
		...
	}
}
```

---

## 4. TUI 审批弹窗（`internal/tui/approval.go`）

### 4.1 approver 实现（interrupt 点）

```go
// approver 实现 tool.Approver：经 program.Send 弹窗，阻塞等决定。
type approver struct{}

func (approver) Approve(ctx context.Context, call message.ToolCall) (bool, error) {
	resp := make(chan bool, 1)
	if program == nil {
		return false, nil // 无 TUI，拒绝
	}
	program.Send(approvalRequestMsg{call: call, resp: resp})
	select {
	case d := <-resp:
		return d, nil
	case <-ctx.Done():
		return false, ctx.Err() // Ctrl+C 中断审批
	}
}

func NewApprover() tool.Approver { return approver{} }
```

### 4.2 审批请求消息 + 弹窗状态

```go
type approvalRequestMsg struct {
	call message.ToolCall
	resp chan bool
}
```

`teaModel` 增 `pendingApproval *approvalRequestMsg`（nil = 无待审批）。

- `Update` 里 `case approvalRequestMsg:` → `m.pendingApproval = &msg`。
- `handleKey` 里若 `pendingApproval != nil`，**优先拦截按键**：`y`/回车 → `resp <- true`；`n`/`esc` → `resp <- false`；清空 `pendingApproval`。
- `View` 里若 `pendingApproval != nil`，在输入区位置显示审批弹窗（命令 + `[y] 允许 / [n] 拒绝`），替换正常输入框。

---

## 5. 接线（`cmd/agent/main.go`）

```go
ag := agent.New("codeclaw", agentInstruction, m, registry, mode, tui.NewApprover())
```

> approver 是 `internal/tui` 导出的构造函数，main 创建后注入 agent；agent goroutine 的 `Approve` 经 `program.Send` 把弹窗请求送进 TUI 主循环，决定经 channel 回传。

---

## 6. 边界情况与错误处理

| 情况 | 处理 |
|---|---|
| yolo 模式 | `Resolve` 恒 `Allow`，approver 不触发 |
| approver 为 nil（测试/无 TUI） | 退化为「拒绝+说明」 |
| 审批中用户按 Ctrl+C | `ctx.Done()` → 返回 false + error，循环收尾 |
| 审批中用户输入新消息 | `pendingApproval != nil` 时 handleKey 拦截，不处理新消息（弹窗是 modal） |
| 用户拒绝 | 返回 `tool denied by user`，agent 换其它方式继续 |

---

## 7. 对外契约

| 类型/函数 | 包 | 后续谁依赖 |
|---|---|---|
| `tool.Approver` | `internal/tool` | `internal/tui`（P4.5）、`internal/subagent`（P6 用 yolo 绕过） |
| `tui.NewApprover()` | `internal/tui` | `cmd/agent` |
| interrupt/resume 机制（阻塞回调 + channel） | `internal/tui` | P6 subagent / advisor 复用 |

---

## 8. 待评审点

1. **Approver 用「阻塞回调 + channel」实现 interrupt/resume**（而非 eino 的 interrupt 错误）——接受吗？
2. **审批弹窗是 modal**（待审批时拦截所有按键，只响应 y/n/esc）——接受吗？
3. **无 approver（测试/无 TUI）时退化为 P4 的「拒绝+说明」**——接受吗？
4. **不做「记住决定」持久化**（allowlist/denylist 留后续）——接受吗？
