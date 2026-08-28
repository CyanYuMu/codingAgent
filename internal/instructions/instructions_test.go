package instructions

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// tree 在临时目录里搭一棵文件树；键是相对路径。
func tree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	if real, err := filepath.EvalSymlinks(root); err == nil {
		root = real
	}
	for rel, content := range files {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestLoadHierarchyOrderAncestorsFirst(t *testing.T) {
	root := tree(t, map[string]string{
		".git/HEAD":              "ref: refs/heads/main",
		"AGENTS.md":              "仓库级约定",
		"packages/api/AGENTS.md": "api 包级约定",
	})
	b, err := Load(filepath.Join(root, "packages", "api"), "", 100000)
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Files) != 2 {
		t.Fatalf("应加载两级：%+v", paths(b))
	}
	if !strings.HasSuffix(b.Files[0].Path, filepath.Join(root, "AGENTS.md")) {
		t.Fatalf("祖先应在前：%v", paths(b))
	}
	if i, j := strings.Index(b.Text, "仓库级约定"), strings.Index(b.Text, "api 包级约定"); i < 0 || j < 0 || i > j {
		t.Fatalf("渲染顺序应是祖先在前、近者在后（近者更强）：%s", b.Text)
	}
	if !strings.Contains(b.Text, `path="`+filepath.Join(root, "AGENTS.md")+`"`) {
		t.Fatalf("应带绝对路径，模型才知道规则来自哪：%s", b.Text)
	}
}

func TestAgentsMdWinsOverClaudeMdAtSameLevel(t *testing.T) {
	root := tree(t, map[string]string{
		".git/HEAD": "ref: refs/heads/main",
		"AGENTS.md": "原生约定",
		"CLAUDE.md": "别的 harness 的约定",
	})
	b, _ := Load(root, "", 100000)
	if len(b.Files) != 1 || !strings.Contains(b.Text, "原生约定") || strings.Contains(b.Text, "别的 harness") {
		t.Fatalf("同一级应只取优先级最高的一个：%v", paths(b))
	}
}

func TestUserLevelComesFirst(t *testing.T) {
	home := tree(t, map[string]string{"AGENTS.md": "用户级偏好"})
	root := tree(t, map[string]string{".git/HEAD": "x", "AGENTS.md": "项目级约定"})
	b, _ := Load(root, home, 100000)
	if len(b.Files) != 2 {
		t.Fatalf("应含用户级与项目级：%v", paths(b))
	}
	if i, j := strings.Index(b.Text, "用户级偏好"), strings.Index(b.Text, "项目级约定"); i > j {
		t.Fatal("用户级应在最前（项目级更近、更强）")
	}
}

func TestImportExpansion(t *testing.T) {
	root := tree(t, map[string]string{
		".git/HEAD":            "x",
		"AGENTS.md":            "改存储前先读 @docs/architecture.md 再动手。",
		"docs/architecture.md": "存储层是追加式日志。",
	})
	b, _ := Load(root, "", 100000)
	if !strings.Contains(b.Text, "存储层是追加式日志") {
		t.Fatalf("@import 应被展开：%s", b.Text)
	}
}

func TestImportRelativeToImportingFile(t *testing.T) {
	root := tree(t, map[string]string{
		".git/HEAD":     "x",
		"pkg/AGENTS.md": "见 @notes.md",
		"pkg/notes.md":  "包内笔记",
		"notes.md":      "仓库根笔记",
	})
	b, _ := Load(filepath.Join(root, "pkg"), "", 100000)
	if !strings.Contains(b.Text, "包内笔记") || strings.Contains(b.Text, "仓库根笔记") {
		t.Fatalf("相对路径应相对导入者所在目录：%s", b.Text)
	}
}

func TestImportDepthLimitAndCycle(t *testing.T) {
	files := map[string]string{".git/HEAD": "x", "AGENTS.md": "开始 @a1.md"}
	for i := 1; i <= 8; i++ {
		files[filepath.Join(".", "a"+itoa(i)+".md")] = "层" + itoa(i) + " @a" + itoa(i+1) + ".md"
	}
	root := tree(t, files)
	b, _ := Load(root, "", 100000)
	if !strings.Contains(b.Text, "层1") {
		t.Fatalf("第一跳应展开：%s", b.Text)
	}
	if strings.Contains(b.Text, "层7") {
		t.Fatalf("超过 %d 跳应停止展开：%s", maxImportHops, b.Text)
	}
}

func TestImportCycleTerminates(t *testing.T) {
	root := tree(t, map[string]string{
		".git/HEAD": "x",
		"AGENTS.md": "见 @a.md",
		"a.md":      "A 内容 @b.md",
		"b.md":      "B 内容 @a.md",
	})
	done := make(chan Block, 1)
	go func() { b, _ := Load(root, "", 100000); done <- b }()
	select {
	case b := <-done:
		if !strings.Contains(b.Text, "A 内容") || !strings.Contains(b.Text, "B 内容") {
			t.Fatalf("互相导入应各展开一次：%s", b.Text)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("互相导入把展开卡死了")
	}
}

func TestImportSkippedInCodeBlocks(t *testing.T) {
	root := tree(t, map[string]string{
		".git/HEAD": "x",
		"AGENTS.md": "正文 @real.md\n\n```\n示例里写 @fake.md 不该展开\n```\n\n行内 `@fake.md` 也不展开。",
		"real.md":   "真的被导入了",
		"fake.md":   "不该出现",
	})
	b, _ := Load(root, "", 100000)
	if !strings.Contains(b.Text, "真的被导入了") {
		t.Fatalf("正常导入应生效：%s", b.Text)
	}
	if strings.Contains(b.Text, "不该出现") {
		t.Fatalf("代码块与行内代码里的 @ 不该展开：%s", b.Text)
	}
}

func TestImportIgnoresEmailAndGitURL(t *testing.T) {
	root := tree(t, map[string]string{
		".git/HEAD": "x",
		"AGENTS.md": "联系 a@b.com，仓库 git@github.com:o/r.git",
	})
	b, _ := Load(root, "", 100000)
	if !strings.Contains(b.Text, "a@b.com") || !strings.Contains(b.Text, "git@github.com:o/r.git") {
		t.Fatalf("邮箱与 git URL 应原样保留：%s", b.Text)
	}
}

func TestMissingImportKeptLiteral(t *testing.T) {
	root := tree(t, map[string]string{".git/HEAD": "x", "AGENTS.md": "见 @nope.md 。"})
	b, _ := Load(root, "", 100000)
	if !strings.Contains(b.Text, "@nope.md") {
		t.Fatalf("导入目标不存在时应原样保留：%s", b.Text)
	}
}

func TestImportTrimsTrailingPunctuation(t *testing.T) {
	root := tree(t, map[string]string{
		".git/HEAD": "x",
		"AGENTS.md": "详见 @docs/x.md。",
		"docs/x.md": "被导入的内容",
	})
	b, _ := Load(root, "", 100000)
	if !strings.Contains(b.Text, "被导入的内容") {
		t.Fatalf("路径尾部的句号不该算进文件名：%s", b.Text)
	}
}

func TestRulesAreStickyAndLast(t *testing.T) {
	root := tree(t, map[string]string{
		".git/HEAD": "x",
		"AGENTS.md": "普通约定",
		"RULES.md":  "绝不提交到 main",
	})
	b, _ := Load(root, "", 100000)
	var sticky int
	for _, f := range b.Files {
		if f.Sticky {
			sticky++
		}
	}
	if sticky != 1 {
		t.Fatalf("RULES.md 应标为粘性：%+v", paths(b))
	}
	if i, j := strings.Index(b.Text, "普通约定"), strings.Index(b.Text, "绝不提交到 main"); i < 0 || j < 0 || i > j {
		t.Fatal("粘性规则应渲染在最后")
	}
	if !strings.Contains(b.Text, "<sticky-rules") {
		t.Fatalf("粘性规则要能被认出来：%s", b.Text)
	}
}

func TestBudgetDropsFarthestAncestorsFirst(t *testing.T) {
	root := tree(t, map[string]string{
		".git/HEAD":     "x",
		"AGENTS.md":     strings.Repeat("祖先", 200),
		"pkg/AGENTS.md": "近处的约定",
		"RULES.md":      "粘性规则",
	})
	b, _ := Load(filepath.Join(root, "pkg"), "", 300)
	if !strings.Contains(b.Text, "近处的约定") || !strings.Contains(b.Text, "粘性规则") {
		t.Fatalf("超预算时应先丢最远的祖先，保留近者与粘性规则：%s", b.Text)
	}
	if !strings.Contains(b.Text, "省略") {
		t.Fatalf("被裁掉的部分要有交代：%s", b.Text)
	}
}

func TestNoFilesGivesEmptyBlock(t *testing.T) {
	b, err := Load(t.TempDir(), "", 100000)
	if err != nil || b.Text != "" || len(b.Files) != 0 {
		t.Fatalf("没有指令文件时应返回空块：%+v err=%v", b, err)
	}
}

// ---- 测试小工具 ----

func paths(b Block) []string {
	out := make([]string, len(b.Files))
	for i, f := range b.Files {
		out[i] = f.Path
	}
	return out
}

func itoa(n int) string { return strconv.Itoa(n) }
