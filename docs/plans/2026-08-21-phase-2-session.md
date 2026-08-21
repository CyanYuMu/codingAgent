# Phase 2 Session 实现计划

> **Goal:** 用自写 JSONL 会话替换 eino session，实现多轮历史可恢复、可 `/clear`、可 fork。
>
> **Architecture:** `Session`（语义：reset/replay/fork）→ `Storage`（底层：append/read）→ JSONL 文件；TUI 每轮记录 user/assistant，启动 replay 恢复历史。
>
> **Tech Stack:** Go stdlib（encoding/json/os/bufio）+ 自建 `internal/session` + `internal/message`。
>
> **Spec / 设计:** [../specs/phase-2-session.md](../specs/phase-2-session.md)（§2-§6 含完整代码，本计划引用之）。

## Global Constraints

- 构建/运行统一 `env -u GOROOT go ...`。
- 只有 `internal/model` 可 import eino；`internal/session`/`internal/tui` 不 import eino。
- 每任务末尾 `go build ./...` + `go test ./...` 通过。

---

## 文件结构

| 文件 | 职责 |
|---|---|
| `internal/session/entry.go` | `Entry` + `EntryType`（§2） |
| `internal/session/storage.go` | `Storage` 接口 + `FileStorage` + `MemoryStorage`（§3） |
| `internal/session/session.go` | `Session`（Append/Reset/Replay/Fork/Close）（§4） |
| `internal/session/session_test.go` | storage + session 单测 |
| `internal/message/message.go` | 加 JSON tag（§5） |
| `internal/tui/tui.go` | 接 session（§6.2） |
| `cmd/agent/main.go` | 建 session（§6.1） |

---

## Task 1: `entry.go`（类型，无测试）

**Files:** Create `internal/session/entry.go`

- [ ] **Step 1:** 照抄 §2（`EntryType` 常量 + `Entry` 结构）。

- [ ] **Step 2:** 构建

```bash
cd /Users/cyanyumu/Projects/GoProject/einoclaw-build && env -u GOROOT go build ./internal/session/ 2>&1 | head
```

---

## Task 2: `message.go` 加 JSON tag（改类型）

**Files:** Modify `internal/message/message.go`

- [ ] **Step 1:** 给 `ContentBlock`/`ToolCall`/`ToolResult`/`Message` 加 §5 的 JSON tag（只加 tag，字段名不变，现有测试不受影响）。

- [ ] **Step 2:** 验证现有测试仍通过

```bash
env -u GOROOT go test ./internal/message/ ./internal/agent/ 2>&1 | tail -6
```

---

## Task 3: `storage.go`（Storage + MemoryStorage + FileStorage，TDD）

**Files:** Create `internal/session/storage.go`, `internal/session/storage_test.go`

- [ ] **Step 1: 写失败测试** `storage_test.go`

```go
package session

import (
	"path/filepath"
	"testing"

	"einoclaw-build/internal/message"
)

func TestMemoryStorageAppendEntries(t *testing.T) {
	st := &MemoryStorage{}
	_ = st.Append(Entry{Type: EntryMessage, Message: &message.Message{Role: message.RoleUser, Blocks: []message.ContentBlock{{Kind: message.BlockText, Text: "hi"}}}})
	_ = st.Append(Entry{Type: EntryReset})
	es, err := st.Entries()
	if err != nil || len(es) != 2 {
		t.Fatalf("entries = %d, err = %v", len(es), err)
	}
	if es[0].Type != EntryMessage || es[1].Type != EntryReset {
		t.Fatalf("entries = %+v", es)
	}
}

func TestFileStorageRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	fs, err := NewFileStorage(path)
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close()

	_ = fs.Append(Entry{Type: EntrySession, Version: 1, ID: "s1"})
	_ = fs.Append(Entry{Type: EntryMessage, Message: &message.Message{Role: message.RoleUser, Blocks: []message.ContentBlock{{Kind: message.BlockText, Text: "你好"}}}})

	es, err := fs.Entries()
	if err != nil || len(es) != 2 {
		t.Fatalf("entries = %d, err = %v", len(es), err)
	}
	if es[1].Message == nil || es[1].Message.Blocks[0].Text != "你好" {
		t.Fatalf("round-trip message = %+v", es[1])
	}
}
```

- [ ] **Step 2: 运行确认失败**

```bash
env -u GOROOT go test ./internal/session/ -run Test -v 2>&1 | head -10
```

- [ ] **Step 3: 实现** `storage.go`（§3，含 `Close`）：

```go
package session

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"sync"
)

// Storage 是会话日志的底层存取抽象。
type Storage interface {
	Append(e Entry) error
	Entries() ([]Entry, error)
	Close() error
}

// FileStorage 把日志写进一个 JSONL 文件。
type FileStorage struct {
	path string
	f    *os.File
	w    *bufio.Writer
}

func NewFileStorage(path string) (*FileStorage, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return nil, err
	}
	return &FileStorage{path: path, f: f, w: bufio.NewWriter(f)}, nil
}

func (fs *FileStorage) Append(e Entry) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if _, err := fs.w.Write(b); err != nil {
		return err
	}
	if err := fs.w.WriteByte('\n'); err != nil {
		return err
	}
	return fs.w.Flush()
}

func (fs *FileStorage) Entries() ([]Entry, error) {
	data, err := os.ReadFile(fs.path)
	if err != nil {
		return nil, err
	}
	var entries []Entry
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var e Entry
		if json.Unmarshal([]byte(line), &e) != nil {
			continue // 跳过残行
		}
		entries = append(entries, e)
	}
	return entries, nil
}

func (fs *FileStorage) Close() error { return fs.f.Close() }

// MemoryStorage 内存实现，供单测用。
type MemoryStorage struct {
	mu      sync.Mutex
	entries []Entry
}

func (m *MemoryStorage) Append(e Entry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries = append(m.entries, e)
	return nil
}

func (m *MemoryStorage) Entries() ([]Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Entry, len(m.entries))
	copy(out, m.entries)
	return out, nil
}

func (m *MemoryStorage) Close() error { return nil }
```

- [ ] **Step 4: 运行确认通过**

```bash
env -u GOROOT go test ./internal/session/ -v 2>&1 | tail -10
```

---

## Task 4: `session.go`（Session，TDD）

**Files:** Create `internal/session/session.go`, `internal/session/session_test.go`

**Consumes:** `Storage`/`Entry`（Task 1/3）、`message.Message`（Task 2）
**Produces:** `Session`（Append/Reset/Replay/Fork/Close）

- [ ] **Step 1: 写失败测试** `session_test.go`

```go
package session

import (
	"testing"

	"einoclaw-build/internal/message"
)

func msg(role message.Role, text string) message.Message {
	return message.Message{Role: role, Blocks: []message.ContentBlock{{Kind: message.BlockText, Text: text}}}
}

func TestReplayAppendsInOrder(t *testing.T) {
	s, _ := New("s1", &MemoryStorage{})
	_ = s.Append(msg(message.RoleUser, "u1"))
	_ = s.Append(msg(message.RoleAssistant, "a1"))

	ms, err := s.Replay()
	if err != nil || len(ms) != 2 {
		t.Fatalf("replay = %d, err = %v", len(ms), err)
	}
	if ms[0].Blocks[0].Text != "u1" || ms[1].Blocks[0].Text != "a1" {
		t.Fatalf("replay = %+v", ms)
	}
}

func TestResetSealsOldContext(t *testing.T) {
	s, _ := New("s1", &MemoryStorage{})
	_ = s.Append(msg(message.RoleUser, "old"))
	_ = s.Reset()
	_ = s.Append(msg(message.RoleUser, "new"))

	ms, err := s.Replay()
	if err != nil || len(ms) != 1 {
		t.Fatalf("replay = %d, err = %v", len(ms), err)
	}
	if ms[0].Blocks[0].Text != "new" {
		t.Fatalf("replay = %+v", ms)
	}
}

func TestForkCopiesAndIsolates(t *testing.T) {
	parent, _ := New("p", &MemoryStorage{})
	_ = parent.Append(msg(message.RoleUser, "u1"))
	_ = parent.Append(msg(message.RoleAssistant, "a1"))

	child, err := parent.Fork("c", &MemoryStorage{})
	if err != nil {
		t.Fatal(err)
	}
	// 子会话初始与父相同
	cm, _ := child.Replay()
	if len(cm) != 2 {
		t.Fatalf("child replay = %d", len(cm))
	}
	// 子会话追加不影响父
	_ = child.Append(msg(message.RoleUser, "u2"))
	pm, _ := parent.Replay()
	if len(pm) != 2 {
		t.Fatalf("parent replay after fork append = %d, want 2", len(pm))
	}
}
```

- [ ] **Step 2: 运行确认失败**

```bash
env -u GOROOT go test ./internal/session/ -run TestReplay -v 2>&1 | head -10
```

- [ ] **Step 3: 实现** `session.go`（§4，注意 `Replay` 拆 `replayLocked` 避免 Fork 里死锁）：

```go
package session

import (
	"sync"

	"einoclaw-build/internal/message"
)

// Session 在 Storage 之上加语义：reset 封存、replay 重建、fork 分支。
type Session struct {
	mu      sync.Mutex
	id      string
	storage Storage
}

func New(id string, st Storage) (*Session, error) {
	if err := st.Append(Entry{Type: EntrySession, Version: 1, ID: id}); err != nil {
		return nil, err
	}
	return &Session{id: id, storage: st}, nil
}

func (s *Session) Append(m message.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.storage.Append(Entry{Type: EntryMessage, Message: &m})
}

func (s *Session) Reset() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.storage.Append(Entry{Type: EntryReset})
}

func (s *Session) Replay() ([]message.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.replayLocked()
}

func (s *Session) replayLocked() ([]message.Message, error) {
	entries, err := s.storage.Entries()
	if err != nil {
		return nil, err
	}
	var msgs []message.Message
	for _, e := range entries {
		switch e.Type {
		case EntryMessage:
			if e.Message != nil {
				msgs = append(msgs, *e.Message)
			}
		case EntryReset:
			msgs = msgs[:0]
		}
	}
	return msgs, nil
}

func (s *Session) Fork(newID string, st Storage) (*Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	msgs, err := s.replayLocked()
	if err != nil {
		return nil, err
	}
	child, err := New(newID, st)
	if err != nil {
		return nil, err
	}
	for _, m := range msgs {
		if err := child.storage.Append(Entry{Type: EntryMessage, Message: &m}); err != nil {
			return nil, err
		}
	}
	return child, nil
}

func (s *Session) Close() error { return s.storage.Close() }
```

- [ ] **Step 4: 运行确认通过**

```bash
env -u GOROOT go test ./internal/session/ -v 2>&1 | tail -12
env -u GOROOT go build ./... 2>&1 | head
```

---

## Task 5: 接线（main.go + tui.go，手动验收）

**Files:** Modify `cmd/agent/main.go`, `internal/tui/tui.go`

- [ ] **Step 1:** `main.go` 建会话（§6.1）：`os.MkdirAll("sessions")` → `session.NewFileStorage("sessions/default.jsonl")` → `session.New("default", st)` → `tui.NewModel(ag, s)` → `defer s.Close()`。

- [ ] **Step 2:** `tui.go` 接 session（§6.2）：
  - `teaModel` 增 `session *session.Session`；`NewModel(ag, s)`。
  - `runAgent` 改成「Replay 历史 → Append 用户 → Run → MessageEnd 时 Append assistant」。
  - Enter 分支开头判 `/clear` → `session.Reset()` + 清 `chatLines`。
  - `NewModel` 里 Replay 历史渲染进初始 `chatLines`。

- [ ] **Step 3:** 构建 + vet + test

```bash
env -u GOROOT go build ./... && env -u GOROOT go vet ./... && env -u GOROOT go test ./... 2>&1 | tail -8
```

- [ ] **Step 4:** 手动验收（用户终端）：跑一次对话 → 退出 → 再 `go run` 看历史恢复；输入 `/clear` 后再问，旧上下文不再注入。

---

## 自检

- **spec 覆盖**：P2 的 5 项产出 → Task 1-5 全覆盖。
- **类型一致性**：`Storage` 接口（Task 3）被 `Session`（Task 4）消费；`Session` 被 main/tui（Task 5）消费，签名一致；`Entry.Message *message.Message` 依赖 Task 2 的 JSON tag。
- **无占位符**：可测部分（Task 3/4）测试与实现全量内联。
