package subagent

import (
	"fmt"
	"strings"
)

// 预检默认值：递归深度 2（主 agent → 子 agent → 孙 agent 即止）；任务描述最少 40 字符。
const (
	defaultMaxDepth     = 2
	defaultMinTaskChars = 40
)

// Env 是一次派发的调用者环境（谁在派、允许派谁、能派多深）。
type Env struct {
	Defs         []AgentDef
	Depth        int                       // 调用者深度：主 agent = 0
	MaxDepth     int                       // 0 = 用默认 2
	Spawns       []string                  // 调用者允许派发的 agent；nil = 无限制（主 agent）；含 "*" = 无限制
	SelfAgent    string                    // 调用者自己的 agent 名（防同名递归）；主 agent = ""
	MinTaskChars int                       // 0 = 用默认 40
	SeqNext      func(agent string) string // 默认命名器；nil = 内部计数器
	NameTaken    func(name string) bool    // 名册里已被占用的名字；nil = 不检查
}

// Resolved 是预检后可以直接执行的一项：定义、生效的 schema 与模式都已定下来。
type Resolved struct {
	Item       TaskItem
	Def        AgentDef
	Schema     map[string]any
	SchemaMode string
}

// Preflight 在起任何子进程之前做全部校验：agent 存在性、深度、spawn policy、任务描述质量、命名去重。
// 任一项不合格整批拒绝——半个批次跑起来更难收拾，且错误文本要能让模型自己改对。
func Preflight(b TaskBatch, env Env) ([]Resolved, error) {
	if len(b.Tasks) == 0 {
		return nil, fmt.Errorf("tasks 必填：至少给一项 {agent, task}")
	}
	if strings.TrimSpace(b.Context) == "" {
		return nil, fmt.Errorf("context 必填：子 agent 看不到你的历史，请写清 Goal（要达成什么）/ Constraints（不能碰什么）/ Contract（跨任务共享的接口或字段）")
	}
	maxDepth := env.MaxDepth
	if maxDepth <= 0 {
		maxDepth = defaultMaxDepth
	}
	if env.Depth >= maxDepth {
		return nil, fmt.Errorf("已达最大委派深度 %d，这一层必须自己完成，不能再派子 agent", maxDepth)
	}
	minChars := env.MinTaskChars
	if minChars <= 0 {
		minChars = defaultMinTaskChars
	}
	seq := env.SeqNext
	if seq == nil {
		n := 0
		seq = func(agent string) string { n++; return fmt.Sprintf("%s-%d", agent, n) }
	}

	used := map[string]bool{}
	out := make([]Resolved, 0, len(b.Tasks))
	for i, item := range b.Tasks {
		def, ok := findDef(env.Defs, item.Agent)
		if !ok {
			return nil, fmt.Errorf("tasks[%d]: 未知 agent %q，可用：%s", i, item.Agent, strings.Join(defNames(env.Defs), ", "))
		}
		if !spawnAllowed(env.Spawns, item.Agent) {
			return nil, fmt.Errorf("tasks[%d]: 不能派发 %q，允许的是：%s", i, item.Agent, strings.Join(env.Spawns, ", "))
		}
		if env.SelfAgent != "" && item.Agent == env.SelfAgent {
			return nil, fmt.Errorf("tasks[%d]: 禁止派发与自己同名的 agent %q（会无限外包），自己完成或换一个 agent", i, item.Agent)
		}
		task := strings.TrimSpace(item.Task)
		if len([]rune(task)) < minChars {
			return nil, fmt.Errorf("tasks[%d]: 任务描述太短（%d 字符）。子 agent 从空白上下文开始，必须写明 Target（涉及的文件/符号与非目标）、Change（要做的改动/步骤）、Acceptance（可观察的验收结果）",
				i, len([]rune(task)))
		}
		item.Task = task
		item.Name = uniqueName(item.Name, item.Agent, used, seq, env.NameTaken)
		used[item.Name] = true

		schema := item.OutputSchema
		if schema == nil {
			schema = def.OutputSchema
		}
		mode := strings.TrimSpace(item.SchemaMode)
		if mode == "" {
			mode = def.EffectiveSchemaMode()
		}
		if mode != SchemaModeStrict {
			mode = SchemaModePermissive
		}
		out = append(out, Resolved{Item: item, Def: def, Schema: schema, SchemaMode: mode})
	}
	return out, nil
}

// uniqueName 生成本批内唯一、且不与名册冲突的运行名。
func uniqueName(want, agent string, used map[string]bool, seq func(string) string, taken func(string) bool) string {
	name := sanitizeName(want)
	if want == "" || name == "" {
		name = sanitizeName(seq(agent))
	}
	conflict := func(n string) bool {
		if used[n] {
			return true
		}
		return taken != nil && taken(n)
	}
	if !conflict(name) {
		return name
	}
	for i := 2; ; i++ {
		cand := fmt.Sprintf("%s-%d", name, i)
		if !conflict(cand) {
			return cand
		}
	}
}

// spawnAllowed 判断调用者是否可以派发目标 agent。nil / 含 "*" = 无限制；空列表 = 全部拒绝。
func spawnAllowed(spawns []string, agent string) bool {
	if spawns == nil {
		return true
	}
	for _, s := range spawns {
		if s == "*" {
			return true
		}
		if s == agent {
			return true
		}
	}
	return false
}

func findDef(defs []AgentDef, name string) (AgentDef, bool) {
	for _, d := range defs {
		if d.Name == name {
			return d, true
		}
	}
	return AgentDef{}, false
}

func defNames(defs []AgentDef) []string {
	out := make([]string, 0, len(defs))
	for _, d := range defs {
		out = append(out, d.Name)
	}
	return out
}

// sanitizeName 把运行名收敛到 [A-Za-z0-9_-]（要当文件名与 hub 地址用）。
func sanitizeName(n string) string {
	var b strings.Builder
	for _, r := range n {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}
