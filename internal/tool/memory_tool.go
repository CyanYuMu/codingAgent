package tool

import (
	"context"
	"fmt"

	"einoclaw-build/internal/memory"
	"einoclaw-build/internal/permission"
	"einoclaw-build/internal/runtime"
)

// rememberTool 让模型显式写记忆。
type rememberTool struct {
	store *memory.Store
}

// NewRememberTool 构造 remember 工具。
func NewRememberTool(store *memory.Store) Tool {
	return rememberTool{store: store}
}

func (rememberTool) Name() string        { return "remember" }
func (rememberTool) Description() string { return "记录一条需要长期记住的信息（用户偏好/事实/决策）" }
func (rememberTool) Parameters() map[string]any {
	return map[string]any{
		"content":     map[string]any{"type": "string"},
		"memory_type": map[string]any{"type": "string"},
	}
}
func (rememberTool) Tier() permission.Tier { return permission.TierWrite }

func (r rememberTool) Execute(ctx context.Context, args map[string]any, sink *runtime.Sink) error {
	content, _ := args["content"].(string)
	if content == "" {
		return fmt.Errorf("content 必填")
	}
	memoryType, _ := args["memory_type"].(string)
	_ = r.store.Remember(content, memory.MemoryOpts{Source: "model", Importance: 0.8, MemoryType: memoryType})
	sink.Write([]byte("remembered"))
	return nil
}
