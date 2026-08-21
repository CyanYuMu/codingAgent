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
		specs = append(specs, model.ToolSpec{
			Name:        t.Name(),
			Description: t.Description(),
			Parameters:  t.Parameters(),
		})
	}
	return specs
}
