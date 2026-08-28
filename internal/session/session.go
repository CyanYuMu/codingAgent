package session

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"sync"
	"time"

	"einoclaw-build/internal/message"
	"einoclaw-build/internal/model"
)

// Session 在 Storage 之上加语义：追加式日志 + 可变 leaf 指针；reset 封存、compaction 替换、fork 分支。
// 条目一旦写入不可变；可变状态只有 leaf（以及内存镜像 entries）。
type Session struct {
	mu      sync.Mutex
	header  Header
	storage Storage
	leafID  string
	entries []Entry // 内存镜像：Open 时从存储加载，之后随 append 增长
}

func newID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// New 创建会话（只有 id 的 header）。
func New(id string, st Storage) (*Session, error) {
	return NewWithHeader(Header{ID: id}, st)
}

// NewWithHeader 创建会话并写入完整 header。
func NewWithHeader(h Header, st Storage) (*Session, error) {
	e := Entry{
		Type: EntrySession, Version: CurrentVersion, ID: h.ID, SessionID: h.ID,
		CWD: h.CWD, Title: h.Title, ParentSession: h.ParentSession, Model: h.Model,
		Timestamp: time.Now().Format(time.RFC3339Nano),
	}
	if err := st.Append(e); err != nil {
		return nil, err
	}
	return &Session{header: h, storage: st, entries: []Entry{e}}, nil
}

// Open 打开已有存储：读全部条目、重建 header 与 leaf。
// v1 条目（无 id）按文件顺序在内存里赋临时 id 串成线性链；空存储等价于新建 "default"。
func Open(st Storage) (*Session, error) {
	entries, err := st.Entries()
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return New("default", st)
	}
	s := &Session{storage: st}
	prev := ""
	for i := range entries {
		e := &entries[i]
		if e.Type == EntrySession {
			s.header = Header{ID: firstNonEmpty(e.SessionID, e.ID), CWD: e.CWD, Title: e.Title, ParentSession: e.ParentSession, Model: e.Model}
			continue
		}
		if e.ID == "" { // v1 兼容
			e.ID = "v1-" + newID()
			e.ParentID = prev
		}
		if e.Type == EntryTitle {
			s.header.Title = e.Title
		}
		prev = e.ID
	}
	if s.header.ID == "" {
		s.header.ID = "default"
	}
	s.entries = entries
	s.leafID = prev
	return s, nil
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// Header 返回会话头视图（含最新标题）。
func (s *Session) Header() Header {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.header
}

// LastEntryID 返回 leaf id。
func (s *Session) LastEntryID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.leafID
}

func (s *Session) appendLocked(e Entry) error {
	e.ID = newID()
	e.ParentID = s.leafID
	e.Timestamp = time.Now().Format(time.RFC3339Nano)
	if err := s.storage.Append(e); err != nil {
		return err
	}
	s.entries = append(s.entries, e)
	s.leafID = e.ID
	return nil
}

// Append 记录一条消息（user/assistant/tool）。
func (s *Session) Append(m message.Message) error { return s.AppendWithUsage(m, model.Usage{}) }

// AppendWithUsage 记录一条消息并附带用量（assistant 消息用，供 trace 聚合）。
func (s *Session) AppendWithUsage(m message.Message, u model.Usage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.appendLocked(Entry{Type: EntryMessage, Message: &m, Usage: u})
}

// AppendInit 记录子 agent 的任务与约束（首条）。
func (s *Session) AppendInit(init SessionInit) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.appendLocked(Entry{Type: EntryInit, Init: &init})
}

// AppendCustom 记录一条非 LLM 状态（tool_execution_start / session_exit …）。
func (s *Session) AppendCustom(customType string, data any) error {
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.appendLocked(Entry{Type: EntryCustom, CustomType: customType, Data: b})
}

// SetTitle 追加一条标题变更并更新 header 视图。
func (s *Session) SetTitle(title string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.header.Title = title
	return s.appendLocked(Entry{Type: EntryTitle, Title: title})
}

// Entries 返回原始条目副本（含用量，供 trace 用）。
func (s *Session) Entries() ([]Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Entry, len(s.entries))
	copy(out, s.entries)
	return out, nil
}

// Reset 写一条 reset_boundary，封存之前的历史。
func (s *Session) Reset() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.appendLocked(Entry{Type: EntryReset})
}

// Replay 返回 leaf 路径上的模型上下文（见 buildContext）。
func (s *Session) Replay() ([]message.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cms := buildContext(pathToLeaf(s.entries, s.leafID))
	out := make([]message.Message, len(cms))
	for i, cm := range cms {
		out[i] = cm.msg
	}
	return out, nil
}

// ContextEntryIDs 返回与 Replay() 一一对应的条目 id（摘要对应 compaction 条目；合成修复消息沿用所属 assistant 条目）。
// context.Manager 用它把压缩切点映射回 FirstKeptEntryID。
func (s *Session) ContextEntryIDs() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cms := buildContext(pathToLeaf(s.entries, s.leafID))
	out := make([]string, len(cms))
	for i, cm := range cms {
		out[i] = cm.id
	}
	return out, nil
}

// Compact 追加一条 compaction：摘要 + 保留起点；不再重追加保留消息。
func (s *Session) Compact(summary, firstKeptID string, tokensBefore int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.appendLocked(Entry{Type: EntryCompaction, Compaction: &Compaction{Summary: summary, FirstKeptEntryID: firstKeptID, TokensBefore: tokensBefore}})
}

// Fork 复制条目（保留 id 链）到新存储，header 带 ParentSession；父子此后互不影响。
func (s *Session) Fork(newID string, st Storage) (*Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	child, err := NewWithHeader(Header{ID: newID, CWD: s.header.CWD, ParentSession: s.header.ID, Model: s.header.Model}, st)
	if err != nil {
		return nil, err
	}
	for _, e := range s.entries {
		if e.Type == EntrySession {
			continue
		}
		if err := child.storage.Append(e); err != nil {
			return nil, err
		}
		child.entries = append(child.entries, e)
		child.leafID = e.ID
	}
	return child, nil
}

// Close 关闭底层存储（FileStorage 关文件）。
func (s *Session) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.storage.Close()
}
