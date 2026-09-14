package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"einoclaw-build/internal/codeintel"
	"einoclaw-build/internal/permission"
	"einoclaw-build/internal/runtime"
	"einoclaw-build/internal/workspace"
)

// ScopedBuiltins is the production tool factory; Builtins remains available to
// legacy embedders/tests. Every actor receives an independent read history but
// shares the project's journal lock and root boundary.
func ScopedBuiltins(bash *runtime.Bash, store *runtime.ArtifactStore, w *workspace.Workspace) []Tool {
	g := newFileGuard()
	g.workspace = w
	return []Tool{newReadFileTool(store, g), newWriteFileTool(g), newEditFileTool(g), scopedSearch{w: w, name: "glob"}, scopedSearch{w: w, name: "grep"}, bashTool{bash: bash}, changeTool{w: w, name: "apply_changes"}, changeTool{w: w, name: "undo_changes"}, changeTool{w: w, name: "change_history"}}
}

func (g *fileGuard) readFile(path string, trusted bool) ([]byte, error) {
	if g.workspace != nil && !trusted {
		return g.workspace.ReadFile(path)
	}
	return os.ReadFile(path)
}
func (g *fileGuard) stat(path string, trusted bool) (fs.FileInfo, error) {
	if g.workspace != nil && !trusted {
		return g.workspace.Stat(path)
	}
	return os.Stat(path)
}
func (g *fileGuard) observe(path string, b []byte) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.hashes == nil {
		g.hashes = map[string]string{}
	}
	g.hashes[fsKey(path)] = workspace.Hash(b)
}
func (g *fileGuard) contentHash(path string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.hashes[fsKey(path)]
}
func (g *fileGuard) applyContent(path string, b []byte) (string, error) {
	p, err := g.workspace.Rel(path)
	if err != nil {
		return "", err
	}
	expected := g.contentHash(p)
	if _, err := g.workspace.Stat(p); os.IsNotExist(err) {
		if expected != "" {
			return "", fmt.Errorf("file was removed since read: %s; use apply_changes with missing to explicitly recreate it", p)
		}
		expected = "missing"
	} else if err != nil {
		return "", err
	}
	if expected == "" {
		return "", fmt.Errorf("read_file must read existing file before write/edit: %s", p)
	}
	tx, err := g.workspace.Apply([]workspace.Change{{Path: p, BeforeHash: expected, Content: string(b)}})
	if err != nil {
		return tx.ID, err
	}
	g.observe(p, b)
	if st, err := g.workspace.Stat(p); err == nil {
		g.markWritten(p, st.ModTime().UnixNano(), st.Size())
	}
	return tx.ID, nil
}

type scopedSearch struct {
	w    *workspace.Workspace
	name string
}

func (t scopedSearch) Name() string { return t.name }
func (t scopedSearch) Description() string {
	if t.name == "glob" {
		return "在工作区按文件模式搜索，支持 **；不跟随符号链接"
	}
	return "在工作区按正则搜索，最多返回 100 条，截断时明确提示"
}
func (t scopedSearch) Tier() permission.Tier    { return permission.TierRead }
func (t scopedSearch) Concurrency() Concurrency { return ConcurrencyShared }
func (t scopedSearch) Required() []string       { return []string{"pattern"} }
func (t scopedSearch) Parameters() map[string]any {
	return map[string]any{"pattern": map[string]any{"type": "string"}, "path": map[string]any{"type": "string"}}
}
func (t scopedSearch) Execute(ctx context.Context, args map[string]any, sink *runtime.Sink) error {
	pattern, _ := args["pattern"].(string)
	if pattern == "" {
		return fmt.Errorf("pattern 必填")
	}
	root, _ := args["path"].(string)
	if root == "" {
		root = "."
	}
	var re *regexp.Regexp
	var err error
	if t.name == "grep" {
		re, err = regexp.Compile(pattern)
		if err != nil {
			return err
		}
	} else {
		pattern, err = t.w.Rel(pattern)
		if err != nil {
			return err
		}
		for _, segment := range strings.Split(filepath.ToSlash(pattern), "/") {
			if segment != "**" {
				if _, err = filepath.Match(segment, ""); err != nil {
					return err
				}
			}
		}
	}
	files, err := t.w.Files(root)
	if err != nil {
		return err
	}
	count := 0
	for _, p := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		if t.name == "glob" {
			if globMatch(filepath.ToSlash(pattern), filepath.ToSlash(p)) {
				fmt.Fprintln(sink, p)
				count++
			}
		} else {
			b, err := t.w.ReadFile(p)
			if err != nil {
				return err
			}
			if strings.ContainsRune(string(b), 0) {
				continue
			}
			for i, line := range strings.Split(string(b), "\n") {
				if re.MatchString(line) {
					fmt.Fprintf(sink, "%s:%d: %s\n", p, i+1, line)
					count++
					if count >= 100 {
						break
					}
				}
			}
		}
		if count >= 100 {
			fmt.Fprintln(sink, "[result limit 100 reached; narrow pattern/path]")
			return nil
		}
	}
	if count == 0 {
		fmt.Fprint(sink, "(no matches)")
	}
	return nil
}
func globMatch(pattern, path string) bool {
	a, b := strings.SplitN(pattern, "/", 2), strings.SplitN(path, "/", 2)
	if a[0] == "**" {
		if len(a) == 1 {
			return true
		}
		if globMatch(a[1], path) {
			return true
		}
		return len(b) == 2 && globMatch(pattern, b[1])
	}
	ok, _ := filepath.Match(a[0], b[0])
	if !ok {
		return false
	}
	if len(a) == 1 || len(b) == 1 {
		return len(a) == len(b)
	}
	return globMatch(a[1], b[1])
}

type changeTool struct {
	w    *workspace.Workspace
	name string
}

func (t changeTool) Name() string { return t.name }
func (t changeTool) Tier() permission.Tier {
	if t.name == "change_history" {
		return permission.TierRead
	}
	return permission.TierWrite
}
func (t changeTool) Concurrency() Concurrency { return ConcurrencyExclusive }
func (t changeTool) Description() string {
	switch t.name {
	case "apply_changes":
		return "事务式批量修改：每个文件要求 read_file 返回的 before_sha256（新文件用 missing）；记录 before/after，支持 undo。content 可以为空；删除须 delete=true。"
	case "undo_changes":
		return "按 transaction_id 撤销或恢复中断的修改；若文件已有用户新改动则拒绝覆盖。"
	default:
		return "列出最近 20 次文件变更的 transaction_id、状态与路径。"
	}
}
func (t changeTool) Required() []string {
	switch t.name {
	case "apply_changes":
		return []string{"changes"}
	case "undo_changes":
		return []string{"transaction_id"}
	}
	return nil
}
func (t changeTool) Parameters() map[string]any {
	if t.name == "undo_changes" {
		return map[string]any{"transaction_id": map[string]any{"type": "string"}}
	}
	if t.name == "change_history" {
		return map[string]any{}
	}
	return map[string]any{"changes": map[string]any{"type": "array", "items": map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}, "before_sha256": map[string]any{"type": "string"}, "content": map[string]any{"type": "string"}, "delete": map[string]any{"type": "boolean"}}, "required": []string{"path", "before_sha256", "content"}}}}
}
func (t changeTool) Execute(ctx context.Context, args map[string]any, sink *runtime.Sink) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if t.name == "change_history" {
		h, err := t.w.History()
		if err != nil {
			return err
		}
		return json.NewEncoder(sink).Encode(h)
	}
	var tx workspace.Transaction
	var err error
	if t.name == "undo_changes" {
		id, _ := args["transaction_id"].(string)
		tx, err = t.w.Undo(id)
	} else {
		b, e := json.Marshal(args["changes"])
		if e != nil {
			return e
		}
		var raw []struct {
			Path    string  `json:"path"`
			Before  string  `json:"before_sha256"`
			Content *string `json:"content"`
			Delete  bool    `json:"delete"`
		}
		if e = json.Unmarshal(b, &raw); e != nil {
			return e
		}
		changes := make([]workspace.Change, 0, len(raw))
		for _, c := range raw {
			if c.Content == nil {
				return fmt.Errorf("content is required even when empty")
			}
			changes = append(changes, workspace.Change{Path: c.Path, BeforeHash: c.Before, Content: *c.Content, Delete: c.Delete})
		}
		tx, err = t.w.Apply(changes)
	}
	if err != nil {
		return err
	}
	for i := range tx.Files {
		if !tx.Files[i].Deleted && t.name == "apply_changes" {
			emitSyntaxDiagnostics(tx.Files[i].Path, tx.Files[i].After, sink)
		}
		tx.Files[i].Before = nil
		tx.Files[i].After = nil
	}
	return json.NewEncoder(sink).Encode(tx)
}

// Diagnostics describe an already applied edit, not a failed write.
func emitSyntaxDiagnostics(path string, source []byte, sink *runtime.Sink) {
	if filepath.Ext(path) != ".go" {
		return
	}
	a, err := codeintel.AnalyzeGo(path, source)
	if err != nil {
		return
	}
	for _, d := range a.Diagnostics {
		fmt.Fprintf(sink, "\n[Go syntax] %s:%d:%d: %s", path, d.Line, d.Column, d.Message)
	}
}
