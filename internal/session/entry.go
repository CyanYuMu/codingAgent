package session

import (
	"encoding/json"

	"einoclaw-build/internal/message"
	"einoclaw-build/internal/model"
)

// EntryType 区分 JSONL 一行的类型。
type EntryType string

const (
	EntrySession    EntryType = "session"        // 会话头（版本 + id + cwd + 标题 + 父会话）
	EntryMessage    EntryType = "message"        // 一条消息（user/assistant/tool）
	EntryReset      EntryType = "reset_boundary" // /clear 封存标记
	EntryCompaction EntryType = "compaction"     // 上下文压缩摘要 + 保留起点
	EntryInit       EntryType = "session_init"   // 子 agent 首条：记录任务与约束
	EntryCustom     EntryType = "custom"         // 非 LLM 状态（tool_execution_start / session_exit …）
	EntryTitle      EntryType = "title_change"   // 标题变更（追加式审计）
)

// CurrentVersion 当前会话文件格式版本。v2：条目带 id/parentId/ts，compaction 用 FirstKeptEntryID。
const CurrentVersion = 2

// Entry 是 JSONL 里的一行。用 Type 区分，Type 对应的字段才有值。
type Entry struct {
	Type      EntryType `json:"type"`
	ID        string    `json:"id,omitempty"`       // 8 hex；header 也带 id（= 会话 id）
	ParentID  string    `json:"parentId,omitempty"` // 追加时 = 当前 leaf；根条目为空
	Timestamp string    `json:"ts,omitempty"`       // RFC3339Nano

	// EntrySession（header）
	Version       int    `json:"version,omitempty"`
	SessionID     string `json:"sessionId,omitempty"`
	CWD           string `json:"cwd,omitempty"`
	Title         string `json:"title,omitempty"` // header 初始标题；EntryTitle 的新标题
	ParentSession string `json:"parentSession,omitempty"`
	Model         string `json:"model,omitempty"`

	// EntryMessage
	Message *message.Message `json:"message,omitempty"`
	Usage   model.Usage      `json:"usage,omitzero"` // assistant 消息的本轮用量（供 trace 聚合）

	// EntryCompaction
	Compaction *Compaction `json:"compaction,omitempty"`

	// EntryInit
	Init *SessionInit `json:"init,omitempty"`

	// EntryCustom
	CustomType string          `json:"customType,omitempty"`
	Data       json.RawMessage `json:"data,omitempty"`
}

// Compaction 记录一次上下文压缩：摘要 + 保留段起点（v1 无起点：保留段被重追加在其后）。
type Compaction struct {
	Summary          string   `json:"summary"`
	FirstKeptEntryID string   `json:"firstKeptEntryId,omitempty"`
	TokensBefore     int      `json:"tokensBefore,omitempty"`
	Files            []string `json:"files,omitempty"` // 兼容 v1 字段
}

// SessionInit 是子 agent 会话的首条记录：它被派来做什么、带着什么约束。
type SessionInit struct {
	Agent            string         `json:"agent"`
	SystemPrompt     string         `json:"systemPrompt,omitempty"`
	Task             string         `json:"task"`
	Tools            []string       `json:"tools,omitempty"`
	OutputSchema     map[string]any `json:"outputSchema,omitempty"`
	Depth            int            `json:"depth"`
	ParentToolCallID string         `json:"parentToolCallId,omitempty"`
}

// Header 是会话头的稳定视图。
type Header struct {
	ID, CWD, Title, ParentSession, Model string
}
