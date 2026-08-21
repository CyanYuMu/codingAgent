# Phase 2 详细设计：Session + Event（JSONL 单一真相源）

> 状态：**待评审** · 所属 spec：[harness-rearchitecture.md](harness-rearchitecture.md) · 前置：[phase-1-agent-loop.md](phase-1-agent-loop.md)
> 本阶段用自写 JSONL 会话替换 eino 的 session，让多轮历史可恢复、可 `/clear`、可 fork。

---

## 0. 目标与边界

### 本阶段产出（P2 完成时）

1. `internal/session/entry.go` —— `Entry`（JSONL 一行）+ `EntryType` 常量。
2. `internal/session/storage.go` —— `Storage` 接口 + `FileStorage` + `MemoryStorage`。
3. `internal/session/session.go` —— `Session`（Append/Reset/Replay/Fork/Close）。
4. `internal/message/message.go` 加 JSON tag（让 JSONL 可读、可 diff）。
5. `cmd/agent/main.go` + `internal/tui/tui.go` —— 接线：固定会话文件、重启恢复历史、每轮记录 user/assistant、`/clear`。

### 本阶段不做（defer）

- **blob 外置**（`blob:sha256:<hash>`）：P2 消息全是文本，无大块内容；P4 工具结果落盘时再加。
- **共享日志 + 多 leaf**（fork 共享同一份日志、只换 leaf 指针）：P6 子 Agent 需要多分支时才值得做；P2 用「快照 fork」。
- 多会话管理 + `/resume`（P9）、compaction（P3）、slash 命令框架（P10，本阶段只硬编码一个 `/clear`）。

### 验收标准

- `env -u GOROOT go build ./...` + `go vet ./...` + `go test ./...` 通过。
- **重启恢复历史**：退出再 `go run`，之前的对话还在（replay 恢复）。
- **`/clear` 封存旧上下文**：输入 `/clear` 后，旧对话不再注入模型（写 `reset_boundary`）。

---

## 1. 参照 oh-my-pi 的设计（3 个概念，逐一落地或 defer）

### 1.1 JSONL：追加式日志，一行一个 entry

oh-my-pi 的 session 是**每行一个 JSON 对象**的追加日志。好处：追加写 O(1)、可 diff、可逐行恢复、崩溃时最多丢最后一行。P2 直接采用——`Session.Append` 就是把一个 entry 序列化成一行 JSON + `\n` 追加到文件。

### 1.2 leaf 指针：可变指针 vs 不可变日志

oh-my-pi 的树是「**追加日志（不可变）+ 单个可变 leaf 指针（当前头）**」。核心思想：历史永不修改，只有「当前到哪」这个指针可变——这让 fork（换个指针）、clear（封存指针之前）、replay（走到指针）都变得安全。

P2 的落地方式：

- **leaf = 文件末尾**（追加日志天然如此，无需单独存指针）。
- **`reset_boundary` 条目 = 日志内的「回放截止点」**：replay 只取最后一个 `reset_boundary` 之后的消息。
- **fork = 快照**：把父会话当前消息复制进一个新会话文件（语义等价于「换 leaf」，只是复制而非共享）。「共享日志 + 多 leaf」的零拷贝优化留到 P6。

### 1.3 blob 外置：大块内容 `blob:sha256:<hash>`

oh-my-pi 把图片/大工具输出存到日志外，日志里只留 `blob:sha256:<hash>` 引用，保持 JSONL 行式、小而可读。**P2 消息全文本，暂无大块**，所以本阶段只立概念、不实现；P4 工具结果超长落盘时再引入。

---

## 2. Entry 类型（`internal/session/entry.go`）

```go
package session

import "einoclaw-build/internal/message"

// EntryType 区分 JSONL 一行的类型。
type EntryType string

const (
	EntrySession EntryType = "session"        // 会话头（版本 + id）
	EntryMessage EntryType = "message"        // 一条消息（user/assistant/tool）
	EntryReset   EntryType = "reset_boundary" // /clear 封存标记
)

// Entry 是 JSONL 里的一行。用 Type 区分，Type 对应的字段才有值。
type Entry struct {
	Type    EntryType        `json:"type"`
	Version int              `json:"version,omitempty"` // EntrySession: 格式版本
	ID      string           `json:"id,omitempty"`      // EntrySession: 会话 id
	Message *message.Message `json:"message,omitempty"` // EntryMessage: 消息内容
}
```

三种行的 JSON 形态：

```json
{"type":"session","version":1,"id":"default"}
{"type":"message","message":{"role":"user","blocks":[{"kind":0,"text":"你好"}]}}
{"type":"reset_boundary"}
```

> `reset_boundary` 无字段，仅靠 `type` 表达（对应 oh-my-pi 的 `reset_boundary` entry）。

---

## 3. Storage 接口（`internal/session/storage.go`）

把「日志存哪」抽象出来，让 Session 可脱离文件单测：

```go
package session

// Storage 是会话日志的底层存取抽象。
type Storage interface {
	Append(e Entry) error   // 追加一行
	Entries() ([]Entry, error) // 读回全部行（原始顺序，不处理 reset）
}

// FileStorage 把日志写进一个 JSONL 文件。
type FileStorage struct {
	mu sync.Mutex
	f  *os.File
}

func NewFileStorage(path string) (*FileStorage, error) {
	// 打开/创建文件（os.O_CREATE|os.O_APPEND|os.O_WRONLY）
	// 用 bufio.Writer 包装，Append 时 encode + 写 \n + flush
}

func (fs *FileStorage) Append(e Entry) error { /* encode → 写一行 */ }
func (fs *FileStorage) Entries() ([]Entry, error) { /* 读文件，逐行 json.Unmarshal */ }

// MemoryStorage 内存实现，供单测用。
type MemoryStorage struct {
	mu      sync.Mutex
	entries []Entry
}
```

设计要点：

1. **Storage 只做「追加」和「读回原始行」**，不知道 reset/fork 语义——那些是 `Session` 的事。分层清晰。
2. `FileStorage.Append` 每次 flush 一次（简单可靠；oh-my-pi 用 no-fsync 追求快，P2 先要正确性）。
3. `Entries` 读文件时，**最后一行若是残行（崩溃截断）就跳过**，不 panic。

---

## 4. Session（`internal/session/session.go`）

```go
package session

import "einoclaw-build/internal/message"

// Session 在 Storage 之上加语义：reset 封存、replay 重建、fork 分支。
type Session struct {
	mu      sync.Mutex
	id      string
	storage Storage
}

// New 创建会话并写 header（version=1, id）。
func New(id string, st Storage) (*Session, error)

// Append 记录一条消息（user/assistant/tool）。
func (s *Session) Append(m message.Message) error

// Reset 写一条 reset_boundary，封存之前的历史。
func (s *Session) Reset() error

// Replay 重放日志，返回最后一个 reset_boundary 之后的消息。
func (s *Session) Replay() ([]message.Message, error)

// Fork 快照出一个新会话：复制当前消息到新 id 的会话。
func (s *Session) Fork(newID string) (*Session, error)

// Close 关闭底层存储（FileStorage 关文件）。
func (s *Session) Close() error
```

### 4.1 Replay 语义（核心）

```go
func (s *Session) Replay() ([]message.Message, error) {
	entries, err := s.storage.Entries()
	if err != nil { return nil, err }
	var msgs []message.Message
	for _, e := range entries {
		switch e.Type {
		case EntryMessage:
			if e.Message != nil {
				msgs = append(msgs, *e.Message)
			}
		case EntryReset:
			msgs = msgs[:0] // 封存：清空之前累积
		}
		// EntrySession（header）忽略
	}
	return msgs, nil
}
```

### 4.2 Fork（快照版）

```go
func (s *Session) Fork(newID string) (*Session, error) {
	msgs, err := s.Replay()
	if err != nil { return nil, err }
	// 新会话需要一个新 Storage（调用方提供），这里只做「复制消息」的逻辑：
	// 实际上 Fork 的签名需要拿到新 storage —— 见 §6 的实现细节
}
```

> 实现细节：`Fork` 需要「新 id + 新 storage」两个参数。签名最终定为 `Fork(newID string, st Storage) (*Session, error)`：新会话写 header、把 `Replay()` 的消息逐条 `Append` 进去。父会话不受影响。

---

## 5. 消息 JSON tag（改 `internal/message/message.go`）

让 JSONL 可读可 diff（默认 Go 序列化会出大写键 + `null` 指针字段）。给 message 类型加 tag：

```go
type ContentBlock struct {
	Kind       BlockKind   `json:"kind"`
	Text       string      `json:"text,omitempty"`
	Thinking   string      `json:"thinking,omitempty"`
	ToolCall   *ToolCall   `json:"tool_call,omitempty"`
	ToolResult *ToolResult `json:"tool_result,omitempty"`
}

type ToolCall struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Args string `json:"args"`
}

type ToolResult struct {
	ToolCallID string `json:"tool_call_id"`
	Name       string `json:"name"`
	Content    string `json:"content"`
	IsError    bool   `json:"is_error"`
}

type Message struct {
	Role   Role           `json:"role"`
	Blocks []ContentBlock `json:"blocks"`
}
```

> `BlockKind` 仍序列化为 int（0/1/2/3），P2 接受（可读性够用）；要字符串化再自定义 `MarshalJSON`（后续可加）。

---

## 6. 接线（`cmd/agent` + `internal/tui`）

### 6.1 main：固定会话文件 + 重启恢复

```go
func main() {
	cfg := loadConfig()
	m, _ := model.New(...)
	ag := agent.New("codeclaw", instruction, m)

	// 固定会话文件，重启即恢复历史（多会话 /resume 在 P9）
	os.MkdirAll("sessions", 0755)
	st, err := session.NewFileStorage("sessions/default.jsonl")
	s, err := session.New("default", st)   // 写/读 header
	defer s.Close()

	program := tea.NewProgram(tui.NewModel(ag, s))
	tui.SetProgram(program)
	program.Run()
}
```

### 6.2 TUI：记录 user/assistant + `/clear`

`teaModel` 增 `session *session.Session` 字段。`runAgent` 变成：

```go
func (m teaModel) runAgent(ctx context.Context, text string) {
	userMsg := message.NewUserMessage(text)

	// 1. 加载历史（到最后一个 reset_boundary）
	history, _ := m.session.Replay()
	// 2. 记录用户消息
	_ = m.session.Append(userMsg)
	// 3. 跑 agent：输入 = 历史 + 用户消息
	input := append(history, userMsg)
	for ev := range m.agent.Run(ctx, input) {
		if program != nil { program.Send(ev) }
		// 4. 定稿后记录 assistant 消息
		if ev.Type == agent.EventMessageEnd {
			_ = m.session.Append(ev.Ended.Message)
		}
	}
}
```

`handleKey` 的 Enter 分支开头加 `/clear`：

```go
case enter:
	text := strings.TrimSpace(m.inputArea.Value())
	if text == "/clear" {
		_ = m.session.Reset()
		m.chatLines = nil
		m.inputArea.Reset()
		return m, nil
	}
	// ... 原有流程
```

**启动时恢复历史**：`NewModel(ag, s)` 里 `Replay()` 出历史，把每条消息渲染进初始 `chatLines`（user → `userPrefix+text`，assistant → `renderAIMessage(text)`）。

---

## 7. 边界情况与错误处理

| 情况 | 处理 |
|---|---|
| 文件不存在（首次运行） | `NewFileStorage` 用 `O_CREATE` 创建 |
| 日志最后一行为残行（崩溃） | `Entries` 跳过残行，不报错 |
| 空会话 Replay | 返回空切片 |
| Replay 遇 `reset_boundary` | 清空累积，从下一个 message 重来 |
| fork 后父/子互不影响 | 快照复制，独立文件 |
| 并发 Append（跨 turn goroutine） | `Storage`/`Session` 的 mutex 串行化 |

---

## 8. 对外契约（后续阶段依赖）

| 类型/函数 | 包 | 后续谁依赖 |
|---|---|---|
| `session.Session`（Append/Replay/Reset/Fork/Close） | `internal/session` | `cmd/agent`、`internal/context`（P3 compaction 依赖 replay）、`internal/subagent`（P6） |
| `session.Storage` 接口 + `FileStorage`/`MemoryStorage` | `internal/session` | 单测 + `internal/trace`（P7 增量解析） |
| `session.Entry` / `EntryType` | `internal/session` | `internal/trace`（P7 读同一份 JSONL） |
| `message.Message` 的 JSON 序列化 | `internal/message` | 上述全部 |

---

## 9. 待评审点

1. **固定会话文件 `sessions/default.jsonl`**（重启恢复历史），多会话 + `/resume` 留 P9——接受吗？
2. **fork 用「快照复制」而非「共享日志 + 多 leaf」**（零拷贝优化留 P6 子 Agent）——接受这个简化吗？
3. **`/clear` 用「输入框硬编码判断」而非完整 slash 命令框架**（后者 P10）——接受吗？
4. **blob 外置本阶段不实现**（P2 消息全文本，P4 工具结果时再加）——接受吗？
5. **`BlockKind` 序列化为 int（0/1/2/3）**，不自定义 `MarshalJSON` 做字符串——接受吗？
