package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"einoclaw-build/internal/agent"
	agentctx "einoclaw-build/internal/context"
	"einoclaw-build/internal/memory"
	"einoclaw-build/internal/message"
	"einoclaw-build/internal/model"
	"einoclaw-build/internal/paths"
	"einoclaw-build/internal/permission"
	rt "einoclaw-build/internal/runtime"
	"einoclaw-build/internal/session"
	"einoclaw-build/internal/subagent"
	"einoclaw-build/internal/tool"
	"einoclaw-build/internal/tui"
)

const baseInstruction = "你是一个编程智能体, 你的名字叫做 codeclaw, 擅长解决编程问题。当用户表达偏好、关键事实或重要决策时，调用 remember 工具记录。"

const alwaysDelegation = `
你是协调者（coordinator），不是执行者。你的工作是：理解任务 → 分解 → 派发子 agent → 综合结果 → 验收。

何时必须委派（MUST delegate）：
- 3+ 文件或跨模块改动 → 分解并委派
- 多个互相独立的调查/验证问题 → 并行派多个子 agent
- 探索未知代码库 → 派 explorer，禁止自己逐文件读
- 非平凡实现/改动后 → 派 reviewer 验收
- 长耗时验证/测试 → 派 worker

唯一例外（可自己做）：约 30 行内单文件编辑、直接回答、用户明确要求你执行某命令、只有一个可运行的 slice（单个子 agent 只是有损转交，不是并行）。

反例（禁止）：
- 委派后不要自己又读一遍文件
- 不要派子 agent 后自己 idle 等
- 不要「派了又自己做一遍」
- 顶层计划必须自己拆，不能外包给子 agent
- 必须拆成真正独立的 slice，禁止假并行
- 只有严格依赖才串行，否则并行
- 每个任务都要写明跳过 formatter/lint/全量测试，统一在最后做一次

子 agent 的 completed 只表示它结束了，不代表结果可接受：综合前必须验收（派 reviewer 或自己核对）。
`

const preferredDelegation = `
多文件改动、独立调查、验证、测试是委派的 strong candidate，优先委派并行；小任务可自己做。子 agent 的结果需要验收。
`

// buildInstruction 按委派模式生成系统提示词。
func buildInstruction(mode string) string {
	switch mode {
	case "always":
		return baseInstruction + alwaysDelegation
	case "conservative":
		return baseInstruction + "\n除非用户明确要求，否则不要派子 agent，自己完成任务。"
	default: // preferred
		return baseInstruction + preferredDelegation
	}
}

// envBlock 生成 L1 环境块：cwd / git root / 日期 / 平台。
func envBlock(cwd string) string {
	var sb strings.Builder
	sb.WriteString("\n\n<env>\n")
	fmt.Fprintf(&sb, "cwd: %s\n", cwd)
	if root := paths.GitRoot(cwd); root != "" {
		fmt.Fprintf(&sb, "git_root: %s\n", root)
	}
	fmt.Fprintf(&sb, "date: %s\n", time.Now().Format("2006-01-02"))
	fmt.Fprintf(&sb, "platform: %s/%s\n", runtime.GOOS, runtime.GOARCH)
	sb.WriteString("</env>")
	return sb.String()
}

// renderMemories 把召回的记忆渲染成 <memories> 背景块。
func renderMemories(mems []memory.Memory) string {
	var sb strings.Builder
	sb.WriteString("<memories>\n")
	for _, m := range mems {
		fmt.Fprintf(&sb, "- [%s] %s（置信 %.1f）\n", m.MemoryType, m.Content, m.Veracity)
	}
	sb.WriteString("</memories>\n（以上是背景上下文，当前用户消息和工具结果优先。）")
	return sb.String()
}

// lastUserText 返回最后一条 user 消息的文本（作为召回 query）。
func lastUserText(msgs []message.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == message.RoleUser {
			var sb strings.Builder
			for _, b := range msgs[i].Blocks {
				if b.Kind == message.BlockText {
					sb.WriteString(b.Text)
				}
			}
			return sb.String()
		}
	}
	return ""
}

func parseMode(s string) permission.Mode {
	switch s {
	case "always-ask":
		return permission.ModeAlwaysAsk
	case "yolo":
		return permission.ModeYolo
	default:
		return permission.ModeWrite
	}
}

// builtinDefs 内置子 agent 定义；超时与轮次上限来自配置。
func builtinDefs(cfg config) []subagent.SubagentSpec {
	mk := func(name, desc, when, prompt string) subagent.SubagentSpec {
		return subagent.SubagentSpec{Name: name, Description: desc, WhenToUse: when, SystemPrompt: prompt,
			Timeout: cfg.Subagent.DefaultTimeout, MaxTurns: cfg.Subagent.DefaultMaxTurns}
	}
	return []subagent.SubagentSpec{
		mk("reviewer", "代码审查", "非平凡实现/改动后验收、代码审查", "你是代码审查专家：核对改动是否满足任务的验收标准，找出正确性问题，用 yield 提交 {findings:[{file,line,severity,summary}], verdict}。"),
		mk("explorer", "探索项目", "探索未知代码库、定位相关代码", "你是项目探索专家：用 glob/grep/read_file 梳理项目结构、定位相关代码，用 yield 提交 {files:[{path,role}], entrypoints:[], notes}。不要修改文件。"),
		mk("planner", "任务规划", "把复杂任务拆解成步骤", "你是任务规划专家：把复杂任务拆解成彼此独立、可并行的步骤，每步给出 Target/Change/Acceptance，用 yield 提交 {steps:[{name,target,change,acceptance}]}。"),
		mk("worker", "实现与验证", "具体的编码实现、跑测试", "你是实现者：按任务说明修改代码并做最小验证，用 yield 提交 {changed_files:[], verification, notes}。"),
	}
}

// warnLegacyData 检测仓库内旧的 sessions/ 与 memory.db（P8 之前的落点），提示一次迁移。
func warnLegacyData(cwd, projectDir string) {
	for _, p := range []string{"sessions", "memory.db"} {
		if _, err := os.Stat(filepath.Join(cwd, p)); err == nil {
			fmt.Fprintf(os.Stderr, "提示：检测到旧数据 ./%s；新版本数据位于 %s，旧数据不会自动迁移。\n", p, projectDir)
		}
	}
}

func main() {
	cwdFlag := flag.String("cwd", "", "工作目录（默认当前目录）")
	yolo := flag.Bool("yolo", false, "强制 approval_mode=yolo")
	prompt := flag.String("p", "", "headless：执行一个提示词后退出（事件打印到 stdout）")
	flag.Parse()

	cwd := *cwdFlag
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	if c, err := paths.Canonical(cwd); err == nil {
		cwd = c
	}
	cfg := loadConfig(cwd)
	if *yolo {
		cfg.ApprovalMode = "yolo"
	}

	m, err := model.New(context.Background(), model.Config{
		Provider: string(cfg.Models[0].Provider),
		APIKey:   cfg.Models[0].APIKey,
		BaseURL:  cfg.Models[0].BaseURL,
		Model:    cfg.Models[0].ModelID,
	})
	if err != nil {
		log.Fatal(err)
	}

	// 项目桶：会话 / 产物 / 记忆 全部按 cwd 隔离
	projectDir, err := paths.ProjectDir(cwd)
	if err != nil {
		log.Fatal(err)
	}
	warnLegacyData(cwd, projectDir)

	mem, err := memory.Open(filepath.Join(projectDir, "memory.db"))
	if err != nil {
		log.Printf("记忆库不可用，禁用记忆: %v", err)
		mem = nil
	}
	if mem != nil {
		defer mem.Close()
	}

	sessMgr, err := session.NewManager(projectDir)
	if err != nil {
		log.Fatal(err)
	}
	s, err := sessMgr.Current(cwd)
	if err != nil {
		log.Fatal(err)
	}
	defer s.Close()
	artifactDir, err := sessMgr.ArtifactDir(s)
	if err != nil {
		log.Fatal(err)
	}
	store := rt.NewArtifactStore(artifactDir)

	// worker 工具工厂：每个调用方（主 agent / 每个子 agent）拿到独立的 bash 实例
	workerTools := func(cwd string, store *rt.ArtifactStore) *tool.Registry {
		reg := tool.NewRegistry()
		for _, t := range tool.Builtins(rt.NewBash(cwd), store) {
			reg.Register(t)
		}
		if mem != nil {
			reg.Register(tool.NewRememberTool(mem))
		}
		for _, srv := range cfg.MCPServers {
			if err := tool.ConnectMCP(context.Background(), reg, srv); err != nil {
				log.Printf("MCP server %s 连接失败: %v", srv.Name, err)
			}
		}
		return reg
	}

	mode := parseMode(cfg.ApprovalMode)
	var approver tool.Approver = headlessApprover{}
	if *prompt == "" {
		approver = tui.NewApprover()
	}

	summ := agentctx.NewModelSummarizer(m)
	mgr := subagent.NewManager(subagent.Options{
		Model: m, WorkerTools: workerTools, Memory: mem, Mode: mode, Approver: approver,
		Escalate: cfg.Subagent.ApprovalEscalation, SessionDir: artifactDir, CWD: cwd,
		MaxConcurrency: cfg.Subagent.MaxConcurrency, Defs: builtinDefs(cfg), Summarizer: summ,
		ContextWindow: cfg.Models[0].ContextWindow,
	})

	// 主 agent 工具集（按委派模式）：always = 只读工具 + task + remember；其它 = 全套 + task
	mainRegistry := tool.NewRegistry()
	full := workerTools(cwd, store)
	if cfg.DelegationMode == "always" {
		for _, t := range full.List() {
			if t.Tier() == permission.TierRead || t.Name() == "remember" {
				mainRegistry.Register(t)
			}
		}
	} else {
		for _, t := range full.List() {
			mainRegistry.Register(t)
		}
	}
	mainRegistry.Register(subagent.NewTaskTool(mgr))
	exec := tool.NewExecutor(mainRegistry, mode, approver)
	exec.SetArtifactStore(store)

	// system 前缀：指令 + 环境块 + 记忆块（按当前会话最后一条用户消息召回）
	instr := buildInstruction(cfg.DelegationMode) + envBlock(cwd)
	var cmgr *agentctx.Manager
	system := func(ctx context.Context) []message.Message {
		msgs := []message.Message{message.NewSystemMessage(instr)}
		if mem != nil {
			if hist, err := cmgr.Session().Replay(); err == nil {
				if q := lastUserText(hist); q != "" {
					if mems, err := mem.Recall(q, 5); err == nil && len(mems) > 0 {
						msgs = append(msgs, message.NewSystemMessage(renderMemories(mems)))
					}
				}
			}
		}
		return msgs
	}
	cmgr = agentctx.New(s, summ, cfg.Models[0].ContextWindow, 16384, system)
	ag := agent.New("codeclaw", m, mainRegistry, exec, cmgr)

	if *prompt != "" {
		os.Exit(runHeadless(context.Background(), ag, cmgr, *prompt))
	}

	program := tea.NewProgram(tui.NewModel(ag, sessMgr, cmgr, mem, cwd))
	tui.SetProgram(program)
	if _, err := program.Run(); err != nil {
		log.Fatal(err)
	}
}
