package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSinkNoTruncation(t *testing.T) {
	s := NewSink(100, 100)
	defer s.Close()
	s.Write([]byte("short"))
	if s.Result() != "short" {
		t.Fatalf("Result = %q", s.Result())
	}
}

func TestSinkTruncatesHeadTail(t *testing.T) {
	s := NewSink(4, 4)
	defer s.Close()
	s.Write([]byte("0123456789abcdef")) // 16 bytes，头4尾4，中间8被 elide
	r := s.Result()
	if !strings.Contains(r, "0123") || !strings.Contains(r, "cdef") {
		t.Fatalf("Result = %q", r)
	}
	if !strings.Contains(r, "elided") {
		t.Fatalf("Result 缺少 elided 标记: %q", r)
	}
}

func TestSinkOffloadsArtifact(t *testing.T) {
	dir := t.TempDir()
	s := NewSink(4, 4)
	s.SetArtifactDir(dir)
	defer s.Close()
	s.Write([]byte("0123456789abcdef"))
	r := s.Result()
	if !strings.Contains(r, "artifact://") {
		t.Fatalf("Result 缺少 artifact 指针: %q", r)
	}
	files, _ := os.ReadDir(dir)
	if len(files) != 1 {
		t.Fatalf("artifact 文件数 = %d, want 1", len(files))
	}
	// artifact 内容应完整
	data, _ := os.ReadFile(filepath.Join(dir, files[0].Name()))
	if string(data) != "0123456789abcdef" {
		t.Fatalf("artifact 内容 = %q", data)
	}
}
