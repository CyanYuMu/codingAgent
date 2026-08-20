package model

import (
	"context"
	"fmt"
	"strings"

	"github.com/cloudwego/eino-ext/components/model/agenticark"
	"github.com/cloudwego/eino-ext/components/model/agenticdeepseek"
	"github.com/cloudwego/eino-ext/components/model/agenticopenai"
	"github.com/cloudwego/eino-ext/components/model/agenticqwen"
	cmodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"einoclaw-build/internal/message"
)

// einoModel 用 eino 的 components/model（AgenticModel 维度）实现 Model。
type einoModel struct {
	base cmodel.AgenticModel // = BaseModel[*schema.AgenticMessage]
}

// Stream 把我们的消息转成 eino 的 AgenticMessage，发起流式调用并包装成 Stream。
// P0 不传工具（冒烟无工具）；tools 参数用 _ 显式忽略，P4 实现工具时改用 WithTools。
func (m *einoModel) Stream(ctx context.Context, msgs []message.Message, _ []ToolSpec) (*Stream, error) {
	agenticMsgs := toAgenticMessages(msgs)
	reader, err := m.base.Stream(ctx, agenticMsgs)
	if err != nil {
		return nil, err
	}
	return &Stream{reader: reader}, nil
}

// Recv 从底层 eino reader 取一个 chunk，转成我们的 ModelEvent 增量。
func (s *Stream) Recv() (ModelEvent, error) {
	chunk, err := s.reader.Recv()
	if err != nil {
		return ModelEvent{}, err // io.EOF 或其它错误
	}
	if chunk.ResponseMeta != nil && chunk.ResponseMeta.TokenUsage != nil {
		s.usage = fromSchemaUsage(chunk.ResponseMeta.TokenUsage)
	}
	var ev ModelEvent
	for _, b := range chunk.ContentBlocks {
		if b.Reasoning != nil && b.Reasoning.Text != "" {
			ev.Thinking += b.Reasoning.Text
		}
		if b.AssistantGenText != nil && b.AssistantGenText.Text != "" {
			ev.Text += b.AssistantGenText.Text
		}
		if b.FunctionToolCall != nil {
			ev.ToolCalls = append(ev.ToolCalls, ToolCallDelta{
				CallID: b.FunctionToolCall.CallID,
				Name:   b.FunctionToolCall.Name,
				Args:   b.FunctionToolCall.Arguments,
			})
		}
	}
	return ev, nil
}

// Close 释放底层 reader。
func (s *Stream) Close() { s.reader.Close() }

// toAgenticMessages 把我们的消息转成 eino 的 AgenticMessage。
// P0 只处理 system/user（冒烟仅需这两类）；assistant/tool 在 P1 加。
func toAgenticMessages(msgs []message.Message) []*schema.AgenticMessage {
	out := make([]*schema.AgenticMessage, 0, len(msgs))
	for _, m := range msgs {
		switch m.Role {
		case message.RoleSystem:
			out = append(out, schema.SystemAgenticMessage(textOf(m)))
		case message.RoleUser:
			out = append(out, schema.UserAgenticMessage(textOf(m)))
		}
	}
	return out
}

// textOf 拼接消息里的所有文本块。
func textOf(m message.Message) string {
	var sb strings.Builder
	for _, b := range m.Blocks {
		if b.Kind == message.BlockText {
			sb.WriteString(b.Text)
		}
	}
	return sb.String()
}

// fromSchemaUsage 把 eino 的 TokenUsage 转成我们的 Usage。
func fromSchemaUsage(u *schema.TokenUsage) Usage {
	return Usage{
		PromptTokens:     u.PromptTokens,
		CompletionTokens: u.CompletionTokens,
		TotalTokens:      u.TotalTokens,
		ReasoningTokens:  u.CompletionTokensDetails.ReasoningTokens,
		CachedTokens:     u.PromptTokenDetails.CachedTokens,
	}
}

// New 根据 Config 构建 Model。返回的 Model 内部持有 AgenticMessage 维度的底层模型。
func New(ctx context.Context, cfg Config) (Model, error) {
	switch cfg.Provider {
	case "qwen":
		mm, err := agenticqwen.New(ctx, &agenticqwen.Config{APIKey: cfg.APIKey, Model: cfg.Model, BaseURL: cfg.BaseURL})
		if err != nil {
			return nil, err
		}
		return &einoModel{base: mm}, nil
	case "openai":
		mm, err := agenticopenai.NewResponsesModel(ctx, &agenticopenai.ResponsesConfig{APIKey: cfg.APIKey, Model: cfg.Model, BaseURL: cfg.BaseURL, EnableAutoCache: true})
		if err != nil {
			return nil, err
		}
		return &einoModel{base: mm}, nil
	case "ark":
		mm, err := agenticark.New(ctx, &agenticark.Config{APIKey: cfg.APIKey, Model: cfg.Model, BaseURL: cfg.BaseURL, EnableAutoCache: true})
		if err != nil {
			return nil, err
		}
		return &einoModel{base: mm}, nil
	case "deepseek":
		mm, err := agenticdeepseek.New(ctx, &agenticdeepseek.Config{APIKey: cfg.APIKey, Model: cfg.Model, BaseURL: cfg.BaseURL})
		if err != nil {
			return nil, err
		}
		return &einoModel{base: mm}, nil
	default:
		return nil, fmt.Errorf("unknown provider %q", cfg.Provider)
	}
}
