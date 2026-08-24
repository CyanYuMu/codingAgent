package subagent

import (
	"strings"
	"testing"
)

func testDefs() []AgentDef {
	return []AgentDef{
		{Name: "explorer", Description: "探索", SystemPrompt: "p", OutputSchema: map[string]any{"type": "object"}},
		{Name: "worker", Description: "干活", SystemPrompt: "p", SchemaMode: SchemaModeStrict},
	}
}

func longTask(prefix string) string {
	return prefix + "：Target=internal/agent/loop.go 的 loop 函数；Change=加一个终止判定；Acceptance=go test ./internal/agent 通过"
}

func TestPreflightRejections(t *testing.T) {
	base := func() TaskBatch {
		return TaskBatch{Context: "目标：改一处", Tasks: []TaskItem{{Agent: "explorer", Task: longTask("看一下")}}}
	}
	cases := []struct {
		name  string
		batch TaskBatch
		env   Env
		want  string
	}{
		{"空 tasks", TaskBatch{Context: "x"}, Env{Defs: testDefs()}, "tasks 必填"},
		{"空 context", TaskBatch{Tasks: base().Tasks}, Env{Defs: testDefs()}, "context 必填"},
		{"一句话派发", TaskBatch{Context: "x", Tasks: []TaskItem{{Agent: "explorer", Task: "看看代码"}}}, Env{Defs: testDefs()}, "任务描述太短"},
		{"未知 agent", TaskBatch{Context: "x", Tasks: []TaskItem{{Agent: "nope", Task: longTask("a")}}}, Env{Defs: testDefs()}, "未知 agent"},
		{"深度已满", base(), Env{Defs: testDefs(), Depth: 2, MaxDepth: 2}, "最大委派深度"},
		{"spawns 不允许", base(), Env{Defs: testDefs(), Spawns: []string{"worker"}}, "不能派发"},
		{"同名递归", base(), Env{Defs: testDefs(), SelfAgent: "explorer"}, "禁止派发与自己同名"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Preflight(tc.batch, tc.env)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want 含 %q", err, tc.want)
			}
		})
	}
}

func TestPreflightAllowsWildcardSpawns(t *testing.T) {
	b := TaskBatch{Context: "x", Tasks: []TaskItem{{Agent: "worker", Task: longTask("干活")}}}
	if _, err := Preflight(b, Env{Defs: testDefs(), Spawns: []string{"*"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := Preflight(b, Env{Defs: testDefs(), Spawns: []string{}}); err == nil {
		t.Fatal("空 spawns 列表应拒绝一切派发")
	}
}

func TestPreflightNamingAndDedup(t *testing.T) {
	b := TaskBatch{Context: "x", Tasks: []TaskItem{
		{Agent: "explorer", Task: longTask("一")},
		{Agent: "explorer", Task: longTask("二")},
		{Name: "Scout", Agent: "worker", Task: longTask("三")},
		{Name: "Scout", Agent: "worker", Task: longTask("四")},
		{Name: "a b/c", Agent: "worker", Task: longTask("五")},
	}}
	taken := map[string]bool{"explorer-1": true}
	got, err := Preflight(b, Env{Defs: testDefs(), NameTaken: func(n string) bool { return taken[n] }})
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(got))
	for i, r := range got {
		names[i] = r.Item.Name
	}
	want := []string{"explorer-1-2", "explorer-2", "Scout", "Scout-2", "a_b_c"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("names = %v, want %v", names, want)
	}
}

func TestPreflightSchemaPrecedence(t *testing.T) {
	itemSchema := map[string]any{"type": "object", "properties": map[string]any{"x": map[string]any{"type": "string"}}}
	b := TaskBatch{Context: "x", Tasks: []TaskItem{
		{Agent: "explorer", Task: longTask("一")},                                                 // 用 def 的 schema，permissive
		{Agent: "explorer", Task: longTask("二"), OutputSchema: itemSchema, SchemaMode: "strict"}, // item 覆盖
		{Agent: "worker", Task: longTask("三")},                                                   // def 是 strict
		{Agent: "worker", Task: longTask("四"), SchemaMode: "permissive"},                         // item 放宽
	}}
	got, err := Preflight(b, Env{Defs: testDefs()})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Schema == nil || got[0].SchemaMode != SchemaModePermissive {
		t.Fatalf("[0] = %+v", got[0])
	}
	props, _ := got[1].Schema["properties"].(map[string]any)
	if props["x"] == nil || got[1].SchemaMode != SchemaModeStrict {
		t.Fatalf("[1] = %+v", got[1])
	}
	if got[2].SchemaMode != SchemaModeStrict || got[3].SchemaMode != SchemaModePermissive {
		t.Fatalf("[2]=%s [3]=%s", got[2].SchemaMode, got[3].SchemaMode)
	}
}
