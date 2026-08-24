package tool

import (
	"context"
	"fmt"
	"strings"

	"einoclaw-build/internal/memory"
	"einoclaw-build/internal/permission"
	"einoclaw-build/internal/runtime"
)

// rememberTool 让模型显式写长期记忆。
type rememberTool struct {
	project *memory.Store
	global  *memory.Store // 可 nil：没开全局库时 scope=global 落项目库并说明
}

// NewRememberTool 构造 remember 工具；global 可为 nil。
func NewRememberTool(project, global *memory.Store) Tool {
	return rememberTool{project: project, global: global}
}

func (rememberTool) Name() string { return "remember" }

func (rememberTool) Description() string {
	return "记一条需要跨会话记住的信息。\n" +
		"该记：用户偏好与工作习惯、项目约定（构建/测试命令、目录规范）、已定下的决策与它的理由、外部资源指针。\n" +
		"不该记：密钥或凭据（会被拒绝）、代码本身、当前任务的临时状态（那些属于对话上下文，不是记忆）。\n" +
		"给了 key 就按 key 覆盖同名记忆；没给 key 时内容高度相似的会自动合并到已有条目。\n" +
		"scope=global 表示与具体项目无关的事实（如用户偏好），默认 project。"
}

func (rememberTool) Parameters() map[string]any {
	return map[string]any{
		"content": map[string]any{"type": "string", "description": "要记住的事实，一句话说清"},
		"kind": map[string]any{"type": "string", "enum": []string{"user", "feedback", "project", "reference", "decision", "fact"},
			"description": "记忆类型：user=用户是谁/偏好，feedback=对你工作方式的要求，project=项目约定，reference=外部资源，decision=已定的决策"},
		"key":   map[string]any{"type": "string", "description": "可选的稳定键（如 build-cmd）；同 key 覆盖"},
		"why":   map[string]any{"type": "string", "description": "可选：为什么值得记（决策类尤其要写）"},
		"scope": map[string]any{"type": "string", "enum": []string{"project", "global"}, "description": "作用域，默认 project"},
	}
}

func (rememberTool) Required() []string       { return []string{"content"} }
func (rememberTool) Tier() permission.Tier    { return permission.TierWrite }
func (rememberTool) Concurrency() Concurrency { return ConcurrencyShared }

func (r rememberTool) Execute(_ context.Context, args map[string]any, sink *runtime.Sink) error {
	content, _ := args["content"].(string)
	if strings.TrimSpace(content) == "" {
		return fmt.Errorf("content 必填")
	}
	scope, _ := args["scope"].(string)
	store, note := r.storeFor(scope)
	if store == nil {
		return fmt.Errorf("记忆库不可用")
	}
	kind, _ := args["kind"].(string)
	key, _ := args["key"].(string)
	why, _ := args["why"].(string)

	id, updated, err := store.Remember(content, memory.Opts{
		Kind: kind, Key: key, Why: why, Source: "model", Importance: 0.8,
	})
	if err != nil {
		return err // 例如密钥被拒：错误文本告诉模型该怎么改
	}
	verb := "已记住"
	if updated {
		verb = "已更新已有记忆"
	}
	// 说明生效时机：记忆块是缓存的背景上下文，不会在本轮立刻重新注入
	fmt.Fprintf(sink, "%s（id=%d）%s。它会在下次压缩或新会话时进入背景上下文；本轮请直接用你刚写下的内容。", verb, id, note)
	return nil
}

// storeFor 选库：scope=global 且开了全局库就写全局，否则写项目库并说明。
func (r rememberTool) storeFor(scope string) (*memory.Store, string) {
	if scope == memory.ScopeGlobal {
		if r.global != nil {
			return r.global, "（全局）"
		}
		return r.project, "（未启用全局库，已记入本项目）"
	}
	return r.project, ""
}

// forgetTool 让模型把过时的记忆标为失效。
type forgetTool struct {
	project *memory.Store
	global  *memory.Store
}

// NewForgetTool 构造 forget 工具。
func NewForgetTool(project, global *memory.Store) Tool {
	return forgetTool{project: project, global: global}
}

func (forgetTool) Name() string { return "forget" }

func (forgetTool) Description() string {
	return "把一条过时的记忆标为失效（按 id 或 key）。行不会被删，只是不再进召回——" +
		"发现记忆与现实不符时用它，并在 reason 里写清为什么过时。"
}

func (forgetTool) Parameters() map[string]any {
	return map[string]any{
		"ref":    map[string]any{"type": "string", "description": "记忆 id（数字）或 key"},
		"reason": map[string]any{"type": "string", "description": "为什么过时了"},
		"scope":  map[string]any{"type": "string", "enum": []string{"project", "global"}, "description": "在哪个作用域找，默认 project"},
	}
}

func (forgetTool) Required() []string       { return []string{"ref"} }
func (forgetTool) Tier() permission.Tier    { return permission.TierWrite }
func (forgetTool) Concurrency() Concurrency { return ConcurrencyShared }

func (f forgetTool) Execute(_ context.Context, args map[string]any, sink *runtime.Sink) error {
	ref, _ := args["ref"].(string)
	if strings.TrimSpace(ref) == "" {
		return fmt.Errorf("ref 必填（记忆 id 或 key）")
	}
	reason, _ := args["reason"].(string)
	store := f.project
	if s, _ := args["scope"].(string); s == memory.ScopeGlobal && f.global != nil {
		store = f.global
	}
	if store == nil {
		return fmt.Errorf("记忆库不可用")
	}
	if err := store.Forget(ref, reason); err != nil {
		return err
	}
	fmt.Fprintf(sink, "已失效：%s", ref)
	return nil
}
