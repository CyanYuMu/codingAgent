package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/bubbles/v2/textarea"

	"einoclaw-build/internal/skills"
)

func tuiSkillCatalog(t *testing.T) *skills.Manager {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, ".codeclaw", "skills", "review", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("---\nname: review\ndescription: review code\nallowed-tools: [read_file, grep]\n---\nReview carefully."), 0o644); err != nil {
		t.Fatal(err)
	}
	catalog, err := skills.NewManager(skills.Options{CWD: root})
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

func TestSkillsListAndShowCommands(t *testing.T) {
	m := teaModel{inputArea: textarea.New(), skillCatalog: tuiSkillCatalog(t), skillCommands: true}
	handled, listed := m.handleSlash("/skills")
	if !handled || !strings.Contains(strings.Join(listed.chatLines, "\n"), "review — review code") {
		t.Fatalf("list output = %q", listed.chatLines)
	}
	handled, shown := m.handleSlash("/skills show review")
	text := strings.Join(shown.chatLines, "\n")
	if !handled || !strings.Contains(text, "Skill: review") || !strings.Contains(text, "read_file, grep") {
		t.Fatalf("show output = %q", shown.chatLines)
	}
}

func TestSkillsCommandsDisabledAreHandledLocally(t *testing.T) {
	m := teaModel{inputArea: textarea.New()}
	handled, got := m.handleSlash("/skills")
	if !handled || len(got.chatLines) == 0 || !strings.Contains(got.chatLines[0], "未启用") {
		t.Fatalf("disabled command should not reach model: %+v", got.chatLines)
	}
}
