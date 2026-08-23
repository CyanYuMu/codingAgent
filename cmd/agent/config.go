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
}

// config 顶层配置。
type config struct {
	Models         []modelConfig    `yaml:"models"`
	ApprovalMode   string           `yaml:"approval_mode"`   // always-ask/write/yolo，默认 write
	MCPServers     []tool.MCPConfig `yaml:"mcp_servers"`     // 外部 MCP server（stdio）
	DelegationMode string           `yaml:"delegation_mode"` // conservative/preferred/always，默认 preferred
	Subagent       subagentConfig   `yaml:"subagent"`
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
