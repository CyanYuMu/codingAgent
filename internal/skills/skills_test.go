package skills

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func writeSkill(t *testing.T, root, dir, frontmatter, body string) string {
	t.Helper()
	path := filepath.Join(root, dir, "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\n" + frontmatter + "\n---\n\n" + body + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestParseSkillDefaultsAndMetadata(t *testing.T) {
	root := t.TempDir()
	path := writeSkill(t, root, "go-debug", "description: |\n  Debug failing Go tests\n  without guessing.\nallowed-tools: read_file, grep\n", "Follow the failure.")
	skill, enabled, err := skillFromFile(path, Source{Kind: "project"}, DefaultMaxFileBytes)
	if err != nil || !enabled {
		t.Fatalf("skillFromFile: enabled=%v err=%v", enabled, err)
	}
	if skill.Name != "go-debug" || skill.Description != "Debug failing Go tests without guessing." {
		t.Fatalf("metadata = %+v", skill)
	}
	if !skill.UserInvocable || strings.Join(skill.AllowedTools, ",") != "read_file,grep" {
		t.Fatalf("invocation metadata = %+v", skill)
	}
}

func TestParseSkillDisabledAndInvalid(t *testing.T) {
	root := t.TempDir()
	disabled := writeSkill(t, root, "off", "name: off\ndescription: disabled\nenabled: false", "Do not load.")
	if _, enabled, err := skillFromFile(disabled, Source{}, DefaultMaxFileBytes); err != nil || enabled {
		t.Fatalf("disabled skill should be skipped: enabled=%v err=%v", enabled, err)
	}
	bad := writeSkill(t, root, "bad", "name: ../bad\ndescription: bad", "Bad.")
	if _, _, err := skillFromFile(bad, Source{}, DefaultMaxFileBytes); err == nil {
		t.Fatal("unsafe name should fail")
	}
}

func TestDiscoveryPriorityStableOrderAndGitBoundary(t *testing.T) {
	outer := t.TempDir()
	repo := filepath.Join(outer, "repo")
	cwd := filepath.Join(repo, "pkg")
	userRoot := filepath.Join(outer, "codeclaw-home")
	for _, dir := range []string{filepath.Join(repo, ".git"), cwd} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeSkill(t, filepath.Join(outer, ".codeclaw", "skills"), "rogue", "description: must not cross git boundary", "Rogue.")
	writeSkill(t, filepath.Join(repo, ".codeclaw", "skills"), "same", "description: repository copy", "Repo.")
	writeSkill(t, filepath.Join(cwd, ".codeclaw", "skills"), "same", "description: nearest copy", "Nearest.")
	writeSkill(t, filepath.Join(userRoot, "skills"), "same", "description: user copy", "User.")
	writeSkill(t, filepath.Join(userRoot, "skills"), "Zulu", "description: zed", "Zed.")
	writeSkill(t, filepath.Join(userRoot, "skills"), "alpha", "description: alpha", "Alpha.")

	mgr, err := NewManager(Options{CWD: cwd, UserSkillsDir: filepath.Join(userRoot, "skills")})
	if err != nil {
		t.Fatal(err)
	}
	got, ok := mgr.Get("SAME")
	if !ok || got.Description != "nearest copy" {
		t.Fatalf("nearest project skill should win: %+v", got)
	}
	if _, ok := mgr.Get("rogue"); ok {
		t.Fatal("skill above git boundary leaked into catalog")
	}
	list := mgr.List()
	if len(list) != 3 || list[0].Name != "alpha" || list[1].Name != "same" || list[2].Name != "Zulu" {
		t.Fatalf("unstable order: %+v", list)
	}
	if len(mgr.Warnings()) == 0 {
		t.Fatal("shadowed skills should produce warnings")
	}
}

func TestDiscoveryIncludeIgnoreAndCompatibilityOptIn(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeSkill(t, filepath.Join(root, ".codeclaw", "skills"), "keep-one", "description: keep", "Keep.")
	writeSkill(t, filepath.Join(root, ".codeclaw", "skills"), "keep-secret", "description: ignored", "Ignore.")
	writeSkill(t, filepath.Join(root, ".claude", "skills"), "claude-one", "description: compatible", "Claude.")

	mgr, err := NewManager(Options{CWD: root, Include: []string{"keep-*", "claude-*"}, Ignore: []string{"*-secret"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(mgr.List()) != 1 || mgr.List()[0].Name != "keep-one" {
		t.Fatalf("unexpected default filters: %+v", mgr.List())
	}
	mgr, err = NewManager(Options{CWD: root, Compatibility: Compatibility{Claude: true}})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := mgr.Get("claude-one"); !ok {
		t.Fatal("opted-in Claude skill was not discovered")
	}
}

func TestRenderIndexAndInvocationGates(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeSkill(t, filepath.Join(root, ".codeclaw", "skills"), "visible", "description: use for <Go> tests", "Visible body.")
	writeSkill(t, filepath.Join(root, ".codeclaw", "skills"), "manual", "description: manual only\ndisable-model-invocation: true", "Manual body.")
	writeSkill(t, filepath.Join(root, ".codeclaw", "skills"), "model-only", "description: model only\nuser-invocable: false", "Model body.")
	mgr, err := NewManager(Options{CWD: root})
	if err != nil {
		t.Fatal(err)
	}
	index := mgr.RenderIndex()
	if !strings.Contains(index, "visible") || !strings.Contains(index, "&lt;Go&gt;") || strings.Contains(index, "manual only") {
		t.Fatalf("bad index: %s", index)
	}
	if _, _, err := mgr.BuildPrompt("manual", "", InvocationModel); err == nil {
		t.Fatal("model should not invoke manual skill")
	}
	prompt, _, err := mgr.BuildPrompt("manual", "focus", InvocationUser)
	if err != nil || !strings.Contains(prompt, "Manual body.") || !strings.Contains(prompt, "User: focus") {
		t.Fatalf("manual user invocation failed: %q err=%v", prompt, err)
	}
	if _, _, err := mgr.BuildPrompt("model-only", "", InvocationUser); err == nil {
		t.Fatal("user should not invoke model-only skill")
	}
}

func TestResolveContainedResources(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	skillRoot := filepath.Join(root, ".codeclaw", "skills")
	writeSkill(t, skillRoot, "demo", "description: demo resources", "Read templates/x.txt.")
	resource := filepath.Join(skillRoot, "demo", "templates", "x.txt")
	if err := os.MkdirAll(filepath.Dir(resource), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(resource, []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "outside.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	mgr, err := NewManager(Options{CWD: root})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := mgr.Resolve("demo/templates/x.txt")
	want, _ := filepath.EvalSymlinks(resource)
	if err != nil || resolved != want {
		t.Fatalf("resolve = %q err=%v", resolved, err)
	}
	for _, ref := range []string{"demo/../outside.txt", "demo/%2e%2e/outside.txt", "demo//etc/passwd"} {
		if _, err := mgr.Resolve(ref); err == nil {
			t.Fatalf("unsafe ref accepted: %s", ref)
		}
	}
	link := filepath.Join(skillRoot, "demo", "escape.txt")
	if err := os.Symlink(outside, link); err == nil {
		if _, err := mgr.Resolve("demo/escape.txt"); err == nil {
			t.Fatal("symlink escape accepted")
		}
	}
}

func TestReloadAtomicallyRefreshesCatalog(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	mgr, err := NewManager(Options{CWD: root})
	if err != nil {
		t.Fatal(err)
	}
	if len(mgr.List()) != 0 {
		t.Fatal("new catalog should be empty")
	}
	writeSkill(t, filepath.Join(root, ".codeclaw", "skills"), "new-one", "description: new", "New.")
	if _, err := mgr.Reload(); err != nil {
		t.Fatal(err)
	}
	if _, ok := mgr.Get("new-one"); !ok {
		t.Fatal("reload did not publish new snapshot")
	}
}

func TestConcurrentReadersAndReload(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeSkill(t, filepath.Join(root, ".codeclaw", "skills"), "stable", "description: stable", "Stable.")
	mgr, err := NewManager(Options{CWD: root})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				_, _ = mgr.Get("stable")
				_ = mgr.List()
				_ = mgr.RenderIndex()
			}
		}()
	}
	for range 20 {
		if _, err := mgr.Reload(); err != nil {
			t.Fatal(err)
		}
	}
	wg.Wait()
}

func TestParseInvocation(t *testing.T) {
	name, args, ok := ParseInvocation("  /skill:go-debug focus on race  ")
	if !ok || name != "go-debug" || args != "focus on race" {
		t.Fatalf("got name=%q args=%q ok=%v", name, args, ok)
	}
	if _, _, ok := ParseInvocation("/skills list"); ok {
		t.Fatal("/skills must not parse as invocation")
	}
}
