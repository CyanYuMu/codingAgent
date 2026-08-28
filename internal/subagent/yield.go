package subagent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"einoclaw-build/internal/permission"
	"einoclaw-build/internal/runtime"
	"einoclaw-build/internal/tool"
)

// yield 的重试预算：schema 不符与空结果各给 3 次自我纠正的机会，之后按模式放行或判失败。
// 3 与 harness 里其它「提醒 3 次」的节奏一致：够纠错，又不至于把一次 run 烧在格式上。
const (
	maxSchemaRetries     = 3
	maxEmptyYieldRetries = 3
)

// YieldState 是一个 Run 的产出累积：由 yield 工具写、驱动器读。
type YieldState struct {
	mu       sync.Mutex
	data     any
	hasData  bool
	sections map[string][]any
	errMsg   string
	terminal bool

	schemaOverridden bool     // permissive：超重试次数后放行
	schemaViolation  bool     // strict：超重试次数后判失败
	issues           []string // 最后一次校验的问题（审计用）

	schemaFailures int
	emptyFailures  int
}

// NewYieldState 构造一个空的产出累积。
func NewYieldState() *YieldState { return &YieldState{sections: map[string][]any{}} }

// Snapshot 返回产出快照（驱动器结算时用）。
func (s *YieldState) Snapshot() (data any, sections map[string][]any, errMsg string, terminal bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.sections) > 0 {
		sections = make(map[string][]any, len(s.sections))
		for k, v := range s.sections {
			sections[k] = append([]any(nil), v...)
		}
	}
	return s.data, sections, s.errMsg, s.terminal
}

// Flags 返回 schema 校验的结果标记。
func (s *YieldState) Flags() (overridden, violation bool, issues []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.schemaOverridden, s.schemaViolation, append([]string(nil), s.issues...)
}

// yieldTool 是子 agent 的结果提交工具，三态：
//
//	data                → 终止提交（校验通过即结束 run）
//	data + section      → 增量提交一段（不终止，继续工作）
//	error               → 主动放弃并说明卡在哪（终止）
type yieldTool struct {
	st     *YieldState
	schema map[string]any
	mode   string
}

// NewYieldTool 构造 yield 工具；schema 为空表示自由结构产出。
func NewYieldTool(st *YieldState, schema map[string]any, mode string) tool.Tool {
	if mode != SchemaModeStrict {
		mode = SchemaModePermissive
	}
	return &yieldTool{st: st, schema: schema, mode: mode}
}

func (*yieldTool) Name() string { return "yield" }

func (y *yieldTool) Description() string {
	var sb strings.Builder
	sb.WriteString("提交任务结果。这是把结果交回主 agent 的唯一方式，三种用法：\n")
	sb.WriteString("- 完成：yield(data=<最终产出>) —— 校验通过后运行立即结束，不会再执行任何工具。\n")
	sb.WriteString("- 分段（可选）：yield(data=<一段>, section=\"<分段名>\") —— 只记录这一段，运行继续；最后仍要不带 section 再调一次收尾。\n")
	sb.WriteString("- 放弃：yield(error=\"尝试过什么、卡在哪里\") —— 无法完成时用，运行结束。\n")
	if y.schema != nil {
		if labels := sectionLabels(y.schema); len(labels) > 0 {
			fmt.Fprintf(&sb, "可用分段名：%s\n", strings.Join(labels, ", "))
		}
		sb.WriteString("data 必须符合以下 schema（不符会被退回并要求重交）：\n")
		sb.WriteString(schemaHint(y.schema))
	} else {
		sb.WriteString("data 为自由结构：按任务里要求的字段组织。\n")
	}
	return sb.String()
}

// schemaHint 把 schema 渲染进工具描述；太大时只留顶层字段名，避免挤爆提示词。
func schemaHint(s map[string]any) string {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Sprintf("%v", s)
	}
	if len(b) <= 2048 {
		return string(b)
	}
	return "顶层字段：" + strings.Join(sectionLabels(s), ", ")
}

func (y *yieldTool) Parameters() map[string]any {
	params := map[string]any{
		"data":  deriveDataSchema(y.schema),
		"error": map[string]any{"type": "string", "description": "无法完成时的说明：尝试过什么、卡在哪里"},
		"section": map[string]any{
			"type":        "string",
			"description": "可选：本次只提交这一段（不结束运行）。留空表示这是最终提交。",
		},
	}
	if y.schema != nil {
		if labels := sectionLabels(y.schema); len(labels) > 0 && closedSchema(y.schema) {
			sec, _ := params["section"].(map[string]any)
			sec["enum"] = labels
		}
	}
	return params
}

func (*yieldTool) Required() []string { return nil } // 三态互斥，语义在工具内校验（顶层组合子对部分 provider 不安全）

func (*yieldTool) Tier() permission.Tier { return permission.TierRead }

// Concurrency 串行：同一条消息里的多次 yield 必须按序执行，重试计数与分段顺序才是确定的。
func (*yieldTool) Concurrency() tool.Concurrency { return tool.ConcurrencyExclusive }

// IsTerminal 只有「没有 section 且工具没退回重试」的调用才结束 run。
func (*yieldTool) IsTerminal(args map[string]any, err error) bool {
	if err != nil {
		return false
	}
	return strings.TrimSpace(argString(args, "section")) == ""
}

func (y *yieldTool) Execute(_ context.Context, args map[string]any, sink *runtime.Sink) error {
	data, hasData := args["data"]
	if hasData && data == nil {
		hasData = false
	}
	errMsg := strings.TrimSpace(argString(args, "error"))
	section := strings.TrimSpace(argString(args, "section"))

	if hasData && errMsg != "" {
		return fmt.Errorf("data 与 error 不能同时给：成功用 yield(data=…)，放弃用 yield(error=…)")
	}
	if errMsg != "" {
		y.st.setError(errMsg)
		sink.Write([]byte("已记录放弃原因，运行结束。"))
		return nil
	}
	if section != "" {
		return y.submitSection(section, data, hasData, sink)
	}
	return y.submitFinal(data, hasData, sink)
}

// submitSection 处理增量提交：校验该段、累积、提示继续工作。
func (y *yieldTool) submitSection(section string, data any, hasData bool, sink *runtime.Sink) error {
	if !hasData {
		return fmt.Errorf("增量提交必须带 data（section=%q 时 data 是这一段的内容）", section)
	}
	if y.schema != nil {
		sub, known := sectionSchema(y.schema, section)
		if !known {
			if closedSchema(y.schema) {
				return fmt.Errorf("未知分段名 %q，可用：%s", section, strings.Join(sectionLabels(y.schema), ", "))
			}
		} else if issues := Validate(sub, data); len(issues) > 0 {
			return y.schemaRetry(fmt.Sprintf("分段 %q", section), issues)
		}
	}
	y.st.addSection(section, data)
	fmt.Fprintf(sink, "已记录分段 %q。继续工作；全部完成后不带 section 再调一次 yield 提交最终结果。", section)
	return nil
}

// submitFinal 处理终止提交：data 缺省时用已累积的分段装配。
func (y *yieldTool) submitFinal(data any, hasData bool, sink *runtime.Sink) error {
	if !hasData {
		assembled, ok := y.st.assemble(y.schema)
		if !ok {
			if n := y.st.countEmpty(); n <= maxEmptyYieldRetries {
				return fmt.Errorf("data 与 error 至少给一个：完成用 yield(data=…)，放弃用 yield(error=…)。空提交还剩 %d 次机会", maxEmptyYieldRetries-n+1)
			}
			y.st.setError(fmt.Sprintf("yield 连续 %d 次空提交，放弃本次运行", maxEmptyYieldRetries+1))
			sink.Write([]byte("连续空提交次数过多，运行按失败结束。"))
			return nil
		}
		data = assembled
	}
	if y.schema != nil {
		if issues := Validate(y.schema, data); len(issues) > 0 {
			if y.st.bumpSchemaFailure() <= maxSchemaRetries {
				return y.schemaRetry("产出", issues)
			}
			// 重试预算用尽：permissive 放行并告警，strict 判失败（两者都终止，避免无限循环）
			y.st.setIssues(issues)
			if y.mode == SchemaModeStrict {
				y.st.setData(data)
				y.st.markViolation()
				sink.Write([]byte("产出仍不符合 schema（strict 模式），已按失败结束并把内容原样带回。"))
				return nil
			}
			y.st.setData(data)
			y.st.markOverridden()
			sink.Write([]byte("结果已提交（schema 校验多次未通过，已放行并附警告）。"))
			return nil
		}
	}
	y.st.setData(data)
	sink.Write([]byte("结果已提交。"))
	return nil
}

// schemaRetry 返回给模型的重试反馈（含剩余次数）。返回 error 因此本次调用不终止 run。
func (y *yieldTool) schemaRetry(scope string, issues []string) error {
	remaining := maxSchemaRetries - y.st.schemaFailureCount()
	if remaining < 0 {
		remaining = 0
	}
	return fmt.Errorf("%s 不符合 schema：%s。修正形状后再调一次 yield（还剩 %d 次机会，之后按 %s 模式处理）",
		scope, strings.Join(issues, "；"), remaining, y.mode)
}

func argString(args map[string]any, key string) string {
	s, _ := args[key].(string)
	return s
}

// ---------- YieldState 写侧（全部加锁：yield 可能在不同 turn 被调用） ----------

func (s *YieldState) setData(v any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data, s.hasData, s.terminal, s.emptyFailures = v, true, true, 0
}

func (s *YieldState) setError(msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.errMsg, s.terminal = msg, true
}

func (s *YieldState) addSection(label string, v any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sections == nil {
		s.sections = map[string][]any{}
	}
	s.sections[label] = append(s.sections[label], v)
	s.emptyFailures = 0
}

func (s *YieldState) bumpSchemaFailure() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.schemaFailures++
	return s.schemaFailures
}

func (s *YieldState) schemaFailureCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.schemaFailures
}

func (s *YieldState) countEmpty() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.emptyFailures++
	return s.emptyFailures
}

func (s *YieldState) setIssues(issues []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.issues = append([]string(nil), issues...)
}

func (s *YieldState) markOverridden() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.schemaOverridden = true
}

func (s *YieldState) markViolation() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.schemaViolation = true
}

// assemble 用累积的分段装配最终 data：数组属性拼成数组，其它取最后一次提交。
func (s *YieldState) assemble(schema map[string]any) (any, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.sections) == 0 {
		return nil, false
	}
	out := map[string]any{}
	for label, vals := range s.sections {
		if len(vals) == 0 {
			continue
		}
		if schema != nil && isArrayProp(schema, label) {
			out[label] = append([]any(nil), vals...)
			continue
		}
		if len(vals) > 1 {
			out[label] = append([]any(nil), vals...) // 无 schema 时多次提交同名分段按数组处理
			continue
		}
		out[label] = vals[0]
	}
	return out, true
}
