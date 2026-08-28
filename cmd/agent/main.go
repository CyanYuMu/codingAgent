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
	"einoclaw-build/internal/bus"
	agentctx "einoclaw-build/internal/context"
	"einoclaw-build/internal/instructions"
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

// projectMapBudget 项目地图的注入预算（估算 token）：给模型一张「哪个文件管什么」的地图，
// 新会话不用从零 explore。
const projectMapBudget = 1500

// renderMemories 把召回的记忆渲染成 <memories> 背景块。
func renderMemories(mems []memory.Memory) string {
	var sb strings.Builder
	sb.WriteString("<memories>\n")
	for _, m := range mems {
		fmt.Fprintf(&sb, "- [%s · %s] %s", m.Kind, m.Scope, m.Content)
		if m.Why != "" {
			fmt.Fprintf(&sb, "（因为：%s）", m.Why)
		}
		fmt.Fprintf(&sb, " (id=%d)\n", m.ID)
	}
	sb.WriteString("</memories>\n（以上是背景上下文，当前用户消息和工具结果优先；发现某条与现实不符就用 forget 让它失效。）")
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

// discoverAgents 发现子 agent 定义：项目 → 用户 → 内置（同名 first-wins）；坏文件只告警。
func discoverAgents(cwd string) []subagent.AgentDef {
	userDir, err := paths.UserAgentsDir()
	if err != nil {
		log.Printf("用户 agent 目录不可用: %v", err)
	}
	res := subagent.Discover(paths.ProjectAgentsDir(cwd), userDir, subagent.Bundled())
	for _, w := range res.Warns {
		log.Printf("跳过 agent 定义: %v", w)
	}
	return res.Defs
}

// loadProjectInstructions 加载 L1 项目指令层（AGENTS.md / CLAUDE.md / RULES.md，含 @import）。
// 读不到就是没有，不该让 agent 起不来。
func loadProjectInstructions(cwd string) string {
	home, err := paths.Home()
	if err != nil {
		home = ""
	}
	b, err := instructions.Load(cwd, home, 0)
	if err != nil {
		log.Printf("项目指令层加载失败: %v", err)
		return ""
	}
	for _, f := range b.Files {
		log.Printf("已加载项目指令: %s", f.Path)
	}
	return b.Text
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
	waitJobs := flag.Duration("wait-jobs", 0, "headless：等后台作业结算的上限（默认取 subagent.default_timeout）")
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

	projectID, err := paths.ProjectID(cwd)
	if err != nil {
		projectID = cwd
	}
	mem, err := memory.Open(filepath.Join(projectDir, "memory.db"), memory.ScopeProject, projectID)
	if err != nil {
		log.Printf("项目记忆库不可用，禁用项目记忆: %v", err)
		mem = nil
	}
	if mem != nil {
		mem.SetMaxPerScope(cfg.Memory.MaxPerScope)
		defer mem.Close()
	}
	// 全局库：跨项目的用户偏好类事实（"用户偏好中文回复"）不该只活在一个项目里
	var globalMem *memory.Store
	if cfg.Memory.GlobalEnabled() {
		if gp, err := paths.GlobalMemoryPath(); err != nil {
			log.Printf("全局记忆库路径不可用: %v", err)
		} else if g, err := memory.Open(gp, memory.ScopeGlobal, ""); err != nil {
			log.Printf("全局记忆库不可用: %v", err)
		} else {
			globalMem = g
			globalMem.SetMaxPerScope(cfg.Memory.MaxPerScope)
			defer globalMem.Close()
		}
	}
	recaller := memory.Union(mem, globalMem)
	// 项目笔记沉淀：explorer 类子 agent 结算时确定性 upsert（summary = role）
	var notes subagent.NoteSink
	if mem != nil {
		notes = mem
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
			reg.Register(tool.NewRememberTool(mem, globalMem))
			reg.Register(tool.NewForgetTool(mem, globalMem))
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
	evbus := bus.New()
	mgr := subagent.NewManager(subagent.Options{
		Model: m, WorkerTools: workerTools, Memory: mem, Mode: mode, Approver: approver,
		Escalate: cfg.Subagent.ApprovalEscalation, SessionDir: artifactDir, CWD: cwd,
		MaxConcurrency: cfg.Subagent.MaxConcurrency, Defs: discoverAgents(cwd), Summarizer: summ,
		ContextWindow: cfg.Models[0].ContextWindow, Bus: evbus,
		DefaultTimeout: cfg.Subagent.DefaultTimeout, DefaultMaxTurns: cfg.Subagent.DefaultMaxTurns,
		SoftBudget: cfg.Subagent.SoftBudget, MaxDepth: cfg.Subagent.MaxRecursionDepth,
		MinTaskChars: cfg.Subagent.MinTaskChars, AllowBackground: cfg.Subagent.BackgroundEnabled(),
		Notes: notes,
	})

	mgr.RegisterSchemes(store) // read_file 可读 agent://<子agent名> 与 history://<子agent名>

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
	mainRegistry.Register(subagent.NewTaskTool(mgr, 0, "", nil)) // 主 agent：深度 0、可派任意 agent
	mainRegistry.Register(subagent.NewHubTool(mgr, subagent.MainName))
	exec := tool.NewExecutor(mainRegistry, mode, approver)
	exec.SetArtifactStore(store)

	// system 前缀的排布是固定的：[基础指令 + env] [项目指令层] [记忆块]，
	// 而且整块被 context.Manager 缓存，只在会话首轮/压缩后/换会话时重算——
	// 前缀每轮变化 = provider 的 prompt cache 每轮失效，这是长会话里最贵的隐性成本。
	instr := buildInstruction(cfg.DelegationMode) + envBlock(cwd)
	projectBlock := loadProjectInstructions(cwd)
	var cmgr *agentctx.Manager
	system := func(ctx context.Context) []message.Message {
		msgs := []message.Message{message.NewSystemMessage(instr)}
		if projectBlock != "" {
			msgs = append(msgs, message.NewSystemMessage(projectBlock))
		}
		if mem == nil {
			return msgs
		}
		hist, err := cmgr.Session().Replay()
		if err != nil {
			log.Printf("回放会话失败，跳过记忆召回: %v", err)
			return msgs
		}
		mems, err := recaller.Recall(memory.BuildRecallQuery(hist), cfg.Memory.RecallTopK)
		if err != nil {
			log.Printf("记忆召回失败: %v", err)
			return msgs
		}
		if len(mems) > 0 {
			msgs = append(msgs, message.NewSystemMessage(renderMemories(mems)))
		}
		// 项目地图：file_notes 的跨会话项目知识。跟随前缀缓存，只在首轮/压缩后/换会话时刷新。
		if cfg.Memory.ProjectMapEnabled() && mem != nil {
			if pm := mem.ProjectMap(projectMapBudget); pm != "" {
				msgs = append(msgs, message.NewSystemMessage(pm))
			}
		}
		return msgs
	}
	cmgr = agentctx.New(s, summ, cfg.Models[0].ContextWindow, 16384, system)
	ag := agent.New("codeclaw", m, mainRegistry, exec, cmgr)

	defer func() {
		if err := mgr.Shutdown(5 * time.Second); err != nil {
			log.Print(err)
		}
	}()

	if *prompt != "" {
		wait := *waitJobs
		if wait == 0 {
			wait = cfg.Subagent.DefaultTimeout
		}
		code := runHeadless(context.Background(), ag, cmgr, mgr, *prompt, wait)
		_ = mgr.Shutdown(5 * time.Second)
		os.Exit(code)
	}

	program := tea.NewProgram(tui.NewModel(ag, sessMgr, cmgr, mem, cwd, mgr, evbus))
	tui.SetProgram(program)
	if _, err := program.Run(); err != nil {
		log.Fatal(err)
	}
}
