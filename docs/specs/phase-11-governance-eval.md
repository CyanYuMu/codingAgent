# Phase 11 详细设计：治理与闭环（M4 · Governance & Eval）

> 状态：**待评审** · 日期：2026-08-28 · 所属方案：[2026-08-24-evolution-plan.md](2026-08-24-evolution-plan.md) §9 M4（E1–E5 · F1 · F2 + F.3 回归套件）
> 前置：M1（P8 地基）、M2（P9 委派运行时）、M3（P10 记忆与上下文）已完成并提交
> 目标：让审批从「要么全问要么全放」变成**规则可写的决策引擎**；让 bash 有**危险分类、超时与进程组回收**；
> 让文件编辑有 **edit（先读后改、mtime 校验）**；让组织护栏以 **hooks** 形式可注入；
> 让「钱花在哪、谁最慢」有 **trace 派生索引** 可查；让评测**可并行、有 pass@k、且能测 harness 自身**。

---

## 0. 已拍板决策

| # | 决策点 | 结论 | 理由 |
|---|---|---|---|
| 1 | 默认审批模式 | 保持 P8 已拍板的 `write`（read/write 放行、exec 询问）；`yolo` 只经 `--yolo` 显式开启 | 已实施；M4 在其上叠规则层 |
| 2 | 规则语法 | `permissions.{allow,ask,deny}` 列表，项为 `tool(args-pattern*)`（如 `bash(git status*)`、`read(**)`）；`*` 通配任意序列；不带括号 = 该工具全部参数 | 与 Claude Code `Bash(git *)` 同构，可读可写进 YAML |
| 3 | bash 分类器落点 | 工具新增可选接口 `Decisioner{ Decision(args) ToolDecision }`：bash 用纯函数分类器返回「只读→TierRead」「危险→Override 强制 prompt（yolo 下也拦）」「其余→TierExec」 | 分类是纯函数（可单测样例集）；`Approval(args)` 进接口而非固定 `Tier()` |
| 4 | 危险命令判定方式 | 按 shell 词法把命令切成管道/逻辑段，**逐段保守判定**：任一段命中危险模式即危险；全部段命中只读白名单才算只读；不认识的命令既不只读也不危险（回落到 TierExec） | 不解析完整 shell 语法；保守方向 = 危险漏判比误判贵，只读错判比正确便宜（漏判只读只是多一次审批） |
| 5 | bash 超时与进程组 | 每次调用 `context.WithTimeout`（默认 120s，可配 600s）；`Setpgid` + 超时先 SIGTERM 进程组、5s 后 SIGKILL | 只杀直接子进程留不住管道/后台孙进程（演进方案 E2） |
| 6 | env 脱敏 | 在现有 `nonInteractiveEnv` 基础上**继承全部环境变量但剔除密钥类**：键命中 `*API_KEY*` / `*TOKEN*` / `*SECRET*` / `*PASSWORD*` / `*CREDENTIAL*`（大小写不敏感）→ 不传给子进程 | 白名单会误伤真实工作流（`GOPROXY`/`GIT_CONFIG_*`/`NPM_CONFIG_*`）；只剔密钥类与 Claude Code 行为一致 |
| 7 | edit 工具 | `edit(path, old, new, replace_all)`：**本会话必须先 read_file 过该文件**（mtime+size 指纹一致），否则拒绝；old 必须唯一匹配（replace_all 除外）；执行前再查 mtime，变了拒绝并要求重读 | 防「凭记忆整文件重写 → 静默丢代码」（演进方案 E3 crit） |
| 8 | hooks 范围 | 先做 shell 型四个事件：`PreToolUse` / `PostToolUse` / `PreCompact` / `SessionStart`；PreToolUse 任一 exit 2 = 阻止（fail-closed），handler 自身出错也按阻止处理 | 覆盖 90% 护栏场景；Go 插件型后置 |
| 9 | trace 索引 | `trace.db`（SQLite）放**项目桶**（`<桶>/trace.db`），写入口挂在会话/工具/子 agent/压缩的既有事件点；`codeclaw stats [--project] [--since]` 子命令聚合回答「钱/时间/工具/子 agent」四类问题 | JSONL 仍是审计真相源，trace.db 是派生查询索引（F1） |
| 10 | eval v2 | 移除 `os.Chdir`：夹具的输入/期望目录用绝对路径传给工具（`ToolContext{CWD}`），夹具可并行；`runs: N` 得 pass@k；`verify:` 命令任一失败即 fail；每次 run 记录 session 并归档；`--output json` | 评测不能并行 = 规模上不去（演进方案 F2） |
| 11 | harness 回归套件 | 沿用 P8/P10 的脚本化 fake model 模式扩展：权限决策表逐条断言、bash 分类样例集、edit 守卫、hook 拦截、子 agent 权限继承 | harness 正确性不依赖真实模型（F.3） |

---

## 1. 本阶段产出与边界

### 产出

| 包 | 改动 |
|---|---|
| `internal/permission/rules.go`（新） | `Rules{Allow,Ask,Deny}`、`ToolDecision`、`ResolveRules`（五步决策，纯函数） |
| `internal/tool/tool.go` | 新增可选接口 `Decisioner`（`Decision(args) permission.ToolDecision`） |
| `internal/tool/executor.go` | 审批改走 `ResolveRules`：规则 → 覆盖 → tier×mode；决策原因进结果文本 |
| `internal/runtime/classify.go`（新） | `Classify(command) (ReadOnly, Dangerous bool, Reason string)` 纯函数 |
| `internal/runtime/bash.go` | bash 接分类器（`Decisioner`）+ 超时 + 进程组回收；`sanitizeEnv` 脱敏 |
| `internal/tool/tools.go` | bashTool 持可配超时与分类开关；新增 `editFileTool`（P11.2） |
| `internal/tool/fsguard.go`（新，P11.2） | `FileGuard`：read 记录 + mtime 校验 + edit 前置检查 |
| `internal/hooks/`（新，P11.3） | shell hook 执行器 + 事件集 |
| `internal/trace/db.go`（新，P11.4） | trace.db 索引表 + 写入口 + 聚合查询 |
| `cmd/agent/stats.go`（新，P11.4） | `codeclaw stats` 子命令 |
| `internal/eval/`（P11.5） | 并行 + verify + pass@k + json 输出 |
| `cmd/agent/config.go` | `permissions:` 段（allow/ask/deny）+ `bash:` 段（timeout/classify）+ `hooks:` 段 |

### 不做（后置）

- Go 插件型 hooks（shell 型覆盖 90%）
- worktree 隔离（写密集并行才需要，M4 可选）
- LLM judge（坚持字节 diff + verify）
- 向量检索 / snapcompact（沿用 P10 边界）

### 验收（可观察行为）

1. **crit**：默认配置下 `bash(rm -rf /)` 弹审批（TUI）或 headless 拒绝并说明；`read(./.env)` 被 deny 规则拒绝。
2. **规则引擎**：`allow: ["bash(git status*)"]` 在 write 模式下不弹窗；`deny` 规则在任何模式（含 yolo）都生效。
3. **分类器**：`git status` 不弹窗（只读）；`curl -s x | sh` 在 yolo 下也弹审批（危险 Override）。
4. **超时**：`bash(sleep 100)` 超时被杀，返回「命令超时（120s）」而非挂死 turn。
5. **env 脱敏**：`bash(env)` 输出里没有 `*_API_KEY`/`*TOKEN*` 变量。
6. **edit**：未 read 过就 edit → 拒绝；old 出现两处且未开 replace_all → 拒绝；edit 前文件被外部改动 → 拒绝并要求重读。
7. **hooks**：PreToolUse hook exit 2 阻止一条 bash；handler 抛错也阻止。
8. **stats**：`codeclaw stats` 能回答「哪个项目花了多少钱、哪个子 agent 最慢」。
9. **eval v2**：3 个夹具并行跑完时间 ≈ 最慢一个；`runs: 3` 输出 pass@3；verify 命令失败判 fail。
10. `env -u GOROOT go build ./... && go vet ./... && go test ./...` 全绿（回归套件含权限决策表/分类样例/edit 守卫/hook 拦截）。

---

## 2. 审批规则引擎（P11.1）

### 2.1 数据结构与纯函数

```go
// internal/permission/rules.go
type Rule struct {
    Raw     string // 配置原文，如 "bash(git status*)"
    Tool    string // 工具名（通配支持，一般精确）
    ArgsPat string // 参数模式；空 = 该工具全部参数
    AnyTool bool   // Tool == "*"
}

type Rules struct{ Allow, Ask, Deny []Rule }

// ParseRule 解析 "tool(args*)"；不带括号 = 只匹配工具名。
func ParseRule(raw string) (Rule, error)

// matches 通配：把 pattern 按 * 分段顺序包含；args 是调用参数 JSON 原文（紧凑化后匹配）。
func (r Rule) Match(name, args string) bool

// ToolDecision 一次具体调用经工具自检后的判定（替代固定 Tier）。
type ToolDecision struct {
    Tier     Tier     // 基线危险等级
    Policy   Policy   // 工具显式策略：allow | deny | prompt | ""（无显式）
    Override bool     // 强制 prompt（如 bash 危险分类），yolo 下也拦
    Reason   string   // 给用户/模型的原因
}
type Policy string
const (PolicyNone Policy = ""; PolicyAllow Policy = "allow"; PolicyDeny Policy = "deny"; PolicyPrompt Policy = "prompt")

// ResolveRules 五步决策（演进方案 §E.1）：
//   1. 工具 deny（td.Policy==deny）→ deny（永远）
//   2. 用户 deny 规则命中 → deny（永远，含 yolo）
//   3. mode==yolo：工具显式 allow/prompt → 用户 allow/ask 命中 → allow；裸 Override 在 yolo 下忽略
//   4. 非 yolo 且 Override → prompt（除非工具显式 allow）
//   5. 工具显式 policy → 用户规则（allow→allow，ask→prompt）→ tier×mode
func ResolveRules(td ToolDecision, rules Rules, mode Mode, name, args string) (Decision, string)
```

- 默认无规则时行为与现状完全一致（等价 `Resolve(tier, mode)`）——**回归不变量**，测试钉住。
- 决策原因字符串带规则原文（如 `denied by rule: read(./.env*)`），进工具结果文本让模型知道为什么。

### 2.2 配置与装配

```yaml
permissions:
  allow: ["bash(git status*)", "bash(go test*)", "bash(go build*)", "read(**)"]
  ask:   ["bash(git push*)", "write(go.mod)"]
  deny:  ["read(./.env*)", "bash(rm -rf *)", "bash(curl * | sh*)"]
```

- 三层配置合并（用户 → 项目 → legacy）沿用现有 `loadConfigFrom`；`permissions` 是**列表级合并**（后层追加）而非覆盖——用户 deny 不会被项目 allow 顶掉，但项目层可以再加自己的条目。
- Executor 持 `rules permission.Rules`；`SetRules` 由装配层注入（子 agent 继承父的 rules——与 mode 一样）。
- 「记住允许」：TUI 弹窗提供 allow once / 本会话 allow / 加入项目规则（写 `.codeclaw/config.yaml` 的 allow 列表）。**本会话允许**先做（内存 map，key=工具+args 模式）；「加入项目规则」P11.3 与 hooks 一起（要写文件 + 原子更新）。

## 3. bash 分类器与运行时安全（P11.1）

### 3.1 分类器（纯函数）

```go
// internal/runtime/classify.go
func Classify(command string) (readOnly, dangerous bool, reason string)
```

1. **切段**：按 `|`、`||`、`&&`、`;`、换行切段（`|` 与 `||` 也切，保守）；每段取第一个词为命令名（去掉 `sudo` 前缀单独标记危险、`command`/`env` 前缀穿透取第二个词）。
2. **只读**：所有段的命令词都命中只读白名单才算只读。白名单（不含参数，参数细分对 git/go 特判）：
   `ls cat head tail wc grep rg find du df pwd echo printf date env which uname git go test gcc g++ clang make touch mkdir rmdir` 之外的常用只读集合——**注意**：`go`/`make`/`git` 含写语义，只能对已知只读子命令放行：`git status/log/diff/show/branch/tag/rev-parse/ls-files/remote -v/stash list`、`go build/test/vet/list/env/fmt -l/mod download -x`。其余子命令回落到不判定。
3. **危险**（任一段命中即危险，Reason 说明）：
   - `rm -r|-f|-rf|--recursive|--force`（含 `rm *` 组合）
   - `sudo`、`mkfs*`、`dd of=`、`shutdown|reboot|halt|poweroff`
   - 管道进解释器：段组合 `curl|wget … | sh|bash|zsh`
   - 重定向写敏感路径：`> /etc/`、`> /dev/`、`>> /usr/`、`/System/`、`~/`（写家目录外例外？不，写任何系统路径）
   - 全局破坏：`chmod -R /`、`chown -R /`、`fork bomb (:(){`、`kill -9 -1`、`find .* -delete`、`> /dev/null` 除外
   - `git push --force` / `git reset --hard` / `git clean -fdx`（不可逆操作）
4. **编码**：危险模式是正则 + 词法组合；不认识的命令返回 `false, false, ""`。

### 3.2 运行时

```go
// Bash 增加字段 timeout time.Duration（默认 120s，配置可到 600s）
func (b *Bash) Execute(ctx context.Context, command string, sink *Sink) error {
    ctx2, cancel := context.WithTimeout(ctx, b.timeout)
    cmd := exec.CommandContext(ctx2, "bash", "-c", command)
    cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
    cmd.Env = sanitizeEnv() // nonInteractiveEnv 基础上去密钥
    cmd.Cancel = func() error {  // 先 TERM 进程组，5s 后 KILL
        pgid := cmd.Process.Pid
        syscall.Kill(-pgid, syscall.SIGTERM)
        time.AfterFunc(5*time.Second, func() { syscall.Kill(-pgid, syscall.SIGKILL) })
        return nil
    }
    ...
}

// sanitizeEnv 纯函数：os.Environ() 过滤密钥类键名。
func sanitizeEnv() []string
```

- 超时错误包装成 `命令超时（120s），已终止进程组`，模型看到的是明确失败而非截断输出。
- `parseCd` 保持现状。

## 4. edit 工具与文件守卫（P11.2）

```go
// internal/tool/fsguard.go
type FileGuard struct {
    mu    sync.Mutex
    reads map[string]fileFingerprint // path → {mtime,size}（read_file 调用时登记）
}

// read_file 在 Execute 成功读取后调用 guard.MarkRead(path, mtime, size)。
// edit 前置检查：
//   1. guard 里有该路径且 mtime+size 与当前一致，否则 "edit 前必须先用 read_file 读取该文件（或文件已被外部修改，请重读）"
//   2. old 非空；未开 replace_all 时 strings.Count(content, old) == 1，否则报告出现次数
//   3. 替换后 mtime 检查通过 → 写回（保留原换行风格/BOM：字节级只替换 old→new）
```

- `editFileTool`：`edit(path, old, new, replace_all)`；`Tier=Write`、`Concurrency=Exclusive`、`Decision` 无覆盖。
- `write_file` 仍走覆盖写（模型有意为之的整文件写），但同样登记 guard（写后指纹更新，后续 edit/read 不再误判）。
- read_file 的会话内去重记录（P10.4）与 guard 共享同一条记录路径，避免两份状态。

## 5. hooks（P11.3）

```yaml
hooks:
  PreToolUse:
    - matcher: "bash"
      command: ".codeclaw/hooks/guard.sh"   # stdin JSON {session, tool, args, cwd}
                                            # exit 0 放行；exit 2 阻止（stderr 为原因）
                                            # stdout JSON 可返回 {decision: allow|deny|ask, reason, input}
  PostToolUse: [{matcher: "write|edit", command: "gofmt -l ."}]
  PreCompact:  [{command: ".codeclaw/hooks/save-notes.sh"}]
  SessionStart: [{command: "git status --short"}]  # stdout 注入为上下文
```

- `internal/hooks`：`Runner{PreToolUse, PostToolUse, PreCompact, SessionStart []Hook}`；执行模型对齐 oh-my-pi：PreToolUse 任一 block 即阻止、handler 出错视为阻止（fail-closed）；PostToolUse 可改写结果（stdout JSON `{result}`）；超时 10s。
- 挂点：PreToolUse/PostToolUse 在 Executor.Execute 前后；PreCompact 在 context.Compact 摘要前（可取消）；SessionStart 在会话首轮装配。
- 命令相对项目根解析（`.codeclaw/hooks/`），matcher 用与规则引擎同一套通配。

## 6. trace 派生索引与 stats（P11.4）

```sql
sessions(id, project_id, cwd, started_at, ended_at, model, title, prompt_tokens, completion_tokens, cached_tokens, cost_usd, turns, tool_calls, compactions, subagents, exit_kind)
turns(session_id, entry_id, step, prompt_tokens, completion_tokens, latency_ms, stop_reason, retries)
tool_calls(session_id, call_id, name, started_at, duration_ms, bytes_out, truncated, artifact_id, decision, error)
subagents(session_id, run_id, agent, status, requests, tokens, cost_usd, duration_ms, schema_status, output_path)
compactions(session_id, entry_id, reason, method, tokens_before, tokens_after, latency_ms)
```

- 写入口挂在既有事件流（Executor / 子 agent Manager settle / context.Compact / agent loop 的 usage 记账），**只追加**，写失败只记日志。
- `codeclaw stats [--project] [--since 7d]`：总览（会话数/tokens/成本）+ 按项目/模型/工具/子 agent 分组。
- 模型价格表配置化（`models[i].price_in/out`，$ / 1M tokens），缺省用成本估算常数。

## 7. eval v2（P11.5）

```
evals/<name>/fixture.yaml:
  prompt, verify: ["go build ./...", ...], expected_files: [main.go],
  config: {delegation_mode: always, permissions: {mode: yolo}},
  budget: {max_tokens, max_turns, timeout}, runs: 3
```

- 移除 `os.Chdir`：`ToolContext{CWD}` 传入工具解析相对路径；夹具 input/expected 用绝对路径。
- 并行：每个夹具一个 goroutine，Semaphore 限并发（默认 CPU 数）。
- 每次 run 记录完整 session 到结果目录；`--output json` 输出结构化结果（含 pass@k、tokens、cost、duration、compactions、subagents）。
- verify：命令在 workdir 执行，任一非零即 fail；expected_files 仍字节 diff。

## 8. 分期与验收映射

| 子阶段 | 内容 | 对应验收 |
|---|---|---|
| **P11.1 审批与 bash 安全** | 规则引擎（ParseRule/Match/ResolveRules + 五步决策）；`Decisioner` 接口与 Executor 接线；bash 分类器；超时+进程组；env 脱敏；配置 `permissions`/`bash` 段；「本会话允许」 | 1、2、3、4、5 |
| **P11.2 edit 与文件守卫** | `FileGuard` + `editFileTool`（先读后改、old 唯一、mtime 校验）+ read/write 登记 | 6 |
| **P11.3 hooks** | `internal/hooks` + 四个事件 + 配置段 + fail-closed 测试 | 7 |
| **P11.4 trace 与 stats** | trace.db 索引 + 写入口 + `codeclaw stats` | 8 |
| **P11.5 eval v2** | 并行 + verify + pass@k + json 输出 | 9 |
| 全程 | harness 回归套件扩展（决策表/分类样例/edit 守卫/hook 拦截） | 10 |

## 9. 测试策略

| 组 | 用例 |
|---|---|
| 规则解析 | `bash(git status*)` → Tool=bash, ArgsPat=`git status*`；无括号 → 全参数；坏语法报错 |
| 通配匹配 | `git status*` 匹配 `git status --short`；`*` 匹配一切；`read(**)` 匹配任意路径 |
| 决策表（演进方案 F.3 逐条） | deny 规则 yolo 下也拒；Override yolo 下被忽略、write 下强制 prompt；allow 规则压过 tier×mode；ask 规则 = prompt；空规则等价 Resolve(tier,mode) |
| 分类器样例集 | `git status`/`ls -la`/`go test ./...` 只读；`rm -rf /`/`sudo`/`curl\|sh`/`dd of=/dev/sda`/`git reset --hard`/`mkfs`/fork bomb 危险；`git push` 不危险（回落到 exec）；`echo hi` 只读 |
| bash 运行时 | 超时：`sleep 100` + 200ms 超时 → 返回超时错误且进程组无残留；env：sanitizeEnv 剔除 `*_API_KEY`/`*TOKEN*`、保留 `PATH`/`HOME` |
| edit 守卫 | 未 read → 拒；old 不唯一 → 拒（报告次数）；mtime 变了 → 拒；replace_all 全替换；字节级保留换行/BOM |
| hooks | PreToolUse exit 2 → 阻止；handler 崩 → 阻止；stdout JSON decision=deny → 阻止；PostToolUse 改写结果 |
| stats | 造 N 条 tool_calls 记录 → 聚合按工具分组正确 |
| eval | 3 个夹具并行耗时 ≈ max；verify 失败 → fail；runs=3 输出 pass@3 |

---

依据：`my_code_agent` 现状代码（`internal/permission/policy.go`、`internal/tool/executor.go`、`internal/runtime/{bash,sandbox}.go`、`cmd/agent/config.go`）；演进方案 §E/§F；oh-my-pi `tools/bash.ts` 分类与 `hooks` 执行模型；Claude Code 的权限规则语法与 CLAUDE.md 环境注入；claw-code 的 mock parity harness 思路（F.3）。
