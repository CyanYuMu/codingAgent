package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/adk/session"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"gopkg.in/yaml.v3"
)

// 全局状态：阶段2 起，agent 不再被直接调用，而是交给 TurnLoop 在后台运行。
var (
	cfg       *config
	baseModel model.BaseModel[adk.AgenticMessage]
	agent     adk.TypedAgent[adk.AgenticMessage]
	turnLoop  *adk.TurnLoop[chatItem, adk.AgenticMessage]

	// 会话：TurnLoop 靠 SessionStore 重建历史，从而支持多轮记忆。
	sessionStore adk.SessionEventStore[adk.AgenticMessage]
	sessionID    string

	ctx = context.Background()

	// turnDone 让主协程等待“当前 turn 的流式输出全部打印完”。
	// 这是阶段2 同步式 REPL 的临时手段；阶段3 引入 TUI 后，输出会经 program.Send
	// 异步驱动界面，就不再需要它了。
	turnDone = make(chan struct{}, 1)
)

// chatItem 是 TurnLoop 的泛型参数 T：一个待处理的用户输入项。
// 阶段2 只用到 query；后续阶段会扩展 id（抢占 ack）、interruptId/action（权限恢复）。
type chatItem struct {
	query string
}

func main() {
	loadConfig()
	loadModel(0)
	loadAgent()
	adk.SetLanguage(adk.LanguageChinese) // 框架内置提示词改用中文
	initTurnLoop()

	fmt.Println("codeclaw (阶段2: TurnLoop + 流式) 已就绪。输入消息开始对话，输入 /exit 退出。")
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for {
		fmt.Print("\n你: ")
		if !scanner.Scan() {
			break
		}
		text := strings.TrimSpace(scanner.Text())
		if text == "" {
			continue
		}
		if text == "/exit" {
			break
		}

		fmt.Print("AI: ")
		// 【关键】Push 是非阻塞的：只是把输入塞进 TurnLoop 缓冲区。
		// 真正的 GenInput -> PrepareAgent -> agent.Run -> OnAgentEvents 都在后台 goroutine 里发生。
		ok, _ := turnLoop.Push(chatItem{query: text})
		if !ok {
			fmt.Println("[TurnLoop 已停止]")
			break
		}
		// 阻塞等本 turn 的流式输出打印完毕（OnAgentEvents 结束时发信号）
		<-turnDone
	}
	if err := scanner.Err(); err != nil {
		log.Println(err)
	}

	turnLoop.Stop()
	turnLoop.Wait()
}

// loadConfig 读取项目目录下的 ./config.yaml；不存在则提示用户先创建。
// 为方便起见从项目本地读取（config.yaml 放在代码旁边）。
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

// loadAgent 用全局 baseModel 装配 Agent。
// 阶段2 仍不带任何工具/中间件；后续阶段的中间件会在这里逐个挂上。
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

// initTurnLoop 创建并启动 TurnLoop。
// turnLoop.Run(ctx) 不阻塞——内部用 go l.run(ctx) 起了一个后台 goroutine。
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
			return agent, nil // 阶段2 只有一个全局 agent，直接返回
		},
		OnAgentEvents: OnAgentEvents,
		SessionID:     sessionID,
		SessionStore:  sessionStore,
		// 注意：阶段2 暂不设 GenResume / InterruptMode——那是阶段7 权限中断才需要的。
	})
	turnLoop.Run(ctx)
}

// GenInput 把缓冲的用户输入项转换成 agent 输入。
//
// 【关键认知】这里 Messages 只放“本轮的新消息”。
// 多轮历史不是我们拼的：Runner 会从 SessionStore 重建历史，再 prepend 到这条新消息前面，
// 然后才喂给模型。所以我们只管“这一句”，框架管“整段对话”。
//
// 返回 Consumed/Remaining：告诉 TurnLoop 本次消费了哪些、哪些留给后续 turn。
func GenInput(ctx context.Context, loop *adk.TurnLoop[chatItem, adk.AgenticMessage], items []chatItem) (*adk.GenInputResult[chatItem, adk.AgenticMessage], error) {
	if len(items) == 0 {
		return nil, nil
	}
	return &adk.GenInputResult[chatItem, adk.AgenticMessage]{
		Input: &adk.TypedAgentInput[adk.AgenticMessage]{
			Messages: []adk.AgenticMessage{
				schema.UserAgenticMessage(items[0].query),
			},
			EnableStreaming: true, // 阶段2 起切到流式
		},
		Consumed:  items[:1],
		Remaining: items[1:],
	}, nil
}

// OnAgentEvents 消费“一轮 agent 执行”产生的全部事件，把流式文本打印出来。
// 每个 turn 调用一次（拿到的是整轮完整事件流）。结束时发 turnDone 信号唤醒主协程。
//
// 事件有三种载荷：
//   - event.Err    : 出错（Stop 时会是 CancelError，框架自管，我们不向上传播）
//   - event.Action : 需要“动作”——权限中断/摘要，阶段7 才会出现
//   - event.Output : 模型输出（MessageOutput）
//
// MessageOutput 分两种：
//   - 非流式 (mv.IsStreaming==false)：完整消息在 mv.Message（工具结果走这条路，阶段5）
//   - 流式   (mv.IsStreaming==true) ：要消费 mv.MessageStream，逐块 Recv
func OnAgentEvents(ctx context.Context, tc *adk.TurnContext[chatItem, adk.AgenticMessage], events *adk.AsyncIterator[*adk.TypedAgentEvent[adk.AgenticMessage]]) error {
	defer func() {
		// 非阻塞发信号（channel buffered=1）；主协程在 <-turnDone 等待
		select {
		case turnDone <- struct{}{}:
		default:
		}
	}()

	for {
		event, ok := events.Next()
		if !ok {
			break
		}

		if event.Err != nil {
			// 不向上传播（Stop 的 CancelError 由框架处理）；只打印给用户看
			fmt.Printf("\n[事件错误] %v\n", event.Err)
			continue
		}
		if event.Action != nil {
			continue // 阶段2 无中间件，不会有 Action
		}
		if event.Output == nil || event.Output.MessageOutput == nil {
			continue
		}
		mv := event.Output.MessageOutput

		// 非流式事件：直接读完整 Message（阶段2 一般不触发，留给阶段5 的工具结果）
		if !mv.IsStreaming {
			if mv.Message != nil {
				for _, b := range mv.Message.ContentBlocks {
					if b.AssistantGenText != nil {
						fmt.Print(b.AssistantGenText.Text)
					}
				}
			}
			continue
		}

		// 流式事件：消费 MessageStream，逐块打印
		stream := mv.MessageStream
		for {
			chunk, err := stream.Recv()
			if err != nil {
				if errors.Is(err, io.EOF) {
					break
				}
				// 非 EOF（含 Stop 的 CancelError）：打印后跳出本流，不向上传播
				fmt.Printf("\n[流错误] %v\n", err)
				break
			}
			// 每个 chunk 也是一条 AgenticMessage，含若干 ContentBlock：
			//   Reasoning         -> 模型思考过程（灰色）
			//   AssistantGenText  -> 正文回复（正常色）
			for _, b := range chunk.ContentBlocks {
				if b.Reasoning != nil && b.Reasoning.Text != "" {
					fmt.Print(gray(b.Reasoning.Text))
				}
				if b.AssistantGenText != nil && b.AssistantGenText.Text != "" {
					fmt.Print(b.AssistantGenText.Text)
				}
			}
		}
	}
	fmt.Println() // 本轮输出收尾换行
	return nil
}

// gray 用 ANSI 灰色渲染思考过程文本，便于和正文区分。
func gray(s string) string {
	return "\033[90m" + s + "\033[0m"
}
