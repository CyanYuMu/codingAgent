package main

import (
	"log"

	"github.com/cloudwego/eino-ext/components/model/agenticark"
	"github.com/cloudwego/eino-ext/components/model/agenticdeepseek"
	"github.com/cloudwego/eino-ext/components/model/agenticopenai"
	"github.com/cloudwego/eino-ext/components/model/agenticqwen"
)

// loadModel 根据 cfg.Models[index] 构造一个 eino 的 BaseModel。
//
// 【eino 概念】BaseModel[M] 是 eino 对“聊天模型”的抽象接口。
// agentic* 系列包（agenticqwen/agenticopenai/...）把各家厂商的 API
// 适配成统一的 model.BaseModel[adk.AgenticMessage]，屏蔽掉各家差异。
// 返回的 baseModel 之后会喂给 Agent。
func loadModel(index int) {
	if index < 0 || index >= len(cfg.Models) {
		log.Fatalf("invalid model index: %d", index)
	}
	mc := cfg.Models[index]

	var err error
	switch mc.Provider {
	case ModelProviderQwen:
		baseModel, err = agenticqwen.New(ctx, &agenticqwen.Config{
			APIKey:         mc.APIKey,
			Model:          mc.ModelID,
			BaseURL:        mc.BaseURL,
			EnableThinking: &mc.EnableThinking,
		})
	case ModelProviderOpenAI:
		baseModel, err = agenticopenai.NewResponsesModel(ctx, &agenticopenai.ResponsesConfig{
			APIKey:          mc.APIKey,
			Model:           mc.ModelID,
			BaseURL:         mc.BaseURL,
			EnableAutoCache: true,
		})
	case ModelProviderArk:
		baseModel, err = agenticark.New(ctx, &agenticark.Config{
			APIKey:          mc.APIKey,
			Model:           mc.ModelID,
			BaseURL:         mc.BaseURL,
			EnableAutoCache: true,
		})
	case ModelProviderDeepseek:
		baseModel, err = agenticdeepseek.New(ctx, &agenticdeepseek.Config{
			APIKey:  mc.APIKey,
			Model:   mc.ModelID,
			BaseURL: mc.BaseURL,
		})
	default:
		log.Fatalf("unsupported model provider: %s", mc.Provider)
	}
	if err != nil {
		log.Fatal(err)
	}
}
