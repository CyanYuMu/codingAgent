package context

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"einoclaw-build/internal/message"
	"einoclaw-build/internal/model"
	"einoclaw-build/internal/session"
)

// Summarizer 抽象摘要能力（生产用 model.Model，测试用假实现）。
type Summarizer interface {
	Summarize(ctx context.Context, msgs []message.Message) (string, error)
}

type summarizer = Summarizer

// modelSummarizer 用 model.Model 实现摘要。
type modelSummarizer struct {
	model model.Model
}

// NewModelSummarizer 构造基于 model.Model 的摘要器。
func NewModelSummarizer(m model.Model) Summarizer {
	return &modelSummarizer{model: m}
}

func (m *modelSummarizer) Summarize(ctx context.Context, msgs []message.Message) (string, error) {
	stream, err := m.model.Stream(ctx, summarizePrompt(msgs), nil)
	if err != nil {
		return "", err
	}
	defer stream.Close()

	var sb strings.Builder
	for {
		ev, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", err
		}
		sb.WriteString(ev.Text)
	}
	return sb.String(), nil
}

// Manager 是循环的真相源：从 session 重建输入、记录消息、超阈值压缩、溢出恢复。
// 它实现 agent.Context。
type Manager struct {
	mu         sync.Mutex // 保护 session 与前缀缓存：TUI 换会话与循环重建输入在不同 goroutine
	session    *session.Session
	summarizer Summarizer
	window     int
	keepRecent int
	lastPrompt int                                         // 最近一次 provider 报告的 prompt tokens（估算校准用）
	system     func(ctx context.Context) []message.Message // 系统提示 + 记忆块等前缀，由装配方注入

	// 前缀缓存：system() 里有记忆召回这类每次都可能变的内容，如果每轮都重算，
	// 提示词前缀就每轮都变——provider 的 prompt cache 全部失效，长会话里这是最大的隐性成本。
	// 因此只在「会话首轮 / 压缩后 / 换会话」这三个时刻刷新。
	sysCache []message.Message
	sysDirty bool
}

// New 构造 Manager。system 为 nil 时不注入前缀；summarizer 为 nil 时不压缩（Compact 恒返回 false）。
func New(s *session.Session, sum Summarizer, window, keepRecent int, system func(context.Context) []message.Message) *Manager {
	if system == nil {
		system = func(context.Context) []message.Message { return nil }
	}
	if keepRecent <= 0 {
		keepRecent = 16384
	}
	return &Manager{session: s, summarizer: sum, window: window, keepRecent: keepRecent, system: system, sysDirty: true}
}

// Session 返回当前会话。
func (m *Manager) Session() *session.Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.session
}

// SetSession 切换当前会话（多会话 /new /resume 时跟随），并让前缀失效。
func (m *Manager) SetSession(s *session.Session) {
	m.mu.Lock()
	m.session, m.sysDirty = s, true
	m.mu.Unlock()
}

// InvalidateSystem 让下一次 Build 重新计算系统前缀（记忆刷新、粘性规则重贴）。
func (m *Manager) InvalidateSystem() {
	m.mu.Lock()
	m.sysDirty = true
	m.mu.Unlock()
}

// systemPrefix 取（或重建）系统前缀。重建时**不持锁**：注入的 system() 闭包会回调 Session()。
func (m *Manager) systemPrefix(ctx context.Context) []message.Message {
	m.mu.Lock()
	cached, dirty := m.sysCache, m.sysDirty
	m.mu.Unlock()
	if !dirty {
		return cached
	}
	built := m.system(ctx)
	m.mu.Lock()
	m.sysCache, m.sysDirty = built, false
	m.mu.Unlock()
	return built
}

// threshold 预算阈值：window − reserve，reserve = max(15%·window, 16384)。
func (m *Manager) threshold() int {
	reserve := max(m.window*15/100, 16384)
	if reserve >= m.window {
		reserve = m.window / 2
	}
	return m.window - reserve
}

// Build 重建模型输入：system 前缀 + 会话回放（含压缩展开与悬空修复）。
func (m *Manager) Build(ctx context.Context) ([]message.Message, error) {
	hist, err := m.Session().Replay()
	if err != nil {
		return nil, err
	}
	prefix := m.systemPrefix(ctx)
	out := make([]message.Message, 0, len(prefix)+len(hist))
	out = append(out, prefix...)
	return append(out, hist...), nil
}

// Record 记录一条消息到会话（assistant 消息带用量）。
func (m *Manager) Record(msg message.Message, u model.Usage) error {
	return m.Session().AppendWithUsage(msg, u)
}

// ShouldCompact 判断上一次调用的 prompt 用量是否超阈值；同时记下真值供估算校准。
func (m *Manager) ShouldCompact(u model.Usage) bool {
	if u.PromptTokens > 0 {
		m.lastPrompt = u.PromptTokens
	}
	return u.PromptTokens > m.threshold()
}

// keepBudget 保留段预算（provider token）：不超过阈值的一半，否则小窗口下整段对话都"最近"，无可压内容。
func (m *Manager) keepBudget() int {
	return max(min(m.keepRecent, m.threshold()/2), 1)
}

// Compact 正常压缩：保留最近 keepBudget 的内容，更早的段落摘要化。返回是否发生了压缩。
func (m *Manager) Compact(ctx context.Context) (bool, error) { return m.compact(ctx, m.keepBudget()) }

// RecoverOverflow 溢出恢复：把保留量减半再压缩；仍无可压内容则只保留最后一段。
func (m *Manager) RecoverOverflow(ctx context.Context) (bool, error) {
	keep := max(m.keepBudget()/2, 512)
	did, err := m.compact(ctx, keep)
	if err != nil || did {
		return did, err
	}
	return m.compact(ctx, 1)
}

// calibrationMinEstimate 低于此估算量时不做比值校准（小样本下比值是噪声）。
const calibrationMinEstimate = 2000

// keepInEstimateUnits 把 provider token 预算换算成本地估算单位：
// 本地估算按 rune/2，中文/代码的真实 token 密度可能高数倍，用「上次 prompt 真值 / 本地估算总量」校准。
func (m *Manager) keepInEstimateUnits(keepProvider int, msgs []message.Message) int {
	est := 0
	for _, mm := range msgs {
		est += estimateTokens(mm)
	}
	if m.lastPrompt <= 0 || est < calibrationMinEstimate {
		return keepProvider
	}
	ratio := min(max(float64(m.lastPrompt)/float64(est), 0.25), 8)
	return max(int(float64(keepProvider)/ratio), 1)
}

// AfterTurn 兼容旧接口：若超阈值则压缩。
func (m *Manager) AfterTurn(ctx context.Context, usage model.Usage) error {
	if !m.ShouldCompact(usage) {
		return nil
	}
	_, err := m.Compact(ctx)
	return err
}

func (m *Manager) compact(ctx context.Context, keep int) (bool, error) {
	if m.summarizer == nil {
		return false, nil
	}
	sess := m.Session()
	msgs, err := sess.Replay()
	if err != nil {
		return false, err
	}
	cut := findCutPoint(msgs, m.keepInEstimateUnits(keep, msgs))
	if cut <= 0 {
		return false, nil
	}
	summary, err := m.summarizer.Summarize(ctx, msgs[:cut])
	if err != nil {
		return false, err
	}
	firstKept, err := m.entryIDOfMessageIndex(cut)
	if err != nil {
		return false, err
	}
	before := 0
	for _, mm := range msgs {
		before += estimateTokens(mm)
	}
	if err := sess.Compact(summary, firstKept, before); err != nil {
		return false, err
	}
	m.InvalidateSystem() // 压缩后重贴记忆与粘性规则：这是前缀允许变化的少数时刻之一
	return true, nil
}

// entryIDOfMessageIndex 把 Replay 下标映射回 session 条目 id（与 Replay 一一对应）。
func (m *Manager) entryIDOfMessageIndex(idx int) (string, error) {
	ids, err := m.Session().ContextEntryIDs()
	if err != nil {
		return "", err
	}
	if idx < 0 || idx >= len(ids) {
		return "", fmt.Errorf("cut index %d out of range %d", idx, len(ids))
	}
	return ids[idx], nil
}
