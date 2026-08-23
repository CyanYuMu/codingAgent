package model

import (
	"errors"
	"testing"
)

func TestIsContextOverflow(t *testing.T) {
	for _, s := range []string{
		"This model's maximum context length is 128000 tokens",
		"context_length_exceeded",
		"prompt is too long: 210000 tokens",
		"input length and max_tokens exceed context limit",
	} {
		if !IsContextOverflow(errors.New(s)) {
			t.Errorf("should be overflow: %q", s)
		}
	}
	if IsContextOverflow(errors.New("rate limit exceeded")) || IsContextOverflow(nil) {
		t.Error("false positive")
	}
}

func TestIsRetryable(t *testing.T) {
	for _, s := range []string{"429 Too Many Requests", "status 503", "connection reset by peer", "server overloaded", "i/o timeout"} {
		if !IsRetryable(errors.New(s)) {
			t.Errorf("should retry: %q", s)
		}
	}
	if IsRetryable(errors.New("invalid api key")) || IsRetryable(errors.New("context_length_exceeded")) || IsRetryable(nil) {
		t.Error("false positive")
	}
}

func TestToSchemaToolsKeepsNestedSchema(t *testing.T) {
	specs := []ToolSpec{{Name: "task", Parameters: map[string]any{
		"tasks": map[string]any{"type": "array", "items": map[string]any{
			"type": "object", "properties": map[string]any{"subagent": map[string]any{"type": "string"}}, "required": []string{"subagent"},
		}},
	}, Required: []string{"tasks"}}}
	infos := toSchemaTools(specs)
	if len(infos) != 1 {
		t.Fatalf("infos = %d", len(infos))
	}
	js, err := infos[0].ParamsOneOf.ToJSONSchema()
	if err != nil || js == nil {
		t.Fatalf("schema err %v", err)
	}
	if js.Type != "object" {
		t.Fatalf("type = %q", js.Type)
	}
	tasks, ok := js.Properties.Get("tasks")
	if !ok || tasks.Items == nil || tasks.Items.Properties == nil {
		t.Fatalf("nested items lost: %+v", tasks)
	}
	if _, ok := tasks.Items.Properties.Get("subagent"); !ok {
		t.Fatal("nested property lost")
	}
	if len(js.Required) != 1 || js.Required[0] != "tasks" {
		t.Fatalf("required = %v", js.Required)
	}
}
