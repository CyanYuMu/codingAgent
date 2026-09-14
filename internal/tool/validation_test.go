package tool

import (
	"context"
	"strings"
	"testing"

	"einoclaw-build/internal/message"
	"einoclaw-build/internal/permission"
	"einoclaw-build/internal/runtime"
)

type validationTool struct{ called *bool }

func (validationTool) Name() string        { return "validated" }
func (validationTool) Description() string { return "" }
func (validationTool) Parameters() map[string]any {
	return map[string]any{
		"operation": map[string]any{"type": "string", "enum": []string{"read", "write"}},
		"count":     map[string]any{"type": "integer", "minimum": 1},
		"changes": map[string]any{
			"type": "array",
			"items": map[string]any{
				"type":       "object",
				"properties": map[string]any{"path": map[string]any{"type": "string"}},
				"required":   []string{"path"},
			},
		},
	}
}
func (validationTool) Required() []string       { return []string{"operation"} }
func (validationTool) Tier() permission.Tier    { return permission.TierRead }
func (validationTool) Concurrency() Concurrency { return ConcurrencyShared }
func (v validationTool) Execute(context.Context, map[string]any, *runtime.Sink) error {
	*v.called = true
	return nil
}

func TestExecutorValidatesArgumentsBeforeExecution(t *testing.T) {
	cases := []struct {
		name, args, want string
	}{
		{"missing required", `{}`, "operation is required"},
		{"wrong type", `{"operation":"read","count":"1"}`, "count must be integer"},
		{"non integer", `{"operation":"read","count":1.5}`, "count must be integer"},
		{"minimum", `{"operation":"read","count":0}`, "count must be >= 1"},
		{"enum", `{"operation":"delete"}`, "must be one of"},
		{"nested required", `{"operation":"write","changes":[{}]}`, "changes[0].path is required"},
		{"unknown top level", `{"operation":"read","commnad":"ls"}`, "commnad is not an allowed argument"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			reg := NewRegistry()
			reg.Register(validationTool{called: &called})
			exec := NewExecutor(reg, permission.ModeYolo, nil)
			got := exec.Execute(context.Background(), message.ToolCall{Name: "validated", Args: tc.args})
			if !got.IsError || !strings.Contains(got.Content, tc.want) {
				t.Fatalf("result = %+v, want validation error containing %q", got, tc.want)
			}
			if called {
				t.Fatal("tool executed despite invalid arguments")
			}
		})
	}
}

func TestExecutorAcceptsValidArguments(t *testing.T) {
	called := false
	reg := NewRegistry()
	reg.Register(validationTool{called: &called})
	exec := NewExecutor(reg, permission.ModeYolo, nil)
	got := exec.Execute(context.Background(), message.ToolCall{
		Name: "validated", Args: `{"operation":"write","count":2,"changes":[{"path":"a.go","future_field":true}]}`,
	})
	if got.IsError || !called {
		t.Fatalf("valid arguments should execute: %+v called=%v", got, called)
	}
}
