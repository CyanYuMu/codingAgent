package tool

import (
	"sort"
	"sync"

	"einoclaw-build/internal/model"
)

// Registry 统一注册表：把内置/MCP（P6）工具归一到一个名字空间。
type Registry struct {
	mu    sync.RWMutex
	tools map[string]Tool
}

func NewRegistry() *Registry {
	return &Registry{tools: map[string]Tool{}}
}

// Register 注册一个工具；同名覆盖（后注册者胜）。
func (r *Registry) Register(t Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tools[t.Name()] = t
}

// Get 按名字取工具。
func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

// Without 返回一个不含指定名字工具的新注册表（供子 agent 用，防递归派发）。
func (r *Registry) Without(name string) *Registry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	nr := NewRegistry()
	for n, t := range r.tools {
		if n != name {
			nr.tools[n] = t
		}
	}
	return nr
}

// List 返回所有工具（按名字排序），供复制/枚举。
func (r *Registry) List() []Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.tools))
	for n := range r.tools {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]Tool, 0, len(names))
	for _, n := range names {
		out = append(out, r.tools[n])
	}
	return out
}

// ConvState 是带会话级状态的工具（如 read_file 的已读区间记录）。
type ConvState interface{ ResetConv() }

// ResetConv 重置所有工具的会话级状态。宿主在换会话（/new /resume）时调用：
// 这些状态以「内容仍在上文中」为前提，跨会话保留会变成谎话。
func (r *Registry) ResetConv() {
	for _, t := range r.List() {
		if c, ok := t.(ConvState); ok {
			c.ResetConv()
		}
	}
}

// Specs 转成给模型的工具定义（按名字排序，保证稳定）。
func (r *Registry) Specs() []model.ToolSpec {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.tools))
	for n := range r.tools {
		names = append(names, n)
	}
	sort.Strings(names)
	specs := make([]model.ToolSpec, 0, len(names))
	for _, n := range names {
		t := r.tools[n]
		spec := model.ToolSpec{
			Name:        t.Name(),
			Description: t.Description(),
			Parameters:  t.Parameters(),
		}
		if rp, ok := t.(RequiredParams); ok {
			spec.Required = rp.Required()
		}
		specs = append(specs, spec)
	}
	return specs
}
