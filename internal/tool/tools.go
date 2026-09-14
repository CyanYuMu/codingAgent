package tool

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"einoclaw-build/internal/permission"
	"einoclaw-build/internal/runtime"
	"einoclaw-build/internal/workspace"
)

// Builtins 返回内置工具。bash 为该 agent 专属实例（cwd 隔离）；store 为会话产物存储（可 nil）。
// read/write/edit 共享一个 fileGuard：read_file 的区间去重与 edit 的「先读后改」守卫同源。
func Builtins(bash *runtime.Bash, store *runtime.ArtifactStore) []Tool {
	g := newFileGuard()
	return []Tool{
		newReadFileTool(store, g),
		newWriteFileTool(g),
		newEditFileTool(g),
		globTool{},
		grepTool{},
		bashTool{bash: bash},
	}
}

// ---------- read_file ----------

type readFileTool struct {
	store *runtime.ArtifactStore

	// 会话内已读记录（与 write/edit 共享 guard）：同一文件、mtime+size 未变、请求区间已被已读
	// 区间覆盖时不再重复返回内容——模型在长会话里反复整读同一文件是最常见的 token 浪费之一。
	// 记录随工具实例（≈会话）存活，换会话（/new /resume）由宿主调用 ResetConv 清空；压缩/剪枝后
	// 由循环调用 InvalidateReadHistory 清区间（旧内容已进摘要/占位，「仍在上文」不再成立）。
	guard *fileGuard
}

// newReadFileTool 构造 read_file 工具；guard 为 nil 时用独立空记录（去重照常工作）。
func newReadFileTool(store *runtime.ArtifactStore, g *fileGuard) *readFileTool {
	if g == nil {
		g = newFileGuard()
	}
	return &readFileTool{store: store, guard: g}
}

// ResetConv 清空会话级状态（宿主在换会话时调用）。
func (t *readFileTool) ResetConv() { t.guard.reset() }

// InvalidateReadHistory 压缩/剪枝后失效去重区间（保留 edit 守卫的指纹，见 fileGuard）。
func (t *readFileTool) InvalidateReadHistory() { t.guard.invalidateReads() }

func (*readFileTool) Name() string { return "read_file" }
func (*readFileTool) Description() string {
	return "按行读取文件内容；offset 为起始行号（1 起），limit 为读取行数（默认 300）。" +
		"file_path 还支持内部 URL：artifact://N（被截断的完整工具输出）、agent://<子agent名>（完整产出）、" +
		"history://<子agent名>（转录）、skill://<name>/<path>（skill 配套资源）。"
}
func (*readFileTool) Parameters() map[string]any {
	return map[string]any{
		"file_path": map[string]any{"type": "string", "description": "文件路径，或 artifact://、agent://、history://、skill:// URL"},
		"offset":    map[string]any{"type": "integer", "description": "起始行号（1 起）"},
		"limit":     map[string]any{"type": "integer", "description": "读取行数，默认 300"},
	}
}
func (*readFileTool) Required() []string       { return []string{"file_path"} }
func (*readFileTool) Tier() permission.Tier    { return permission.TierRead }
func (*readFileTool) Concurrency() Concurrency { return ConcurrencyShared }

const defaultReadLines = 300

func (t *readFileTool) Execute(ctx context.Context, args map[string]any, sink *runtime.Sink) error {
	path, _ := args["file_path"].(string)
	if path == "" {
		return fmt.Errorf("file_path 必填")
	}
	sessionURL := strings.Contains(path, "://") // 会话内 URL：artifact / agent / history 都由产物存储路由
	if sessionURL {
		if t.store == nil {
			return fmt.Errorf("本会话没有产物目录，无法读取 %s", path)
		}
		p, err := t.store.Resolve(path)
		if err != nil {
			return err
		}
		path = p
	}
	if !sessionURL && t.guard.workspace != nil {
		var err error
		path, err = t.guard.workspace.Rel(path)
		if err != nil {
			return err
		}
	}
	data, err := t.guard.readFile(path, sessionURL)
	if err != nil {
		return err
	}
	var mtime, size int64
	if st, err := t.guard.stat(path, sessionURL); err == nil {
		mtime, size = st.ModTime().UnixNano(), st.Size()
	}
	lines := strings.Split(string(data), "\n")
	start := 0
	if off, ok := args["offset"].(float64); ok && off > 1 {
		start = min(int(off)-1, len(lines))
	}
	limit := defaultReadLines
	if lim, ok := args["limit"].(float64); ok && lim > 0 {
		limit = int(lim)
	}
	end := min(start+limit, len(lines))
	// 空文件或 offset 越过 EOF 时没有可读内容，跳过去重记录（否则会产生「读过第 1-1 行」这类误导提示）
	hasRange := start < end && size > 0

	// 会话内去重只对真实文件路径生效（会话内 URL 的内容不会在上文里重复）
	contentUnchanged := t.guard.workspace == nil || t.guard.contentHash(path) == workspace.Hash(data)
	if !sessionURL && hasRange && contentUnchanged && t.guard.alreadyRead(path, mtime, size, start+1, end) {
		fmt.Fprintf(sink, "文件未变更（上次读过第 %d-%d 行），内容仍在上文中；需要其它区间就带 offset/limit 再读。", start+1, end)
		return nil
	}
	sink.Write([]byte(strings.Join(lines[start:end], "\n")))
	if !sessionURL && t.guard.workspace != nil {
		t.guard.observe(path, data)
		fmt.Fprintf(sink, "\n[sha256=%s]", t.guard.contentHash(path))
	}
	if end < len(lines) {
		fmt.Fprintf(sink, "\n[共 %d 行，已显示 %d-%d；继续读取请用 offset=%d]", len(lines), start+1, end, end+1)
	}
	if !sessionURL && hasRange {
		t.guard.recordRead(path, mtime, size, start+1, end)
	}
	return nil
}

// ---------- write_file ----------

type writeFileTool struct{ guard *fileGuard }

// newWriteFileTool 构造 write_file 工具；写成功后把新指纹登记进 guard（后续 edit/read 不再误判旧内容）。
func newWriteFileTool(g *fileGuard) writeFileTool {
	if g == nil {
		g = newFileGuard()
	}
	return writeFileTool{guard: g}
}

func (writeFileTool) Name() string        { return "write_file" }
func (writeFileTool) Description() string { return "写入文件（覆盖）" }
func (writeFileTool) Parameters() map[string]any {
	return map[string]any{
		"file_path": map[string]any{"type": "string"},
		"content":   map[string]any{"type": "string"},
	}
}
func (writeFileTool) Required() []string       { return []string{"file_path", "content"} }
func (writeFileTool) Tier() permission.Tier    { return permission.TierWrite }
func (writeFileTool) Concurrency() Concurrency { return ConcurrencyExclusive }

func (t writeFileTool) Execute(ctx context.Context, args map[string]any, sink *runtime.Sink) error {
	path, _ := args["file_path"].(string)
	content, contentOK := args["content"].(string)
	if !contentOK {
		return fmt.Errorf("content 必须是字符串（可为空）")
	}
	if path == "" {
		return fmt.Errorf("file_path 必填")
	}
	if t.guard.workspace != nil {
		id, err := t.guard.applyContent(path, []byte(content))
		if err != nil {
			return err
		}
		fmt.Fprintf(sink, "wrote %d bytes to %s [transaction=%s]", len(content), path, id)
		emitSyntaxDiagnostics(path, []byte(content), sink)
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return err
	}
	if st, err := os.Stat(path); err == nil {
		t.guard.markWritten(path, st.ModTime().UnixNano(), st.Size())
	}
	fmt.Fprintf(sink, "wrote %d bytes to %s", len(content), path)
	return nil
}

// ---------- edit ----------

// editFileTool 精确替换：先读后改（本会话必须 read_file 过且指纹一致）+ old 唯一匹配 + mtime 校验。
// 防「凭记忆整文件重写 → 静默丢代码」；替换在原始字节上做，换行风格与 BOM 天然保留。
type editFileTool struct{ guard *fileGuard }

// newEditFileTool 构造 edit 工具；guard 必须与 read_file/write_file 共享同一实例（Builtins 装配）。
func newEditFileTool(g *fileGuard) editFileTool {
	if g == nil {
		g = newFileGuard()
	}
	return editFileTool{guard: g}
}

func (editFileTool) Name() string { return "edit" }
func (editFileTool) Description() string {
	return "精确替换文件中的一段内容（先读后改）：old_string 必须与文件内容逐字节唯一匹配" +
		"（replace_all=true 时替换全部出现），new_string 为替换后的内容（可为空串表示删除）。" +
		"edit 前本会话必须先用 read_file 读取该文件；若读后文件被外部修改（mtime 校验），会被拒绝并要求重读。"
}
func (editFileTool) Parameters() map[string]any {
	return map[string]any{
		"file_path":   map[string]any{"type": "string"},
		"old_string":  map[string]any{"type": "string", "description": "要被替换的原文，须与文件内容逐字节一致"},
		"new_string":  map[string]any{"type": "string", "description": "替换后的内容，可为空串（删除）"},
		"replace_all": map[string]any{"type": "boolean", "description": "替换全部出现；缺省 false 时 old_string 必须唯一"},
	}
}
func (editFileTool) Required() []string       { return []string{"file_path", "old_string", "new_string"} }
func (editFileTool) Tier() permission.Tier    { return permission.TierWrite }
func (editFileTool) Concurrency() Concurrency { return ConcurrencyExclusive }

func (t editFileTool) Execute(ctx context.Context, args map[string]any, sink *runtime.Sink) error {
	path, _ := args["file_path"].(string)
	oldStr, _ := args["old_string"].(string)
	newStr, _ := args["new_string"].(string)
	replaceAll, _ := args["replace_all"].(bool)
	if path == "" || oldStr == "" {
		return fmt.Errorf("file_path 与 old_string 必填（new_string 可为空串表示删除）")
	}
	if oldStr == newStr {
		return fmt.Errorf("old_string 与 new_string 相同，无需编辑")
	}
	if _, ok := args["new_string"].(string); !ok {
		return fmt.Errorf("new_string 必须是字符串")
	}
	if t.guard.workspace != nil {
		var err error
		path, err = t.guard.workspace.Rel(path)
		if err != nil {
			return err
		}
	}
	st, err := t.guard.stat(path, false)
	if err != nil {
		return fmt.Errorf("文件不存在：%s", path)
	}
	// 先读后改守卫：没读过、或读后文件被外部改动，都要求重新 read_file（fail-safe）
	if t.guard.workspace == nil && !t.guard.freshRead(path, st.ModTime().UnixNano(), st.Size()) {
		return fmt.Errorf("edit 前必须先用 read_file 读取该文件；若已读过但文件刚被外部修改（mtime 校验不过），请重读后再 edit")
	}
	data, err := t.guard.readFile(path, false)
	if err != nil {
		return err
	}
	content := string(data)
	n := strings.Count(content, oldStr)
	switch {
	case n == 0:
		return fmt.Errorf("old_string 在文件中未找到（逐字节匹配，注意空白与换行）；请重读文件确认后重试")
	case n > 1 && !replaceAll:
		return fmt.Errorf("old_string 出现 %d 次（要求唯一）；请扩大上下文使其唯一，或设 replace_all=true 全部替换", n)
	}
	out := content
	if replaceAll {
		out = strings.ReplaceAll(content, oldStr, newStr)
	} else {
		out = strings.Replace(content, oldStr, newStr, 1)
	}
	if t.guard.workspace != nil {
		id, err := t.guard.applyContent(path, []byte(out))
		if err != nil {
			return err
		}
		fmt.Fprintf(sink, "edited %s [transaction=%s]", path, id)
		emitSyntaxDiagnostics(path, []byte(out), sink)
		return nil
	}
	if err := os.WriteFile(path, []byte(out), st.Mode().Perm()); err != nil {
		return err
	}
	if st2, err := os.Stat(path); err == nil {
		t.guard.markWritten(path, st2.ModTime().UnixNano(), st2.Size())
	}
	fmt.Fprintf(sink, "edited %s（替换 %d 处）", path, n)
	return nil
}

// ---------- glob ----------

type globTool struct{}

func (globTool) Name() string        { return "glob" }
func (globTool) Description() string { return "按 pattern 匹配文件名" }
func (globTool) Parameters() map[string]any {
	return map[string]any{"pattern": map[string]any{"type": "string"}}
}
func (globTool) Required() []string       { return []string{"pattern"} }
func (globTool) Tier() permission.Tier    { return permission.TierRead }
func (globTool) Concurrency() Concurrency { return ConcurrencyShared }

func (globTool) Execute(ctx context.Context, args map[string]any, sink *runtime.Sink) error {
	pattern, _ := args["pattern"].(string)
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return err
	}
	if len(matches) == 0 {
		sink.Write([]byte("(no matches)"))
		return nil
	}
	sink.Write([]byte(strings.Join(matches, "\n")))
	return nil
}

// ---------- bash ----------

type bashTool struct {
	bash *runtime.Bash
}

func (bashTool) Name() string        { return "bash" }
func (bashTool) Description() string { return "执行 shell 命令" }
func (bashTool) Parameters() map[string]any {
	return map[string]any{"command": map[string]any{"type": "string"}}
}
func (bashTool) Required() []string       { return []string{"command"} }
func (bashTool) Tier() permission.Tier    { return permission.TierExec }
func (bashTool) Concurrency() Concurrency { return ConcurrencyExclusive }

// Decision 按命令内容分类：只读 → read（write 模式免审批）；危险 → Override 强制询问（yolo 也拦）；
// 其余回落 exec tier。分类是纯函数，误判只读只多一次审批、漏判危险才是事故，故保守。
func (bashTool) Decision(args map[string]any) permission.ToolDecision {
	command, _ := args["command"].(string)
	ro, dang, reason := runtime.Classify(command)
	switch {
	case dang:
		return permission.ToolDecision{Tier: permission.TierExec, Override: true, Reason: reason}
	case ro:
		return permission.ToolDecision{Tier: permission.TierRead, Reason: "只读命令"}
	}
	return permission.ToolDecision{Tier: permission.TierExec}
}

func (b bashTool) Execute(ctx context.Context, args map[string]any, sink *runtime.Sink) error {
	command, _ := args["command"].(string)
	if command == "" {
		return fmt.Errorf("command 必填")
	}
	return b.bash.Execute(ctx, command, sink)
}
