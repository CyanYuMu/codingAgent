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

func TestRestoreReadStateRequiresMatchingCurrentBytes(t *testing.T) {
	w, err := workspace.Open(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	factory := func() (*Registry, *Executor) {
		r := NewRegistry()
		for _, tool := range ScopedBuiltins(runtime.NewBash(w.Path()), nil, w) {
			r.Register(tool)
		}
		return r, NewExecutor(r, permission.ModeYolo, nil)
	}
	path := filepath.Join(w.Path(), "file")
	if err := os.WriteFile(path, []byte("known contents"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, e := factory()
	call := message.ToolCall{ID: "read", Name: "read_file", Args: `{"file_path":"file"}`}
	res := e.Execute(context.Background(), call)
	if res.IsError {
		t.Fatal(res)
	}
	history := []message.Message{{Role: message.RoleAssistant, Blocks: []message.ContentBlock{{Kind: message.BlockToolCall, ToolCall: &call}}}, message.NewToolMessage(call.ID, call.Name, res.Content, false)}
	r, resumed := factory()
	r.RestoreReadState(history)
	if got := resumed.Execute(context.Background(), message.ToolCall{Name: "edit", Args: `{"file_path":"file","old_string":"known","new_string":"edited"}`}); got.IsError {
		t.Fatal(got)
	}
	if stringMustRead(t, path) != "edited contents" {
		t.Fatal("restored edit did not run")
	}
	// An old read does not authorize overwriting a different current file.
	r, resumed = factory()
	r.RestoreReadState(history)
	write := message.ToolCall{Name: "write_file", Args: `{"file_path":"file","content":"blind overwrite"}`}
	if got := resumed.Execute(context.Background(), write); !got.IsError {
		t.Fatal("stale hash accepted")
	}
	if stringMustRead(t, path) != "edited contents" {
		t.Fatal("stale restore overwrote file")
	}
	// Restoring a guard must not claim old read ranges remain in the prompt.
	if err := os.WriteFile(path, []byte("known contents"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, resumed = factory()
	r.RestoreReadState(history)
	if got := resumed.Execute(context.Background(), call); got.IsError || !strings.Contains(got.Content, "known contents") {
		t.Fatalf("read was incorrectly deduplicated: %+v", got)
	}
	// A tool-result-looking string without a matching assistant call grants nothing.
	r, resumed = factory()
	r.RestoreReadState(history[1:])
	if got := resumed.Execute(context.Background(), write); !got.IsError {
		t.Fatal("unpaired result granted write access")
	}
}

func stringMustRead(t *testing.T, path string) string {
	t.Helper()
	b, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	return string(b)
}
