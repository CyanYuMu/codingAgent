// Package bus 是进程内的极简发布/订阅：一个 channel 名对应多个订阅者。
// 用途是把子 agent 的生命周期/进度/原始事件送到 UI 与 Manager，而不经过父 agent 的上下文。
// 语义取舍：投递永不阻塞发布者——订阅者缓冲满就丢这一条。总线只服务渲染与观测，
// 真相源始终是会话 JSONL，丢事件不影响正确性。
package bus

import (
	"sync"
	"time"
)

// Envelope 是一条发布出去的消息。
type Envelope struct {
	Channel string
	Payload any
	At      time.Time
}

// Bus 是订阅表。零值不可用，用 New。
type Bus struct {
	mu   sync.RWMutex
	subs map[string]map[int]chan Envelope
	seq  int
}

// New 构造总线。
func New() *Bus { return &Bus{subs: map[string]map[int]chan Envelope{}} }

// Publish 向一个 channel 的所有订阅者投递；缓冲满的订阅者会丢掉这一条。
// 持读锁期间不可能有人 close（注销要写锁），所以不会向已关闭通道发送。
func (b *Bus) Publish(channel string, payload any) {
	if b == nil {
		return // 未装配总线时（如单测）静默忽略，调用方不必判空
	}
	env := Envelope{Channel: channel, Payload: payload, At: time.Now()}
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, ch := range b.subs[channel] {
		select {
		case ch <- env:
		default:
		}
	}
}

// Subscribe 订阅一个 channel，返回只读通道与幂等的注销函数（注销时关闭通道）。
func (b *Bus) Subscribe(channel string, buf int) (<-chan Envelope, func()) {
	if buf <= 0 {
		buf = 1
	}
	ch := make(chan Envelope, buf)
	b.mu.Lock()
	id := b.seq
	b.seq++
	if b.subs[channel] == nil {
		b.subs[channel] = map[int]chan Envelope{}
	}
	b.subs[channel][id] = ch
	b.mu.Unlock()

	var once sync.Once
	return ch, func() {
		once.Do(func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			if m := b.subs[channel]; m != nil {
				delete(m, id)
				if len(m) == 0 {
					delete(b.subs, channel)
				}
			}
			close(ch)
		})
	}
}
