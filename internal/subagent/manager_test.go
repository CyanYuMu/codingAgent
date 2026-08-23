package subagent

import (
	"context"
	"strings"
	"testing"
)

func TestManagerUnknownSubagent(t *testing.T) {
	m := NewManager(nil, nil, nil, []SubagentSpec{{Name: "reviewer"}})
	if _, err := m.Run(context.Background(), "nope", "x"); err == nil || !strings.Contains(err.Error(), "unknown subagent") {
		t.Fatalf("未知子 agent 应报错，got %v", err)
	}
}
