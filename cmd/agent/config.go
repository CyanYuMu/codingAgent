package main

import (
	"errors"
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"

	"einoclaw-build/internal/paths"
	"einoclaw-build/internal/tool"
)

// ModelProvider 标识模型服务商。
type ModelProvider string

const (
	ModelProviderQwen     ModelProvider = "qwen"
	ModelProviderOpenAI   ModelProvider = "openai"
	ModelProviderArk      ModelProvider = "ark"
	ModelProviderDeepseek ModelProvider = "deepseek"
)

// modelConfig 单个模型的配置。
type modelConfig struct {
	APIKey         string        `yaml:"api_key"`
	BaseURL        string        `yaml:"base_url"`
	Provider       ModelProvider `yaml:"provider"`
	ModelName      string        `yaml:"model_name"`
	ModelID        string        `yaml:"model_id"`
	EnableThinking bool          `yaml:"enable_thinking"`
	ContextWindow  int           `yaml:"context_window"` // 上下文窗口大小，0 用默认
}

// subagentConfig 子 agent 运行时配置。
type subagentConfig struct {
	MaxConcurrency     int           `yaml:"max_concurrency"`
	ApprovalEscalation bool          `yaml:"approval_escalation"` // headless 子 agent 的 Prompt 决策升级到父弹窗
	DefaultTimeout     time.Duration `yaml:"default_timeout"`
	DefaultMaxTurns    int           `yaml:"default_max_turns"`
	SoftBudget         int           `yaml:"soft_budget"`         // 累计模型请求软预算上限；0 = 关闭护栏
	MaxRecursionDepth  int           `yaml:"max_recursion_depth"` // 委派递归深度上限
	MinTaskChars       int           `yaml:"min_task_chars"`      // 任务描述最短长度（拒绝一句话派发）
	Background         *bool         `yaml:"background"`          // 是否允许 task background:true；默认 true
}

// BackgroundEnabled 返回是否允许后台作业（未配置时默认允许）。
func (s subagentConfig) BackgroundEnabled() bool { return s.Background == nil || *s.Background }

// memoryConfig 记忆与召回配置。
type memoryConfig struct {
	Global      *bool `yaml:"global"`        // 是否启用 <Home>/memory/global.db；默认启用
	RecallTopK  int   `yaml:"recall_top_k"`  // 每轮注入几条
	MaxPerScope int   `yaml:"max_per_scope"` // 每个作用域的条数上限
	ProjectMap  *bool `yaml:"project_map"`   // 会话首轮注入项目地图（P10.4）
	ReadNotes   *bool `yaml:"read_notes"`    // read_file 命中未变更文件时用笔记顶替内容（默认关）
}

// GlobalEnabled 未配置时默认启用全局记忆库。
func (m memoryConfig) GlobalEnabled() bool { return m.Global == nil || *m.Global }

// ProjectMapEnabled 未配置时默认注入项目地图。
func (m memoryConfig) ProjectMapEnabled() bool { return m.ProjectMap == nil || *m.ProjectMap }

// ReadNotesEnabled 默认关：拿摘要冒充文件内容会让模型基于旧信息改代码。
func (m memoryConfig) ReadNotesEnabled() bool { return m.ReadNotes != nil && *m.ReadNotes }

// config 顶层配置。
type config struct {
	Models         []modelConfig    `yaml:"models"`
	ApprovalMode   string           `yaml:"approval_mode"`   // always-ask/write/yolo，默认 write
	MCPServers     []tool.MCPConfig `yaml:"mcp_servers"`     // 外部 MCP server（stdio）
	DelegationMode string           `yaml:"delegation_mode"` // conservative/preferred/always，默认 preferred
	Subagent       subagentConfig   `yaml:"subagent"`
	Memory         memoryConfig     `yaml:"memory"`
}

// configPaths 返回三层配置路径（用户 → 项目 → 仓库内 legacy），后者覆盖前者。
func configPaths(cwd string) []string {
	var out []string
	if p, err := paths.UserConfigPath(); err == nil {
		out = append(out, p)
	}
	out = append(out, paths.ProjectConfigPath(cwd), "config.yaml")
	return out
}

// loadConfigFrom 按顺序读取存在的文件并合并（后者覆盖前者的非零值），最后补默认值。
func loadConfigFrom(files []string) (config, error) {
	var cfg config
	found := false
	for _, p := range files {
		data, err := os.ReadFile(p)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return cfg, err
		}
		var layer config
		if err := yaml.Unmarshal(data, &layer); err != nil {
			return cfg, fmt.Errorf("%s: %w", p, err)
		}
		mergeConfig(&cfg, layer)
		found = true
	}
	if !found || len(cfg.Models) == 0 || cfg.Models[0].APIKey == "" {
		return cfg, errors.New("未找到模型配置：请在 ~/.codeclaw/config.yaml 或 <项目>/.codeclaw/config.yaml 填入 models（参考 example.yaml）")
	}
	applyDefaults(&cfg)
	return cfg, nil
}

// mergeConfig 把 src 的非零字段覆盖进 dst；MCP servers 累加。
func mergeConfig(dst *config, src config) {
	if len(src.Models) > 0 {
		dst.Models = src.Models
	}
	if src.ApprovalMode != "" {
		dst.ApprovalMode = src.ApprovalMode
	}
	if len(src.MCPServers) > 0 {
		dst.MCPServers = append(dst.MCPServers, src.MCPServers...)
	}
	if src.DelegationMode != "" {
		dst.DelegationMode = src.DelegationMode
	}
	if src.Subagent.MaxConcurrency != 0 {
		dst.Subagent.MaxConcurrency = src.Subagent.MaxConcurrency
	}
	if src.Subagent.ApprovalEscalation {
		dst.Subagent.ApprovalEscalation = true
	}
	if src.Subagent.DefaultTimeout != 0 {
		dst.Subagent.DefaultTimeout = src.Subagent.DefaultTimeout
	}
	if src.Subagent.DefaultMaxTurns != 0 {
		dst.Subagent.DefaultMaxTurns = src.Subagent.DefaultMaxTurns
	}
	if src.Subagent.SoftBudget != 0 {
		dst.Subagent.SoftBudget = src.Subagent.SoftBudget
	}
	if src.Subagent.MaxRecursionDepth != 0 {
		dst.Subagent.MaxRecursionDepth = src.Subagent.MaxRecursionDepth
	}
	if src.Subagent.MinTaskChars != 0 {
		dst.Subagent.MinTaskChars = src.Subagent.MinTaskChars
	}
	if src.Subagent.Background != nil {
		dst.Subagent.Background = src.Subagent.Background
	}
	if src.Memory.Global != nil {
		dst.Memory.Global = src.Memory.Global
	}
	if src.Memory.RecallTopK != 0 {
		dst.Memory.RecallTopK = src.Memory.RecallTopK
	}
	if src.Memory.MaxPerScope != 0 {
		dst.Memory.MaxPerScope = src.Memory.MaxPerScope
	}
	if src.Memory.ProjectMap != nil {
		dst.Memory.ProjectMap = src.Memory.ProjectMap
	}
	if src.Memory.ReadNotes != nil {
		dst.Memory.ReadNotes = src.Memory.ReadNotes
	}
}

// applyDefaults 补默认值：approval_mode=write、delegation_mode=preferred、窗口 128k、子 agent 并发 4 / 超时 10m / 50 轮。
func applyDefaults(cfg *config) {
	if cfg.Models[0].ContextWindow == 0 {
		cfg.Models[0].ContextWindow = 128000
	}
	if cfg.ApprovalMode == "" {
		cfg.ApprovalMode = "write"
	}
	if cfg.DelegationMode == "" {
		cfg.DelegationMode = "preferred"
	}
	if cfg.Subagent.MaxConcurrency == 0 {
		cfg.Subagent.MaxConcurrency = 4
	}
	if cfg.Subagent.DefaultTimeout == 0 {
		cfg.Subagent.DefaultTimeout = 10 * time.Minute
	}
	if cfg.Subagent.DefaultMaxTurns == 0 {
		cfg.Subagent.DefaultMaxTurns = 50
	}
	if cfg.Subagent.SoftBudget == 0 {
		cfg.Subagent.SoftBudget = 200
	}
	if cfg.Subagent.MaxRecursionDepth == 0 {
		cfg.Subagent.MaxRecursionDepth = 2
	}
	if cfg.Subagent.MinTaskChars == 0 {
		cfg.Subagent.MinTaskChars = 40
	}
	if cfg.Memory.RecallTopK == 0 {
		cfg.Memory.RecallTopK = 5
	}
	if cfg.Memory.MaxPerScope == 0 {
		cfg.Memory.MaxPerScope = 500
	}
}

// loadConfig 读取三层配置；失败则打印原因退出。
func loadConfig(cwd string) config {
	cfg, err := loadConfigFrom(configPaths(cwd))
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	return cfg
}
