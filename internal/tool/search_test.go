package tool

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGrepMatches(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\nfunc foo() {}\n"), 0644)
	os.WriteFile(filepath.Join(dir, "b.go"), []byte("package b\nfunc bar() {}\n"), 0644)

	matches, err := grepMatches("foo", dir, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 || !strings.Contains(matches[0], "a.go") {
		t.Fatalf("matches = %+v", matches)
	}
}

func TestGrepInvalidPatternFallsBack(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.go"), []byte("x (bad\n"), 0644)

	matches, err := grepMatches("(bad", dir, 10)
	if err != nil {
		t.Fatalf("非法 pattern 不应报错: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("matches = %+v", matches)
	}
}
