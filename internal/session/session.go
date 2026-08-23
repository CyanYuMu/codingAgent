package session

import (
	"sync"

	"einoclaw-build/internal/message"
	"einoclaw-build/internal/model"
)

// Session 在 Storage 之上加语义：reset 封存、replay 重建、fork 分支。
type Session struct {
	mu      sync.Mutex
	id      string
	storage Storage
}

// New 创建会话并写 header（version=1, id）。
func New(id string, st Storage) (*Session, error) {
	if err := st.Append(Entry{Type: EntrySession, Version: 1, ID: id}); err != nil {
		return nil, err
	}
	return &Session{id: id, storage: st}, nil
}

// Append 记录一条消息（user/assistant/tool）。
func (s *Session) Append(m message.Message) error {
	return s.AppendWithUsage(m, model.Usage{})
}

// AppendWithUsage 记录一条消息并附带用量（assistant 消息用，供 trace 聚合）。
func (s *Session) AppendWithUsage(m message.Message, u model.Usage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.storage.Append(Entry{Type: EntryMessage, Message: &m, Usage: u})
}

// Entries 返回原始条目（含用量，供 trace 用）。
func (s *Session) Entries() ([]Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.storage.Entries()
}

// Reset 写一条 reset_boundary，封存之前的历史。
func (s *Session) Reset() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.storage.Append(Entry{Type: EntryReset})
}

// Replay 重放日志，返回最后一个 reset_boundary 之后的消息。
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
			msgs = msgs[:0] // 封存：清空之前累积
		case EntryCompaction:
			// 压缩条目：把之前累积的消息替换成 [摘要]
			if e.Compaction != nil {
				msgs = []message.Message{message.NewUserMessage(e.Compaction.Summary)}
			}
		}
		// EntrySession（header）忽略
	}
	return msgs, nil
}

// Fork 快照出一个新会话：复制当前消息到新 id 的会话（新 storage 由调用方提供）。
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

// Compact 原子地写一条 compaction 条目，然后重追加保留的消息。
// 保留的消息在日志里出现两次（压缩前 + 重追加），换来无需索引追踪的简单实现。
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

// Close 关闭底层存储（FileStorage 关文件）。
func (s *Session) Close() error { return s.storage.Close() }
