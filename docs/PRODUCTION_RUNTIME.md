# 生产运行时初版使用说明

详细设计、代码构造与每步验收见 [production-runtime spec](specs/2026-09-14-production-runtime.md)。

## 配置

默认 `sandbox.mode=required`：macOS 需要系统 sandbox-exec，Linux 需要 bubblewrap 以及可用的用户 namespace。后端不可用时不会自动改为宿主执行。所有网络（包括本地端口）默认不可访问；需要依赖时先在宿主准备好缓存，再授予只读路径。

下面只写到用户级 `$CODECLAW_HOME/config.yaml`（默认 `~/.codeclaw/config.yaml`），不能放在仓库配置中。路径是示例，需替换成实际绝对路径；不要把整个 HOME 加为读取根。

```yaml
sandbox:
  mode: required
  read_roots:
    - /absolute/path/to/go-toolchain
    - /absolute/path/to/go/pkg/mod
  env:
    GOMODCACHE: /absolute/path/to/go/pkg/mod
lsp:
  command: /absolute/path/to/gopls
  language_id: go
  timeout: 30s
```

可以在宿主用 `go env GOROOT GOMODCACHE` 查询工具链与模块缓存路径，用 `command -v gopls` 查询已安装的服务端位置。额外工具链读取权限不会给予写权限。未配置 LSP 时，内置 Go AST 仍可用。

项目级 `.codeclaw/config.yaml` 可设置验证命令：

```yaml
verification:
  commands:
    - go test ./...
    - go vet ./...
  timeout: 120s
```

`verification.commands` 默认空，避免猜测每个项目的构建流程；配置后开启完成检查。`verify` 为 exec 档，走现有审批。headless 没有交互审批器时，可在用户权限配置中明确允许固定的 verify 工具；未授权的检查会作为阻塞报告。

当前无沙箱 MCP 启动器与 required 模式不能同时使用；启动时会解释原因并退出。MCP 的受限进程启动与远程认证将在后续阶段接入。若明确设置用户级 `sandbox.mode: off`，shell/LSP/验证命令回到宿主执行，文件工具仍有 workspace 路径边界。

## 模型可调用的工具

```json
{"name":"code_intel","arguments":{"operation":"ast","file_path":"main.go"}}
{"name":"code_intel","arguments":{"operation":"definition","file_path":"main.go","line":12,"character":8}}
{"name":"apply_changes","arguments":{"changes":[{"path":"new.go","before_sha256":"missing","content":"package main\n"}]}}
{"name":"change_history","arguments":{}}
{"name":"undo_changes","arguments":{"transaction_id":"从写入结果或历史取得的32位ID"}}
{"name":"verify","arguments":{}}
```

修改既有文件前先 `read_file`；使用返回的 `sha256` 作为 `before_sha256`。`write_file`、`edit` 自动进入日志，写后 Go 语法错误会立即反馈。`apply_changes` 支持显式 `delete: true`，即使删除也须提供 `content: ""`。恢复日志位于 `<项目数据桶>/changes/`，不会写进用户仓库或改动用户 Git 历史。

文件工具的相对路径固定相对工作区根；shell 的 `cd` 只影响当前调用。例：`cd 'sub dir' && go test` 不会改变下一次 `read_file` 的基准目录。

## 本地验收

```sh
go test ./...
go test -race ./...
go vet ./...
CODECLAW_NATIVE_TEST=1 go test ./internal/runtime ./internal/codeintel -run 'TestNativeSandbox|TestRealGopls' -v -count=1
```

最后一条要求真实 OS 沙箱和已经安装的 gopls；一旦显式开启，后端不可用、隔离失效或 LSP 失败都会使测试失败。普通单测不会把“未运行原生测试”误报为“原生隔离已验证”。

首轮仍需补齐 worktree/overlay 隔离、三方合并、资源配额、网络代理、语言服务器池、Linux 实机 CI 和 eval v2。日志恢复不能替代多文件原子提交，也不能撤销任意 shell 或外部服务的副作用。
