# Phase 1 Agent Loop 实现计划

> **Goal:** 手写事件驱动循环（`Run → <-chan AgentEvent`）+ 累积器 + TUI 重建接线，替换 eino `TurnLoop`，实现单轮流式问答。
>
> **Architecture:** `agent.Agent.Run` 调 `model.Stream`，把增量累积成完整消息并吐 `AgentEvent`；TUI 后台 goroutine 消费事件经 `program.Send` 桥接进 BubbleTea 主循环。
>
> **Tech Stack:** Go + 自建 `internal/agent`/`internal/tui` + P0 的 `internal/model`/`internal/message` + BubbleTea v2 + glamour/lipgloss。
>
> **Spec / 设计:** [../specs/phase-1-agent-loop.md](../specs/phase-1-agent-loop.md)（§2-§6 含完整代码，本计划引用之）。

## Global Constraints

- 构建/运行统一 `env -u GOROOT go ...`。
- 只有 `internal/model` 可 import eino；`internal/agent`/`internal/tui` **不 import eino**。
- TUI 用阶段 4.5/5 的**上下文最新内容**重建（git 只有旧版），不含 eino。
- 每任务末尾 `go build ./...` + `go vet ./...` 通过（commit 可选）。

---

## 文件结构

| 文件 | 职责 |
|---|---|
| `internal/agent/event.go` | `AgentEvent` 事件联合 + `MessageUpdate`/`MessageEnd`（§2） |
| `internal/agent/agent.go` | `Agent` 结构 + `New`（§3） |
| `internal/agent/state.go` | `streamAccumulator` 累积器（§4.1） |
| `internal/agent/loop.go` | `Run` + `consumeStream` + `eventStream` 接口（§4.2/4.3） |
| `internal/agent/state_test.go` | 累积器单测 |
| `internal/agent/loop_test.go` | `consumeStream` 事件序列单测 |
| `internal/tui/markdown.go` | 重建（阶段 4.5 完整内容，package 改 tui） |
| `internal/tui/tui.go` | 重建 + 改造（消费 AgentEvent） |
| `cmd/agent/main.go` | 双协程接线（§5.3） |

---

## Task 1: 事件类型 + Agent 定义（`event.go` + `agent.go`）

**Files:** Create `internal/agent/event.go`, `internal/agent/agent.go`

- [ ] **Step 1:** 写 `event.go`（照抄 §2 完整代码：`EventType` 常量 + `AgentEvent` + `MessageUpdate` + `MessageEnd`）。

- [ ] **Step 2:** 写 `agent.go`（照抄 §3：`Agent` 结构 + `New`；`Run` 方法签名先留空，Task 3 实现）。

- [ ] **Step 3:** 构建

```bash
env -u GOROOT go build ./...
```

---

## Task 2: `streamAccumulator` 累积器（`state.go`，TDD）

**Files:** Create `internal/agent/state.go`, `internal/agent/state_test.go`

- [ ] **Step 1: 写失败测试** `state_test.go`

```go
package agent

import (
	"testing"

	"einoclaw-build/internal/message"
	"einoclaw-build/internal/model"
)

func TestAccumulatorMergesTextThinkingToolCalls(t *testing.T) {
	acc := newStreamAccumulator()
	acc.add(model.ModelEvent{Thinking: "think1"})
	acc.add(model.ModelEvent{Thinking: "think2"})
	acc.add(model.ModelEvent{Text: "hello "})
	acc.add(model.ModelEvent{Text: "world"})
	acc.add(model.ModelEvent{ToolCalls: []model.ToolCallDelta{{CallID: "c1", Name: "read", Args: `{"file_`}}})
	acc.add(model.ModelEvent{ToolCalls: []model.ToolCallDelta{{CallID: "c1", Args: `path":"a.go"}`}}})

	got := acc.message()
	if got.Role != message.RoleAssistant {
		t.Fatalf("role = %q, want assistant", got.Role)
	}
	if len(got.Blocks) != 3 {
		t.Fatalf("blocks = %d, want 3 (thinking,text,toolCall)", len(got.Blocks))
	}
	if got.Blocks[0].Kind != message.BlockThinking || got.Blocks[0].Thinking != "think1think2" {
		t.Fatalf("thinking block = %+v", got.Blocks[0])
	}
	if got.Blocks[1].Kind != message.BlockText || got.Blocks[1].Text != "hello world" {
		t.Fatalf("text block = %+v", got.Blocks[1])
	}
	tc := got.Blocks[2]
	if tc.Kind != message.BlockToolCall || tc.ToolCall == nil {
		t.Fatalf("toolCall block = %+v", tc)
	}
	if tc.ToolCall.ID != "c1" || tc.ToolCall.Name != "read" || tc.ToolCall.Args != `{"file_path":"a.go"}` {
		t.Fatalf("toolCall = %+v", tc.ToolCall)
	}
}

func TestAccumulatorEmpty(t *testing.T) {
	acc := newStreamAccumulator()
	got := acc.message()
	if got.Role != message.RoleAssistant || len(got.Blocks) != 0 {
		t.Fatalf("empty message = %+v", got)
	}
}
```

- [ ] **Step 2: 运行确认失败**

```bash
env -u GOROOT go test ./internal/agent/ -run TestAccumulator -v
# 预期：编译失败（newStreamAccumulator 未定义）
```

- [ ] **Step 3: 实现** `state.go`（照抄 §4.1 完整代码）。

- [ ] **Step 4: 运行确认通过**

```bash
env -u GOROOT go test ./internal/agent/ -run TestAccumulator -v
# 预期：PASS
```

---

## Task 3: 循环 `Run` + `consumeStream`（`loop.go`，TDD）

**Files:** Create `internal/agent/loop.go`, `internal/agent/loop_test.go`

**Consumes:** `Agent`（Task 1）、`streamAccumulator`（Task 2）
**Produces:** `Agent.Run(ctx, input []message.Message) <-chan AgentEvent`

**为可测试，把「消费流 → emit 事件」的核心抽成 `consumeStream(ctx, eventStream, emit)`**，其中 `eventStream` 是最小接口 `{ Recv() (model.ModelEvent, error) }`（`*model.Stream` 满足它）。

- [ ] **Step 1: 写失败测试** `loop_test.go`

```go
package agent

import (
	"context"
	"errors"
	"io"
	"testing"

	"einoclaw-build/internal/model"
)

type fakeStream struct {
	events []model.ModelEvent
	err    error
	i      int
}

func (f *fakeStream) Recv() (model.ModelEvent, error) {
	if f.i < len(f.events) {
		ev := f.events[f.i]
		f.i++
		return ev, nil
	}
	if f.err != nil {
		return model.ModelEvent{}, f.err
	}
	return model.ModelEvent{}, io.EOF
}

func TestConsumeStreamEventSequence(t *testing.T) {
	fs := &fakeStream{events: []model.ModelEvent{
		{Text: "Hello"},
		{Thinking: "think"},
	}}
	var got []AgentEvent
	consumeStream(context.Background(), fs, func(e AgentEvent) { got = append(got, e) })

	wantTypes := []EventType{EventMessageStart, EventMessageUpdate, EventMessageUpdate, EventMessageEnd}
	if len(got) != len(wantTypes) {
		t.Fatalf("got %d events, want %d", len(got), len(wantTypes))
	}
	for i, wt := range wantTypes {
		if got[i].Type != wt {
			t.Fatalf("event[%d].Type = %v, want %v", i, got[i].Type, wt)
		}
	}
	if got[1].Update == nil || got[1].Update.Text != "Hello" {
		t.Fatalf("event[1] update = %+v", got[1].Update)
	}
	if got[2].Update == nil || got[2].Update.Thinking != "think" {
		t.Fatalf("event[2] update = %+v", got[2].Update)
	}
	if got[3].Ended == nil || got[3].Ended.Message.Blocks[0].Text != "Hello" {
		t.Fatalf("event[3] ended = %+v", got[3].Ended)
	}
}

func TestConsumeStreamErrorEmitsError(t *testing.T) {
	fs := &fakeStream{err: errors.New("boom")}
	var got []AgentEvent
	consumeStream(context.Background(), fs, func(e AgentEvent) { got = append(got, e) })

	if len(got) != 2 { // message_start, error
		t.Fatalf("got %d events, want 2", len(got))
	}
	if got[0].Type != EventMessageStart || got[1].Type != EventError || got[1].Err == nil {
		t.Fatalf("events = %+v", got)
	}
}
```

- [ ] **Step 2: 运行确认失败**

```bash
env -u GOROOT go test ./internal/agent/ -run TestConsumeStream -v
# 预期：编译失败（consumeStream 未定义）
```

- [ ] **Step 3: 实现** `loop.go`（照抄 §4.2 的 `Run` + 下面的 `consumeStream` 抽取版）：

```go
package agent

import (
	"context"
	"errors"
	"io"

	"einoclaw-build/internal/message"
	"einoclaw-build/internal/model"
)

// eventStream 是 Run 依赖的最小流接口；*model.Stream 满足它。
// 抽成接口是为了让 consumeStream 可单测（测试注入 fakeStream）。
type eventStream interface {
	Recv() (model.ModelEvent, error)
}

func (a *Agent) Run(ctx context.Context, input []message.Message) <-chan AgentEvent {
	ch := make(chan AgentEvent, 16)
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
		} else {
			defer stream.Close()
			consumeStream(ctx, stream, emit)
		}

		emit(AgentEvent{Type: EventTurnEnd})
		emit(AgentEvent{Type: EventAgentEnd})
	}()
	return ch
}

// consumeStream 消费一个事件流，累积成完整消息并 emit 对应事件。
func consumeStream(ctx context.Context, stream eventStream, emit func(AgentEvent)) {
	emit(AgentEvent{Type: EventMessageStart})
	acc := newStreamAccumulator()
	for {
		ev, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if ctx.Err() != nil {
				break // 取消：正常收尾
			}
			emit(AgentEvent{Type: EventError, Err: err})
			break
		}
		acc.add(ev)
		emit(AgentEvent{Type: EventMessageUpdate, Update: &MessageUpdate{Text: ev.Text, Thinking: ev.Thinking}})
	}
	emit(AgentEvent{Type: EventMessageEnd, Ended: &MessageEnd{Message: acc.message()}})
}
```

- [ ] **Step 4: 运行确认通过**

```bash
env -u GOROOT go test ./internal/agent/ -v
env -u GOROOT go build ./...
# 预期：全 PASS + build 通过
```

---

## Task 4: 重建 `internal/tui/markdown.go`

**Files:** Create `internal/tui/markdown.go`

- [ ] **Step 1:** 用**上下文里的阶段 4.5 完整内容**重建（`initMarkdown`/`renderMarkdown`/`renderThinking`/`streamingMarkdown` + 安全边界函数族），只把 `package main` 改为 `package tui`。内容不含 eino。
- [ ] **Step 2:** 构建

```bash
env -u GOROOT go build ./internal/tui/ 2>&1 | head
```

---

## Task 5: 重建 + 改造 `internal/tui/tui.go`

**Files:** Create `internal/tui/tui.go`

- [ ] **Step 1:** 用阶段 5 的 `tui.go` 重建，做 §5.2 的改造：
  - `package main` → `package tui`；删 eino import（`filesystem`/`adk`）。
  - 删工具渲染函数（`renderToolCall`/`renderToolResult`/`formatToolCall`/`toolLabel`/`toolColor`）与 `defaultToolResultLines`。
  - 删旧消息类型分支（`aiTextChunkMsg`/`aiThinkingChunkMsg`/`toolCallMsg`/`toolResultMsg`），换成 `case agent.AgentEvent:` → `handleAgentEvent`。
  - `teaModel` 增 `agent *agent.Agent` 字段；`newTeaModel` 改成 `NewModel(ag *agent.Agent) teaModel`。
  - 保留 `userPrefix`/`aiPrefix`/`finalizeStreaming`/`renderAIMessage` + 流式状态字段。
  - 增 `renderError(err error) string`（简单红色前缀行）。
  - `handleKey` 的 Enter：追加用户行 → 起后台 goroutine 跑 `ag.Run` 并把事件 `program.Send`；Ctrl+C：cancel 当前 run + `tea.Quit`。

- [ ] **Step 2:** 构建

```bash
env -u GOROOT go build ./... 2>&1 | head
```

---

## Task 6: 接线 `cmd/agent/main.go` + 冒烟验收

**Files:** Modify `cmd/agent/main.go`

- [ ] **Step 1:** 把 `main.go` 从「冒烟」改成「TUI 接线」：`loadConfig` → `model.New` → `agent.New` → `tui.NewModel` → `tea.NewProgram` → 注入全局 `program`/cancel → `program.Run`。

- [ ] **Step 2:** 构建 + vet

```bash
env -u GOROOT go build ./... && env -u GOROOT go vet ./...
# 预期：零错误
```

- [ ] **Step 3:** 冒烟验收（手动跑 TUI）

```bash
env -u GOROOT go run ./cmd/agent
# 输入一句话 → 流式 Markdown 正文 + 灰色思考块；Ctrl+C 立刻停
```

---

## 自检

- **spec 覆盖**：P1 的 7 项产出 → Task 1-6 全覆盖（event.go/agent.go/state.go/loop.go/markdown.go/tui.go/main.go）。
- **类型一致性**：`streamAccumulator.message()` 返回 `message.Message`（Task 2），被 `consumeStream` 的 `MessageEnd` 使用（Task 3），签名一致；`Agent.Run` 签名（Task 1 §3）与 Task 3 实现一致。
- **无占位符**：可测部分（Task 2/3）测试代码全量内联；重建部分（Task 4/5）引用设计文档 §5 + 阶段源码。
