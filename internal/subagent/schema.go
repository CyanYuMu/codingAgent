package subagent

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// 这是一个「够用就好」的 JSON Schema 子集校验器，只服务一个目的：
// 把子 agent 的 yield 产出挡在契约之外时，给模型一条能自己改对的反馈。
//
// 支持：type（object/array/string/number/integer/boolean/null，含类型数组）、required、
// properties（递归）、items（递归）、enum。
// 有意忽略：$ref、oneOf/anyOf/allOf、pattern、format、min/max/length 等约束——
// 它们要么在自定义 agent 里罕见，要么误报代价高于漏报（默认 permissive 模式下漏报只是少一次提醒）。

const maxIssues = 20

// Validate 校验 value 是否符合 schema，返回人类可读的问题列表（空 = 通过）。
func Validate(schema map[string]any, value any) []string {
	if schema == nil {
		return nil
	}
	var issues []string
	validateNode(schema, value, "$", &issues)
	if len(issues) > maxIssues {
		issues = append(issues[:maxIssues], fmt.Sprintf("…还有 %d 处问题未列出", len(issues)-maxIssues))
	}
	return issues
}

func validateNode(schema map[string]any, value any, path string, issues *[]string) {
	if len(*issues) > maxIssues {
		return
	}
	if types := schemaTypes(schema); len(types) > 0 && !matchesAnyType(types, value) {
		*issues = append(*issues, fmt.Sprintf("%s 类型应为 %s，实际是 %s", path, strings.Join(types, "|"), typeName(value)))
		return // 类型都不对，再查字段只会产生噪音
	}
	if enum, ok := schema["enum"].([]any); ok && len(enum) > 0 {
		hit := false
		for _, e := range enum {
			if sameScalar(e, value) {
				hit = true
				break
			}
		}
		if !hit {
			*issues = append(*issues, fmt.Sprintf("%s 必须是枚举值之一 %v，实际是 %v", path, enum, value))
		}
	}
	switch v := value.(type) {
	case map[string]any:
		for _, req := range stringsOf(schema["required"]) {
			if got, ok := v[req]; !ok || got == nil {
				*issues = append(*issues, fmt.Sprintf("%s 缺少必填字段 %q", path, req))
			}
		}
		props, _ := schema["properties"].(map[string]any)
		for name, sub := range props {
			child, ok := v[name]
			if !ok {
				continue // 缺失由 required 负责，可选字段缺失不是问题
			}
			if subSchema, ok := sub.(map[string]any); ok {
				validateNode(subSchema, child, path+"."+name, issues)
			}
		}
	case []any:
		if items, ok := schema["items"].(map[string]any); ok {
			for i, el := range v {
				validateNode(items, el, fmt.Sprintf("%s[%d]", path, i), issues)
			}
		}
	}
}

// schemaTypes 取 type 字段（支持字符串或字符串数组）。
func schemaTypes(schema map[string]any) []string {
	switch t := schema["type"].(type) {
	case string:
		if t == "" {
			return nil
		}
		return []string{t}
	case []any:
		return stringsOf(t)
	case []string:
		return t
	}
	return nil
}

func matchesAnyType(types []string, value any) bool {
	for _, t := range types {
		if matchesType(t, value) {
			return true
		}
	}
	return false
}

func matchesType(t string, value any) bool {
	switch t {
	case "object":
		_, ok := value.(map[string]any)
		return ok
	case "array":
		_, ok := value.([]any)
		return ok
	case "string":
		_, ok := value.(string)
		return ok
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "null":
		return value == nil
	case "number":
		_, ok := asFloat(value)
		return ok
	case "integer":
		f, ok := asFloat(value)
		return ok && f == math.Trunc(f)
	}
	return true // 未知 type 关键字不判错
}

// asFloat 归一数字：JSON 解出 float64，YAML 解出 int/int64/uint64。
func asFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint64:
		return float64(n), true
	}
	return 0, false
}

// sameScalar 比较枚举值与实际值（数字跨类型比较，其余按字符串形式比较）。
func sameScalar(a, b any) bool {
	if af, ok := asFloat(a); ok {
		if bf, ok := asFloat(b); ok {
			return af == bf
		}
		return false
	}
	return fmt.Sprintf("%v", a) == fmt.Sprintf("%v", b)
}

func typeName(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case string:
		return "string"
	case bool:
		return "boolean"
	}
	if _, ok := asFloat(v); ok {
		return "number"
	}
	return fmt.Sprintf("%T", v)
}

func stringsOf(v any) []string {
	switch xs := v.(type) {
	case []string:
		return xs
	case []any:
		out := make([]string, 0, len(xs))
		for _, x := range xs {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case string:
		return []string{xs}
	}
	return nil
}

// deriveDataSchema 由 outputSchema 派生 yield 的 data 参数 schema：
// 递归去掉 required 并对对象放开 additionalProperties —— 增量分段提交的是部分数据，
// 线格式必须容得下它；完整性由工具内的 Validate（用原 schema）负责。
func deriveDataSchema(s map[string]any) map[string]any {
	if s == nil {
		return map[string]any{"type": "object", "description": "结构化产出（本任务未声明 schema，按任务要求的字段自行组织）"}
	}
	out, _ := relaxCopy(s).(map[string]any)
	return out
}

func relaxCopy(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			if k == "required" {
				continue
			}
			out[k] = relaxCopy(val)
		}
		if t, _ := out["type"].(string); t == "object" || out["properties"] != nil {
			out["additionalProperties"] = true
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, el := range x {
			out[i] = relaxCopy(el)
		}
		return out
	}
	return v
}

// sectionSchema 返回某个增量分段应满足的 schema：数组属性给 items（一次提交一条），标量/对象属性给自身。
func sectionSchema(schema map[string]any, label string) (map[string]any, bool) {
	props, _ := schema["properties"].(map[string]any)
	prop, ok := props[label].(map[string]any)
	if !ok {
		return nil, false
	}
	if t, _ := prop["type"].(string); t == "array" {
		if items, ok := prop["items"].(map[string]any); ok {
			return items, true
		}
	}
	return prop, true
}

// sectionLabels 返回 schema 声明的分段名（字典序）。
func sectionLabels(schema map[string]any) []string {
	props, _ := schema["properties"].(map[string]any)
	out := make([]string, 0, len(props))
	for k := range props {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// closedSchema 判断 schema 是否「封闭」：声明了 properties 且没显式放开 additionalProperties。
// 封闭时未知分段名视为契约不符（退回重试），开放时放行。
func closedSchema(schema map[string]any) bool {
	props, _ := schema["properties"].(map[string]any)
	if len(props) == 0 {
		return false
	}
	if extra, ok := schema["additionalProperties"].(bool); ok && extra {
		return false
	}
	return true
}

// isArrayProp 判断分段是否为数组属性（决定装配时累积成数组还是取最后一次）。
func isArrayProp(schema map[string]any, label string) bool {
	props, _ := schema["properties"].(map[string]any)
	prop, ok := props[label].(map[string]any)
	if !ok {
		return false
	}
	t, _ := prop["type"].(string)
	return t == "array"
}
