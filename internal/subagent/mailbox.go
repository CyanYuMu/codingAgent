package subagent

import (
	"sync"
	"time"
)

// Mail 是一条 peer 之间的消息。只用来协调（确认接口、说进度、问一句），
// 长内容走 agent:// / artifact:// / 文件路径，不塞进消息里。
type Mail struct {
	From    string
	Text    string
	ReplyTo string
	At      time.Time
}

// mailbox 是一个收件人的信箱。运行中的子 agent 收到消息是直接注入它的 steering 通道的，
// 信箱只服务两种情况：主 agent（没有 Run，靠 TUI/headless 取件）与还没被取走的历史消息。
type mailbox struct {
	mu   sync.Mutex
	msgs []Mail
	wake chan struct{} // 缓冲 1：给 hub wait 用的「有新消息」信号
}

func newMailbox() *mailbox { return &mailbox{wake: make(chan struct{}, 1)} }

func (b *mailbox) push(m Mail) {
	b.mu.Lock()
	b.msgs = append(b.msgs, m)
	b.mu.Unlock()
	select {
	case b.wake <- struct{}{}:
	default: // 已经有未消费的信号，不必再叠
	}
}

// drain 取走全部消息（一次性）。
func (b *mailbox) drain() []Mail {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := b.msgs
	b.msgs = nil
	return out
}

func (b *mailbox) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.msgs)
}

func (b *mailbox) waitCh() <-chan struct{} { return b.wake }

// MainName 是主 agent 在名册与 hub 里的固定地址。
const MainName = "Main"

// box 取（或创建）一个收件人的信箱。
func (m *Manager) box(name string) *mailbox {
	m.mu.Lock()
	defer m.mu.Unlock()
	if b, ok := m.boxes[name]; ok {
		return b
	}
	b := newMailbox()
	m.boxes[name] = b
	return b
}

// TakeMainInbox 取走发给主 agent 的消息（TUI/headless 负责把它们注入主会话）。
func (m *Manager) TakeMainInbox() []Mail { return m.box(MainName).drain() }

// MainInboxWait 返回主信箱的「有新消息」信号（headless 等待用）。
func (m *Manager) MainInboxWait() <-chan struct{} { return m.box(MainName).waitCh() }
