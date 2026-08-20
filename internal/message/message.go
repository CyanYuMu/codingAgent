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
