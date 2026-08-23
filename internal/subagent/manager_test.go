package subagent

import (
	"context"
	"strings"
	"testing"
)

func TestManagerUnknownSubagent(t *testing.T) {
	m := NewManager(nil, nil, nil, []SubagentSpec{{Name: "reviewer"}})
	r := m.Run(context.Background(), "nope", "x")
	if r.Status != StatusFailed || r.Err == nil || !strings.Contains(r.Err.Error(), "unknown subagent") {
		t.Fatalf("未知子 agent 应 failed，got %+v", r)
	}
}

func TestRunManyBatchOrder(t *testing.T) {
	m := NewManager(nil, nil, nil, []SubagentSpec{{Name: "reviewer"}})
	results := m.RunMany(context.Background(), []Task{
		{Subagent: "nope1", Prompt: "x"},
		{Subagent: "nope2", Prompt: "y"},
	})
	if len(results) != 2 {
		t.Fatalf("len = %d, want 2", len(results))
	}
	for _, r := range results {
		if r.Status != StatusFailed {
			t.Fatalf("未知子 agent 应 failed，got %+v", r)
		}
	}
	// 结果按输入序返回
	if results[0].ID != "nope1" || results[1].ID != "nope2" {
		t.Fatalf("results 顺序错，got %+v", results)
	}
}
