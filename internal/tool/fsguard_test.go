package tool

import (
	"os"
	"path/filepath"
	"testing"
)

// statOf 返回文件当前指纹 (mtime, size)。
func statOf(t *testing.T, p string) (int64, int64) {
	t.Helper()
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return st.ModTime().UnixNano(), st.Size()
}

func TestFileGuardFreshRead(t *testing.T) {
	p := writeTempFile(t, "v1")
	g := newFileGuard()
	m, s := statOf(t, p)
	if g.freshRead(p, m, s) {
		t.Fatal("未登记的文件不应通过守卫")
	}
	g.recordRead(p, m, s, 1, 1)
	if !g.freshRead(p, m, s) {
		t.Fatal("登记过且指纹一致应通过守卫")
	}
	// 外部修改：改长度保证指纹必然变化（不依赖时间戳分辨率）
	if err := os.WriteFile(p, []byte("v2 longer now"), 0o644); err != nil {
		t.Fatal(err)
	}
	m2, s2 := statOf(t, p)
	if g.freshRead(p, m2, s2) {
		t.Fatal("文件被外部修改后不应通过守卫（须重读）")
	}
}

func TestFileGuardMarkWritten(t *testing.T) {
	p := writeTempFile(t, "v1")
	g := newFileGuard()
	m, s := statOf(t, p)
	g.recordRead(p, m, s, 1, 1)
	if err := os.WriteFile(p, []byte("written content longer"), 0o644); err != nil {
		t.Fatal(err)
	}
	m2, s2 := statOf(t, p)
	g.markWritten(p, m2, s2)
	if !g.freshRead(p, m2, s2) {
		t.Fatal("写后应通过守卫（write 登记新指纹，后续 edit 不再要求重读）")
	}
	if g.alreadyRead(p, m2, s2, 1, 1) {
		t.Fatal("写后旧已读区间应失效：read 需真读新内容")
	}
}

func TestFileGuardInvalidateReadsKeepsFingerprint(t *testing.T) {
	p := writeTempFile(t, "v1")
	g := newFileGuard()
	m, s := statOf(t, p)
	g.recordRead(p, m, s, 1, 1)
	g.invalidateReads()
	if g.alreadyRead(p, m, s, 1, 1) {
		t.Fatal("失效后去重不应命中")
	}
	if !g.freshRead(p, m, s) {
		t.Fatal("失效只清已读区间：edit 守卫仍认「本会话读过」")
	}
}

func TestFileGuardReset(t *testing.T) {
	p := writeTempFile(t, "v1")
	g := newFileGuard()
	m, s := statOf(t, p)
	g.recordRead(p, m, s, 1, 1)
	g.reset()
	if g.freshRead(p, m, s) || g.alreadyRead(p, m, s, 1, 1) {
		t.Fatal("reset 后应全部失效")
	}
}

func TestFileGuardCleansPathKey(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(p, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	g := newFileGuard()
	m, s := statOf(t, p)
	g.recordRead(dir+"/./f.txt", m, s, 1, 1) // 未归一形式
	if !g.freshRead(p, m, s) {
		t.Fatal("路径键应 Clean 归一：./f.txt 与 f.txt 视为同一文件")
	}
}
