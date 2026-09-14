package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"

	"einoclaw-build/internal/codeintel"
	"einoclaw-build/internal/permission"
	"einoclaw-build/internal/runtime"
	"einoclaw-build/internal/workspace"
)

type codeIntelTool struct {
	w       *workspace.Workspace
	sandbox *runtime.ProcessSandbox
	cfg     codeintel.LSPConfig
}

func NewCodeIntelTool(w *workspace.Workspace, s *runtime.ProcessSandbox, cfg codeintel.LSPConfig) Tool {
	return codeIntelTool{w, s, cfg}
}
func (codeIntelTool) Name() string { return "code_intel" }
func (codeIntelTool) Description() string {
	return "代码分析：ast 返回 Go 语法诊断和符号；lsp_symbols/definition/references/hover 使用配置的 LSP。line 从 1 起，character 为从 0 起的 UTF-16 偏移；语义结果仅描述这次读取的文件版本。"
}
func (codeIntelTool) Parameters() map[string]any {
	return map[string]any{"operation": map[string]any{"type": "string", "enum": []string{"ast", "lsp_symbols", "definition", "references", "hover"}}, "file_path": map[string]any{"type": "string"}, "line": map[string]any{"type": "integer"}, "character": map[string]any{"type": "integer"}}
}
func (codeIntelTool) Required() []string       { return []string{"operation", "file_path"} }
func (codeIntelTool) Tier() permission.Tier    { return permission.TierRead }
func (codeIntelTool) Concurrency() Concurrency { return ConcurrencyShared }
func (t codeIntelTool) Execute(ctx context.Context, args map[string]any, sink *runtime.Sink) error {
	p, _ := args["file_path"].(string)
	p, err := t.w.Rel(p)
	if err != nil {
		return err
	}
	b, err := t.w.ReadFile(p)
	if err != nil {
		return err
	}
	op, _ := args["operation"].(string)
	if op == "ast" {
		a, err := codeintel.AnalyzeGo(p, b)
		if err != nil {
			return err
		}
		return json.NewEncoder(sink).Encode(a)
	}
	method := map[string]string{"lsp_symbols": "textDocument/documentSymbol", "definition": "textDocument/definition", "references": "textDocument/references", "hover": "textDocument/hover"}[op]
	if method == "" {
		return fmt.Errorf("unknown code_intel operation: %s", op)
	}
	line, _ := args["line"].(float64)
	ch, _ := args["character"].(float64)
	if op != "lsp_symbols" && (line < 1 || ch < 0 || line != float64(int(line)) || ch != float64(int(ch))) {
		return fmt.Errorf("line must be a positive integer; character is a zero-based UTF-16 offset")
	}
	raw, err := codeintel.Query(ctx, t.sandbox, t.cfg, t.w.Path(), filepath.Join(t.w.Path(), p), string(b), method, max(0, int(line)-1), int(ch))
	if err != nil {
		return err
	}
	return json.NewEncoder(sink).Encode(map[string]any{"sha256": workspace.Hash(b), "result": raw})
}
