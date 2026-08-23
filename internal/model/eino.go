package model

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cloudwego/eino-ext/components/model/agenticark"
	"github.com/cloudwego/eino-ext/components/model/agenticdeepseek"
	"github.com/cloudwego/eino-ext/components/model/agenticopenai"
	"github.com/cloudwego/eino-ext/components/model/agenticqwen"
	cmodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/eino-contrib/jsonschema"

	"einoclaw-build/internal/message"
)

// einoModel 用 eino 的 components/model（AgenticModel 维度）实现 Model。
type einoModel struct {
	base cmodel.AgenticModel // = BaseModel[*schema.AgenticMessage]
}

// Stream 把我们的消息转成 eino 的 AgenticMessage，带工具定义发起流式调用。
func (m *einoModel) Stream(ctx context.Context, msgs []message.Message, tools []ToolSpec) (ModelStream, error) {
	agenticMsgs := toAgenticMessages(msgs)
	var opts []cmodel.Option
	if len(tools) > 0 {
		opts = append(opts, cmodel.WithTools(toSchemaTools(tools)))
	}
	reader, err := m.base.Stream(ctx, agenticMsgs, opts...)
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
			idx := -1
			if b.StreamingMeta != nil {
				idx = b.StreamingMeta.Index
			}
			ev.ToolCalls = append(ev.ToolCalls, ToolCallDelta{
				Index:  idx,
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

// toAgenticMessages 把我们的消息转成 eino 的 AgenticMessage（四类角色）。
func toAgenticMessages(msgs []message.Message) []*schema.AgenticMessage {
	out := make([]*schema.AgenticMessage, 0, len(msgs))
	for _, m := range msgs {
		switch m.Role {
		case message.RoleSystem:
			out = append(out, schema.SystemAgenticMessage(textOf(m)))
		case message.RoleUser:
			out = append(out, schema.UserAgenticMessage(textOf(m)))
		case message.RoleAssistant:
			out = append(out, toAssistantAgenticMessage(m))
		case message.RoleTool:
			out = append(out, toToolAgenticMessage(m))
		}
	}
	return out
}

// toAssistantAgenticMessage 把 assistant 消息转成 AgenticMessage（正文/思考/工具调用块）。
func toAssistantAgenticMessage(m message.Message) *schema.AgenticMessage {
	am := &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant}
	for _, b := range m.Blocks {
		switch b.Kind {
		case message.BlockText:
			am.ContentBlocks = append(am.ContentBlocks, schema.NewContentBlock(&schema.AssistantGenText{Text: b.Text}))
		case message.BlockThinking:
			am.ContentBlocks = append(am.ContentBlocks, schema.NewContentBlock(&schema.Reasoning{Text: b.Thinking}))
		case message.BlockToolCall:
			if b.ToolCall != nil {
				am.ContentBlocks = append(am.ContentBlocks, schema.NewContentBlock(&schema.FunctionToolCall{
					CallID:    b.ToolCall.ID,
					Name:      b.ToolCall.Name,
					Arguments: b.ToolCall.Args,
				}))
			}
		}
	}
	return am
}

// toToolAgenticMessage 把 tool 消息转成 AgenticMessage（工具结果用 user 角色 + FunctionToolResult 块）。
func toToolAgenticMessage(m message.Message) *schema.AgenticMessage {
	tm := &schema.AgenticMessage{Role: schema.AgenticRoleTypeUser}
	for _, b := range m.Blocks {
		if b.Kind == message.BlockToolResult && b.ToolResult != nil {
			tm.ContentBlocks = append(tm.ContentBlocks, schema.NewContentBlock(&schema.FunctionToolResult{
				CallID: b.ToolResult.ToolCallID,
				Name:   b.ToolResult.Name,
				Content: []*schema.FunctionToolResultContentBlock{
					{Type: schema.FunctionToolResultContentBlockTypeText, Text: &schema.UserInputText{Text: b.ToolResult.Content}},
				},
			}))
		}
	}
	return tm
}

// toSchemaTools 把我们的 ToolSpec 转成 eino 的 ToolInfo：
// 把 {type:object, properties, required} 整体编码再解析为 *jsonschema.Schema，嵌套的 items/properties/enum/description 原样透传。
func toSchemaTools(tools []ToolSpec) []*schema.ToolInfo {
	out := make([]*schema.ToolInfo, 0, len(tools))
	for _, t := range tools {
		js, err := toolJSONSchema(t)
		if err != nil {
			continue
		}
		out = append(out, &schema.ToolInfo{
			Name:        t.Name,
			Desc:        t.Description,
			ParamsOneOf: schema.NewParamsOneOfByJSONSchema(js),
		})
	}
	return out
}

// toolJSONSchema 把 ToolSpec 的参数描述转成完整的对象 JSON Schema。
func toolJSONSchema(t ToolSpec) (*jsonschema.Schema, error) {
	props := t.Parameters
	if props == nil {
		props = map[string]any{}
	}
	required := t.Required
	if required == nil {
		required = []string{}
	}
	raw := map[string]any{"type": "object", "properties": props, "required": required}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	var js jsonschema.Schema
	if err := json.Unmarshal(b, &js); err != nil {
		return nil, err
	}
	return &js, nil
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
