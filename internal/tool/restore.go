package tool

import (
	"encoding/json"
	"regexp"
	"strings"

	"einoclaw-build/internal/message"
	"einoclaw-build/internal/workspace"
)

var observedHashLine = regexp.MustCompile(`(?m)^\[sha256=([a-f0-9]{64})\]$`)

func (t *readFileTool) RestoreReadState(history []message.Message) {
	if t.guard.workspace == nil {
		return
	}
	calls := map[string]message.ToolCall{}
	type observed struct{ path, hash string }
	var candidates []observed
	for _, m := range history {
		for _, b := range m.Blocks {
			if m.Role == message.RoleAssistant && b.ToolCall != nil {
				calls[b.ToolCall.ID] = *b.ToolCall
			}
			r := b.ToolResult
			if m.Role != message.RoleTool || r == nil || r.IsError || r.Name != "read_file" {
				continue
			}
			call, ok := calls[r.ToolCallID]
			if !ok || call.Name != "read_file" {
				continue
			}
			var args struct {
				Path string `json:"file_path"`
			}
			if json.Unmarshal([]byte(call.Args), &args) != nil || args.Path == "" || strings.Contains(args.Path, "://") {
				continue
			}
			matches := observedHashLine.FindAllStringSubmatch(r.Content, -1)
			if len(matches) == 0 {
				continue
			}
			path, err := t.guard.workspace.Rel(args.Path)
			if err == nil {
				candidates = append(candidates, observed{path, matches[len(matches)-1][1]})
			}
		}
	}
	seen := map[string]bool{}
	for i := len(candidates) - 1; i >= 0 && len(seen) < 128; i-- {
		c := candidates[i]
		if seen[c.path] {
			continue
		}
		seen[c.path] = true
		data, err := t.guard.workspace.ReadFile(c.path)
		if err != nil || workspace.Hash(data) != c.hash {
			continue
		}
		st, err := t.guard.workspace.Stat(c.path)
		if err != nil {
			continue
		}
		t.guard.observe(c.path, data)
		t.guard.markWritten(c.path, st.ModTime().UnixNano(), st.Size())
	}
}
