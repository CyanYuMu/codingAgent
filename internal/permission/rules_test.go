package permission

import (
	"strings"
	"testing"
)

func mustRule(t *testing.T, raw string) Rule {
	t.Helper()
	r, err := ParseRule(raw)
	if err != nil {
		t.Fatalf("ParseRule(%q): %v", raw, err)
	}
	return r
}

func TestParseRuleToolOnly(t *testing.T) {
	r := mustRule(t, "bash")
	if r.Tool != "bash" || r.ArgsPat != "" || r.AnyTool {
		t.Fatalf("got %+v", r)
	}
}

func TestParseRuleWithArgs(t *testing.T) {
	r := mustRule(t, "bash(git status*)")
	if r.Tool != "bash" || r.ArgsPat != "git status*" || r.AnyTool {
		t.Fatalf("got %+v", r)
	}
}

func TestParseRuleAnyTool(t *testing.T) {
	r := mustRule(t, "*(./.env*)")
	if !r.AnyTool || r.ArgsPat != "./.env*" {
		t.Fatalf("got %+v", r)
	}
}

func TestParseRuleBad(t *testing.T) {
	for _, raw := range []string{"", "bash(git", "bash()", "(", "*(nope"} {
		if _, err := ParseRule(raw); err == nil {
			t.Errorf("ParseRule(%q) 应报错", raw)
		}
	}
}

func TestMatchArgsWildcard(t *testing.T) {
	cases := []struct {
		rule string
		name string
		args string
		want bool
	}{
		{"bash(git status*)", "bash", `{"command":"git status --short"}`, true},
		{"bash(git status*)", "bash", `{"command":"git push origin main"}`, false},
		{"read(**) ", "read_file", `{"file_path":"./.env"}`, true},
		{"read(./.env*)", "read_file", `{"file_path":"./.env"}`, true},
		{"read(./.env*)", "read_file", `{"file_path":"src/main.go"}`, false},
		{"bash(git status*)", "grep", `{"command":"git status"}`, false}, // 工具名不匹配
		{"bash", "bash", `{"command":"rm -rf /"}`, true},                 // 无括号 = 全部参数
		{"bash(*push*)", "bash", `{"command":"git push origin"}`, true},
		{"*", "bash", `{"command":"anything"}`, true}, // AnyTool 无参数模式 = 匹配一切
	}
	for _, c := range cases {
		r := mustRule(t, c.rule)
		if got := r.Match(c.name, c.args); got != c.want {
			t.Errorf("Match(%q, %q, %q) = %v, want %v", c.rule, c.name, c.args, got, c.want)
		}
	}
}

func TestResolveRulesEmptyRulesEquivalent(t *testing.T) {
	// 回归不变量：空规则 + 无显式策略/Override 时等价 Resolve(tier, mode)
	for _, mode := range []Mode{ModeYolo, ModeWrite, ModeAlwaysAsk, Mode("unknown")} {
		for _, tier := range []Tier{TierRead, TierWrite, TierExec} {
			got, reason := ResolveRules(ToolDecision{Tier: tier}, Rules{}, mode, "bash", `{"command":"x"}`)
			want := Resolve(tier, mode)
			if got != want {
				t.Errorf("mode=%s tier=%s: ResolveRules=%s, Resolve=%s", mode, tier, got, want)
			}
			if got == DecisionAllow && reason != "" {
				t.Errorf("allow 不应带 reason，got %q", reason)
			}
		}
	}
}

func TestResolveRulesDenyRuleAlwaysWins(t *testing.T) {
	rules := Rules{Deny: []Rule{mustRule(t, "bash(rm -rf *)")}}
	// yolo + 工具显式 allow 也拦不住 deny
	got, reason := ResolveRules(ToolDecision{Tier: TierExec, Policy: PolicyAllow}, rules, ModeYolo, "bash", `{"command":"rm -rf /tmp/x"}`)
	if got != DecisionDeny || reason == "" || !strings.Contains(reason, "rm -rf *") {
		t.Fatalf("got %s %q", got, reason)
	}
	// 不命中的命令不受影响
	got, _ = ResolveRules(ToolDecision{Tier: TierExec}, rules, ModeYolo, "bash", `{"command":"ls"}`)
	if got != DecisionAllow {
		t.Fatalf("未命中 deny 的调用在 yolo 下应 allow，got %s", got)
	}
}

func TestResolveRulesToolPolicyDenyWins(t *testing.T) {
	got, reason := ResolveRules(ToolDecision{Tier: TierRead, Policy: PolicyDeny, Reason: "只读数据源不可用于写"}, Rules{}, ModeWrite, "read_file", `{}`)
	if got != DecisionDeny || !strings.Contains(reason, "只读数据源") {
		t.Fatalf("got %s %q", got, reason)
	}
}

func TestResolveRulesYoloIgnoresOverride(t *testing.T) {
	td := ToolDecision{Tier: TierExec, Override: true, Reason: "危险"}
	got, reason := ResolveRules(td, Rules{}, ModeYolo, "bash", `{"command":"rm -rf /"}`)
	if got != DecisionAllow || reason != "" {
		t.Fatalf("yolo 下裸 Override 应忽略：got %s %q", got, reason)
	}
	// yolo 下工具显式 prompt 也是放行
	got, _ = ResolveRules(ToolDecision{Tier: TierExec, Policy: PolicyPrompt, Reason: "x"}, Rules{}, ModeYolo, "bash", `{}`)
	if got != DecisionAllow {
		t.Fatalf("yolo 下工具显式 prompt 应 allow，got %s", got)
	}
}

func TestResolveRulesOverrideForcesPrompt(t *testing.T) {
	td := ToolDecision{Tier: TierExec, Override: true, Reason: "rm -rf 危险"}
	got, reason := ResolveRules(td, Rules{}, ModeWrite, "bash", `{"command":"rm -rf /"}`)
	if got != DecisionPrompt || !strings.Contains(reason, "rm -rf") {
		t.Fatalf("write 下 Override 应强制 prompt：got %s %q", got, reason)
	}
	// 工具显式 allow 压过 Override
	td.Policy = PolicyAllow
	got, _ = ResolveRules(td, Rules{}, ModeWrite, "bash", `{}`)
	if got != DecisionAllow {
		t.Fatalf("显式 allow 应压过 Override，got %s", got)
	}
}

func TestResolveRulesExplicitPolicyBeatsTier(t *testing.T) {
	got, _ := ResolveRules(ToolDecision{Tier: TierExec, Policy: PolicyAllow}, Rules{}, ModeWrite, "bash", `{}`)
	if got != DecisionAllow {
		t.Fatalf("显式 allow 应压过 tier×mode，got %s", got)
	}
	got, reason := ResolveRules(ToolDecision{Tier: TierRead, Policy: PolicyPrompt, Reason: "需要确认"}, Rules{}, ModeWrite, "read_file", `{}`)
	if got != DecisionPrompt || !strings.Contains(reason, "需要确认") {
		t.Fatalf("显式 prompt 应询问：got %s %q", got, reason)
	}
}

func TestResolveRulesUserRulesBeatTier(t *testing.T) {
	// ask 规则把 read 工具（write 模式默认放行）升级为询问
	rules := Rules{Ask: []Rule{mustRule(t, "read(./.env*)")}}
	got, reason := ResolveRules(ToolDecision{Tier: TierRead}, rules, ModeWrite, "read_file", `{"file_path":"./.env"}`)
	if got != DecisionPrompt || !strings.Contains(reason, ".env*") {
		t.Fatalf("ask 规则应询问：got %s %q", got, reason)
	}
	// allow 规则把 exec 工具（write 模式默认询问）降为放行
	rules = Rules{Allow: []Rule{mustRule(t, "bash(go test*)")}}
	got, _ = ResolveRules(ToolDecision{Tier: TierExec}, rules, ModeWrite, "bash", `{"command":"go test ./..."}`)
	if got != DecisionAllow {
		t.Fatalf("allow 规则应放行，got %s", got)
	}
	// 未命中规则时回落 tier×mode
	got, _ = ResolveRules(ToolDecision{Tier: TierExec}, rules, ModeWrite, "bash", `{"command":"rm x"}`)
	if got != DecisionPrompt {
		t.Fatalf("未命中规则应回落 tier×mode 的 prompt，got %s", got)
	}
}
