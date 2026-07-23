package main

// ModelProvider 标识模型服务商
type ModelProvider string

const (
	ModelProviderQwen     ModelProvider = "qwen"
	ModelProviderOpenAI   ModelProvider = "openai"
	ModelProviderArk      ModelProvider = "ark"
	ModelProviderDeepseek ModelProvider = "deepseek"
)

// modelConfig 单个模型的配置
// 阶段1只用到这几个字段；后续阶段会扩展 config 结构
type modelConfig struct {
	APIKey         string        `yaml:"api_key"`
	BaseURL        string        `yaml:"base_url"`
	Provider       ModelProvider `yaml:"provider"`
	ModelName      string        `yaml:"model_name"`
	ModelID        string        `yaml:"model_id"`
	EnableThinking bool          `yaml:"enable_thinking"`
}

// config 顶层配置
// 阶段1只有 models；handlers/coze_loop 等会在后续阶段逐步加入
type config struct {
	Models []modelConfig `yaml:"models"`
}
