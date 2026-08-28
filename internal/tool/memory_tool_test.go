package tool

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"einoclaw-build/internal/memory"
	"einoclaw-build/internal/runtime"
)

func openStores(t *testing.T) (project, global *memory.Store) {
	t.Helper()
	dir := t.TempDir()
	p, err := memory.Open(filepath.Join(dir, "p.db"), memory.ScopeProject, "proj-1")
	if err != nil {
		t.Fatal(err)
	}
	g, err := memory.Open(filepath.Join(dir, "g.db"), memory.ScopeGlobal, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close(); g.Close() })
	return p, g
}

func runTool(t *testing.T, tl Tool, args map[string]any) (string, error) {
	t.Helper()
	sink := runtime.NewSink(4000, 4000)
	defer sink.Close()
	err := tl.Execute(context.Background(), args, sink)
	return sink.Result(), err
}

func TestRememberToolPassesFields(t *testing.T) {
	p, g := openStores(t)
	out, err := runTool(t, NewRememberTool(p, g), map[string]any{
		"content": "构建命令是 env -u GOROOT go build ./...",
		"kind":    "project", "key": "build-cmd", "why": "GOROOT 被系统装的 Go 污染了",
	})
	if err != nil || !strings.Contains(out, "已记住") {
		t.Fatalf("out=%q err=%v", out, err)
	}
	got, err := p.Recall("构建命令", 5)
	if err != nil || len(got) != 1 {
		t.Fatalf("召回 = %v err=%v", got, err)
	}
	if got[0].Kind != "project" || got[0].Key != "build-cmd" || !strings.Contains(got[0].Why, "GOROOT") {
		t.Fatalf("字段没落库：%+v", got[0])
	}
	if n, _ := g.Count(); n != 0 {
		t.Fatalf("默认作用域应是项目，全局库不该有内容：%d", n)
	}
}

func TestRememberToolGlobalScope(t *testing.T) {
	p, g := openStores(t)
	if _, err := runTool(t, NewRememberTool(p, g), map[string]any{
		"content": "用户偏好中文回复", "kind": "user", "scope": "global",
	}); err != nil {
		t.Fatal(err)
	}
	if n, _ := g.Count(); n != 1 {
		t.Fatalf("global 应落全局库：%d", n)
	}
	if n, _ := p.Count(); n != 0 {
		t.Fatalf("项目库不该有：%d", n)
	}
}

func TestRememberToolFallsBackWhenNoGlobalStore(t *testing.T) {
	p, _ := openStores(t)
	out, err := runTool(t, NewRememberTool(p, nil), map[string]any{"content": "用户偏好中文回复", "scope": "global"})
	if err != nil || !strings.Contains(out, "未启用全局库") {
		t.Fatalf("应降级到项目库并说明：out=%q err=%v", out, err)
	}
	if n, _ := p.Count(); n != 1 {
		t.Fatalf("项目库应有 1 条：%d", n)
	}
}

func TestRememberToolReportsUpdate(t *testing.T) {
	p, g := openStores(t)
	tl := NewRememberTool(p, g)
	if _, err := runTool(t, tl, map[string]any{"content": "用户偏好中文回复"}); err != nil {
		t.Fatal(err)
	}
	out, err := runTool(t, tl, map[string]any{"content": "用户偏好中文回复。"})
	if err != nil || !strings.Contains(out, "已更新已有记忆") {
		t.Fatalf("近重复应报告为更新：out=%q err=%v", out, err)
	}
}

func TestRememberToolRejectsSecret(t *testing.T) {
	p, g := openStores(t)
	_, err := runTool(t, NewRememberTool(p, g), map[string]any{"content": "线上 key 是 sk-abcdefghijklmnopqrstuvwx"})
	if err == nil || !strings.Contains(err.Error(), "拒绝写入记忆") {
		t.Fatalf("应拒绝并说明：%v", err)
	}
	if n, _ := p.Count(); n != 0 {
		t.Fatalf("不该落库：%d", n)
	}
}

func TestRememberToolRequiresContent(t *testing.T) {
	p, g := openStores(t)
	if _, err := runTool(t, NewRememberTool(p, g), map[string]any{"content": "   "}); err == nil {
		t.Fatal("空 content 应报错")
	}
}

func TestForgetTool(t *testing.T) {
	p, g := openStores(t)
	if _, err := runTool(t, NewRememberTool(p, g), map[string]any{
		"content": "构建命令是 go build ./...", "key": "build-cmd"}); err != nil {
		t.Fatal(err)
	}
	out, err := runTool(t, NewForgetTool(p, g), map[string]any{"ref": "build-cmd", "reason": "改用 makefile"})
	if err != nil || !strings.Contains(out, "已失效") {
		t.Fatalf("out=%q err=%v", out, err)
	}
	got, _ := p.Recall("构建命令", 5)
	if len(got) != 0 {
		t.Fatalf("失效后不该召回：%+v", got)
	}
	if _, err := runTool(t, NewForgetTool(p, g), map[string]any{"ref": "nope"}); err == nil {
		t.Fatal("未知 ref 应报错")
	}
}
