package tool

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"einoclaw-build/internal/permission"
	"einoclaw-build/internal/skills"
)

func TestSkillToolLoadsPromptAsReadTier(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, ".codeclaw", "skills", "review", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("---\nname: review\ndescription: review code\n---\nCheck correctness."), 0o644); err != nil {
		t.Fatal(err)
	}
	catalog, err := skills.NewManager(skills.Options{CWD: root})
	if err != nil {
		t.Fatal(err)
	}
	tool := NewSkillTool(catalog)
	if tool.Tier() != permission.TierRead {
		t.Fatalf("tier = %v", tool.Tier())
	}
	out, err := runTool(t, tool, map[string]any{"name": "review", "args": "auth only"})
	if err != nil || !strings.Contains(out, "Check correctness.") || !strings.Contains(out, "User: auth only") {
		t.Fatalf("output=%q err=%v", out, err)
	}
}
