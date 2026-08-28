package permission

import (
	"errors"
	"fmt"
	"strings"
)

// Policy 工具对本次调用的显式策略（在 tier 之外由工具自检给出）。
type Policy string

const (
	PolicyNone   Policy = ""       // 无显式策略
	PolicyAllow  Policy = "allow"  // 工具显式放行
	PolicyDeny   Policy = "deny"   // 工具显式拒绝（永远生效）
	PolicyPrompt Policy = "prompt" // 工具显式要求询问
)

// ToolDecision 一次具体调用经工具自检后的判定（替代固定 Tier 的扩展形态）。
type ToolDecision struct {
	Tier     Tier   // 基线危险等级
	Policy   Policy // 工具显式策略
	Override bool   // 强制 prompt（如 bash 危险分类），yolo 下被忽略、其余模式强制询问
	Reason   string // 给用户/模型的说明（Override 时必填）
}

// Rule 一条审批规则：tool(args-pattern*)。* 通配任意序列；不带括号 = 该工具全部参数。
type Rule struct {
	Raw     string // 配置原文
	Tool    string // 工具名（"*" = 任意工具）
	ArgsPat string // 参数模式；空 = 全部参数
	AnyTool bool
}

// Rules 用户配置的规则集。
type Rules struct {
	Allow, Ask, Deny []Rule
}

// ParseRule 解析 "tool(args*)"；不带括号 = 只匹配工具名。
func ParseRule(raw string) (Rule, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Rule{}, errors.New("空规则")
	}
	r := Rule{Raw: raw}
	if i := strings.Index(raw, "("); i >= 0 {
		if !strings.HasSuffix(raw, ")") {
			return Rule{}, fmt.Errorf("规则括号不闭合：%q", raw)
		}
		r.Tool = raw[:i]
		r.ArgsPat = raw[i+1 : len(raw)-1]
		if r.ArgsPat == "" {
			return Rule{}, fmt.Errorf("规则参数为空：%q", raw)
		}
	} else {
		r.Tool = raw
	}
	r.AnyTool = r.Tool == "*"
	if !r.AnyTool && strings.ContainsAny(r.Tool, "*()") {
		return Rule{}, fmt.Errorf("坏工具名：%q", raw)
	}
	return r, nil
}

// Match 判断规则是否命中这次调用。name 是工具名，args 是调用参数 JSON 原文。
func (r Rule) Match(name, args string) bool {
	if !r.AnyTool && !wildcardMatch(r.Tool, name) {
		return false
	}
	if r.ArgsPat == "" {
		return true
	}
	return wildcardMatch(r.ArgsPat, args)
}

// wildcardMatch：pattern 按 * 分段，各段须按序作为子串出现在 s 中。
func wildcardMatch(pattern, s string) bool {
	rest := s
	for _, part := range strings.Split(pattern, "*") {
		if part == "" {
			continue
		}
		i := strings.Index(rest, part)
		if i < 0 {
			return false
		}
		rest = rest[i+len(part):]
	}
	return true
}

// ResolveRules 五步决策（演进方案 §E.1）：
//  1. 工具 deny 永远 deny；2. 用户 deny 永远 deny（含 yolo）；
//  3. yolo：工具显式 allow/prompt 或用户 allow/ask 命中 → allow（裸 Override 忽略）；
//  4. 非 yolo 且 Override → prompt（除非工具显式 allow）；
//  5. 工具显式 policy → 用户规则 → tier×mode。
//
// 空规则 + 无显式 policy/Override 时与 Resolve(tier, mode) 完全等价（回归不变量）。
func ResolveRules(td ToolDecision, rules Rules, mode Mode, name, args string) (Decision, string) {
	// 1. 工具 deny 永远 deny
	if td.Policy == PolicyDeny {
		return DecisionDeny, toolReason(td, "tool denies")
	}
	// 2. 用户 deny 永远 deny
	for _, r := range rules.Deny {
		if r.Match(name, args) {
			return DecisionDeny, "denied by rule: " + r.Raw
		}
	}

	if mode == ModeYolo {
		// 3. yolo：显式策略与用户 allow/ask 都是放行；裸 Override 忽略
		if td.Policy == PolicyAllow || td.Policy == PolicyPrompt {
			return DecisionAllow, ""
		}
		for _, r := range rules.Allow {
			if r.Match(name, args) {
				return DecisionAllow, ""
			}
		}
		for _, r := range rules.Ask {
			if r.Match(name, args) {
				return DecisionAllow, ""
			}
		}
		return DecisionAllow, ""
	}

	// 4. 非 yolo 且 Override → prompt（除非工具显式 allow）
	if td.Override && td.Policy != PolicyAllow {
		return DecisionPrompt, toolReason(td, "dangerous command detected")
	}

	// 5. 工具显式 policy 压过用户规则
	if td.Policy == PolicyAllow {
		return DecisionAllow, ""
	}
	if td.Policy == PolicyPrompt {
		return DecisionPrompt, toolReason(td, "tool requires approval")
	}

	// 用户规则压过 tier×mode
	for _, r := range rules.Allow {
		if r.Match(name, args) {
			return DecisionAllow, ""
		}
	}
	for _, r := range rules.Ask {
		if r.Match(name, args) {
			return DecisionPrompt, "rule asks: " + r.Raw
		}
	}
	return Resolve(td.Tier, mode), ""
}

func toolReason(td ToolDecision, fallback string) string {
	if td.Reason != "" {
		return td.Reason
	}
	return fallback
}
