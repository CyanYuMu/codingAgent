package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"einoclaw-build/internal/eval"
	"einoclaw-build/internal/memory"
	"einoclaw-build/internal/model"
)

type modelConfig struct {
	APIKey   string `yaml:"api_key"`
	BaseURL  string `yaml:"base_url"`
	Provider string `yaml:"provider"`
	ModelID  string `yaml:"model_id"`
}

type config struct {
	Models []modelConfig `yaml:"models"`
}

func main() {
	data, err := os.ReadFile("config.yaml")
	if err != nil {
		log.Fatalf("读 config.yaml 失败: %v", err)
	}
	var cfg config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		log.Fatal(err)
	}

	m, err := model.New(context.Background(), model.Config{
		Provider: cfg.Models[0].Provider,
		APIKey:   cfg.Models[0].APIKey,
		BaseURL:  cfg.Models[0].BaseURL,
		Model:    cfg.Models[0].ModelID,
	})
	if err != nil {
		log.Fatal(err)
	}

	mem, _ := memory.Open("/tmp/einoclaw-eval-memory.db")
	if mem != nil {
		defer mem.Close()
	}

	// 遍历 evals/ 下的 fixture 目录（含 prompt.md 的目录）
	dirs, _ := filepath.Glob("evals/*/prompt.md")
	if len(dirs) == 0 {
		log.Fatal("未找到 fixture（evals/<name>/prompt.md）")
	}

	passCount := 0
	for _, promptPath := range dirs {
		dir := filepath.Dir(promptPath)
		fx, err := eval.LoadFixture(dir)
		if err != nil {
			fmt.Printf("✗ %s: 加载失败 %v\n", filepath.Base(dir), err)
			continue
		}
		r := eval.Run(context.Background(), fx, m, mem)
		status := "✗ FAIL"
		if r.Pass {
			status = "✓ PASS"
			passCount++
		}
		fmt.Printf("%s %s\n", status, r.Name)
		if r.Detail != "" {
			fmt.Printf("    %s\n", truncate(r.Detail, 200))
		}
	}
	fmt.Printf("\n%d/%d 通过\n", passCount, len(dirs))
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}
