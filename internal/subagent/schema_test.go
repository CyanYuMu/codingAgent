package subagent

import (
	"encoding/json"
	"strings"
	"testing"
)

func mustJSON(t *testing.T, s string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

var findingsSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"findings": map[string]any{
			"type": "array",
			"items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"file":     map[string]any{"type": "string"},
					"line":     map[string]any{"type": "integer"},
					"severity": map[string]any{"type": "string", "enum": []any{"crit", "high", "med", "low"}},
				},
				"required": []any{"file", "severity"},
			},
		},
		"verdict": map[string]any{"type": "string"},
	},
	"required": []any{"findings", "verdict"},
}

func TestValidateAcceptsConformingValue(t *testing.T) {
	v := mustJSON(t, `{"findings":[{"file":"a.go","line":3,"severity":"high"}],"verdict":"ok"}`)
	if issues := Validate(findingsSchema, v); len(issues) != 0 {
		t.Fatalf("issues = %v", issues)
	}
}

func TestValidateReportsPathAndReason(t *testing.T) {
	cases := []struct {
		name, value, want string
	}{
		{"缺必填", `{"findings":[]}`, `$ 缺少必填字段 "verdict"`},
		{"嵌套缺必填", `{"findings":[{"file":"a.go"}],"verdict":"x"}`, `$.findings[0] 缺少必填字段 "severity"`},
		{"类型错", `{"findings":{},"verdict":"x"}`, "$.findings 类型应为 array"},
		{"整数要求", `{"findings":[{"file":"a","severity":"low","line":1.5}],"verdict":"x"}`, "$.findings[0].line 类型应为 integer"},
		{"枚举外", `{"findings":[{"file":"a","severity":"blocker"}],"verdict":"x"}`, "$.findings[0].severity 必须是枚举值之一"},
		{"顶层类型错", `[]`, "$ 类型应为 object"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			issues := Validate(findingsSchema, mustJSON(t, tc.value))
			if len(issues) == 0 || !strings.Contains(strings.Join(issues, "|"), tc.want) {
				t.Fatalf("issues = %v, want 含 %q", issues, tc.want)
			}
		})
	}
}

func TestValidateNilSchemaAcceptsAnything(t *testing.T) {
	if issues := Validate(nil, mustJSON(t, `{"whatever":1}`)); issues != nil {
		t.Fatalf("issues = %v", issues)
	}
}

func TestValidateIntegerAcceptsWholeFloatAndYAMLInt(t *testing.T) {
	s := map[string]any{"type": "integer"}
	for _, v := range []any{float64(3), 3, int64(3)} {
		if issues := Validate(s, v); len(issues) != 0 {
			t.Fatalf("%T(%v) 应通过：%v", v, v, issues)
		}
	}
	if issues := Validate(s, 3.5); len(issues) == 0 {
		t.Fatal("3.5 不该是 integer")
	}
}

func TestDeriveDataSchemaStripsRequiredWithoutMutating(t *testing.T) {
	got := deriveDataSchema(findingsSchema)
	if got["required"] != nil {
		t.Fatalf("顶层 required 应被去掉：%v", got["required"])
	}
	if got["additionalProperties"] != true {
		t.Fatalf("对象层应放开 additionalProperties：%v", got)
	}
	props, _ := got["properties"].(map[string]any)
	findings, _ := props["findings"].(map[string]any)
	items, _ := findings["items"].(map[string]any)
	if items["required"] != nil {
		t.Fatalf("嵌套 required 应被去掉：%v", items["required"])
	}
	if items["type"] != "object" || items["properties"] == nil {
		t.Fatalf("结构应保留：%v", items)
	}
	if findingsSchema["required"] == nil {
		t.Fatal("原 schema 被修改了（必须深拷贝）")
	}
	// 派生出的 schema 应接受增量分段那样的部分数据
	if issues := Validate(got, mustJSON(t, `{"verdict":"x"}`)); len(issues) != 0 {
		t.Fatalf("派生 schema 应接受部分数据：%v", issues)
	}
}

func TestDeriveDataSchemaNil(t *testing.T) {
	got := deriveDataSchema(nil)
	if got["type"] != "object" || got["description"] == nil {
		t.Fatalf("无 schema 时应给出自由结构：%v", got)
	}
}

func TestSectionSchemaAndLabels(t *testing.T) {
	sub, ok := sectionSchema(findingsSchema, "findings")
	if !ok || sub["type"] != "object" {
		t.Fatalf("数组分段应返回 items：%v %v", sub, ok)
	}
	if issues := Validate(sub, mustJSON(t, `{"file":"a.go","severity":"low"}`)); len(issues) != 0 {
		t.Fatalf("单条 finding 应通过 items 校验：%v", issues)
	}
	if sub, ok := sectionSchema(findingsSchema, "verdict"); !ok || sub["type"] != "string" {
		t.Fatalf("标量分段应返回自身：%v %v", sub, ok)
	}
	if _, ok := sectionSchema(findingsSchema, "nope"); ok {
		t.Fatal("未知分段名应返回 false")
	}
	if strings.Join(sectionLabels(findingsSchema), ",") != "findings,verdict" {
		t.Fatalf("labels = %v（应字典序）", sectionLabels(findingsSchema))
	}
	if !isArrayProp(findingsSchema, "findings") || isArrayProp(findingsSchema, "verdict") {
		t.Fatal("isArrayProp 判定错")
	}
}

func TestClosedSchema(t *testing.T) {
	if !closedSchema(findingsSchema) {
		t.Fatal("声明了 properties 且未放开 additionalProperties → 封闭")
	}
	if closedSchema(map[string]any{"type": "object"}) {
		t.Fatal("没有 properties → 开放")
	}
	if closedSchema(deriveDataSchema(findingsSchema)) {
		t.Fatal("放开 additionalProperties 后 → 开放")
	}
}
