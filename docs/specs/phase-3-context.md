# Phase 3 详细设计：Context Engineering（预算 + 增量压缩）

> 状态：**待评审** · 所属 spec：[harness-rearchitecture.md](harness-rearchitecture.md) · 前置：[phase-2-session.md](phase-2-session.md)
> 本阶段立起三条承重不变量里最「重」的一条——**上下文治理**：用 provider 的 usage 做 token 真值，超阈值自动增量压缩，让长对话不溢出。

---

## 0. 目标与边界

### 本阶段产出（P3 完成时）

1. `internal/context/tokenizer.go` —— 本地 token 估算（只用于找压缩切点，真值靠 provider usage）。
2. `internal/context/compaction.go` —— 找切点 + 摘要 + 压缩。
3. `internal/context/manager.go` —— `ContextManager`（阈值模型 + AfterTurn 触发）。
4. `internal/session` —— 新增 `EntryCompaction` + `Compact` + Replay 处理压缩。
5. `internal/agent` —— 把 usage 经 `MessageEnd` 暴露出来。
6. 接线 —— main 建 ContextManager，TUI 每轮后调 `AfterTurn`。

### 本阶段不做（defer）

- **响应式溢出恢复**（模型报「context too long」→ 压缩 → 重试）：P3 只做**主动式**（超阈值提前压缩，根本不让它溢出）。响应式 + **retry 双通道**（瞬时错误退避重试 vs 溢出压缩）留后续。
- **多级摘要方法**（remote/snapcompact/handoff/shake/soft）：P3 只做「soft」——用我们自己的模型做摘要。
- **快照/视觉压缩**（snapcompact 成像）：stretch，不进主线。

### 验收标准

- `env -u GOROOT go build ./...` + `go vet ./...` + `go test ./...` 通过。
- **长对话触发自动摘要**：把阈值调小（比如窗口设 2000 token），连问几句后，session 里出现 `compaction` 条目，模型上下文变成 `[摘要, ...最近消息]`，不再溢出。

---

## 1. 参照 oh-my-pi 的设计（4 个点）

### 1.1 provider usage 是 token 真值，本地 tokenizer 只算尾部

oh-my-pi 明确：**不要用本地 tokenizer 每轮重算整个 transcript**（O(n) 且不准）。provider 每次返回的 `usage.promptTokens` 就是「上一轮实际发出去的上下文大小」的**真值**。本地估算只用来做**压缩切点**（决定保留最近多少 token）。

P3 落地：`ContextManager` 用 `MessageEnd.Usage.PromptTokens` 做阈值判断（真值）；`tokenizer.go` 的估算只用于 `findCutPoint`。

### 1.2 预算模型：threshold = window − reserve

oh-my-pi：`effectiveReserve = max(window*15%, reserveTokens||16384)`，`threshold = window − reserve`。reserve 是你「拒绝消耗的余量」——给工具循环、思维链、新回复留头寸。

P3 落地：`threshold() = window − max(window*15%, 16384)`。

### 1.3 增量压缩 + 摘要回喂

oh-my-pi 压缩是**增量**的：只处理「上次压缩之后」的条目，且**上一份摘要会回喂进下一次摘要**（迭代式）。这样摘要永远 O(一份摘要) 而不是 O(全部历史)。

P3 落地：`compact` 对当前有效消息找切点，摘要「更早部分」；下一次压缩时，那份摘要作为一条消息自然进入「更早部分」被再次摘要——**迭代式回喂是天然发生的**（因为摘要就在消息列表里）。

### 1.4 绝不切断语义单元（toolResult 要跟着 toolCall）

oh-my-pi：切点不能落在 `toolResult` 上。P3 消息全是 user/assistant 文本（工具 P4 才有），所以本阶段切点只需落在**消息边界**（天然满足）。P4 加工具后补「切点对齐 toolCall/toolResult 成对」。

---

## 2. 用量暴露（改 `internal/agent`）

P1 的 agent 没把 usage 传出来。P3 需要它。改动：

```go
// event.go：MessageEnd 增加 Usage
type MessageEnd struct {
	Message message.Message
	Usage   model.Usage // 本次 turn 的用量（P3 上下文记账）
}

// loop.go：eventStream 接口加 Usage()，consumeStream 结束时带上
type eventStream interface {
	Recv() (model.ModelEvent, error)
	Usage() model.Usage
}

func consumeStream(ctx, stream, emit) {
	// ... 循环同 P1 ...
	emit(AgentEvent{Type: EventMessageEnd, Ended: &MessageEnd{Message: acc.message(), Usage: stream.Usage()}})
}
```

> `*model.Stream` 已有 `Usage()`，天然满足扩展后的 `eventStream`；测试里的 `fakeStream` 补一个 `Usage()` 方法（返回零值即可）。

---

## 3. Session 改动（`internal/session`）

### 3.1 新增 compaction 条目

```go
// entry.go
const EntryCompaction EntryType = "compaction"

type Entry struct {
	Type       EntryType        `json:"type"`
	Version    int              `json:"version,omitempty"`
	ID         string           `json:"id,omitempty"`
	Message    *message.Message `json:"message,omitempty"`
	Compaction *Compaction      `json:"compaction,omitempty"`
}

type Compaction struct {
	Summary string   `json:"summary"`
	Files   []string `json:"files,omitempty"` // P4 确定性追踪的文件，现在留空
}
```

### 3.2 Compact：原子写「压缩 + 重追加保留」

**关键设计：不做 `firstKept` 索引追踪，而是「写摘要 + 把保留的消息重追加一遍」。**

```go
// Compact 原子地写一条 compaction 条目，然后重追加保留的消息。
func (s *Session) Compact(summary string, kept []message.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.storage.Append(Entry{Type: EntryCompaction, Compaction: &Compaction{Summary: summary}}); err != nil {
		return err
	}
	for _, m := range kept {
		if err := s.storage.Append(Entry{Type: EntryMessage, Message: &m}); err != nil {
			return err
		}
	}
	return nil
}
```

### 3.3 Replay 处理 compaction（替换累积前缀）

```go
// replayLocked 里加一个 case：
case EntryCompaction:
	// 压缩条目：把之前累积的消息替换成 [摘要]
	msgs = []message.Message{message.NewUserMessage(e.Compaction.Summary)}
```

**为什么重追加是对的**（用例子讲）：日志 `[m0,m1,m2,m3,m4,m5]`，压缩「摘要 m0-m2、保留 m3-m5」→ 追加 `compaction(S)` + 重追加 `m3,m4,m5`。Replay 顺序走：累积 m0..m5 → 遇 compaction 替换成 `[S]` → 继续累积重追加的 m3..m5 → 结果 `[S,m3,m4,m5]`。✓ 代价是保留的消息在日志里出现两次（日志稍大，可接受；不引入索引追踪的复杂度）。

---

## 4. Token 估算（`internal/context/tokenizer.go`）

```go
// estimateTokens 粗略估算一条消息的 token 数。只用于找压缩切点，真值靠 provider usage。
// 启发式：2 个 rune ≈ 1 token（中英混合的粗略近似）+ 每消息 framing 开销。
func estimateTokens(m message.Message) int {
	n := 0
	for _, b := range m.Blocks {
		if b.Kind == message.BlockText {
			n += len([]rune(b.Text)) / 2
		}
	}
	return n + 4
}
```

---

## 5. 压缩算法（`internal/context/compaction.go`）

### 5.1 找切点（最新往前累积）

```go
// findCutPoint 从最新消息往前累积 token，直到 >= keepTokens，返回保留区起始索引。
// 返回 0 表示「无更早内容可压」。
func findCutPoint(msgs []message.Message, keepTokens int) int {
	acc := 0
	for i := len(msgs) - 1; i >= 0; i-- {
		acc += estimateTokens(msgs[i])
		if acc >= keepTokens {
			return i
		}
	}
	return 0
}
```

### 5.2 摘要（用我们自己的模型）

> 摘要用**六字段任务导向压缩**（目标/状态/决策/文件/失败/下一步），而非泛化总结——保留「未来 Agent 继续任务所需的信息」。`sixFieldInstruction` + `summarizePrompt` 定义在 `compaction.go`。

```go
// summarize 把更早的消息压缩成六字段任务导向摘要。
func (cm *ContextManager) summarize(ctx context.Context, msgs []message.Message) (string, error) {
	stream, err := cm.model.Stream(ctx, summarizePrompt(msgs), nil)
	if err != nil {
		return "", err
	}
	defer stream.Close()
	var sb strings.Builder
	for {
		ev, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", err
		}
		sb.WriteString(ev.Text)
	}
	return sb.String(), nil
}
```

### 5.3 serializeConversation

```go
// serializeConversation 把消息序列化成纯文本（role: text 每行）。
func serializeConversation(msgs []message.Message) string {
	var sb strings.Builder
	for _, m := range msgs {
		sb.WriteString(string(m.Role))
		sb.WriteString(": ")
		sb.WriteString(messageText(m)) // 拼接文本块
		sb.WriteString("\n")
	}
	return sb.String()
}
```

---

## 6. ContextManager（`internal/context/manager.go`）

```go
type ContextManager struct {
	session          *session.Session
	model            model.Model
	window           int // 上下文窗口大小
	keepRecentTokens int // 压缩时保留最近多少 token
}

func New(s *session.Session, m model.Model, window, keepRecentTokens int) *ContextManager {
	return &ContextManager{session: s, model: m, window: window, keepRecentTokens: keepRecentTokens}
}

// threshold 预算阈值：window − reserve，reserve = max(15%·window, 16384)。
func (cm *ContextManager) threshold() int {
	reserve := max(cm.window*15/100, 16384)
	if reserve >= cm.window {
		reserve = cm.window / 2
	}
	return cm.window - reserve
}

// AfterTurn 每轮结束后调用；若上下文超阈值则压缩。
func (cm *ContextManager) AfterTurn(ctx context.Context, usage model.Usage) error {
	if usage.PromptTokens <= cm.threshold() {
		return nil
	}
	return cm.compact(ctx)
}

func (cm *ContextManager) compact(ctx context.Context) error {
	msgs, err := cm.session.Replay()
	if err != nil {
		return err
	}
	cut := findCutPoint(msgs, cm.keepRecentTokens)
	if cut <= 0 {
		return nil // 无更早内容可压
	}
	summary, err := cm.summarize(ctx, msgs[:cut])
	if err != nil {
		return err
	}
	return cm.session.Compact(summary, msgs[cut:])
}
```

---

## 7. 接线

### 7.1 main：建 ContextManager

- `config.yaml` 的 `modelConfig` 增 `context_window`（默认 128000）。
- `keepRecentTokens` 用固定值（如 16384）或 `context_window` 的一个比例。

```go
window := cfg.Models[0].ContextWindow  // 默认 128000
cm := context.NewManager(s, m, window, 16384)
program := tea.NewProgram(tui.NewModel(ag, s, cm))
```

### 7.2 TUI：每轮后调 AfterTurn

`runAgent` 里 `EventMessageEnd` 分支追加 assistant 消息后：

```go
if ev.Type == agent.EventMessageEnd {
	_ = m.session.Append(ev.Ended.Message)
	_ = m.context.AfterTurn(ctx, ev.Ended.Usage) // P3：超阈值则压缩
}
```

> 摘要调用是阻塞的（几秒），发生在后台 goroutine 里，不阻塞 TUI 渲染。压缩对 TUI 透明——聊天区仍显示完整历史，模型看到的才是 `[摘要, ...最近]`。

---

## 8. 边界情况与错误处理

| 情况 | 处理 |
|---|---|
| `PromptTokens` 为 0（某些 provider 不给 usage） | `<= threshold` 恒真，跳过压缩（不误压） |
| 摘要调用失败 | `AfterTurn` 返回 err，TUI 忽略（下一轮再试），不崩 |
| `findCutPoint` 返回 0（历史很短） | 不压缩 |
| 连续多轮超阈值 | 每轮压缩一次，摘要迭代回喂，上下文稳定在 `keepRecentTokens + 摘要` 附近 |
| 压缩写盘（Compact）与并发 turn | `Session` 的 mutex 保证「写摘要 + 重追加」原子 |

---

## 9. 对外契约（后续阶段依赖）

| 类型/函数 | 包 | 后续谁依赖 |
|---|---|---|
| `ContextManager.AfterTurn(ctx, usage)` | `internal/context` | `internal/tui`（P3）、`internal/agent`（P4 工具循环内也可触发） |
| `Session.Compact(summary, kept)` | `internal/session` | `internal/context`（P3）、`internal/tui`（手动 /compact，P10） |
| `estimateTokens` / `findCutPoint` | `internal/context` | 本阶段内部 + P4 切点对齐工具调用 |
| `MessageEnd.Usage` | `internal/agent` | `internal/context`（P3）、`internal/trace`（P7 用量审计） |

---

## 10. 待评审点

1. **压缩用「写摘要 + 重追加保留」而非「firstKept 索引」**——日志里保留的消息会出现两次（日志稍大），换来实现简单、无需索引追踪。接受吗？
2. **摘要消息存成 user 角色**（oh-my-pi 约定）——接受吗？
3. **只做「主动式压缩」，响应式溢出恢复 + retry 双通道留后续**——接受吗？
4. **token 估算用 `len(runes)/2` 的粗糙启发式**（只用于找切点，真值靠 provider usage）——接受吗？
5. **`context_window` 加进 `config.yaml` 的 `modelConfig`（默认 128000）**——接受吗？
