# Phase 3 Context Engineering 实现计划

> **Goal:** 用 provider usage 做 token 真值，超阈值自动增量压缩，让长对话不溢出。
>
> **Architecture:** `agent` 暴露 usage → `session` 支持 compaction 条目 → `context.ContextManager` 每轮后判阈值、找切点、摘要、压缩。
>
> **Tech Stack:** Go stdlib + 自建 `internal/context` + 扩展 `internal/session`/`internal/agent`。
>
> **Spec / 设计:** [../specs/phase-3-context.md](../specs/phase-3-context.md)（§2-§7 含完整代码）。

## Global Constraints

- 构建/运行统一 `env -u GOROOT go ...`。
- 只有 `internal/model` 可 import eino。
- 每任务末尾 `go build ./...` + `go test ./...` 通过。

---

## 文件结构

| 文件 | 职责 |
|---|---|
| `internal/agent/event.go` | `MessageEnd` 加 `Usage`（§2） |
| `internal/agent/loop.go` | `eventStream` 加 `Usage()`，consumeStream 带上（§2） |
| `internal/agent/loop_test.go` | `fakeStream` 补 `Usage()` |
| `internal/session/entry.go` | `EntryCompaction` + `Compaction`（§3.1） |
| `internal/session/session.go` | `Compact` + Replay 处理 compaction（§3.2/3.3） |
| `internal/session/session_test.go` | 压缩测试 |
| `internal/context/tokenizer.go` | `estimateTokens`（§4） |
| `internal/context/compaction.go` | `findCutPoint` + `serializeConversation`（§5.1/5.3） |
| `internal/context/manager.go` | `summarizer` 接口 + `ContextManager`（§6） |
| `internal/context/context_test.go` | tokenizer/cut/threshold/compact 测试 |
| `cmd/agent/config.go` | `modelConfig` 加 `ContextWindow`（§7.1） |
| `cmd/agent/main.go` + `tui.go` | 接线（§7） |

---

## Task 1: agent 暴露 usage（改 P1 代码）

**Files:** Modify `internal/agent/event.go`, `internal/agent/loop.go`, `internal/agent/loop_test.go`

- [ ] **Step 1:** `event.go` 的 `MessageEnd` 加 `Usage model.Usage` 字段（`model` import 已在）。

- [ ] **Step 2:** `loop.go` 的 `eventStream` 加 `Usage() model.Usage`；`consumeStream` 的 `EventMessageEnd` 带上 `Usage: stream.Usage()`。

- [ ] **Step 3:** `loop_test.go` 的 `fakeStream` 加 `Usage()` 方法（返回零值 `model.Usage{}`）。

- [ ] **Step 4:** 验证

```bash
cd /Users/cyanyumu/Projects/GoProject/einoclaw-build && env -u GOROOT go test ./internal/agent/ 2>&1 | tail -5
```

---

## Task 2: session 压缩条目 + Compact + Replay（TDD）

**Files:** Modify `internal/session/entry.go`, `internal/session/session.go`, Create `internal/session/compact_test.go`

- [ ] **Step 1: 写失败测试** `compact_test.go`

```go
package session

import (
	"testing"

	"einoclaw-build/internal/message"
)

func TestCompactReplacesPrefixWithSummary(t *testing.T) {
	s, _ := New("s1", &MemoryStorage{})
	_ = s.Append(msg(message.RoleUser, "m0"))
	_ = s.Append(msg(message.RoleAssistant, "m1"))
	_ = s.Append(msg(message.RoleUser, "m2"))
	_ = s.Append(msg(message.RoleAssistant, "m3"))

	// 压缩：摘要 m0-m1，保留 m2-m3
	kept := []message.Message{msg(message.RoleUser, "m2"), msg(message.RoleAssistant, "m3")}
	if err := s.Compact("SUMMARY", kept); err != nil {
		t.Fatal(err)
	}

	ms, err := s.Replay()
	if err != nil || len(ms) != 3 {
		t.Fatalf("replay = %d, err = %v", len(ms), err)
	}
	if ms[0].Blocks[0].Text != "SUMMARY" || ms[1].Blocks[0].Text != "m2" || ms[2].Blocks[0].Text != "m3" {
		t.Fatalf("replay = %+v", ms)
	}
}
```

- [ ] **Step 2: 运行确认失败**

```bash
env -u GOROOT go test ./internal/session/ -run TestCompact -v 2>&1 | head -8
```

- [ ] **Step 3: 实现**（§3）：
  - `entry.go`：加 `EntryCompaction` 常量 + `Entry.Compaction *Compaction` + `Compaction{Summary string}`。
  - `session.go`：加 `Compact(summary, kept)` 方法；`replayLocked` 加 `case EntryCompaction: msgs = []message.Message{message.NewUserMessage(e.Compaction.Summary)}`。

- [ ] **Step 4: 运行确认通过**

```bash
env -u GOROOT go test ./internal/session/ -v 2>&1 | tail -8
```

---

## Task 3: tokenizer + findCutPoint + serialize（TDD）

**Files:** Create `internal/context/tokenizer.go`, `internal/context/compaction.go`, `internal/context/context_test.go`

- [ ] **Step 1: 写失败测试**（含 helper `msg`）

```go
package context

import (
	"testing"

	"einoclaw-build/internal/message"
)

func ctxMsg(role message.Role, text string) message.Message {
	return message.Message{Role: role, Blocks: []message.ContentBlock{{Kind: message.BlockText, Text: text}}}
}

func TestEstimateTokens(t *testing.T) {
	if got := estimateTokens(ctxMsg(message.RoleUser, "hello world")); got != 9 { // 11 runes/2 + 4
		t.Fatalf("estimateTokens = %d, want 9", got)
	}
}

func TestFindCutPoint(t *testing.T) {
	msgs := []message.Message{
		ctxMsg(message.RoleUser, "aaaa"), ctxMsg(message.RoleAssistant, "bbbb"), ctxMsg(message.RoleUser, "cccc"),
	}
	// 每条 4 runes/2+4 = 6 token；keep=8：cccc(6)<8 → +bbbb(12)>=8 → cut=1
	if got := findCutPoint(msgs, 8); got != 1 {
		t.Fatalf("findCutPoint = %d, want 1", got)
	}
}
```

- [ ] **Step 2: 运行确认失败** → **Step 3: 实现**（§4/§5.1/§5.3）→ **Step 4: 确认通过**

```bash
env -u GOROOT go test ./internal/context/ -run Test -v 2>&1 | tail -8
```

---

## Task 4: ContextManager + summarizer + compact（TDD）

**Files:** Modify `internal/context/manager.go`, `internal/context/context_test.go`

- [ ] **Step 1: 写失败测试**

```go
type fakeSummarizer struct {
	got []message.Message
	out string
}

func (f *fakeSummarizer) Summarize(_ context.Context, msgs []message.Message) (string, error) {
	f.got = msgs
	return f.out, nil
}

func TestThreshold(t *testing.T) {
	cm := New(nil, nil, 1000, 100)
	if got := cm.threshold(); got != 500 { // 1000 - max(150,16384)→reserve>=window→500
		t.Fatalf("threshold = %d, want 500", got)
	}
}

func TestAfterTurnCompactsWhenOverThreshold(t *testing.T) {
	st := &session.MemoryStorage{}
	s, _ := session.New("s1", st)
	_ = s.Append(ctxMsg(message.RoleUser, "m0"))
	_ = s.Append(ctxMsg(message.RoleAssistant, "m1"))
	_ = s.Append(ctxMsg(message.RoleUser, "m2"))
	_ = s.Append(ctxMsg(message.RoleAssistant, "m3"))

	fs := &fakeSummarizer{out: "SUMMARY"}
	cm := New(s, fs, 1000, 12) // keep=12 → 保留最后 2 条

	// usage.PromptTokens=600 > threshold=500 → 压缩
	if err := cm.AfterTurn(context.Background(), model.Usage{PromptTokens: 600}); err != nil {
		t.Fatal(err)
	}
	// 摘要输入应是更早的 [m0,m1]
	if len(fs.got) != 2 || fs.got[0].Blocks[0].Text != "m0" {
		t.Fatalf("summarize input = %+v", fs.got)
	}
	// replay 应是 [SUMMARY, m2, m3]
	ms, _ := s.Replay()
	if len(ms) != 3 || ms[0].Blocks[0].Text != "SUMMARY" {
		t.Fatalf("replay = %+v", ms)
	}
}

func TestAfterTurnNoCompactWhenUnderThreshold(t *testing.T) {
	s, _ := session.New("s1", &session.MemoryStorage{})
	_ = s.Append(ctxMsg(message.RoleUser, "m0"))
	fs := &fakeSummarizer{out: "X"}
	cm := New(s, fs, 1000, 12)
	if err := cm.AfterTurn(context.Background(), model.Usage{PromptTokens: 100}); err != nil {
		t.Fatal(err)
	}
	if fs.got != nil {
		t.Fatalf("summarizer should not be called, got %+v", fs.got)
	}
}
```

- [ ] **Step 2: 运行确认失败** → **Step 3: 实现**（§6 + summarizer 接口）→ **Step 4: 确认通过**

```bash
env -u GOROOT go test ./internal/context/ -v 2>&1 | tail -12
env -u GOROOT go build ./... 2>&1 | head
```

---

## Task 5: 接线（config + main + tui）

**Files:** Modify `cmd/agent/config.go`, `cmd/agent/main.go`, `internal/tui/tui.go`

- [ ] **Step 1:** `config.go` 的 `modelConfig` 加 `ContextWindow int yaml:"context_window"`；`loadConfig` 里若为 0 默认 128000。

- [ ] **Step 2:** `main.go` 建 `context.NewManager(s, context.NewModelSummarizer(m), window, 16384)`，传给 `tui.NewModel(ag, s, cm)`。

- [ ] **Step 3:** `tui.go` 的 `teaModel` 加 `context *context.ContextManager`；`NewModel` 签名加参数；`runAgent` 的 `EventMessageEnd` 分支追加 assistant 后调 `m.context.AfterTurn(ctx, ev.Ended.Usage)`。

- [ ] **Step 4:** 构建 + vet + test

```bash
env -u GOROOT go build ./... && env -u GOROOT go vet ./... && env -u GOROOT go test ./... 2>&1 | tail -8
```

- [ ] **Step 5:** 手动验收（用户终端）：把 `config.yaml` 的 `context_window` 临时设 2000，连问几轮，`cat sessions/default.jsonl` 应出现 `compaction` 行。

---

## 自检

- **spec 覆盖**：P3 的 6 项产出 → Task 1-5 全覆盖。
- **类型一致性**：`MessageEnd.Usage`（Task 1）被 `AfterTurn(usage)`（Task 4）消费；`Session.Compact`（Task 2）被 `ContextManager.compact`（Task 4）调用；`summarizer` 接口（Task 4）与 `model.Model` 适配。
- **无占位符**：可测部分（Task 2/3/4）测试与实现全量内联。
