package tool

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"einoclaw-build/internal/message"
	"einoclaw-build/internal/permission"
	"einoclaw-build/internal/runtime"
	"einoclaw-build/internal/workspace"
)

func TestScopedFileToolsAndBashCWD(t *testing.T) {
	w, err := workspace.Open(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	s, err := runtime.NewProcessSandbox(w.Path(), runtime.SandboxConfig{Mode: "off"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	b := runtime.NewBash(w.Path())
	b.SetSandbox(s)
	r := NewRegistry()
	for _, tool := range ScopedBuiltins(b, nil, w) {
		r.Register(tool)
	}
	e := NewExecutor(r, permission.ModeYolo, nil)
	call := func(name, args string) Result {
		return e.Execute(context.Background(), message.ToolCall{Name: name, Args: args})
	}
	os.WriteFile(filepath.Join(w.Path(), "file"), []byte("old"), 0o644)
	if !call("write_file", `{"file_path":"file","content":"overwrite"}`).IsError {
		t.Fatal("blind overwrite accepted")
	}
	if got := call("read_file", `{"file_path":"file"}`); got.IsError || !strings.Contains(got.Content, "sha256=") {
		t.Fatal(got)
	}
	if got := call("edit", `{"file_path":"file","old_string":"old","new_string":"new"}`); got.IsError {
		t.Fatal(got)
	}
	if !call("write_file", `{"file_path":"file","content":`).IsError {
		t.Fatal("invalid JSON executed")
	}
	if !call("write_file", `{"file_path":"file"}`).IsError {
		t.Fatal("missing content erased file")
	}
	if !call("write_file", `{"file_path":"../escape","content":"bad"}`).IsError {
		t.Fatal("yolo bypassed workspace")
	}
	if got := call("bash", `{"command":"mkdir -p 'with space'; cd 'with space' && pwd"}`); got.IsError {
		t.Fatal(got)
	}
	if b.CWD() != w.Path() {
		t.Fatal("shell-local cd leaked to file tool cwd")
	}
	if got := call("glob", `{"pattern":"**/file"}`); got.IsError || !strings.Contains(got.Content, "file") {
		t.Fatal(got)
	}
	h, _ := w.History()
	if len(h) != 1 {
		t.Fatalf("journal=%+v", h)
	}
	if _, err = w.Undo(h[0].ID); err != nil {
		t.Fatal(err)
	}
	data, _ := w.ReadFile("file")
	if string(data) != "old" {
		t.Fatal(string(data))
	}
}
