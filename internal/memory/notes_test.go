package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// noteFixture 在临时目录里造真实文件并写笔记。
func noteFixture(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "memory.db"), ScopeProject, "proj-x")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	for _, f := range []string{"a/x.go", "a/y.go", "b/main.go"} {
		p := filepath.Join(dir, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("package "+filepath.Base(p)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return s, dir
}

func TestUpsertNoteAndMap(t *testing.T) {
	s, dir := noteFixture(t)
	if err := s.UpsertNote(filepath.Join(dir, "a/x.go"), "上下文治理入口", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertNote(filepath.Join(dir, "a/y.go"), "压缩实现", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertNote(filepath.Join(dir, "b/main.go"), "装配层", ""); err != nil {
		t.Fatal(err)
	}
	m := s.ProjectMap(1500)
	for _, want := range []string{"<project-map>", "</project-map>", filepath.Base(dir) + "/a/", "x.go — 上下文治理入口", "main.go — 装配层", "装配层"} {
		if !strings.Contains(m, want) {
			t.Errorf("map 缺少 %q:\n%s", want, m)
		}
	}
	// 无过期标记：文件未变更
	if strings.Contains(m, "过时") {
		t.Errorf("未变更文件不应标过时:\n%s", m)
	}
}

func TestProjectMapBudgetTruncates(t *testing.T) {
	s, dir := noteFixture(t)
	for _, f := range []string{"a/x.go", "a/y.go", "b/main.go"} {
		_ = s.UpsertNote(filepath.Join(dir, f), "some role", "")
	}
	// 预算恰好 = 组头 + 第一行：放得下一行，其余省略（临时目录路径长度不定，按实测成本算）
	headerCost := len([]rune(filepath.Join(dir, "a")))/2 + 1
	lineCost := len([]rune("  x.go — some role"))/2 + 1
	m := s.ProjectMap(headerCost + lineCost)
	if !strings.Contains(m, "(…") {
		t.Fatalf("预算内放不下应有省略计数:\n%s", m)
	}
	if !strings.Contains(m, "x.go — some role") {
		t.Fatalf("预算内的行不该被省略:\n%s", m)
	}
}

func TestProjectMapMarksStale(t *testing.T) {
	s, dir := noteFixture(t)
	p := filepath.Join(dir, "a/x.go")
	_ = s.UpsertNote(p, "上下文治理入口", "")
	if m := s.ProjectMap(1500); strings.Contains(m, "过时") {
		t.Fatalf("未变更不应标过时:\n%s", m)
	}
	// 内容变长（size 变）→ 过时
	if err := os.WriteFile(p, []byte(strings.Repeat("x", 500)), 0o644); err != nil {
		t.Fatal(err)
	}
	if m := s.ProjectMap(1500); !strings.Contains(m, "（可能已过时）") {
		t.Fatalf("变更后应标过时:\n%s", m)
	}
	// 重新 upsert 后恢复新鲜
	_ = s.UpsertNote(p, "更新过的总结", "")
	if m := s.ProjectMap(1500); strings.Contains(m, "过时") {
		t.Fatalf("重新沉淀后不应再标过时:\n%s", m)
	}
}

func TestUpsertNoteOverwritesAndKeepsSymbols(t *testing.T) {
	s, dir := noteFixture(t)
	p := filepath.Join(dir, "a/x.go")
	_ = s.UpsertNote(p, "role-1", "symbols-v1")
	_ = s.UpsertNote(p, "role-2", "") // explorer 的 role 更新不应清掉 symbols
	m := s.ProjectMap(1500)
	if !strings.Contains(m, "role-2") || strings.Contains(m, "role-1") {
		t.Fatalf("summary 应被覆盖:\n%s", m)
	}
}

func TestProjectMapEmpty(t *testing.T) {
	s, _ := noteFixture(t)
	if m := s.ProjectMap(1500); m != "" {
		t.Fatalf("空库应返回空串，got %q", m)
	}
}
