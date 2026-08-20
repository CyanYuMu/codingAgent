package main

import (
	"errors"
	"fmt"
	"log"
	"os"

	"gopkg.in/yaml.v3"
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
}

// config 顶层配置。
type config struct {
	Models []modelConfig `yaml:"models"`
}

// loadConfig 读取项目目录下的 ./config.yaml；不存在或未填 APIKey 则提示并退出。
func loadConfig() config {
	data, err := os.ReadFile("config.yaml")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			fmt.Println("未找到配置文件: ./config.yaml\n请参考项目目录下的 example.yaml 创建并填入模型配置后重试。")
			os.Exit(0)
		}
		log.Fatal(err)
	}
	var cfg config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		log.Fatal(err)
	}
	if len(cfg.Models) == 0 || cfg.Models[0].APIKey == "" {
		fmt.Println("请在配置文件中填入模型相关配置后重试。")
		os.Exit(0)
	}
	return cfg
}
