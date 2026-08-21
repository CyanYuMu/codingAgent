package context

import (
	"context"
	"errors"
	"io"
	"strings"

	"einoclaw-build/internal/message"
	"einoclaw-build/internal/model"
	"einoclaw-build/internal/session"
)

// summarizer 抽象摘要能力（生产用 model.Model，测试用假实现）。
type summarizer interface {
	Summarize(ctx context.Context, msgs []message.Message) (string, error)
}

// modelSummarizer 用 model.Model 实现摘要。
type modelSummarizer struct {
	model model.Model
}

// NewModelSummarizer 构造基于 model.Model 的摘要器。
func NewModelSummarizer(m model.Model) summarizer {
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

// ContextManager 管理上下文预算：超阈值自动压缩。
type ContextManager struct {
	session          *session.Session
	summarizer       summarizer
	window           int
	keepRecentTokens int
}

func New(s *session.Session, sum summarizer, window, keepRecentTokens int) *ContextManager {
	return &ContextManager{session: s, summarizer: sum, window: window, keepRecentTokens: keepRecentTokens}
}

// threshold 预算阈值：window − reserve，reserve = max(15%·window, 16384)。
func (cm *ContextManager) threshold() int {
	reserve := max(cm.window*15/100, 16384)
	if reserve >= cm.window {
		reserve = cm.window / 2
	}
	return cm.window - reserve
}

// AfterTurn 每轮结束后调用；若上下文超阈值则压缩。
func (cm *ContextManager) AfterTurn(ctx context.Context, usage model.Usage) error {
	if usage.PromptTokens <= cm.threshold() {
		return nil
	}
	return cm.compact(ctx)
}

func (cm *ContextManager) compact(ctx context.Context) error {
	msgs, err := cm.session.Replay()
	if err != nil {
		return err
	}
	cut := findCutPoint(msgs, cm.keepRecentTokens)
	if cut <= 0 {
		return nil // 无更早内容可压
	}
	summary, err := cm.summarizer.Summarize(ctx, msgs[:cut])
	if err != nil {
		return err
	}
	return cm.session.Compact(summary, msgs[cut:])
}
