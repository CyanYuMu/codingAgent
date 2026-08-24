package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSinkNoTruncation(t *testing.T) {
	s := NewSink(100, 100)
	defer s.Close()
	s.Write([]byte("short"))
	if s.Result() != "short" || s.Truncated() {
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
	s.SetArtifactStore(NewArtifactStore(dir), "bash")
	defer s.Close()
	s.Write([]byte("0123456789abcdef"))
	r := s.Result()
	if !strings.Contains(r, "artifact://0") {
		t.Fatalf("Result 缺少 artifact 指针: %q", r)
	}
	files, _ := os.ReadDir(dir)
	if len(files) != 1 || files[0].Name() != "0.bash.log" {
		t.Fatalf("artifact 文件 = %v", files)
	}
	// artifact 内容应完整
	data, _ := os.ReadFile(filepath.Join(dir, files[0].Name()))
	if string(data) != "0123456789abcdef" {
		t.Fatalf("artifact 内容 = %q", data)
	}
}

func TestSinkNoArtifactStoreNoPanic(t *testing.T) {
	s := NewSink(4, 4)
	defer s.Close()
	s.Write([]byte("0123456789abcdef")) // 16 bytes > 8 窗口
	r := s.Result()
	if !strings.Contains(r, "elided") {
		t.Fatalf("应截断，got %q", r)
	}
	if strings.Contains(r, "artifact://") {
		t.Fatalf("未配置存储不应有 artifact 指针，got %q", r)
	}
}

func TestArtifactStoreAllocatesAfterExisting(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "7.bash.log"), []byte("x"), 0o644)
	s := NewArtifactStore(dir)
	id, f, err := s.Create("grep")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if id != "8" || filepath.Base(f.Name()) != "8.grep.log" {
		t.Fatalf("id=%s name=%s", id, f.Name())
	}
	p, err := s.Resolve("artifact://8")
	if err != nil || !strings.HasSuffix(p, "8.grep.log") {
		t.Fatalf("resolve = %q err %v", p, err)
	}
	if _, err := s.Resolve("artifact://99"); err == nil {
		t.Fatal("missing artifact should error")
	}
	if _, err := s.Resolve("artifact://../x"); err == nil {
		t.Fatal("non-numeric id should error")
	}
	id2, f2, _ := s.Create("we ird/tool")
	f2.Close()
	if id2 != "9" || !strings.HasSuffix(f2.Name(), "9.we_ird_tool.log") {
		t.Fatalf("id2=%s name=%s", id2, f2.Name())
	}
}

func TestSinkSpillsToArtifactStore(t *testing.T) {
	s := NewArtifactStore(t.TempDir())
	sink := NewSink(10, 10)
	sink.SetArtifactStore(s, "bash")
	sink.Write([]byte(strings.Repeat("a", 50)))
	sink.Write([]byte(strings.Repeat("b", 50)))
	out := sink.Result()
	sink.Close()
	if !strings.Contains(out, "artifact://0") || sink.ArtifactID() != "0" {
		t.Fatalf("result = %q", out)
	}
	p, _ := s.Resolve("0")
	b, _ := os.ReadFile(p)
	if len(b) != 100 {
		t.Fatalf("artifact bytes = %d, want 100", len(b))
	}
}

func TestArtifactStoreSchemeDispatch(t *testing.T) {
	s := NewArtifactStore(t.TempDir())
	id, f, err := s.Create("bash")
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("hello")
	f.Close()

	s.AddScheme("agent", func(rest string) (string, error) {
		if rest != "Reviewer" {
			return "", fmt.Errorf("no such agent %q", rest)
		}
		return "/tmp/Reviewer.md", nil
	})

	if p, err := s.Resolve("artifact://" + id); err != nil || !strings.HasSuffix(p, id+".bash.log") {
		t.Fatalf("artifact:// = %q err %v", p, err)
	}
	if p, err := s.Resolve(id); err != nil || !strings.HasSuffix(p, id+".bash.log") {
		t.Fatalf("裸 id 应保持兼容：%q err %v", p, err)
	}
	if p, err := s.Resolve("agent://Reviewer"); err != nil || p != "/tmp/Reviewer.md" {
		t.Fatalf("agent:// = %q err %v", p, err)
	}
	if _, err := s.Resolve("agent://Nope"); err == nil {
		t.Fatal("未知 agent 应报错")
	}
	_, err = s.Resolve("memory://3")
	if err == nil || !strings.Contains(err.Error(), "agent") {
		t.Fatalf("未注册方案的错误应列出可用方案：%v", err)
	}
}
