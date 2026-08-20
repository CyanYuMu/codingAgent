# Phase 0 地基 实现计划

> **Goal:** 建立多包骨架 + 共享消息类型 + 把 eino 关进 `internal/model`，用冒烟 main 证明「模型客户端能自组流式吐字 + 回收 usage」。
>
> **Architecture:** `internal/message`（零依赖共享词汇）← `internal/model`（唯一 import eino 的包，双消息模型边界转换）← `cmd/agent`（装配 + 冒烟）。
>
> **Tech Stack:** Go + eino `components/model`（agentic provider 包）+ `gopkg.in/yaml.v3`。
>
> **Spec / 设计:** [../specs/phase-0-foundation.md](../specs/phase-0-foundation.md)（§3/§4/§5 含完整实现代码，本计划引用之）。

## Global Constraints

- 构建/运行统一用 `env -u GOROOT go ...`（本机 GOROOT 工具链不匹配）。
- **只有 `internal/model` 可以 import eino**；其余包 import 仅 stdlib + yaml。
- `internal/agent.Message` 是自建 struct（在 `internal/message` 包）。
- 模型 client 用 agentic provider 包（`AgenticModel` 维度），**不用**非 agentic 的 `openai/qwen/ark/deepseek@v0.1.x`（版本不兼容 eino v0.10）。
- 每任务末尾：`go build ./...` + `go vet ./...` 通过（commit 可选，用户要求才提交）。

---

## 文件结构

| 文件 | 职责 |
|---|---|
| `internal/message/message.go` | 消息类型 + 构造器（§3） |
| `internal/message/message_test.go` | 构造器单测 |
| `internal/model/model.go` | `Model` 接口 + `Stream`/`ModelEvent`/`ToolSpec`/`Usage`/`ToolCallDelta`/`Config`（§4） |
| `internal/model/eino.go` | eino 适配：`einoModel` + 转换函数 + `New`（§5.2/5.3） |
| `internal/model/eino_test.go` | 纯转换函数单测 |
| `cmd/agent/config.go` | 配置（§6，从根 config.go 迁移） |
| `cmd/agent/main.go` | 冒烟 main（§7） |
| 删除 | 根 `main.go`/`model.go`/`handlers.go`/`messages.go`/`tui.go`/`markdown.go`/`config.go` |

---

## Task 1: 共享消息类型（`internal/message`）

**Files:** Create `internal/message/message.go`, `internal/message/message_test.go`

**Produces:** `message.Message` / `ContentBlock` / `BlockKind` / `Role` / `ToolCall` / `ToolResult` + `NewSystemMessage` / `NewUserMessage` / `NewToolMessage`。

- [ ] **Step 1: 写失败测试** `message_test.go`

```go
package message

import "testing"

func TestNewUserMessage(t *testing.T) {
	m := NewUserMessage("hello")
	if m.Role != RoleUser {
		t.Fatalf("role = %q, want %q", m.Role, RoleUser)
	}
	if len(m.Blocks) != 1 || m.Blocks[0].Kind != BlockText || m.Blocks[0].Text != "hello" {
		t.Fatalf("blocks = %+v, want single text block 'hello'", m.Blocks)
	}
}

func TestNewToolMessage(t *testing.T) {
	m := NewToolMessage("c1", "read", "content", true)
	if m.Role != RoleTool {
		t.Fatalf("role = %q, want %q", m.Role, RoleTool)
	}
	if len(m.Blocks) != 1 {
		t.Fatalf("blocks len = %d, want 1", len(m.Blocks))
	}
	b := m.Blocks[0]
	if b.Kind != BlockToolResult || b.ToolResult == nil {
		t.Fatalf("block = %+v, want toolResult", b)
	}
	r := b.ToolResult
	if r.ToolCallID != "c1" || r.Name != "read" || r.Content != "content" || !r.IsError {
		t.Fatalf("toolResult = %+v", r)
	}
}
```

- [ ] **Step 2: 运行确认失败**

```bash
cd /Users/cyanyumu/Projects/GoProject/einoclaw-build
env -u GOROOT go test ./internal/message/ -run TestNew -v
# 预期：编译失败（NewUserMessage 未定义）
```

- [ ] **Step 3: 实现** `message.go`（照抄设计文档 §3 的完整代码：BlockKind/常量/ToolCall/ToolResult/ContentBlock/Role/常量/Message + 三个构造器）

- [ ] **Step 4: 运行确认通过**

```bash
env -u GOROOT go test ./internal/message/ -v
# 预期：PASS
```

---

## Task 2: 模型客户端抽象 + eino 适配（`internal/model`）

**Files:** Create `internal/model/model.go`, `internal/model/eino.go`, `internal/model/eino_test.go`

**Consumes:** `message.Message`（Task 1）
**Produces:** `model.Model`（`Stream` 方法）、`model.Stream`、`model.Usage`、`model.ToolSpec`、`model.New(ctx, Config)`

- [ ] **Step 1: 写失败测试** `eino_test.go`（只测纯转换函数，网络路径由冒烟覆盖）

```go
package model

import (
	"testing"

	"github.com/cloudwego/eino/schema"

	"einoclaw-build/internal/message"
)

func TestTextOf(t *testing.T) {
	m := message.Message{Role: message.RoleUser, Blocks: []message.ContentBlock{
		{Kind: message.BlockText, Text: "a"},
		{Kind: message.BlockThinking, Thinking: "think"},
		{Kind: message.BlockText, Text: "b"},
	}}
	if got := textOf(m); got != "ab" {
		t.Fatalf("textOf = %q, want %q", got, "ab")
	}
}

func TestToAgenticMessagesSystemUser(t *testing.T) {
	msgs := []message.Message{
		message.NewSystemMessage("sys"),
		message.NewUserMessage("hi"),
	}
	out := toAgenticMessages(msgs)
	if len(out) != 2 {
		t.Fatalf("len = %d, want 2", len(out))
	}
	if out[0].Role != schema.AgenticRoleTypeSystem {
		t.Fatalf("out[0].Role = %q, want system", out[0].Role)
	}
	if got := out[0].ContentBlocks[0].UserInputText.Text; got != "sys" {
		t.Fatalf("out[0] text = %q, want sys", got)
	}
	if out[1].Role != schema.AgenticRoleTypeUser {
		t.Fatalf("out[1].Role = %q, want user", out[1].Role)
	}
	if got := out[1].ContentBlocks[0].UserInputText.Text; got != "hi" {
		t.Fatalf("out[1] text = %q, want hi", got)
	}
}

func TestFromSchemaUsage(t *testing.T) {
	u := fromSchemaUsage(&schema.TokenUsage{
		PromptTokens:     10,
		CompletionTokens: 5,
		TotalTokens:      15,
		PromptTokenDetails:     schema.PromptTokenDetails{CachedTokens: 3},
		CompletionTokensDetails: schema.CompletionTokensDetails{ReasoningTokens: 2},
	})
	if u.PromptTokens != 10 || u.CompletionTokens != 5 || u.TotalTokens != 15 ||
		u.CachedTokens != 3 || u.ReasoningTokens != 2 {
		t.Fatalf("usage = %+v", u)
	}
}
```

- [ ] **Step 2: 运行确认失败**

```bash
env -u GOROOT go test ./internal/model/ -run Test -v
# 预期：编译失败（textOf/toAgenticMessages/fromSchemaUsage 未定义）
```

- [ ] **Step 3: 实现**
  - `model.go`：照抄 §4（`ToolSpec`/`Usage`/`ToolCallDelta`/`ModelEvent`/`Stream`/`Model`/`Config`）。`Stream` 的 `reader` 字段类型是 `*schema.StreamReader[*schema.AgenticMessage]`（见 §4，实现放 eino.go）。
  - `eino.go`：照抄 §5.2/5.3（`einoModel`/`Stream.Recv`/`Close`/`toAgenticMessages`/`textOf`/`fromSchemaUsage`/`New`）。注意 import `strings`（`textOf` 用到）。

- [ ] **Step 4: 运行确认通过**

```bash
env -u GOROOT go test ./internal/model/ -v
env -u GOROOT go build ./...
# 预期：test PASS + build 通过
```

---

## Task 3: 配置 + 冒烟 main（`cmd/agent`）

**Files:** Create `cmd/agent/config.go`, `cmd/agent/main.go`

**Consumes:** `model.New`/`model.Config`/`model.Model`（Task 2）
**Produces:** 可运行的 `cmd/agent`

- [ ] **Step 1: 写 `config.go`**：照抄 §6（`ModelProvider` 常量 + `modelConfig` + `config` + `loadConfig`，读 `./config.yaml`，校验 Models 非空且第一个含 APIKey）。

- [ ] **Step 2: 写 `main.go`**：照抄 §7（`loadConfig` → 组装 `model.Config` → `model.New` → `Stream` → 循环 `Recv` 打印 text/thinking → 打印 usage）。

- [ ] **Step 3: 构建**

```bash
env -u GOROOT go build ./...
# 预期：通过（此时旧的根 package main 与新 cmd/agent 并存，都编译）
```

---

## Task 4: 删除旧扁平文件 + 依赖清理 + 验收

**Files:** Delete 根 `main.go`/`model.go`/`handlers.go`/`messages.go`/`tui.go`/`markdown.go`/`config.go`

- [ ] **Step 1: 删除旧文件**

```bash
cd /Users/cyanyumu/Projects/GoProject/einoclaw-build
rm main.go model.go handlers.go messages.go tui.go markdown.go config.go
```

- [ ] **Step 2: 依赖清理**（移除不再用到的 adk/local、cozeloop、TUI 库）

```bash
env -u GOROOT go mod tidy
```

- [ ] **Step 3: 构建 + vet**

```bash
env -u GOROOT go build ./...
env -u GOROOT go vet ./...
# 预期：零错误
```

- [ ] **Step 4: 冒烟验收**

```bash
env -u GOROOT go run ./cmd/agent
# 预期：流式打印 "AI: ..." 一句回复 + "[tokens prompt=... completion=...]"
```

---

## 自检（写计划后核对）

- **spec 覆盖**：P0 的 4 项产出（message 包 / model 包 / config / 冒烟 main）→ Task 1/2/3/4 全部覆盖。
- **类型一致性**：`message.Message`（Task 1）被 `model` 的 `toAgenticMessages`/`Model.Stream`（Task 2）消费，签名一致；`model.Config{Provider,APIKey,BaseURL,Model}`（Task 2）被 `cmd/agent`（Task 3）组装，字段一致。
- **无占位符**：各 Task 的实现代码引用设计文档 §（已含完整代码），测试代码全量内联。
