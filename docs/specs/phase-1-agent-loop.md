# Phase 1 详细设计：Agent Loop（事件驱动循环）

> 状态：**待评审** · 所属 spec：[harness-rearchitecture.md](harness-rearchitecture.md) · 前置：[phase-0-foundation.md](phase-0-foundation.md)
> 本阶段立起三条承重不变量里最关键的一条——**事件驱动的循环**，替换掉 eino 的 `TurnLoop`。

---

## 0. 目标与边界

### 本阶段产出（P1 完成时）

1. `internal/agent/event.go` —— `AgentEvent` 事件联合（delta vs 定稿分离）。
2. `internal/agent/agent.go` —— `Agent` 定义（名字/指令/模型）。
3. `internal/agent/state.go` —— `streamAccumulator`（流式累积器，把增量合并成完整消息）。
4. `internal/agent/loop.go` —— `Run(ctx, input) → <-chan AgentEvent`（循环编排 + 中断）。
5. `internal/tui/markdown.go` —— 重建（阶段 4.5 完整内容：全量渲染 + streamingMarkdown 增量 + 安全边界算法）。
6. `internal/tui/tui.go` —— 重建 + 改造（消费 `AgentEvent`，移除 eino/工具渲染）。
7. `cmd/agent/main.go` —— 双协程接线（TUI 主循环 + agent 后台，`program.Send` 桥接）。

### 本阶段不做（defer）

- 工具执行（P4，含三档中断的完整版、工具调用展示）。
- Session 持久化 / 多轮历史（P2，本阶段 `Run` 只吃单轮 `[user]`，历史 P2 拼）。
- 上下文记账 / compaction（P3）。
- 聊天列表虚拟滚动（P8，本阶段沿用 `chatLines []string` 简单渲染）。

### 验收标准

- `env -u GOROOT go build ./...` + `go vet ./...` 通过。
- 单轮：在 TUI 输入一句话 → 流式 Markdown 渲染正文 + 灰色思考块；`message_end` 后正文落定进聊天区。
- Ctrl+C 立刻停（取消当前流 + 退出）。

---

## 1. 参照 oh-my-pi 的设计（本阶段吸收的 4 个点）

### 1.1 delta vs 定稿：`message_update` 与 `message_end` 分开

oh-my-pi 的事件把「流式增量」（`message_update`，只含 delta）和「定稿消息」（`message_end`，含完整内容）分开。P0 的 `ModelEvent` 只产增量；P1 的循环把增量**包一层**变成 `AgentEvent`，并在流结束时发一个「定稿」事件。TUI 靠这个区分「边流边画」与「画完落定」。

### 1.2 turn / step / run 三层词汇

- **run** = 一次 `Run()` 执行（一个用户输入）。
- **turn** = 一次模型回复 + 它的工具调用/结果（P1 无工具，所以一 turn = 一 step）。
- **step** = 一次 LLM 调用。

P1 只跑一层循环（无工具循环），但事件序列已经把 turn/step 的骨架立好，P4 往里塞工具循环即可。

### 1.3 累积 → 定稿消息（accumulate then settle）

流式 delta 不能直接当历史存（P2 要回放完整消息），必须**累积成一条完整的 assistant 消息**。oh-my-pi 用 `AppendOnlyLog` + 每消息摘要；P1 先用一个 `streamAccumulator` 把 delta 合并成完整 `message.Message`（text 拼接、thinking 拼接、工具调用按 CallID 合并）。

### 1.4 中断：context 取消（P1 简化版，三档留 P4）

oh-my-pi 的三档中断（硬杀/软信号/跳过）本质是**保护有副作用的工具**——P1 还没有工具，所以先用最简单的：`context.Context` 取消。`Run(ctx, ...)` 把 ctx 透传给 `Model.Stream`，取消时底层 HTTP 请求中止、`Recv` 返回错误，循环优雅退出。三档中断在 P4 有工具后再补。

---

## 2. 核心类型：`AgentEvent`（`internal/agent/event.go`）

```go
package agent

import "einoclaw-build/internal/message"

// EventType 事件类型。
type EventType int

const (
	EventAgentStart     EventType = iota // run 开始
	EventTurnStart                       // turn 开始
	EventMessageStart                    // assistant 消息开始（流即将到来）
	EventMessageUpdate                   // 流式增量（只含 delta）
	EventMessageEnd                      // 消息定稿（完整累积的消息）
	EventTurnEnd                         // turn 结束
	EventAgentEnd                        // run 结束
	EventError                           // 出错
)

// AgentEvent 是 agent 执行过程中吐出的流式事件。
type AgentEvent struct {
	Type EventType

	// EventMessageUpdate: 流式增量
	Update *MessageUpdate
	// EventMessageEnd: 定稿消息
	Ended *MessageEnd
	// EventError
	Err error
}

// MessageUpdate 是一次流式增量。P1 只含 text/thinking（TUI 渲染用）。
// 工具调用增量只进累积器，不上 TUI（P4 加工具展示时再扩展）。
type MessageUpdate struct {
	Text     string
	Thinking string
}

// MessageEnd 是定稿的完整 assistant 消息。
type MessageEnd struct {
	Message message.Message
}
```

### 设计要点

1. **事件是值类型**（struct），可直接 `program.Send(ev)` 当 `tea.Msg` 用，TUI 的 `Update` 里 `case agent.AgentEvent:` 按 `Type` 分派。
2. **`MessageUpdate` 只含 Text/Thinking**——工具调用增量由循环内部累积，不出现在 `AgentEvent` 里（P1 无工具展示）。P4 会新增 `ToolStart/ToolEnd` 事件。
3. **`MessageEnd` 携带完整 `message.Message`**——这是 P2 session 要存的历史单元。

---

## 3. Agent 定义（`internal/agent/agent.go`）

```go
package agent

import (
	"context"

	"einoclaw-build/internal/message"
	"einoclaw-build/internal/model"
)

// Agent 是一个可运行的编程智能体（P1 只有名字/指令/模型；工具 P4 加）。
type Agent struct {
	name        string
	instruction string
	model       model.Model
}

// New 创建一个 Agent。
func New(name, instruction string, m model.Model) *Agent {
	return &Agent{name: name, instruction: instruction, model: m}
}

// Run 执行一次 run：把 system 指令 + 输入消息发给模型，流式吐出 AgentEvent。
// input 是 system 之后的消息（P1 传单条 user；P2 传历史+user）。
func (a *Agent) Run(ctx context.Context, input []message.Message) <-chan AgentEvent {
	// 见 loop.go
}
```

> 状态（`state.go`）见 §4.1 的 `streamAccumulator`——它是 run 期间的瞬态状态（累积中的 text/thinking/toolCalls），对应 oh-my-pi `AgentState.streamMessage + pendingToolCalls`。

---

## 4. 循环（`internal/agent/loop.go` + `state.go`）

### 4.1 `streamAccumulator`（`state.go`，纯函数、可单测）

```go
package agent

import (
	"strings"

	"einoclaw-build/internal/message"
	"einoclaw-build/internal/model"
)

// streamAccumulator 把流式增量累积成一条完整的 assistant 消息。
// text/thinking 拼接；工具调用按 CallID 合并（同 CallID 的 Args 片段拼接）。
type streamAccumulator struct {
	text      strings.Builder
	thinking  strings.Builder
	toolCalls map[string]*message.ToolCall // key = CallID
	toolOrder []string                     // 保留工具调用到达顺序
}

func newStreamAccumulator() *streamAccumulator {
	return &streamAccumulator{toolCalls: map[string]*message.ToolCall{}}
}

func (a *streamAccumulator) add(ev model.ModelEvent) {
	if ev.Text != "" {
		a.text.WriteString(ev.Text)
	}
	if ev.Thinking != "" {
		a.thinking.WriteString(ev.Thinking)
	}
	for _, tc := range ev.ToolCalls {
		t, ok := a.toolCalls[tc.CallID]
		if !ok {
			t = &message.ToolCall{ID: tc.CallID}
			a.toolCalls[tc.CallID] = t
			a.toolOrder = append(a.toolOrder, tc.CallID)
		}
		if tc.Name != "" {
			t.Name = tc.Name // 名字通常只在首个 delta 出现
		}
		t.Args += tc.Args // 参数片段拼接
	}
}

// message 产出累积完成的 assistant 消息。
// 块顺序固定：thinking → text → toolCalls（按到达序）。
func (a *streamAccumulator) message() message.Message {
	var blocks []message.ContentBlock
	if a.thinking.Len() > 0 {
		blocks = append(blocks, message.ContentBlock{Kind: message.BlockThinking, Thinking: a.thinking.String()})
	}
	if a.text.Len() > 0 {
		blocks = append(blocks, message.ContentBlock{Kind: message.BlockText, Text: a.text.String()})
	}
	for _, id := range a.toolOrder {
		blocks = append(blocks, message.ContentBlock{Kind: message.BlockToolCall, ToolCall: a.toolCalls[id]})
	}
	return message.Message{Role: message.RoleAssistant, Blocks: blocks}
}
```

> 这是 P1 唯一值得 TDD 的纯逻辑：喂一串 `ModelEvent` 增量，断言合并出的 `Message` 正确（text 拼接、thinking 拼接、工具调用按 CallID 合并、块顺序）。

### 4.2 `Run` 流程（`loop.go`，编排）

```go
func (a *Agent) Run(ctx context.Context, input []message.Message) <-chan AgentEvent {
	ch := make(chan AgentEvent, 16) // 缓冲，避免消费者慢时阻塞生产
	go func() {
		defer close(ch)
		emit := func(e AgentEvent) {
			select {
			case ch <- e:
			case <-ctx.Done():
			}
		}

		emit(AgentEvent{Type: EventAgentStart})
		emit(AgentEvent{Type: EventTurnStart})

		msgs := append([]message.Message{message.NewSystemMessage(a.instruction)}, input...)
		stream, err := a.model.Stream(ctx, msgs, nil)
		if err != nil {
			emit(AgentEvent{Type: EventError, Err: err})
			emit(AgentEvent{Type: EventTurnEnd})
			emit(AgentEvent{Type: EventAgentEnd})
			return
		}
		defer stream.Close()

		emit(AgentEvent{Type: EventMessageStart})
		acc := newStreamAccumulator()
		for {
			ev, err := stream.Recv()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				if ctx.Err() != nil {
					break // 取消：正常收尾，不报错
				}
				emit(AgentEvent{Type: EventError, Err: err})
				break
			}
			acc.add(ev)
			emit(AgentEvent{Type: EventMessageUpdate, Update: &MessageUpdate{Text: ev.Text, Thinking: ev.Thinking}})
		}

		emit(AgentEvent{Type: EventMessageEnd, Ended: &MessageEnd{Message: acc.message()}})
		emit(AgentEvent{Type: EventTurnEnd})
		emit(AgentEvent{Type: EventAgentEnd})
	}()
	return ch
}
```

### 4.3 中断（P1 简化：ctx 取消）

- `emit` 用 `select { ch <- e / <-ctx.Done() }`：消费者不读了（或 ctx 取消）时，生产者不会被永久阻塞。
- `Recv` 出错时先判 `ctx.Err() != nil`：取消导致的错误**正常收尾**，不误报 `EventError`。
- Ctrl+C → 调用 cancel → 底层 HTTP 中止 → 循环退出 → `agent_end` 发出。

---

## 5. TUI 恢复与改造（`internal/tui`）

### 5.1 `markdown.go`：重建（纯渲染，无改动）

用阶段 4.5 的完整内容重建，只把 `package main` 改成 `package tui`。内容（`initMarkdown`/`renderMarkdown`/`renderThinking`/`streamingMarkdown` + `Render/Reset/tryAdvanceFromEmpty/renderTrailing/glueRenders` + 安全边界函数族）**原样保留**，不含任何 eino 依赖。

### 5.2 `tui.go`：重建 + 改造

从阶段 5 的 `tui.go` 改，差异如下：

| 项 | 阶段 5（旧） | P1（新） |
|---|---|---|
| package | `main` | `tui` |
| eino import | `filesystem`/`adk` | **删掉** |
| 消息类型 | `aiTextChunkMsg`/`aiThinkingChunkMsg`/`toolCallMsg`/`toolResultMsg` | `agent.AgentEvent`（按 `Type` 分派） |
| 工具渲染 | `renderToolCall`/`renderToolResult`/`formatToolCall`/`toolLabel`/`toolColor` | **删掉**（P4 加） |
| 全局 | `turnLoop`/`program` | `agent`（指针）+ 桥接 `program` |
| 流式状态 | `streamingThinking`/`streaming`/`stream`/`finalizeStreaming`/`renderAIMessage` | **保留** |

保留的核心（`teaModel` 字段 + `finalizeStreaming` + `renderAIMessage` + `renderThinking` 调用）逻辑不变，只是事件源从「旧消息类型」换成 `agent.AgentEvent`：

```go
func (m teaModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		// 同旧
	case tea.KeyPressMsg:
		return m.handleKey(msg)
	case agent.AgentEvent:
		return m.handleAgentEvent(msg)
	}
	// 其余交给 textarea
	var cmd tea.Cmd
	m.inputArea, cmd = m.inputArea.Update(msg)
	return m, cmd
}

func (m teaModel) handleAgentEvent(ev agent.AgentEvent) (teaModel, tea.Cmd) {
	switch ev.Type {
	case agent.EventMessageUpdate:
		// 思考收尾 → 累积正文（逻辑同旧的 aiThinkingChunkMsg/aiTextChunkMsg）
		if ev.Update.Thinking != "" {
			m.streamingThinking += ev.Update.Thinking
		}
		if ev.Update.Text != "" {
			if m.streamingThinking != "" {
				m.chatLines = append(m.chatLines, renderThinking(m.streamingThinking, m.width)...)
				m.streamingThinking = ""
			}
			if m.stream == nil {
				m.stream = &streamingMarkdown{}
			}
			m.streaming += ev.Update.Text
		}
	case agent.EventMessageEnd:
		m = m.finalizeStreaming() // 正文落定进 chatLines
	case agent.EventError:
		m.chatLines = append(m.chatLines, renderError(ev.Err))
	}
	return m, nil
}
```

> 注意：`MessageUpdate` 一次事件可能同时带 Text 和 Thinking，处理顺序和旧代码一致（先 thinking 后 text）。

### 5.3 双协程桥接（`cmd/agent/main.go`）

沿用旧「主协程跑 TUI、后台跑 agent、`program.Send` 桥接」的模式：

```
主 goroutine:  program.Run()  → TUI Update/View 循环
后台 goroutine: agent.Run(ctx, input) → AgentEvent 通道 → program.Send(ev)
```

```go
func main() {
	cfg := loadConfig()
	m := model.New(...)                    // P0 的模型客户端
	ag := agent.New("codeclaw", instruction, m)

	tm := tui.NewModel(ag)                // teaModel 持有 agent（指针）
	program = tea.NewProgram(tm)          // 全局 program（桥接用，同旧模式）
	if _, err := program.Run(); err != nil { log.Fatal(err) }
}
```

- `tui.NewModel(ag *agent.Agent) tea.Model` 把 agent 指针注入 teaModel（指针在值拷贝间共享）。
- Enter → 后台 goroutine：`go func() { ctx, cancel := context.WithCancel(...); saveCancel(cancel); for ev := range ag.Run(ctx, msgs) { program.Send(ev) } }()`。
- Ctrl+C → 调 cancel 停当前流 → `tea.Quit` 退出。
- `program` 与「当前 run 的 cancel」用包级变量承载（旧代码就是全局 `turnLoop`/`program`，P1 延续这个简单模式；P10 再抽成 run-manager）。

---

## 6. 边界情况与错误处理

| 情况 | 处理 |
|---|---|
| 用户空输入按 Enter | 忽略（同旧 `text == ""` return） |
| 流中途 ctx 取消 | `Recv` 返回 `context.Canceled`，`ctx.Err() != nil` → 正常收尾发 `agent_end`，不发 `Error` |
| 流中途其它错误 | 发 `EventError` 后 `break`，仍发 `turn_end`/`agent_end` |
| 消费者（TUI）已退出 | `emit` 的 `select ctx.Done()` 让生产者不阻塞 |
| 连续两次 Enter（上一轮还在流） | 先 cancel 上一轮 ctx，再起新一轮（覆盖 cancel 引用） |

---

## 7. 对外契约（后续阶段依赖）

| 类型/函数 | 包 | 后续谁依赖 |
|---|---|---|
| `agent.AgentEvent`（事件联合） | `internal/agent` | `internal/tui`（P1）、`internal/trace`（P7） |
| `agent.Agent.New(name, instr, model)` | `internal/agent` | `cmd/agent` |
| `agent.Agent.Run(ctx, input []message.Message) <-chan AgentEvent` | `internal/agent` | `internal/tui`、`internal/subagent`（P6） |
| `streamAccumulator` 产出的完整 `message.Message` | `internal/agent` | `internal/session`（P2 存历史） |

---

## 8. 待评审点

1. **`Run` 的 input 是 `[]message.Message`（system 之后的整段）**，agent 自己 prepend system 指令——接受吗？（P2 多轮历史就是往这个切片里塞历史。）
2. **`MessageUpdate` 只含 Text/Thinking，工具调用增量不上 TUI**——工具展示留到 P4。接受吗？
3. **块顺序固定 thinking→text→toolCalls**（丢失 text 与 toolCall 的原始交错顺序）——对 P2 历史回放够用，接受吗？（若将来要精确交错，再升级累积器。）
4. **`markdown.go`/`tui.go` 用「上下文里的最新内容」重建，而非 `git checkout`**（git 只有阶段3/4 旧版）——你确认这个来源没问题？
