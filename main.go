package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/adk/session"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"gopkg.in/yaml.v3"
)

// 阶段3 起：主协程跑 BubbleTea 事件循环，TurnLoop 在后台 goroutine 跑。
// 两者靠 program.Send / Update 这条消息通道接合(双协程架构)。
var (
	cfg       *config
	baseModel model.BaseModel[adk.AgenticMessage]
	agent     adk.TypedAgent[adk.AgenticMessage]
	turnLoop  *adk.TurnLoop[chatItem, adk.AgenticMessage]
	program   *tea.Program // 桥梁：后台 OnAgentEvents 通过它把事件塞进 TUI 主循环

	sessionStore adk.SessionEventStore[adk.AgenticMessage]
	sessionID    string

	ctx = context.Background()
)

// chatItem 是 TurnLoop 的泛型参数 T：一个待处理的用户输入项。
type chatItem struct {
	query string
}

func main() {
	loadConfig()
	loadModel(0)
	loadAgent()
	adk.SetLanguage(adk.LanguageChinese) // 框架内置提示词改用中文
	initTurnLoop()

	// 启动 TUI(阻塞)。主协程从此进入 BubbleTea 事件循环。
	program = tea.NewProgram(newTeaModel())
	if _, err := program.Run(); err != nil {
		log.Fatal(err)
	}

	// 用户退出后，收尾 TurnLoop
	turnLoop.Stop()
	turnLoop.Wait()
}

// loadConfig 读取项目目录下的 ./config.yaml；不存在则提示用户先创建。
func loadConfig() {
	data, err := os.ReadFile("config.yaml")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			fmt.Println("未找到配置文件: ./config.yaml\n请参考项目目录下的 example.yaml 创建并填入模型配置后重试。")
			os.Exit(0)
		}
		log.Fatal(err)
	}
	cfg = &config{}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		log.Fatal(err)
	}
	if len(cfg.Models) == 0 || cfg.Models[0].APIKey == "" {
		fmt.Println("请在配置文件中填入模型相关配置后重试。")
		os.Exit(0)
	}
}

// loadAgent 用全局 baseModel 装配 Agent。后续阶段的中间件会在这里逐个挂上。
func loadAgent() {
	var err error
	agent, err = adk.NewTypedChatModelAgent(ctx, &adk.TypedChatModelAgentConfig[adk.AgenticMessage]{
		Name:        "codeclaw",
		Description: "a code agent which can do many things",
		Instruction: "你是一个编程智能体, 你的名字叫做 codeclaw, 擅长解决编程问题。",
		Model:       baseModel,
	})
	if err != nil {
		log.Fatal(err)
	}
}

// initTurnLoop 创建并启动 TurnLoop(非阻塞，内部起后台 goroutine)。
func initTurnLoop() {
	if err := os.MkdirAll("./sessions", 0755); err != nil {
		log.Fatal(err)
	}
	var err error
	sessionStore, err = session.NewFileStore[adk.AgenticMessage]("./sessions", nil)
	if err != nil {
		log.Fatal(err)
	}
	sessionID = fmt.Sprintf("%d", time.Now().Unix())

	turnLoop = adk.NewTurnLoop(adk.TurnLoopConfig[chatItem, adk.AgenticMessage]{
		GenInput: GenInput,
		PrepareAgent: func(ctx context.Context, loop *adk.TurnLoop[chatItem, adk.AgenticMessage], consumed []chatItem) (adk.TypedAgent[adk.AgenticMessage], error) {
			return agent, nil
		},
		OnAgentEvents: OnAgentEvents,
		SessionID:     sessionID,
		SessionStore:  sessionStore,
	})
	turnLoop.Run(ctx)
}

// GenInput 把缓冲的用户输入项转成 agent 输入。
// Messages 只放"本轮新消息"；多轮历史由 Runner 从 SessionStore 重建后 prepend。
func GenInput(ctx context.Context, loop *adk.TurnLoop[chatItem, adk.AgenticMessage], items []chatItem) (*adk.GenInputResult[chatItem, adk.AgenticMessage], error) {
	if len(items) == 0 {
		return nil, nil
	}
	return &adk.GenInputResult[chatItem, adk.AgenticMessage]{
		Input: &adk.TypedAgentInput[adk.AgenticMessage]{
			Messages: []adk.AgenticMessage{
				schema.UserAgenticMessage(items[0].query),
			},
			EnableStreaming: true,
		},
		Consumed:  items[:1],
		Remaining: items[1:],
	}, nil
}

// OnAgentEvents 消费一轮 agent 执行的全部事件。
// 阶段3：只把流式正文块经 program.Send 发给 TUI，不再直接 fmt.Print。
// 注意：不再有 turnDone 同步信号--TUI 是异步事件驱动，输出到了 Update 自然就渲染。
func OnAgentEvents(ctx context.Context, tc *adk.TurnContext[chatItem, adk.AgenticMessage], events *adk.AsyncIterator[*adk.TypedAgentEvent[adk.AgenticMessage]]) error {
	for {
		event, ok := events.Next()
		if !ok {
			break
		}
		if event.Err != nil {
			// CancelError(来自 Stop / Preempt)由框架自管，直接跳过，不向上传播
			continue
		}
		if event.Action != nil {
			continue // 阶段3 无中间件，不会有 Action(权限中断/摘要等)
		}
		if event.Output == nil || event.Output.MessageOutput == nil {
			continue
		}
		mv := event.Output.MessageOutput
		if !mv.IsStreaming {
			continue // 非流式(工具结果等)阶段5 再处理
		}

		// 流式：消费 MessageStream，把正文块经 program.Send 发给 TUI 主循环
		stream := mv.MessageStream
		for {
			chunk, err := stream.Recv()
			if err != nil {
				break // EOF 或 CancelError：结束本流
			}
			for _, b := range chunk.ContentBlocks {
				if b.AssistantGenText != nil && b.AssistantGenText.Text != "" {
					program.Send(aiTextChunkMsg{text: b.AssistantGenText.Text})
				}
				// Reasoning(思考过程)的展示留到阶段4
			}
		}
	}
	return nil
}
