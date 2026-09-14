package tool

import (
	"context"
	"fmt"

	"einoclaw-build/internal/permission"
	"einoclaw-build/internal/runtime"
	"einoclaw-build/internal/skills"
)

// skillTool lets the model progressively load a discovered SKILL.md. Loading
// instructions is read-only; any actions requested by the skill still go
// through their normal tools and permission tiers.
type skillTool struct{ catalog *skills.Manager }

// NewSkillTool constructs the model-facing skill loader.
func NewSkillTool(catalog *skills.Manager) Tool { return skillTool{catalog: catalog} }

func (skillTool) Name() string { return "skill" }
func (skillTool) Description() string {
	return "按名称加载一个可用 skill 的完整说明。system prompt 中的 available-skills 只有索引；任务匹配时先调用本工具，再按返回说明工作。"
}
func (skillTool) Parameters() map[string]any {
	return map[string]any{
		"name": map[string]any{"type": "string", "description": "available-skills 中的 skill 名称"},
		"args": map[string]any{"type": "string", "description": "传给 skill 的当前任务或额外要求，可省略"},
	}
}
func (skillTool) Required() []string       { return []string{"name"} }
func (skillTool) Tier() permission.Tier    { return permission.TierRead }
func (skillTool) Concurrency() Concurrency { return ConcurrencyShared }

func (t skillTool) Execute(_ context.Context, args map[string]any, sink *runtime.Sink) error {
	if t.catalog == nil {
		return fmt.Errorf("skills catalog 未配置")
	}
	name, _ := args["name"].(string)
	if name == "" {
		return fmt.Errorf("name 必填")
	}
	userArgs, _ := args["args"].(string)
	prompt, _, err := t.catalog.BuildPrompt(name, userArgs, skills.InvocationModel)
	if err != nil {
		return err
	}
	_, err = sink.Write([]byte(prompt))
	return err
}
