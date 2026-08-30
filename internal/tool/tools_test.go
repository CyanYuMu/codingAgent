package tool

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"einoclaw-build/internal/permission"
	"einoclaw-build/internal/runtime"
)

// writeTempFile 写一个临时文件并返回路径。
func writeTempFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestReadFileDedupesUnchanged(t *testing.T) {
	content := strings.Repeat("line\n", 49) + "line" // 恰好 50 行
	p := writeTempFile(t, content)
	tl := newReadFileTool(nil, nil)
	out1, err := runTool(t, tl, map[string]any{"file_path": p})
	if err != nil || !strings.Contains(out1, "line") {
		t.Fatalf("首次读应返回内容：out=%q err=%v", out1, err)
	}
	out2, err := runTool(t, tl, map[string]any{"file_path": p})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out2, "未变更（上次读过第 1-50 行）") {
		t.Fatalf("第二次应返回未变更提示：%q", out2)
	}
	if strings.Contains(out2, "line\nline") {
		t.Fatalf("去重提示里不该再带文件内容：%q", out2)
	}
}

func TestReadFileRereadsAfterChange(t *testing.T) {
	p := writeTempFile(t, "v1 content")
	tl := newReadFileTool(nil, nil)
	if _, err := runTool(t, tl, map[string]any{"file_path": p}); err != nil {
		t.Fatal(err)
	}
	// 改内容且改长度（长度不同保证 mtime+size 指纹必然变化，不依赖时间戳分辨率）
	if err := os.WriteFile(p, []byte("v2 content is longer now"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := runTool(t, tl, map[string]any{"file_path": p})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "v2 content is longer now") {
		t.Fatalf("文件变更后应返回真实内容：%q", out)
	}
	if strings.Contains(out, "未变更") {
		t.Fatalf("变更后的文件不该被去重：%q", out)
	}
}

func TestReadFileDifferentRangeStillReads(t *testing.T) {
	var b strings.Builder
	for i := 1; i <= 10; i++ {
		b.WriteString("L" + strings.Repeat("x", i) + "\n")
	}
	p := writeTempFile(t, strings.TrimSuffix(b.String(), "\n"))
	tl := newReadFileTool(nil, nil)
	if _, err := runTool(t, tl, map[string]any{"file_path": p, "offset": float64(1), "limit": float64(5)}); err != nil {
		t.Fatal(err)
	}
	out, err := runTool(t, tl, map[string]any{"file_path": p, "offset": float64(6), "limit": float64(5)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Lxxxxxx") { // 第 6 行
		t.Fatalf("未读过的区间应正常读：%q", out)
	}
	if strings.Contains(out, "未变更") {
		t.Fatalf("区间未覆盖时不该去重：%q", out)
	}
	// 两次区间合并后覆盖了 [1,10]，再整读应去重
	out, err = runTool(t, tl, map[string]any{"file_path": p})
	if err != nil || !strings.Contains(out, "未变更（上次读过第 1-10 行）") {
		t.Fatalf("合并区间后整读应去重：out=%q err=%v", out, err)
	}
}

func TestReadFilePartialOverlapReads(t *testing.T) {
	var b strings.Builder
	for i := 1; i <= 10; i++ {
		b.WriteString("L" + strings.Repeat("x", i) + "\n")
	}
	p := writeTempFile(t, b.String())
	tl := newReadFileTool(nil, nil)
	// [1,5] 已读；请求 [4,8] 只覆盖 [4,5]，必须真读
	if _, err := runTool(t, tl, map[string]any{"file_path": p, "offset": float64(1), "limit": float64(5)}); err != nil {
		t.Fatal(err)
	}
	out, err := runTool(t, tl, map[string]any{"file_path": p, "offset": float64(4), "limit": float64(5)})
	if err != nil || !strings.Contains(out, "Lxxxxxxxx") {
		t.Fatalf("部分覆盖应真读：out=%q err=%v", out, err)
	}
}

func TestReadFileResetConv(t *testing.T) {
	content := "aaa\nbbb\nccc\n"
	p := writeTempFile(t, content)
	tl := newReadFileTool(nil, nil)
	if _, err := runTool(t, tl, map[string]any{"file_path": p}); err != nil {
		t.Fatal(err)
	}
	if out, _ := runTool(t, tl, map[string]any{"file_path": p}); !strings.Contains(out, "未变更") {
		t.Fatalf("重置前第二次读应去重：%q", out)
	}
	tl.ResetConv()
	out, err := runTool(t, tl, map[string]any{"file_path": p})
	if err != nil || !strings.Contains(out, "aaa") {
		t.Fatalf("换会话后应重新返回内容：out=%q err=%v", out, err)
	}
}

func TestReadFilePastEOFDoesNotPoisonRecord(t *testing.T) {
	p := writeTempFile(t, "a\nb\nc\n")
	tl := newReadFileTool(nil, nil)
	out, err := runTool(t, tl, map[string]any{"file_path": p, "offset": float64(100)})
	if err != nil || out != "" {
		t.Fatalf("越过 EOF 应返回空且无错：out=%q err=%v", out, err)
	}
	// 退化区间不能记录：正常读仍应返回内容而非「未变更」
	out, err = runTool(t, tl, map[string]any{"file_path": p})
	if err != nil || !strings.Contains(out, "a\nb") {
		t.Fatalf("正常读应返回内容：out=%q err=%v", out, err)
	}
}

func TestReadFileEmptyFile(t *testing.T) {
	p := writeTempFile(t, "")
	tl := newReadFileTool(nil, nil)
	out, err := runTool(t, tl, map[string]any{"file_path": p})
	if err != nil || out != "" {
		t.Fatalf("空文件应返回空：out=%q err=%v", out, err)
	}
	out, err = runTool(t, tl, map[string]any{"file_path": p})
	if err != nil || strings.Contains(out, "未变更") {
		t.Fatalf("空文件不该有去重提示：out=%q err=%v", out, err)
	}
}

func TestReadFileSessionURLBypassesDedup(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "1.echo.log"), []byte("full content"), 0o644); err != nil {
		t.Fatal(err)
	}
	tl := newReadFileTool(runtime.NewArtifactStore(dir), nil)
	for i := 0; i < 2; i++ {
		out, err := runTool(t, tl, map[string]any{"file_path": "artifact://1"})
		if err != nil || !strings.Contains(out, "full content") {
			t.Fatalf("会话内 URL 每次都应返回完整内容：out=%q err=%v", out, err)
		}
	}
}

func TestInsertRange(t *testing.T) {
	cases := []struct {
		name  string
		start []lineRange
		add   lineRange
		want  []lineRange
	}{
		{"empty", nil, lineRange{1, 3}, []lineRange{{1, 3}}},
		{"adjacent merge", []lineRange{{1, 3}}, lineRange{4, 6}, []lineRange{{1, 6}}},
		{"overlap merge", []lineRange{{5, 8}}, lineRange{6, 9}, []lineRange{{5, 9}}},
		{"disjoint sorted", []lineRange{{10, 20}}, lineRange{5, 8}, []lineRange{{5, 8}, {10, 20}}},
		{"bridges two", []lineRange{{1, 3}, {7, 9}}, lineRange{4, 6}, []lineRange{{1, 9}}},
		{"absorb middle", []lineRange{{1, 10}}, lineRange{3, 5}, []lineRange{{1, 10}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := insertRange(c.start, c.add.from, c.add.to)
			if len(got) != len(c.want) {
				t.Fatalf("insertRange(%v, %v) = %v, want %v", c.start, c.add, got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("insertRange(%v, %v) = %v, want %v", c.start, c.add, got, c.want)
				}
			}
		})
	}
}

func TestBashToolDecision(t *testing.T) {
	bt := bashTool{bash: runtime.NewBash(t.TempDir())}
	if td := bt.Decision(map[string]any{"command": "git status"}); td.Tier != permission.TierRead || td.Override {
		t.Fatalf("只读命令应判 read 无覆盖：%+v", td)
	}
	if td := bt.Decision(map[string]any{"command": "rm -rf /tmp/x"}); !td.Override || td.Reason == "" || !strings.Contains(td.Reason, "rm") {
		t.Fatalf("危险命令应 Override 带原因：%+v", td)
	}
	if td := bt.Decision(map[string]any{"command": "node server.js"}); td.Tier != permission.TierExec || td.Override {
		t.Fatalf("未知命令应回落 exec：%+v", td)
	}
}

// ---------- edit（P11.2）----------

// readThenEdit 先用真 read_file 登记（走共享 guard），返回可用于 edit 的工具。
func readThenEdit(t *testing.T, g *fileGuard, p string) (Tool, Tool) {
	t.Helper()
	rt := newReadFileTool(nil, g)
	if _, err := runTool(t, rt, map[string]any{"file_path": p}); err != nil {
		t.Fatal(err)
	}
	return rt, newEditFileTool(g)
}

func editArgs(p, old, nw string, replaceAll bool) map[string]any {
	return map[string]any{"file_path": p, "old_string": old, "new_string": nw, "replace_all": replaceAll}
}

func TestEditRequiresRead(t *testing.T) {
	p := writeTempFile(t, "hello world")
	et := newEditFileTool(newFileGuard())
	_, err := runTool(t, et, editArgs(p, "hello", "hi", false))
	if err == nil || !strings.Contains(err.Error(), "read_file") {
		t.Fatalf("未读应拒绝并引导先 read_file：%v", err)
	}
}

func TestEditReplacesUniqueAndChains(t *testing.T) {
	p := writeTempFile(t, "hello world")
	_, et := readThenEdit(t, newFileGuard(), p)
	out, err := runTool(t, et, editArgs(p, "hello", "goodbye", false))
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(p)
	if err != nil || string(data) != "goodbye world" {
		t.Fatalf("替换结果不符：%q err=%v", data, err)
	}
	if !strings.Contains(out, "1") {
		t.Fatalf("输出应含替换次数：%q", out)
	}
	// 连续第二次 edit 不需重读：edit 后指纹已随写更新
	if _, err := runTool(t, et, editArgs(p, "goodbye", "bye", false)); err != nil {
		t.Fatalf("edit 后应可连续编辑：%v", err)
	}
	if data, _ := os.ReadFile(p); string(data) != "bye world" {
		t.Fatalf("第二次编辑结果不符：%q", data)
	}
}

func TestEditNonUniqueRejected(t *testing.T) {
	p := writeTempFile(t, "x\nx\n")
	_, et := readThenEdit(t, newFileGuard(), p)
	_, err := runTool(t, et, editArgs(p, "x", "y", false))
	if err == nil || !strings.Contains(err.Error(), "2") || !strings.Contains(err.Error(), "replace_all") {
		t.Fatalf("不唯一应拒绝并报告次数与出路：%v", err)
	}
	if data, _ := os.ReadFile(p); string(data) != "x\nx\n" {
		t.Fatalf("拒绝时不得改动文件：%q", data)
	}
}

func TestEditReplaceAll(t *testing.T) {
	p := writeTempFile(t, "x\nx\nx\n")
	_, et := readThenEdit(t, newFileGuard(), p)
	if _, err := runTool(t, et, editArgs(p, "x", "y", true)); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(p); string(data) != "y\ny\ny\n" {
		t.Fatalf("replace_all 应替换全部：%q", data)
	}
}

func TestEditRejectsExternalChange(t *testing.T) {
	p := writeTempFile(t, "v1")
	_, et := readThenEdit(t, newFileGuard(), p)
	if err := os.WriteFile(p, []byte("v2 longer now"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := runTool(t, et, editArgs(p, "v1", "v3", false))
	if err == nil || !strings.Contains(err.Error(), "重读") {
		t.Fatalf("外部修改后应拒绝并要求重读：%v", err)
	}
	if data, _ := os.ReadFile(p); string(data) != "v2 longer now" {
		t.Fatalf("拒绝时不得改动文件：%q", data)
	}
}

func TestEditPreservesCRLFAndBOM(t *testing.T) {
	// 字节级替换：BOM 与 CRLF 不经字符串语义，原样保留
	p := writeTempFile(t, "\xEF\xBB\xBFline1\r\nline2\r\n")
	_, et := readThenEdit(t, newFileGuard(), p)
	if _, err := runTool(t, et, editArgs(p, "line2", "line2 edited", false)); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(p)
	if want := "\xEF\xBB\xBFline1\r\nline2 edited\r\n"; string(data) != want {
		t.Fatalf("应字节级保留 BOM/CRLF：got %q want %q", data, want)
	}
}

func TestEditOldNotFound(t *testing.T) {
	p := writeTempFile(t, "abc")
	_, et := readThenEdit(t, newFileGuard(), p)
	_, err := runTool(t, et, editArgs(p, "xyz", "q", false))
	if err == nil || !strings.Contains(err.Error(), "未找到") {
		t.Fatalf("old 不存在应拒绝：%v", err)
	}
}

func TestEditEmptyOldRejected(t *testing.T) {
	p := writeTempFile(t, "abc")
	_, et := readThenEdit(t, newFileGuard(), p)
	_, err := runTool(t, et, editArgs(p, "", "q", false))
	if err == nil || !strings.Contains(err.Error(), "必填") {
		t.Fatalf("old_string 为空应拒绝：%v", err)
	}
}

func TestEditSameOldNewRejected(t *testing.T) {
	p := writeTempFile(t, "abc")
	_, et := readThenEdit(t, newFileGuard(), p)
	_, err := runTool(t, et, editArgs(p, "abc", "abc", false))
	if err == nil || !strings.Contains(err.Error(), "相同") {
		t.Fatalf("old 与 new 相同应拒绝：%v", err)
	}
}

func TestEditMissingFile(t *testing.T) {
	et := newEditFileTool(newFileGuard())
	_, err := runTool(t, et, editArgs(filepath.Join(t.TempDir(), "nope.txt"), "a", "b", false))
	if err == nil || !strings.Contains(err.Error(), "不存在") {
		t.Fatalf("文件不存在应拒绝：%v", err)
	}
}

func TestEditAfterWriteFileNoReread(t *testing.T) {
	p := writeTempFile(t, "aaa")
	g := newFileGuard()
	wt := newWriteFileTool(g)
	if _, err := runTool(t, wt, map[string]any{"file_path": p, "content": "bbb ccc"}); err != nil {
		t.Fatal(err)
	}
	et := newEditFileTool(g)
	if _, err := runTool(t, et, editArgs(p, "bbb", "ddd", false)); err != nil {
		t.Fatalf("write_file 后 edit 不应要求重读（write 登记了新指纹）：%v", err)
	}
	if data, _ := os.ReadFile(p); string(data) != "ddd ccc" {
		t.Fatalf("编辑结果不符：%q", data)
	}
}

func TestRegistryInvalidateReadHistory(t *testing.T) {
	p := writeTempFile(t, "alpha\nbeta\n")
	g := newFileGuard()
	rt := newReadFileTool(nil, g)
	et := newEditFileTool(g)
	reg := NewRegistry()
	reg.Register(rt)
	reg.Register(et)

	if _, err := runTool(t, rt, map[string]any{"file_path": p}); err != nil {
		t.Fatal(err)
	}
	if out, _ := runTool(t, rt, map[string]any{"file_path": p}); !strings.Contains(out, "未变更") {
		t.Fatalf("第二次读应去重：%q", out)
	}
	reg.InvalidateReadHistory()
	out, err := runTool(t, rt, map[string]any{"file_path": p})
	if err != nil || !strings.Contains(out, "alpha") || strings.Contains(out, "未变更") {
		t.Fatalf("失效后重读应返回真实内容：%q err=%v", out, err)
	}
	// 守卫仍认「本会话读过」：edit 直接放行（指纹保留，只清了区间）
	if _, err := runTool(t, et, editArgs(p, "alpha", "A", false)); err != nil {
		t.Fatalf("失效后 edit 守卫应放行：%v", err)
	}
}
