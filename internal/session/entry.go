package session

import (
	"einoclaw-build/internal/message"
	"einoclaw-build/internal/model"
)

// EntryType 区分 JSONL 一行的类型。
type EntryType string

const (
	EntrySession    EntryType = "session"        // 会话头（版本 + id）
	EntryMessage    EntryType = "message"        // 一条消息（user/assistant/tool）
	EntryReset      EntryType = "reset_boundary" // /clear 封存标记
	EntryCompaction EntryType = "compaction"     // 上下文压缩摘要
)

// Entry 是 JSONL 里的一行。用 Type 区分，Type 对应的字段才有值。
type Entry struct {
	Type       EntryType        `json:"type"`
	Version    int              `json:"version,omitempty"`    // EntrySession: 格式版本
	ID         string           `json:"id,omitempty"`         // EntrySession: 会话 id
	Message    *message.Message `json:"message,omitempty"`    // EntryMessage: 消息内容
	Compaction *Compaction      `json:"compaction,omitempty"` // EntryCompaction: 摘要
	Usage      model.Usage      `json:"usage,omitzero"`       // EntryMessage(assistant): 本轮用量
}

// Compaction 记录一次上下文压缩：把更早的消息浓缩成一段摘要。
type Compaction struct {
	Summary string   `json:"summary"`
	Files   []string `json:"files,omitempty"` // P4 确定性追踪的文件/产物，现在留空
}
