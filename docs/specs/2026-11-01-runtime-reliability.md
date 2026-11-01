# 运行可靠性与可见性修正

## 证据与参考

真实模型验收暴露了三条不同的边界：peer 消息已送达却不能唤醒 wait；管道末端掩盖测试失败；headless 委派期间缺少进度。审批拒绝还错误建议已启用 yolo 的调用方再启用 yolo。

参考本地 oh-my-pi 的 `packages/coding-agent/src/tools/hub/index.ts` / `messaging.ts`：等待先检查已缓冲消息，随后竞争消息、作业结算、超时和取消；`task/index.ts` 维护进度快照。本地 claude-code 的 `rust/crates/runtime/src/bash.rs` 将 shell 输出、退出码解释、后台任务及沙箱边界分别表达。下述修改按本项目的 Go 接口实现，不宣称两个参考实现已有同样的 shell 选项。

## 单一 peer 收件箱

运行中的 peer 消息由 `Deliver` 进入 mailbox；消息数组是真相源，容量为一的 wake 通道仅通知，不承载消息内容。`hub wait/inbox` 查看子 agent 收件箱，不提前删除。Agent 的同步 `beforeStep` hook 在下一步构建输入前，将消息记录到 sidecar，写入成功后逐条 ack。写入失败终止本轮并保留待处理消息；会话作用域保持隔离。预算通知仍可使用 steering，它不再承担 peer 消息投递。

这保证进程内的消息确认与会话写入顺序。mailbox 仍是内存队列，不承诺进程崩溃前未落盘消息的持久投递。

Main 没有子 agent 的 ingress hook，现有 `hub` 工具结果或 TUI/headless 自动投递继续承载 Main 消息。

## Shell 与验证

普通 bash 在宿主/沙箱两个分支都以 `--noprofile --norc -o pipefail` 启动；不启用全局 errexit，以保留工具脚本显式处理退出码的能力。`false | cat` 返回失败，`false | cat || true` 则是调用方明确处理失败。

专用 verify 以 `-e -o pipefail` 运行每条受审批的配置命令，避免 `test-command | tail` 或 `test-command; echo done` 掩盖失败。仍保留工作区指纹、超时及持久化验证证据。调用方显式吞掉错误的脚本仍不可能靠 shell 选项自动判出业务失败。

## 观测与交互

headless 每两秒从 Manager 的权威名册取快照，状态改变时显示 name/status/current tool/requests/tokens/elapsed，无变化的活跃任务每十秒显示心跳。它不参与预算或结算；没有事件总线也可工作。headless 主循环也使用同步 RunObserved，退出码不依赖可丢弃的展示事件。

TUI 在 Hub 折叠时默认显示最多三个活跃子 agent；Ctrl+A 仍展开完整面板，沿用现有窗口预算和滚动处理。

审批提示明确：Override 在 yolo 仍需审批，headless 无法弹窗；应使用受控工具或交互父审批，不能通过再次启用 yolo 绕过。

小任务提示改为直接执行，真正独立的 slice 才并行委派；集中 verify，已通过且没有新改动时不重复整套检查。恢复时从当前回放中的成功 read_file 结果恢复指纹，且重新校验当前文件 SHA-256；不一致时仍须 read_file。去重区间不恢复。

`--cwd` 的 legacy config 路径也绑定到指定工作区，避免从源码目录启动时误读源码目录的 config.yaml。

## 回归验收

- peer 消息在 wait 前到达或在等待期间到达，均及时返回；失败存储不 ack；成功注入仅一次；不同 session 不串收件箱。
- 无事件总线且模型仍在等待时，headless 仍显示活跃进度。
- bash 两个分支不掩盖管道错误；verify 不掩盖管道/命令序列错误。
- 保持已有 schema/yield/预算、TUI 布局、沙箱等测试，并运行全量 race 与 vet。
- 真实模型场景：一个后台 worker 经 READY-47/FORMAT-47 握手后实现日志统计 CLI，下一轮加入 --level；独立用例验证正常、空行、多词消息、过滤、错误行与空 stdout。用户在 iTerm 启动 go run ./cmd/agent，并在两轮后进入交互 TUI。

## 后续实测：直接执行任务的进度

JSON 笔记 CLI 场景中，没有子 agent 的主循环仍会在长模型调用期间静默。headless 主循环新增独立的显示心跳，覆盖请求模型（首个 chunk 前）、生成、工具执行与重试阶段，展示请求/流块计数和耗时；不存储生成内容，也不参与预算及结算。通过同步事件更新阶段、单独 ticker 输出，结束后停止。

显示心跳不参与持久审计。运行审计在同步执行路径内落盘，不能依赖显示事件通道。


## 审计、恢复与并发回归收尾

`context.Manager` 提供可选 RecordAudit；主/子 Agent 都会写入 run_start、model_request、model_retry、run_error、context_compaction、run_end。run_end 区分 completed、terminated、canceled、timeout、iteration_limit、error，记录响应数、请求尝试数、工具数、用量和耗时。事件带 run_id；落盘失败会停止执行并反馈错误。进程被强杀时可能只有 start 而没有 end，可据此识别不完整运行。

模型输入审计只保存系统前缀 SHA-256、系统/历史消息数；记忆召回保存选中条目的 id、scope、kind，不复制查询和记忆正文。模型错误仅保存分类和错误类型，不复制可能携带凭据的 provider 报错正文。原始消息与工具结果仍按既有转录机制保存；这些元数据不会进入模型回放。

Scoped 文件工具从当前回放里匹配 assistant read_file 调用和成功 tool_result，取工具追加的最后一个 SHA-256 标记，重新读取工作区文件确认一致，再恢复编辑守卫。最多恢复最近 128 个文件，内部 URL、未配对结果、失效/被剪枝结果或已变更内容不授予编辑状态。不恢复 read 去重区间。写入时仍有 Workspace.Apply 的哈希前置条件，防止恢复后外部改动造成覆盖。

`examples/kvnote` 收录真实生成任务的修复版：稳定 sidecar flock 覆盖整个读改写，临时文件同步后 rename，避免两个进程都返回成功却丢掉更新。多进程回归同时运行 set、del、import；原有损坏 JSON、空值、失败不写和权限用例继续保留。这是可重复验收样例，不属于 Agent 核心状态库。macOS/Linux 的 advisory 锁要求所有写入方遵守协议，不替代 worktree 隔离或外部系统事务。

恢复实测还遇到了请求创建阶段的 `Post ...: EOF`：模型错误分类现在识别包装的 io.EOF/io.ErrUnexpectedEOF 以及适配器展开后的末尾 EOF，走现有有界指数退避；正常流结束的 EOF 仍由 consumeStream 处理。上下文溢出仍优先走压缩，鉴权错误不因此重试。真实接口复测在第一次 EOF 后重试成功，随后无需重读便完成已恢复指纹的编辑和 verify。
