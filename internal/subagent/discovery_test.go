package subagent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const fullDef = `---
name: reviewer
description: 代码审查
when_to_use: 改完验收
tools: read_file, glob , grep
spawns: [worker, explorer]
model: "@fast"
read_only: true
blocking: true
max_turns: 12
soft_budget: 70
timeout: 90s
schema_mode: strict
output:
  type: object
  properties:
    verdict: {type: string}
  required: [verdict]
---
你是审查者。
第二行正文。
`

func TestParseFullFrontmatter(t *testing.T) {
	d, err := ParseAgentFile("x.md", []byte(fullDef), "project")
	if err != nil {
		t.Fatal(err)
	}
	if d.Name != "reviewer" || d.Description != "代码审查" || d.WhenToUse != "改完验收" {
		t.Fatalf("头部字段 = %+v", d)
	}
	if strings.Join(d.Tools, ",") != "read_file,glob,grep" {
		t.Fatalf("tools = %v（CSV 应 trim 后切分）", d.Tools)
	}
	if strings.Join(d.Spawns, ",") != "worker,explorer" {
		t.Fatalf("spawns = %v", d.Spawns)
	}
	if !d.ReadOnly || !d.Blocking || d.MaxTurns != 12 || d.SoftBudget != 70 || d.Timeout != 90*time.Second {
		t.Fatalf("标量字段 = %+v", d)
	}
	if d.EffectiveSchemaMode() != SchemaModeStrict {
		t.Fatalf("schema mode = %q", d.EffectiveSchemaMode())
	}
	props, _ := d.OutputSchema["properties"].(map[string]any)
	if props["verdict"] == nil {
		t.Fatalf("output schema = %+v（嵌套 map 应解析成 map[string]any）", d.OutputSchema)
	}
	if d.SystemPrompt != "你是审查者。\n第二行正文。" {
		t.Fatalf("正文 = %q", d.SystemPrompt)
	}
	if d.Source != "project" || d.FilePath != "x.md" {
		t.Fatalf("来源 = %q %q", d.Source, d.FilePath)
	}
}

func TestParseRejectsIncomplete(t *testing.T) {
	cases := map[string]string{
		"缺 frontmatter": "name: a\n正文",
		"缺 name":        "---\ndescription: d\n---\n正文",
		"缺 description": "---\nname: a\n---\n正文",
		"缺正文":           "---\nname: a\ndescription: d\n---\n",
		"坏 YAML":        "---\nname: [unclosed\n---\n正文",
		"无结束分隔":         "---\nname: a\ndescription: d\n",
	}
	for label, src := range cases {
		if _, err := ParseAgentFile("bad.md", []byte(src), "user"); err == nil {
			t.Fatalf("%s：应报错", label)
		}
	}
}

func TestParseBadDurationDegradesOnly(t *testing.T) {
	d, err := ParseAgentFile("x.md", []byte("---\nname: a\ndescription: d\ntimeout: 十分钟\n---\n正文"), "user")
	if err != nil {
		t.Fatalf("单个字段格式错不该废掉整个定义: %v", err)
	}
	if d.Timeout != 0 {
		t.Fatalf("timeout = %v，应降级为 0（用配置默认值）", d.Timeout)
	}
}

func TestDiscoverPrecedenceAndWarns(t *testing.T) {
	proj, user := t.TempDir(), t.TempDir()
	write := func(dir, name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(proj, "reviewer.md", "---\nname: reviewer\ndescription: 项目版\n---\np")
	write(user, "reviewer.md", "---\nname: reviewer\ndescription: 用户版\n---\nu")
	write(user, "helper.md", "---\nname: helper\ndescription: 用户独有\n---\nu")
	write(user, "broken.md", "---\nname: \n---\n")

	res := Discover(proj, user, []AgentDef{
		{Name: "reviewer", Description: "内置版", SystemPrompt: "b", Source: "bundled"},
		{Name: "worker", Description: "内置", SystemPrompt: "b", Source: "bundled"},
	})
	got := map[string]AgentDef{}
	for _, d := range res.Defs {
		got[d.Name] = d
	}
	if len(res.Defs) != 3 {
		t.Fatalf("defs = %d: %+v", len(res.Defs), defNames(res.Defs))
	}
	if got["reviewer"].Description != "项目版" || got["reviewer"].Source != "project" {
		t.Fatalf("项目定义应覆盖用户与内置：%+v", got["reviewer"])
	}
	if got["helper"].Source != "user" || got["worker"].Source != "bundled" {
		t.Fatalf("来源标记错：%+v", got)
	}
	if len(res.Warns) != 1 {
		t.Fatalf("坏文件应只告警一条，got %v", res.Warns)
	}
}

func TestDiscoverMissingDirsIsNotAnError(t *testing.T) {
	res := Discover(filepath.Join(t.TempDir(), "nope"), "", []AgentDef{{Name: "a", Description: "d", SystemPrompt: "p"}})
	if len(res.Defs) != 1 || len(res.Warns) != 0 {
		t.Fatalf("res = %+v", res)
	}
}

func TestBundledParses(t *testing.T) {
	defs := Bundled()
	if len(defs) != 4 {
		t.Fatalf("内置 agent 数 = %d: %v", len(defs), defNames(defs))
	}
	byName := map[string]AgentDef{}
	for _, d := range defs {
		if d.SystemPrompt == "" || d.Description == "" || d.Source != "bundled" {
			t.Fatalf("内置定义不完整: %+v", d)
		}
		byName[d.Name] = d
	}
	for _, n := range []string{"explorer", "reviewer", "planner", "worker"} {
		d, ok := byName[n]
		if !ok {
			t.Fatalf("缺内置 agent %s", n)
		}
		if d.OutputSchema == nil {
			t.Fatalf("%s 应带 outputSchema", n)
		}
	}
	if !byName["explorer"].ReadOnly || !byName["reviewer"].ReadOnly {
		t.Fatal("explorer/reviewer 应为只读")
	}
	if byName["worker"].ReadOnly {
		t.Fatal("worker 不该是只读")
	}
}
