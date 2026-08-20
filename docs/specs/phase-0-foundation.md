# Phase 0 详细设计：地基 —— 消息类型 + 模型客户端封装

> 状态：**已修订（v2，修正 eino provider 版本兼容性）** · 所属 spec：[harness-rearchitecture.md](harness-rearchitecture.md)
> 本阶段只做「地基」：定义共享消息类型 + 把 eino 关进 `internal/model`。循环、session、TUI 都在后续阶段。

---

## 0. 目标与边界

### 本阶段产出（P0 完成时）

1. `internal/message` —— 共享消息类型包（零依赖，纯 stdlib）。
2. `internal/model` —— `Model` 接口 + eino 适配实现（**唯一 import eino 的包**）。
3. `cmd/agent/config.go` —— 配置（从根 `config.go` 迁移）。
4. `cmd/agent/main.go` —— 临时冒烟 main：读配置 → 建模型 → 流式打印一句话。

### 本阶段明确不做（defer）

- Agent 循环（P1）、Session/JSONL（P2）、上下文管理（P3）、工具/运行时/审批（P4）、记忆（P5）、subagent/MCP（P6）、trace/eval（P7）。
- TUI 与 markdown 渲染（P1 迁入 `internal/tui`，本阶段用 `fmt.Print` 冒烟即可）。
- 工具调用合并（P1 的循环负责按 `CallID` 合并；本阶段模型层只做**忠实透传**增量）。

### 验收标准

- `env -u GOROOT go build ./...` 与 `go vet ./...` 通过。
- `go run ./cmd/agent` 能流式打印一句模型回复 + 末尾 token 用量。

---

## 1. 参照 oh-my-pi 的设计（本阶段吸收的 3 个点）

### 1.1 内容块模型，而非单一字符串

oh-my-pi 的消息不是 `{role, content string}`，而是区分**正文 / 思考 / 工具调用 / 工具结果**四类内容块。这样流式渲染、工具循环、上下文记账才能各取所需。P0 先把这四类块的类型定义立起来。

### 1.2 流式增量的语义：delta vs 定稿

oh-my-pi 的事件把「流式增量」（`message_update`，只含 delta）和「定稿消息」（`message_end`，含完整内容）分开。**P0 的模型层只产出增量**（`ModelEvent` 就是 delta），「何时定稿」是 P1 循环的职责。这个边界是整个事件驱动架构的起点。

### 1.3 provider 的 usage 是 token 真值

oh-my-pi 明确：**provider 返回的 usage 是 token 记账的 ground truth**，本地 tokenizer 只用来估算「锚点之后的尾部」。P0 就把 `Usage` 类型立起来，并在冒烟里验证它真的能从 eino 流里回收——这是 P3 上下文记账的地基。

---

## 2. 为什么引入 `internal/message`（而非把类型塞进 agent）

spec 初版把 `Message/ContentBlock` 放在 `internal/agent`。这里有一个**隐式循环依赖**要现在解决：

```
P1 时：internal/agent(循环) 要 import internal/model(调 Model.Stream)
       而 internal/model 要 import internal/agent(拿 Message 类型)
       → import cycle ❌
```

解法：把**共享词汇**（消息/块/工具调用/工具结果/角色）下沉到零依赖的 `internal/message`，`agent` 和 `model` 都 import 它。这正好对应 eino 自己的 `schema` 包与 `components/model` 包分离——**类型词汇** 与 **会做 I/O 的模型客户端** 是两回事。

依赖方向（P0 后）：

```
internal/message        （零依赖，纯 stdlib）
     ▲        ▲
     │        │
internal/model ── internal/agent(P1)
（import eino）     （import model + message）
```

`internal/model` 是唯一 import eino 的包。`internal/agent` 永远看不到 eino。

---

## 3. 消息类型设计（`internal/message/message.go`）

```go
package message

// BlockKind 区分内容块类型。
type BlockKind int

const (
	BlockText       BlockKind = iota // 普通文本
	BlockThinking                    // 推理/思考过程
	BlockToolCall                    // 工具调用（assistant 发起）
	BlockToolResult                  // 工具结果（tool 角色）
)

// ToolCall 一次工具调用：名字 + JSON 参数 + 唯一 ID。
type ToolCall struct {
	ID   string
	Name string
	Args string // 原始 JSON 参数
}

// ToolResult 一次工具调用的结果。
type ToolResult struct {
	ToolCallID string
	Name       string
	Content    string // 文本结果
	IsError    bool
}

// ContentBlock 是消息的一个内容块。用 Kind + 对应字段表达；每块只填一种。
type ContentBlock struct {
	Kind       BlockKind
	Text       string      // BlockText
	Thinking   string      // BlockThinking
	ToolCall   *ToolCall   // BlockToolCall
	ToolResult *ToolResult // BlockToolResult
}

// Role 消息角色。
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message 一条消息（自建 struct，不依赖 eino schema）。
type Message struct {
	Role   Role
	Blocks []ContentBlock
}
```

### 构造器（便于使用）

```go
func NewSystemMessage(text string) Message {
	return Message{Role: RoleSystem, Blocks: []ContentBlock{{Kind: BlockText, Text: text}}}
}
func NewUserMessage(text string) Message {
	return Message{Role: RoleUser, Blocks: []ContentBlock{{Kind: BlockText, Text: text}}}
}
func NewToolMessage(callID, name, content string, isErr bool) Message {
	return Message{Role: RoleTool, Blocks: []ContentBlock{{
		Kind:       BlockToolResult,
		ToolResult: &ToolResult{ToolCallID: callID, Name: name, Content: content, IsError: isErr},
	}}}
}
```

### 映射约定（写进契约，P1 起生效）

- `RoleTool` 的消息**恰好一个** `BlockToolResult` 块（eino 里每个工具结果是一条独立 `AgenticMessage`）。
- `RoleAssistant` 的消息可混排 `BlockText` / `BlockThinking` / `BlockToolCall` 多块。
- 文本块在映射到 eino 时拼接成文本内容（P0 冒烟只含单文本块，多块拼接 P1 再细化）。

---

## 4. 模型客户端抽象（`internal/model/model.go`）

```go
package model

import (
	"context"
	"io"

	"einoclaw-build/internal/message"
)

// ToolSpec 描述一个可供模型调用的工具（模型视角，不含执行逻辑）。
type ToolSpec struct {
	Name        string
	Description string
	Parameters  map[string]any // JSON Schema 的 properties 部分
}

// Usage 一次模型调用的 token 用量（provider 真值，P3 记账基石）。
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	ReasoningTokens  int // 思考/推理 token
	CachedTokens     int // 提示词缓存命中
}

// ToolCallDelta 流式下工具调用的一个增量片段。
// 一次完整工具调用 = 多个同 CallID 的 delta 按序拼接（P1 循环负责合并）。
type ToolCallDelta struct {
	CallID string
	Name   string // 函数名
	Args   string // 参数 JSON 片段（分块到达）
}

// ModelEvent 模型流式输出的一个增量事件。
// 通常一次只有一个字段非空：要么正文、要么思考、要么工具调用增量。
type ModelEvent struct {
	Text      string
	Thinking  string
	ToolCalls []ToolCallDelta
}

// Stream 是一次流式调用的事件流。Recv 直到 io.EOF。
type Stream struct {
	reader *schema.StreamReader[*schema.AgenticMessage] // 内部持有 eino reader（见 eino.go）
	usage  Usage
}

// Recv 返回下一个增量事件；io.EOF 表示流结束。结束后可读 Usage()。
func (s *Stream) Recv() (ModelEvent, error) {
	// 实现见 §5.2
}

// Usage 返回本次调用的用量（流结束后才完整）。
func (s *Stream) Usage() Usage { return s.usage }

// Close 释放底层 reader（必须 defer 调用）。
func (s *Stream) Close() {}

// Model 是模型客户端抽象 —— 唯一的 eino 依赖点。
type Model interface {
	Stream(ctx context.Context, msgs []message.Message, tools []ToolSpec) (*Stream, error)
}
```

### 设计要点

1. **`Stream` 采用 `Recv() (ModelEvent, error)` + `io.EOF` 哨兵**——和 eino 的 `StreamReader.Recv()` 同构。
2. **`Usage` 从 `Stream.Usage()` 回收，而不是作为 `Stream` 的参数**——因为用量只在流结束（最后一帧）才完整，天然适合「结束后读」。
3. **`ToolSpec.Parameters` 用 `map[string]any` 表达 JSON Schema**——P0 只立类型，P4 才真正填。模型层不 import 工具包，避免反向依赖。

---

## 5. eino 适配实现（`internal/model/eino.go`）

### 5.0 ⚠️ 版本兼容性核实（开发前必须知道）

写本设计时查了依赖链，发现一个**关键事实**，直接决定了适配层的落点：

- 非 agentic 的 `eino-ext/components/model/{openai,qwen,ark,deepseek}@v0.1.x` **全部 `require eino v0.7.13`**（老 API 时代），与本项目 `eino v0.10.0-alpha.9` 不兼容。
- agentic provider 包（`agenticopenai@v0.2.2` 等）**不是**「包装非 agentic 包」，而是直接基于 OpenAI 官方 SDK + `eino-ext/libs/acl/openai` 实现，`require eino v0.9.5`，经 MVS 解析到 `v0.10.0-alpha.9`——**兼容**。

结论：**必须用 agentic provider 包拿到 `model.AgenticModel`（`= BaseModel[*schema.AgenticMessage]`），直接调 `.Stream()` 自组流式**。「下沉到 components/model 自组流式」的正确含义 = 绕过 `adk.TurnLoop`/`NewTypedChatModelAgent`，直接拿裸的 `components/model` 接口自己组装增量，而不是去拿 `*schema.Message` 维度的非 agentic 包。

### 5.1 已验证的 eino API（本设计依据，开发时不必再查）

```go
// github.com/cloudwego/eino/components/model
type AgenticModel = BaseModel[*schema.AgenticMessage]
type BaseModel[M] interface {
	Generate(ctx context.Context, input []M, opts ...Option) (M, error)
	Stream(ctx context.Context, input []M, opts ...Option) (*schema.StreamReader[M], error)
}
func WithTools(tools []*schema.ToolInfo) Option  // AgenticModel 在请求时传工具

// github.com/cloudwego/eino/schema
type StreamReader[T any] struct{ ... }
func (sr *StreamReader[T]) Recv() (T, error)   // io.EOF 结束
func (sr *StreamReader[T]) Close()

type AgenticMessage struct {
	Role          AgenticRoleType
	ContentBlocks []*ContentBlock
	ResponseMeta  *AgenticResponseMeta   // TokenUsage 在这里
}
type ContentBlock struct {
	Reasoning          *Reasoning          // 思考块
	AssistantGenText   *AssistantGenText   // 正文块
	FunctionToolCall   *FunctionToolCall   // 工具调用块
	FunctionToolResult *FunctionToolResult // 工具结果块
	// 还有 UserInputText / 多模态等，P0 用不到
}
type FunctionToolCall struct { CallID, Name, Arguments string } // 合并键是 CallID，无 Index
type Reasoning struct { Text string }
type AssistantGenText struct { Text string }
type AgenticResponseMeta struct { TokenUsage *TokenUsage }
type TokenUsage struct {
	PromptTokens, CompletionTokens, TotalTokens int
	PromptTokenDetails PromptTokenDetails            // CachedTokens
	CompletionTokensDetails CompletionTokensDetails  // ReasoningTokens
}
func SystemAgenticMessage(text string) *AgenticMessage
func UserAgenticMessage(text string) *AgenticMessage
func ConcatAgenticMessages(msgs []*AgenticMessage) (*AgenticMessage, error) // P1 合并工具调用用
```

四个 agentic provider 构造（均已核实，返回 `model.AgenticModel`）：

```go
agenticqwen.New(ctx,      &agenticqwen.Config{APIKey, Model, BaseURL, EnableThinking})
agenticopenai.NewResponsesModel(ctx, &agenticopenai.ResponsesConfig{APIKey, Model, BaseURL, EnableAutoCache})
agenticark.New(ctx,       &agenticark.Config{APIKey, Model, BaseURL, EnableAutoCache})
agenticdeepseek.New(ctx,  &agenticdeepseek.Config{APIKey, Model, BaseURL})
```

### 5.2 核心实现：包装 + 流式装配

```go
package model

import (
	"context"
	"io"

	cmodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"einoclaw-build/internal/message"
)

// einoModel 用 eino 的 components/model（AgenticModel 维度）实现 Model。
type einoModel struct {
	base cmodel.AgenticModel // = BaseModel[*schema.AgenticMessage]
}

func (m *einoModel) Stream(ctx context.Context, msgs []message.Message, tools []ToolSpec) (*Stream, error) {
	agenticMsgs := toAgenticMessages(msgs)

	var opts []cmodel.Option
	if len(tools) > 0 {
		opts = append(opts, cmodel.WithTools(toSchemaTools(tools)))
	}
	reader, err := m.base.Stream(ctx, agenticMsgs, opts...)
	if err != nil {
		return nil, err
	}
	return &Stream{reader: reader}, nil
}

func (s *Stream) Recv() (ModelEvent, error) {
	chunk, err := s.reader.Recv()
	if err != nil {
		return ModelEvent{}, err // io.EOF 或其它错误
	}
	if chunk.ResponseMeta != nil && chunk.ResponseMeta.TokenUsage != nil {
		s.usage = fromSchemaUsage(chunk.ResponseMeta.TokenUsage)
	}
	var ev ModelEvent
	for _, b := range chunk.ContentBlocks {
		if b.Reasoning != nil && b.Reasoning.Text != "" {
			ev.Thinking += b.Reasoning.Text
		}
		if b.AssistantGenText != nil && b.AssistantGenText.Text != "" {
			ev.Text += b.AssistantGenText.Text
		}
		if b.FunctionToolCall != nil {
			ev.ToolCalls = append(ev.ToolCalls, ToolCallDelta{
				CallID: b.FunctionToolCall.CallID,
				Name:   b.FunctionToolCall.Name,
				Args:   b.FunctionToolCall.Arguments,
			})
		}
	}
	return ev, nil
}

func (s *Stream) Close() { s.reader.Close() }
```

**转换函数**（P0 只写输入侧的 system/user；assistant/tool 的构造 P1 历史回放再加）：

```go
// toAgenticMessages 把我们的消息转成 eino 的 AgenticMessage。
// P0 只处理 system/user（冒烟仅需这两类）；assistant/tool 在 P1 加。
func toAgenticMessages(msgs []message.Message) []*schema.AgenticMessage {
	out := make([]*schema.AgenticMessage, 0, len(msgs))
	for _, m := range msgs {
		switch m.Role {
		case message.RoleSystem:
			out = append(out, schema.SystemAgenticMessage(textOf(m)))
		case message.RoleUser:
			out = append(out, schema.UserAgenticMessage(textOf(m)))
		}
	}
	return out
}

// textOf 拼接消息里的所有文本块。
func textOf(m message.Message) string {
	var sb strings.Builder
	for _, b := range m.Blocks {
		if b.Kind == message.BlockText {
			sb.WriteString(b.Text)
		}
	}
	return sb.String()
}

func fromSchemaUsage(u *schema.TokenUsage) Usage {
	return Usage{
		PromptTokens:     u.PromptTokens,
		CompletionTokens: u.CompletionTokens,
		TotalTokens:      u.TotalTokens,
		ReasoningTokens:  u.CompletionTokensDetails.ReasoningTokens,
		CachedTokens:     u.PromptTokenDetails.CachedTokens,
	}
}
```

> `toSchemaTools` 本阶段**不写**（YAGNI）：P0 冒烟不传工具，不经过该路径；等 P4 真正做工具时再加并补测试。

### 5.3 provider 装配（`New`）

```go
// Config 描述要构建的模型。
type Config struct {
	Provider string // qwen | openai | ark | deepseek
	APIKey   string
	BaseURL  string
	Model    string // 模型 ID，如 "deepseek-chat" / "qwen-plus" / "gpt-4o"
}

// New 根据 Config 构建 Model。返回的 Model 内部持有 AgenticMessage 维度的底层模型。
func New(ctx context.Context, cfg Config) (Model, error) {
	switch cfg.Provider {
	case "qwen":
		mm, err := agenticqwen.New(ctx, &agenticqwen.Config{APIKey: cfg.APIKey, Model: cfg.Model, BaseURL: cfg.BaseURL})
		if err != nil { return nil, err }
		return &einoModel{base: mm}, nil
	case "openai":
		mm, err := agenticopenai.NewResponsesModel(ctx, &agenticopenai.ResponsesConfig{APIKey: cfg.APIKey, Model: cfg.Model, BaseURL: cfg.BaseURL, EnableAutoCache: true})
		if err != nil { return nil, err }
		return &einoModel{base: mm}, nil
	case "ark":
		mm, err := agenticark.New(ctx, &agenticark.Config{APIKey: cfg.APIKey, Model: cfg.Model, BaseURL: cfg.BaseURL, EnableAutoCache: true})
		if err != nil { return nil, err }
		return &einoModel{base: mm}, nil
	case "deepseek":
		mm, err := agenticdeepseek.New(ctx, &agenticdeepseek.Config{APIKey: cfg.APIKey, Model: cfg.Model, BaseURL: cfg.BaseURL})
		if err != nil { return nil, err }
		return &einoModel{base: mm}, nil
	default:
		return nil, fmt.Errorf("unknown provider %q", cfg.Provider)
	}
}
```

> 各 provider 的 `Config` 字段名以 `go doc` 为准（上述已从现有 `model.go` 核实：qwen 有 `EnableThinking`、openai/ark 有 `EnableAutoCache`）。P0 先不暴露 `EnableThinking`/`EnableAutoCache` 到配置，用默认值；后续需要时再透传。

---

## 6. 配置迁移（`cmd/agent/config.go`）

沿用现有根 `config.go` 的字段，仅搬到 `cmd/agent`（package main）：

```go
package main

// ModelProvider 标识模型服务商。
type ModelProvider string

const (
	ModelProviderQwen     ModelProvider = "qwen"
	ModelProviderOpenAI   ModelProvider = "openai"
	ModelProviderArk      ModelProvider = "ark"
	ModelProviderDeepseek ModelProvider = "deepseek"
)

type modelConfig struct {
	APIKey         string        `yaml:"api_key"`
	BaseURL        string        `yaml:"base_url"`
	Provider       ModelProvider `yaml:"provider"`
	ModelName      string        `yaml:"model_name"` // 展示名，暂未用
	ModelID        string        `yaml:"model_id"`   // 传给模型客户端的 ID
	EnableThinking bool          `yaml:"enable_thinking"`
}

type config struct {
	Models []modelConfig `yaml:"models"`
}

func loadConfig() config {
	// 读 ./config.yaml → yaml.Unmarshal → 校验 Models 非空、第一个含 APIKey。
	// 与现有根 config.go 逻辑一致。
}
```

> `config.yaml` / `example.yaml` 已在项目里，字段不变（`models[].provider/api_key/base_url/model_id/model_name/enable_thinking`）。本阶段不新增字段。

---

## 7. 冒烟 main（`cmd/agent/main.go`）

```go
package main

func main() {
	cfg := loadConfig()
	mc := model.Config{
		Provider: string(cfg.Models[0].Provider),
		APIKey:   cfg.Models[0].APIKey,
		BaseURL:  cfg.Models[0].BaseURL,
		Model:    cfg.Models[0].ModelID,
	}
	m, err := model.New(context.Background(), mc)
	if err != nil { log.Fatal(err) }

	msgs := []message.Message{message.NewUserMessage("用一句话解释什么是 goroutine")}
	stream, err := m.Stream(context.Background(), msgs, nil)
	if err != nil { log.Fatal(err) }
	defer stream.Close()

	fmt.Print("AI: ")
	for {
		ev, err := stream.Recv()
		if errors.Is(err, io.EOF) { break }
		if err != nil { log.Fatal(err) }
		if ev.Text != "" { fmt.Print(ev.Text) }
		if ev.Thinking != "" { fmt.Print("\n[思考] " + ev.Thinking) }
	}
	u := stream.Usage()
	fmt.Printf("\n\n[tokens prompt=%d completion=%d]\n", u.PromptTokens, u.CompletionTokens)
}
```

---

## 8. 边界情况与错误处理

| 情况 | 处理 |
|---|---|
| 流中途出错（`Recv` 返回非 `io.EOF` 错误） | 直接向上抛给调用方（P0 冒烟 `log.Fatal`；P1 循环转为 `Error` 事件） |
| 用量 `ResponseMeta` 或 `TokenUsage` 为 nil | `Recv` 里判 nil 才赋值，`Usage()` 返回零值 Usage——不 panic |
| `provider` 未知 | `New` 返回 `fmt.Errorf("unknown provider %q")` |
| 配置文件缺失 / 无 APIKey | 沿用现有提示并 `os.Exit(0)` |
| 工具调用增量 | P0 透传 `ToolCallDelta`（含 CallID），**不合并**——合并是 P1 循环职责 |

---

## 9. 本阶段对外契约（后续阶段依赖这些名字）

| 类型/函数 | 包 | 后续谁依赖 |
|---|---|---|
| `message.Message` / `ContentBlock` / `Role` / `ToolCall` / `ToolResult` | `internal/message` | agent/session/context/memory/tool 全部 |
| `model.Model`（`Stream` 方法） | `internal/model` | `internal/agent`（P1 循环） |
| `model.Stream.Recv/Usage/Close` | `internal/model` | `internal/agent` |
| `model.Usage` | `internal/model` | `internal/context`（P3 记账） |
| `model.ToolSpec` | `internal/model` | `internal/tool`（P4 注册表） |
| `model.New(ctx, Config)` | `internal/model` | `cmd/agent` |

---

## 10. 待评审点（已确认 + 本次修订新增）

已确认（上一轮评审）：① `internal/message` 包名；② `ModelEvent.ToolCalls` 用切片；③ P0 不写 `toSchemaTools`（YAGNI）。

本次修订新增，请你留意：

4. **eino 维度从 `*schema.Message` 换成 `*schema.AgenticMessage`**：因为非 agentic provider 包锁死 eino v0.7.13 不兼容。这意味着 `internal/model` 内部用的是 `AgenticMessage`（含 ContentBlocks），与我们自建的 `message.Message` 是**两个不同概念**——前者是 eino 的模型消息，后者是我们的业务词汇。转换发生在 `internal/model` 边界。是否接受？
5. **`ToolCallDelta` 合并键从 `Index` 改为 `CallID`**：AgenticMessage 的 `FunctionToolCall` 只有 `CallID`（无 Index）。是否 OK？
