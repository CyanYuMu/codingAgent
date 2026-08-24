package subagent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"einoclaw-build/internal/runtime"
	"einoclaw-build/internal/tool"
)

// exec 调一次 yield，返回 (给模型的文本, 错误, 是否终止)。
func execYield(t *testing.T, y tool.Tool, argsJSON string) (string, error, bool) {
	t.Helper()
	var args map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		t.Fatal(err)
	}
	sink := runtime.NewSink(4000, 4000)
	defer sink.Close()
	err := y.Execute(context.Background(), args, sink)
	terminal := y.(tool.Terminal).IsTerminal(args, err)
	return sink.Result(), err, terminal
}

func TestYieldTerminalSuccess(t *testing.T) {
	st := NewYieldState()
	y := NewYieldTool(st, nil, SchemaModePermissive)
	out, err, terminal := execYield(t, y, `{"data":{"x":1}}`)
	if err != nil || !terminal || !strings.Contains(out, "已提交") {
		t.Fatalf("out=%q err=%v terminal=%v", out, err, terminal)
	}
	data, _, _, done := st.Snapshot()
	if !done || data.(map[string]any)["x"] == nil {
		t.Fatalf("state = %v %v", data, done)
	}
}

func TestYieldIncrementalAccumulatesWithoutTerminating(t *testing.T) {
	st := NewYieldState()
	y := NewYieldTool(st, findingsSchema, SchemaModePermissive)
	for _, args := range []string{
		`{"section":"findings","data":{"file":"a.go","severity":"high"}}`,
		`{"section":"findings","data":{"file":"b.go","severity":"low"}}`,
	} {
		out, err, terminal := execYield(t, y, args)
		if err != nil || terminal {
			t.Fatalf("增量提交不该终止：out=%q err=%v terminal=%v", out, err, terminal)
		}
		if !strings.Contains(out, "继续工作") {
			t.Fatalf("应提示继续工作：%q", out)
		}
	}
	_, sections, _, done := st.Snapshot()
	if done || len(sections["findings"]) != 2 {
		t.Fatalf("sections = %v done = %v", sections, done)
	}
}

func TestYieldErrorState(t *testing.T) {
	st := NewYieldState()
	y := NewYieldTool(st, findingsSchema, SchemaModeStrict)
	_, err, terminal := execYield(t, y, `{"error":"依赖的接口还不存在"}`)
	if err != nil || !terminal {
		t.Fatalf("err=%v terminal=%v", err, terminal)
	}
	if _, _, msg, done := st.Snapshot(); !done || msg != "依赖的接口还不存在" {
		t.Fatalf("state = %q %v", msg, done)
	}
}

func TestYieldRejectsBadCombinations(t *testing.T) {
	st := NewYieldState()
	y := NewYieldTool(st, nil, SchemaModePermissive)
	for _, tc := range []struct{ args, want string }{
		{`{"data":{"x":1},"error":"boom"}`, "不能同时给"},
		{`{"section":"findings"}`, "必须带 data"},
	} {
		_, err, terminal := execYield(t, y, tc.args)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("args %s → err %v, want 含 %q", tc.args, err, tc.want)
		}
		if terminal {
			t.Fatalf("args %s：退回重试的调用不该终止", tc.args)
		}
	}
}

func TestYieldEmptyRetriesThenAborts(t *testing.T) {
	st := NewYieldState()
	y := NewYieldTool(st, nil, SchemaModePermissive)
	for i := 1; i <= maxEmptyYieldRetries; i++ {
		_, err, terminal := execYield(t, y, `{}`)
		if err == nil || terminal {
			t.Fatalf("第 %d 次空提交应退回重试：err=%v terminal=%v", i, err, terminal)
		}
		if !strings.Contains(err.Error(), "还剩") {
			t.Fatalf("应告知剩余次数：%v", err)
		}
	}
	_, err, terminal := execYield(t, y, `{}`)
	if err != nil || !terminal {
		t.Fatalf("超过次数应直接结束：err=%v terminal=%v", err, terminal)
	}
	if _, _, msg, _ := st.Snapshot(); !strings.Contains(msg, "空提交") {
		t.Fatalf("应记下放弃原因：%q", msg)
	}
}

func TestYieldSchemaRetryThenPass(t *testing.T) {
	st := NewYieldState()
	y := NewYieldTool(st, findingsSchema, SchemaModePermissive)
	_, err, terminal := execYield(t, y, `{"data":{"findings":[]}}`) // 缺 verdict
	if err == nil || terminal {
		t.Fatalf("err=%v terminal=%v", err, terminal)
	}
	if !strings.Contains(err.Error(), "verdict") || !strings.Contains(err.Error(), "还剩 2 次") {
		t.Fatalf("反馈应含缺失字段与剩余次数：%v", err)
	}
	_, err, terminal = execYield(t, y, `{"data":{"findings":[],"verdict":"ok"}}`)
	if err != nil || !terminal {
		t.Fatalf("改对后应通过：err=%v terminal=%v", err, terminal)
	}
	if over, viol, _ := st.Flags(); over || viol {
		t.Fatalf("正常通过不该打标记：over=%v viol=%v", over, viol)
	}
}

func TestYieldSchemaOverrideVsStrict(t *testing.T) {
	for _, tc := range []struct {
		mode               string
		wantOver, wantViol bool
	}{
		{SchemaModePermissive, true, false},
		{SchemaModeStrict, false, true},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			st := NewYieldState()
			y := NewYieldTool(st, findingsSchema, tc.mode)
			for i := 0; i < maxSchemaRetries; i++ {
				if _, err, _ := execYield(t, y, `{"data":{"findings":[]}}`); err == nil {
					t.Fatalf("第 %d 次仍应退回", i+1)
				}
			}
			_, err, terminal := execYield(t, y, `{"data":{"findings":[]}}`)
			if err != nil || !terminal {
				t.Fatalf("重试用尽后应终止：err=%v terminal=%v", err, terminal)
			}
			over, viol, issues := st.Flags()
			if over != tc.wantOver || viol != tc.wantViol || len(issues) == 0 {
				t.Fatalf("over=%v viol=%v issues=%v", over, viol, issues)
			}
			if data, _, _, _ := st.Snapshot(); data == nil {
				t.Fatal("内容仍应原样带回，便于父 agent 判断")
			}
		})
	}
}

func TestYieldAssembleFromSections(t *testing.T) {
	st := NewYieldState()
	y := NewYieldTool(st, findingsSchema, SchemaModeStrict)
	execYield(t, y, `{"section":"findings","data":{"file":"a.go","severity":"high"}}`)
	execYield(t, y, `{"section":"findings","data":{"file":"b.go","severity":"low"}}`)
	execYield(t, y, `{"section":"verdict","data":"两处问题"}`)
	out, err, terminal := execYield(t, y, `{}`) // 收尾：用分段装配
	if err != nil || !terminal {
		t.Fatalf("out=%q err=%v terminal=%v", out, err, terminal)
	}
	data, _, _, _ := st.Snapshot()
	m, _ := data.(map[string]any)
	arr, _ := m["findings"].([]any)
	if len(arr) != 2 || m["verdict"] != "两处问题" {
		t.Fatalf("装配结果 = %#v", data)
	}
	if over, viol, _ := st.Flags(); over || viol {
		t.Fatalf("装配结果应通过校验：over=%v viol=%v", over, viol)
	}
}

func TestYieldUnknownSectionOnClosedSchema(t *testing.T) {
	st := NewYieldState()
	y := NewYieldTool(st, findingsSchema, SchemaModePermissive)
	_, err, _ := execYield(t, y, `{"section":"nope","data":{"x":1}}`)
	if err == nil || !strings.Contains(err.Error(), "未知分段名") || !strings.Contains(err.Error(), "verdict") {
		t.Fatalf("err = %v（应列出可用分段名）", err)
	}
	// 开放 schema 放行任意分段名
	open := map[string]any{"type": "object"}
	y2 := NewYieldTool(NewYieldState(), open, SchemaModePermissive)
	if _, err, _ := execYield(t, y2, `{"section":"anything","data":{"x":1}}`); err != nil {
		t.Fatalf("开放 schema 不该限制分段名：%v", err)
	}
}

func TestYieldParametersAndDescriptionDerived(t *testing.T) {
	y := NewYieldTool(NewYieldState(), findingsSchema, SchemaModeStrict)
	params := y.Parameters()
	data, _ := params["data"].(map[string]any)
	if data["required"] != nil {
		t.Fatalf("data 的线格式不该带 required：%v", data)
	}
	sec, _ := params["section"].(map[string]any)
	labels, _ := sec["enum"].([]string)
	if strings.Join(labels, ",") != "findings,verdict" {
		t.Fatalf("封闭 schema 下 section 应枚举分段名：%v", sec)
	}
	desc := y.Description()
	for _, want := range []string{"yield(data=", "section=", "yield(error=", "verdict"} {
		if !strings.Contains(desc, want) {
			t.Fatalf("描述缺 %q：%s", want, desc)
		}
	}
}
