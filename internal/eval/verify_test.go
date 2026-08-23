package eval

import (
	"os"
	"path/filepath"
	"testing"
)

func TestVerify(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello"), 0644)

	diffs := verify(dir, map[string]string{
		"a.txt": "hello",  // 一致
		"b.txt": "missing", // 缺失
		"c.txt": "wrong",  // 缺失
	})
	if len(diffs) != 2 {
		t.Fatalf("diffs = %v, want 2 个（b.txt、c.txt）", diffs)
	}
}

func TestVerifyPass(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello"), 0644)

	if diffs := verify(dir, map[string]string{"a.txt": "hello"}); len(diffs) != 0 {
		t.Fatalf("应无 diff，got %v", diffs)
	}
}
