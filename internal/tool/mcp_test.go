package tool

import (
	"testing"

	"einoclaw-build/internal/permission"
)

func TestMCPToolNameNormalization(t *testing.T) {
	mt := mcpTool{server: "filesystem", name: "read_file"}
	if mt.Name() != "mcp__filesystem_read_file" {
		t.Fatalf("Name = %q, want mcp__filesystem_read_file", mt.Name())
	}
	if mt.Tier() != permission.TierWrite {
		t.Fatalf("MCP 工具默认应为 write tier，got %s", mt.Tier())
	}
}

func TestInputSchemaToMap(t *testing.T) {
	// 模拟一个 MCP InputSchema 的 JSON 形态
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{"type": "string"},
		},
		"required": []string{"path"},
	}
	props, required := inputSchemaToMap(schema)
	if props == nil || props["path"] == nil {
		t.Fatalf("props = %+v, want path", props)
	}
	if len(required) != 1 || required[0] != "path" {
		t.Fatalf("required = %v", required)
	}
}
