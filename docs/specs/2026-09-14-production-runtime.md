# 生产化第一轮：执行边界、代码分析、可恢复修改、验证

状态：第一轮实现已落地，回归和平台验收记录见文末。基于当前工作区（包含尚未提交的 skills 改动）。

## 目标与次序

| 步骤 | 设计含义 | 代码构造 | 验收 |
|---|---|---|---|
| 1. Workspace | 所有文件操作绑定项目根，不再依赖进程 cwd；阻止路径穿越及符号链接逃逸 | `internal/workspace`，Go `os.Root`，受限读取/遍历、内容哈希 | `../`、外部绝对路径、symlink、两个工作区并发、超大/特殊文件 |
| 2. Process sandbox | 审批决定是否执行；OS 决定执行后能访问什么。yolo 不能解除沙箱 | `internal/runtime/process_sandbox.go`；macOS Seatbelt、Linux bubblewrap；后端失败关闭 | 可在项目写文件；不能读写外部临时探针；不能连接本地 TCP；超时回收 |
| 3. Change journal | 编辑前检查内容哈希，逐文件原子替换，保存 before/after 与恢复记录 | Workspace 的 `Apply`/`Undo`/`History`；日志位于项目数据桶，不在代码目录 | 多文件预检全通过才写；冲突不覆盖；失败保留恢复信息；重启后可撤销；撤销不覆盖用户新改动 |
| 4. Code intelligence | Go AST 提供无需外部服务的语法诊断和符号；LSP 提供类型级导航 | `internal/codeintel`：Go parser；stdio LSP framing、初始化、取消、关闭；`code_intel` 工具 | 语法错误定位；函数/方法/类型范围；fake LSP 协议回归；可选真实 gopls |
| 5. Verification | 检查结果绑定源码版本；编辑后旧的“通过”失效 | `verify` 工具、配置的命令集、Agent 完成检查 | 失败和取消不能记成通过；检查期间文件变化判失效；fake-model 从失败反馈继续修复 |
| 6. 接线与文档 | 主 agent、worker、headless 使用同样的能力边界 | 工具工厂、config、example、使用和验证记录 | 全量测试、race、vet；真实本地沙箱与 gopls 冒烟 |

## 首轮边界

本轮交付可使用的初版，不等同于完成全部生产安全认证。

- 默认文件工具局限工作区；只读的 `artifact://`、`skill://` 等资源仍由宿主注册的 resolver 授权。
- 进程沙箱默认启用、禁止网络；额外的工具链读取目录由宿主配置。无后端时拒绝运行进程，不静默降级。
- 新沙箱配置只从用户级配置读取，仓库配置不能关闭隔离或扩大访问权限。
- Linux/macOS 的原生沙箱不等于虚拟机；资源配额、域名代理、远程执行留下一轮。
- 首轮变更日志提供单文件原子替换、多文件预检和可恢复补偿，不声称跨文件 ACID。外部编辑器/进程不服从进程内锁；冲突通过内容哈希发现，仍须独立 worktree 才能隔离恶意并发写。
- `write_file`/`edit`/`apply_changes` 进入日志；任意 shell 的文件副作用不在撤销保证内。验证基于工作区快照，能够使 shell 改动令旧证据失效。
- 首轮完成检查针对配置了验证命令的主 agent；子 agent 的 yield 只表示交付给父级验收。验证命令通过现有审批入口执行，完成检查本身不偷跑命令。
- Go AST 不冒充类型解析器；其他语言通过 LSP 加入。首轮 LSP 为有界、按调用启动的 stdio 客户端，后续再引入长驻服务器池和增量同步。
- 启用沙箱时不连接现有无沙箱 MCP 启动器，避免绕过执行边界；受限 MCP 生命周期作为后续阶段。

## 实际代码结构

```text
cmd/agent
  config.go              用户级能力配置、项目级验证配置
  main.go                Workspace / ProcessSandbox / verifier 的统一装配与关闭
internal/workspace
  workspace.go           os.Root、路径和文件类型边界、SHA-256、工作区指纹
  changes.go             批量预检、prepared/applied/undone、原子替换、恢复与历史
  verification.go        验证报告写入项目数据桶
internal/runtime
  process_sandbox.go     argv 构造、Seatbelt/bubblewrap、只读派生、环境与取消
  bash.go                生产路径接入沙箱；cd 仅对当前 shell 调用有效
internal/codeintel
  go.go                  Go AST 符号、函数/方法/类型范围、语法诊断
  lsp.go                 有界 JSON-RPC framing、初始化、服务端请求、导航、关闭
internal/verification
  verification.go        验证指纹、命令执行结果、取消/过期识别、完成检查
internal/tool
  workspace.go           ScopedBuiltins、apply_changes、undo_changes、change_history
  codeintel.go           code_intel 的 AST / LSP 操作
  verify.go              verify、审批预览中的实际命令
internal/agent
  agent.go / loop.go     completionCheck 与有界修复反馈
internal/eval
  evaluator.go           同样使用 ScopedBuiltins；移除全局 os.Chdir
```

### 步骤 1：文件边界为什么必须落在实际 I/O

`Workspace.Open` 固定一个 `os.Root` 目录句柄，`Rel` 把工作区内的绝对路径转为相对路径。`ReadFile`、`Stat`、遍历和替换最终都通过该 root 完成。这样即使模型提供 `../` 或指向工作区外的链接，实际访问也受 root 限制，而不是仅在字符串层判断一次。

首版主动拒绝文件工具路径上的所有 symlink；检索不跟随 symlink，验证指纹包含 symlink 自身的目标文本。写操作保护 `.git/.codeclaw/.agents/.codex/.claude` 元数据，日志强制位于 workspace 之外。普通文件最多 8 MiB、一次事务最多 64 文件/32 MiB；遍历最多 20,000 文件、验证快照最多 128 MiB，超过后明确报错。FIFO/设备不按普通源码读取。

`ScopedBuiltins` 给每个 actor 一份独立读历史，却共享 workspace/journal。`write_file` 覆盖已有文件前要求先读；`read_file` 返回 SHA-256。`edit` 保留原有的唯一文本匹配要求，并将写入升级为内容哈希前置条件。非法 JSON、缺失 content 不能退化成空文件写入。

### 步骤 2：审批和隔离分别控制什么

现有 Executor 仍执行审批决策；允许之后才进入 ProcessSandbox。`--yolo` 只影响审批，不更改 sandbox.mode。`required` 默认禁止网络、项目外写入和未授权的项目外内容读取；工具链仅授予系统路径与用户明确列出的 `read_roots`。HOME、TMPDIR、GOCACHE 指向专用临时目录，BASH_ENV 和任意继承变量不进入生产子进程。用户可设置有限的 `sandbox.env`（PATH/GOROOT/GOPATH/GOMODCACHE/CGO_ENABLED）。

`ReadOnly()` 派生 LSP 使用的能力，只保留项目读取和临时缓存写入。bash、verify、LSP 共用 `Command`，均使用 argv，不把模型参数拼成宿主命令。取消采用进程组 SIGKILL，WaitDelay 有界。`sandbox.mode: off` 是用户显式选择的旧版宿主进程模式；文件工具仍然受 Workspace 限制。

macOS 实测中 dyld 启动需要根目录本身的 file-read-data；profile 使用 `(literal "/")`，没有给予根目录下所有文件的内容读取权限。Linux backend 使用 namespace、只读系统挂载、独立网络；本机不是 Linux，Linux 运行时验收仍待 CI。Linux 首版仅把已存在的顶层元数据只读挂载，不能宣称与 macOS 的递归、大小写无关 metadata deny 完全等价。

### 步骤 3：修改怎样留下可恢复的证据

`Apply` 的执行顺序是：

1. 拿到共享工作区锁与日志文件锁；其他进程占用日志时返回 busy，不无限等待。
2. 全批路径、大小、重复项和 `before_sha256` 预检；`missing` 表示确认创建新文件。
3. 将全部 before/after、存在状态和权限写入 `prepared` 日志，并 fsync。
4. 每个文件再次核对当前内容，用同目录临时文件 + fsync + rename 安装；不 truncate 原 inode，因此不会连带修改外部 hardlink 的另一名字。
5. 全部成功后追加更新状态为 `applied`；中途失败保留 prepared 记录和原文。

`Undo` 先检查全批当前内容。只有与记录的 before/after 一致才恢复，用户新改动会触发冲突；因此进程重启后也能恢复“只写完一半”的批次。回滚只处理记录中的文件，可能留下新建的空目录；不会撤销任意 shell、网络或数据库副作用。完整 worktree 隔离、三方合并和多文件统一发布属于下一阶段。

### 步骤 4：语法分析和语义查询的职责

`code_intel(operation=ast)` 使用 Go 标准库 parser/ast，返回语法错误、符号类型与起止行；write/edit/apply_changes 写入 Go 文件后也会直接返回语法诊断。语法错误作为“已写入后的反馈”，不伪装成写入失败。

`lsp_symbols/definition/references/hover` 启动配置的语言服务器，执行 initialize → initialized → didOpen → query → shutdown/exit。客户端处理 workspace/configuration 和进度初始化请求，拒绝服务端 applyEdit；写操作未来将显式转为 Workspace transaction。帧长度按 UTF-8 字节计算，上限 16 MiB；请求总时限默认 30 秒；初始化协商只接受 UTF-16。工具的 line 从 1 起，character 为 0 起的 UTF-16 偏移。

首版没有长驻 server、跨文件增量 overlay、类型诊断自动推送、AST 重写和多语言路由。这些不能仅凭 Go parser 的存在标记为完成。

### 步骤 5：验证如何约束完成

`verification.Service` 记录启动基线；显式 `verify` 通过既有权限入口运行全部配置命令。审批预览展示真实命令，而不是空 `{}`。执行前后分别计算源码指纹；命令失败、超时、取消、源码改变或证据落盘失败都会阻止记为通过。报告包含命令、输出、错误、耗时和前后 SHA-256。

当主 agent 产生无工具调用的回复准备结束时，`completionCheck` 检查工作区是否变化且拥有当前版本的通过证据。未通过则将反馈写入 session，给模型最多两次继续修复机会；仍未解决就发出 EventError。迭代上限也不能成为成功完成。这个检查不自行执行命令，审批不会被自动验证绕过。模型流中的自然语言仍可能提前说“完成”，宿主最终状态由检查结果决定。

验证只证明配置命令检查到的内容。对外部编辑器在检查期间改了又改回、工作区外依赖版本变化、恶意测试伪造等情况，首版不能给出事务隔离保证；隔离快照验证和工具链指纹属于下一阶段。

### 步骤 6：入口一致性

主 agent 和 worker 工具工厂共享 Workspace 和 ProcessSandbox。readonly 子 agent 的原有工具过滤保留；LSP 另有进程只读能力。协调模式下主 agent 保留 verify 工具，便于统一验收 worker 交付。`cmd/eval` 使用相同的 scoped 文件工具和默认 required sandbox，fixture 不再调用全局 `os.Chdir`。`RunWithSandbox` 供可信嵌入方/离线测试显式选择配置。

headless 退出前显式关闭 workspace 和 process sandbox（`os.Exit` 不执行 defer）。现有长期 memory/session 格式不依赖新的日志数据库迁移。

## 回归与平台验收记录

第一轮已执行：

- `go test ./...`：全部通过，包括现有 skills/会话/压缩/子 agent 回归。
- `go test -race ./...`：全部通过。
- `go vet ./...`、`go build ./...`：本机 macOS arm64 均通过。
- `TestBoundary`、`TestJournalOutsideRootAndHardlinkReplacement`：越界、symlink、日志位置、hardlink 原子替换。
- `TestBatchPreflightAndContentFingerprint`、`TestConcurrentWritersUseCompareAndSwap`：批量预检、外部变化、竞争写入。
- `TestJournalUndoAfterReopenAndUserConflict`、`TestRecoverPreparedBatch`：重启恢复、部分提交、用户并发编辑保护。
- `TestGoAST`、`TestFrameValidation`、`TestLSPQueryLifecycleAndTimeout`：AST 错误位置、协议帧、服务端请求、超时回收。
- `TestCodingLoopEditsFailsRepairsAndVerifies`：fake model 驱动真实文件工具和验证命令，经历修改 → 过早完成 → 验证失败 → 修复 → 验证成功。
- `TestRepositoryCannotWidenRuntimeCapabilities`：仓库配置不能关闭沙箱、增加读取根或更换 LSP 程序。
- `TestUnavailableUserConfigCannotPromoteProjectCapabilities`：用户配置路径失效时，不把项目配置提升为可信权限层。
- `TestConcurrentFixturesKeepIndependentWorkingDirectories`、`TestFixtureBoundariesAndModelFailure`：并发 fixture 目录隔离、输入/期望路径边界；模型失败即使文件已匹配也不能算通过。
- `CODECLAW_NATIVE_TEST=1 go test ./internal/runtime ./internal/codeintel -run 'TestNativeSandbox|TestRealGopls' -v -count=1`：macOS 原生读写/网络拒绝、环境白名单、进程组超时回收、LSP 只读访问、真实 gopls 符号和定义跳转均通过。非临时用户文件未作为破坏性测试目标。

本机 `go build ./...` 已通过。Linux 交叉构建已尝试，但当前下载的 darwin Go 1.26.4 工具链缺失 `internal/runtime/cgroup`、`internal/runtime/syscall/linux` 等目标平台源码，尚未进入项目编译即失败；没有将其记录为 Linux 构建通过。Linux 编译和原生测试都必须补上独立 Linux CI。

以上闭环使用 fake model 验证真实工具和命令的行为；本轮没有调用远程付费模型，尚不构成真实模型端到端任务成功率评测。

## 后续生产门槛

1. 独立 worktree/overlay + base revision + 三方合并；防止父子并发修改同一路径。
2. 工具链镜像、资源配额、密钥 broker、网络代理与远程隔离执行。
3. LSP server pool、文档版本与 UTF-16 位置映射、引用/重命名的 WorkspaceEdit 事务化应用、多语言适配。
4. baseline failure、定向测试选择、flaky 区分；运行账本记录验证命令、工作区指纹、结果和耗时。
5. hooks、trace 派生索引、真实仓库 eval、崩溃/断电故障注入、安全对抗与 Linux CI。

## 依据

- [Go traversal-resistant file APIs](https://go.dev/blog/osroot)：使用 `os.Root`，避免仅靠 `EvalSymlinks` 后再普通 open 的检查/使用竞态。
- [LSP 3.17](https://microsoft.github.io/language-server-protocol/specifications/lsp/3.17/specification/)：Content-Length framing、initialize/initialized、JSON-RPC 请求与服务端通知。
- [Anthropic sandbox runtime](https://github.com/anthropics/sandbox-runtime) 与 [bubblewrap](https://github.com/containers/bubblewrap)：进程树文件系统/网络隔离、平台能力检测。
