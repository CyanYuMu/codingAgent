# CodeClaw Web

本地 React + TypeScript 界面，复用 Go Agent 的模型、工具、权限、沙箱、记忆与子 Agent。布局参考 ChatGPT 的侧栏和对话界面，保留 CodeClaw 品牌。无云端部署、无前端模型密钥。

## 启动

在仓库根目录执行：

```sh
npm --prefix web ci
npm --prefix web run build
go run ./cmd/agent --web 127.0.0.1:8080
```

浏览器打开 http://127.0.0.1:8080 。使用现有用户级模型配置。当前 Agent 要求 `models`、`sandbox`、`mcp_servers`、`permissions.allow` 等能力配置放在 `~/.codeclaw/config.yaml`（或 `$CODECLAW_HOME/config.yaml`），项目内配置只能收紧权限。若旧仓库 `config.yaml` 仍含这些字段，需先迁移配置；网页入口不会绕过校验。

指定目标项目时，构建目录仍相对启动命令所在目录解析：

```sh
go run ./cmd/agent --web 127.0.0.1:8080 --cwd /path/to/project --web-assets /absolute/path/to/my_code_agent/web/dist
```

`--web` 与 `-p` 互斥。只允许绑定 `127.0.0.1`。Ctrl+C 结束服务并取消活动任务。模型请求仍发送到用户配置的模型服务商；“本地执行”指文件与命令在本机运行。

## 前端开发

先按上述方式启动 Go 服务，再开另一个终端：

```sh
npm --prefix web run dev
```

访问 Vite 输出的 `http://127.0.0.1:5173`。Vite 将 `/api` 代理到 8080。开发来源仅允许此固定地址；不要使用 `localhost` 别名或自动切换后的端口。API 客户端集中在 `src/api.ts`，组件不依赖 Wails / Electron，可在后续桌面端复用。

## 当前能力与语义

- 新建、搜索、切换会话，查看持久化消息与 Markdown / 代码块。
- 通过 `RunObserved` 执行真实 harness，展示文本、思考、工具参数、工具输出和用量。
- 按原有审批策略显示“允许本次 / 拒绝”，包括配置允许升级的子 Agent 审批。没有网页端全局放行按钮。
- 运行中发送补充指令；在下一步模型调用前进入上下文。可停止当前运行及该会话的子任务。
- 一个服务绑定一个项目、同时一条主运行；运行或后台作业未结束时不能切换会话。
- SSE 推送完整状态快照（最多每 250 ms 一次）；慢浏览器不会阻塞工具执行。重新连接恢复当前快照，不依赖浏览器缓存或重放所有 token。
- 浏览器关闭不取消运行；服务进程重启后恢复已落盘的消息，不能恢复正在执行的进程或未完成审批。
- 子 Agent 状态可查看；后台结果使用先持久化再 ACK 的交付协议，自动续跑最多 3 轮，与 headless 上限一致。
- Cmd/Ctrl+K 新建对话，Enter 发送，Shift+Enter 换行，中文输入法组合状态不会误提交。

暂未提供图形化模型配置、项目选择器、独立文件树/diff 面板或终端模拟器。文件变更与验证结果目前通过对应工具输出查看。装配仍共用 `cmd/agent`；后续桌面接入时可提取独立 application 包，不复制 Agent 循环。

## 验证

```sh
npm --prefix web run build
go test -race ./internal/webui ./cmd/agent
```

HTTP 集成测试用脚本模型驱动真实 Agent、权限执行器、工作区文件工具和 JSONL，会验证审批前不写文件、审批后实际写入、会话恢复、断线不中断、取消、重复审批与跨站访问拒绝。不会请求付费模型。

需要手动浏览器 QA 时，可启动明确标注“隔离测试 · 脚本模型”的临时服务：

```sh
CODECLAW_WEB_BROWSER_QA=1 go test ./internal/webui -run '^TestBrowserQA$' -v -timeout 20m
```

监听 8080，需要先完成前端构建。该测试模型只会请求写入临时目录的 `hello.txt`；不用于实际编码。测试结束时请 Ctrl+C 关闭测试进程。
