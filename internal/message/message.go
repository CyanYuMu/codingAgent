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
	ID   string `json:"id"`
	Name string `json:"name"`
	Args string `json:"args"`
}

// ToolResult 一次工具调用的结果。
type ToolResult struct {
	ToolCallID string `json:"tool_call_id"`
	Name       string `json:"name"`
	Content    string `json:"content"`
	IsError    bool   `json:"is_error"`
}

// ContentBlock 是消息的一个内容块。用 Kind + 对应字段表达；每块只填一种。
type ContentBlock struct {
	Kind       BlockKind   `json:"kind"`
	Text       string      `json:"text,omitempty"`        // BlockText
	Thinking   string      `json:"thinking,omitempty"`    // BlockThinking
	ToolCall   *ToolCall   `json:"tool_call,omitempty"`   // BlockToolCall
	ToolResult *ToolResult `json:"tool_result,omitempty"` // BlockToolResult
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
	Role   Role           `json:"role"`
	Blocks []ContentBlock `json:"blocks"`
}

// NewSystemMessage 构造一条 system 消息。
func NewSystemMessage(text string) Message {
	return Message{Role: RoleSystem, Blocks: []ContentBlock{{Kind: BlockText, Text: text}}}
}

// NewUserMessage 构造一条 user 消息。
func NewUserMessage(text string) Message {
	return Message{Role: RoleUser, Blocks: []ContentBlock{{Kind: BlockText, Text: text}}}
}

// NewToolMessage 构造一条 tool 消息（恰好一个工具结果块）。
func NewToolMessage(callID, name, content string, isErr bool) Message {
	return Message{Role: RoleTool, Blocks: []ContentBlock{{
		Kind:       BlockToolResult,
		ToolResult: &ToolResult{ToolCallID: callID, Name: name, Content: content, IsError: isErr},
	}}}
}
