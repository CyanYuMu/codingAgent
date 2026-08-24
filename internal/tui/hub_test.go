package tui

import (
	"strings"
	"testing"
	"time"

	"einoclaw-build/internal/subagent"
)

func rows() []subagent.RunView {
	return []subagent.RunView{
		{Name: "Scout", Agent: "explorer", Status: "running", CurrentTool: "read_file", Requests: 3, ToolCalls: 5, Tokens: 4200, Age: 12 * time.Second},
		{Name: "Reviewer", Agent: "reviewer", Status: "parked", Requests: 7, ToolCalls: 9, Tokens: 8100, Age: 90 * time.Second},
		{Name: "Fixer", Agent: "worker", Status: "killed", Requests: 20, Tokens: 30000, Age: time.Hour},
	}
}

func TestRenderHubRows(t *testing.T) {
	out := renderHub(rows(), 1, 120, 0)
	if len(out) != 4 {
		t.Fatalf("应为表头 + 3 行，实际 %d：%v", len(out), out)
	}
	head := out[0]
	if !strings.Contains(head, "运行中 1") || !strings.Contains(head, "已结束 1") || !strings.Contains(head, "42.3k") {
		t.Fatalf("表头聚合错：%s", head)
	}
	if !strings.Contains(out[1], "Scout") || !strings.Contains(out[1], "read_file") || !strings.Contains(out[1], "req=3") {
		t.Fatalf("行内容缺字段：%s", out[1])
	}
	if !strings.Contains(out[2], "▸") {
		t.Fatalf("选中行应有标记：%s", out[2])
	}
	if strings.Contains(out[1], "▸") {
		t.Fatalf("未选中行不该有标记：%s", out[1])
	}
	if !strings.Contains(out[3], "—") {
		t.Fatalf("没有当前工具时应显示占位：%s", out[3])
	}
}

func TestRenderHubEmptyAndWindowed(t *testing.T) {
	if out := renderHub(nil, 0, 80, 5); len(out) != 2 || !strings.Contains(out[1], "还没有派发") {
		t.Fatalf("空名册提示 = %v", out)
	}
	out := renderHub(rows(), 2, 80, 1) // 只显示一行时应把选中行带出来，并提示被折叠的行数
	if len(out) != 3 || !strings.Contains(out[1], "上面还有 2 行") || !strings.Contains(out[2], "Fixer") {
		t.Fatalf("窗口化渲染 = %v", out)
	}
	out = renderHub(rows(), 0, 80, 1)
	if len(out) != 3 || !strings.Contains(out[1], "Scout") || !strings.Contains(out[2], "下面还有 2 行") {
		t.Fatalf("窗口化渲染（顶部）= %v", out)
	}
}

func TestParseAgentCommand(t *testing.T) {
	name, msg, ok := parseAgentCommand("/agent Reviewer 再核对一下 X")
	if !ok || name != "Reviewer" || msg != "再核对一下 X" {
		t.Fatalf("got %q %q %v", name, msg, ok)
	}
	for _, bad := range []string{"/agent", "/agent ", "/agent Reviewer", "/agent Reviewer   "} {
		if _, _, ok := parseAgentCommand(bad); ok {
			t.Fatalf("%q 应被判为用法错误", bad)
		}
	}
}

func TestClipVisibleKeepsEscapes(t *testing.T) {
	colored := "\x1b[31mABCDEF\x1b[0m"
	got := clipVisible(colored, 3)
	if !strings.HasPrefix(got, "\x1b[31m") || strings.Contains(got, "DEF") {
		t.Fatalf("截断结果 = %q", got)
	}
}
