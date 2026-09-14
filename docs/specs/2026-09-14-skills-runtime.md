# Skills 加载与运行时设计

## 目标

为 codeclaw 增加可审计、按需加载的 Skills 能力，同时保持现有三条承重不变量：

1. session JSONL 仍是会话真相源；
2. 每一步仍由 `context.Manager.Build` 重建输入；
3. skill 只提供指令，执行脚本仍必须经过现有 Tool/Runtime/Permission 链路。

本阶段不实现网络市场、远程安装、自动执行脚本或文件监听。

## 核心选择

采用 oh-my-pi 与 Claude Code 的混合模型：

- 启动时发现并解析 `name + description + path`，system prompt 只注入轻量索引；
- 模型匹配到 skill 后调用只读 `skill` 工具加载正文；
- `skill://<name>/<path>` 通过现有 `ArtifactStore` 路由和 `read_file` 读取配套资源；
- 用户通过 `/skill:<name> [args]` 显式注入 skill；
- 主 agent 和子 agent 共享同一个并发安全的 skill catalog；
- reload 原子替换 catalog，新建的子 agent 使用新快照，正在执行的模型步骤不读取半成品。

不把所有 SKILL.md 正文放入 system prompt。这样 skill 数量增长不会线性吞噬上下文，也不会让不相关 skill 的正文彼此干扰。

## 文件布局与发现边界

原生目录：

```text
$CODECLAW_HOME/skills/<name>/SKILL.md
<workspace>/.codeclaw/skills/<name>/SKILL.md
```

在 Git 工作区中，项目目录只从 cwd 向上扫描到最近的 `.git` 边界；非 Git 目录只扫描 cwd。禁止无边界扫描到文件系统根，避免父项目和共享临时目录中的 skill 泄漏。

兼容目录默认关闭，可分别开启 Claude、Codex 和 Agent Skills 目录：

```text
.claude/skills
.codex/skills
.agents/skills
```

优先级从高到低：

1. 离 cwd 最近的项目原生/兼容目录；
2. 显式 `custom_directories`（按配置顺序）；
3. 用户原生目录；
4. 用户兼容目录。

同名 skill 大小写不敏感且 first-wins；被覆盖项产生 warning。真实路径相同的符号链接静默去重。目录内与最终 catalog 均确定性排序，保护 prompt cache。

## SKILL.md 契约

```markdown
---
name: go-test-debugger
description: 当 Go 测试失败，需要定位竞态、mock 或断言问题时使用。
disable-model-invocation: false
user-invocable: true
allowed-tools: [read_file, grep, bash]
---

# Go test debugger

先运行最小失败测试……
```

要求：

- `name` 可省略并回退到目录名，但必须是安全的单段名称；
- `description` 和正文必须非空；
- `description` 规范化为单行并限制长度后才进入 system prompt；
- `enabled: false` 跳过；
- 未识别 frontmatter 字段向前兼容；
- `allowed-tools` 是能力提示，不授予权限；
- 单文件默认最大 256 KiB，单次最多加载 500 个 skill；
- 单个坏文件只产生 warning，不阻止其它 skill 或 agent 启动。

## 包与依赖

新增 `internal/skills`，只负责发现、解析、catalog、正文构造和安全路径解析，不依赖 `tool`、`runtime`、`message` 或 TUI：

```text
cmd/agent ──> internal/skills <── internal/tool(skill tool)
    │               │
    │               └── resolver registered on ArtifactStore
    ├──> system prompt index
    └──> TUI commands
```

`skills.Manager` 持有不可变 Snapshot，并通过锁原子替换。调用侧永远通过 Manager 获取当前完整快照，不自行扫描磁盘。

## 运行链路

### 模型自动选择

1. system prompt 列出可被模型发现的 `name: description`；
2. 模型调用 `skill(name, args)`；
3. 工具重新读取对应 SKILL.md，剥离 frontmatter，返回正文、绝对 base directory 和用户参数；
4. 工具调用与结果按现有 agent loop 写入 JSONL；
5. 正文要求读取附件时，模型使用 `read_file(skill://name/path)`；
6. 正文要求执行脚本时，模型使用 bash，权限策略照常生效。

### 用户显式选择

`/skill:<name> [args]` 由宿主解析并直接构造 user 消息，不让模型猜是否需要加载。会话同时追加 `skill_invocation` custom entry，用于 trace 审计。

`disable-model-invocation: true` 的 skill 不进入 system 索引且拒绝模型工具调用，但仍可在 `user-invocable` 未关闭时由用户显式调用。

### 子 agent

`workerTools` 注册 skill 工具和 URL resolver，因此主/子 agent 共享发现结果。子 agent system prompt 仅在其实际工具集中含 `skill` 时加入索引。只读子 agent 应按 `TierRead` 过滤工具，避免每增加一个只读工具都更新硬编码白名单。

## TUI/CLI 表面

首版提供：

```text
/skills
/skills list
/skills show <name>
/skills reload
/skill:<name> [args]
```

`reload` 完成后调用 `context.Manager.InvalidateSystem`，使下一次 Build 刷新索引。安装与卸载留到下一阶段，并要求显式 `--scope user|project`。

## 配置

```yaml
skills:
  enabled: true
  enable_commands: true
  compatibility:
    claude: false
    codex: false
    agents: false
  custom_directories: []
  include: []
  ignore: []
  max_file_bytes: 262144
  max_skills: 500
```

布尔字段使用指针合并，确保项目层能够显式关闭用户层配置。

## 安全不变量

- discovery/load 不执行 skill 中的任何代码；
- `skill://` 拒绝绝对路径、`..`、编码后的 traversal 和不存在目标；
- 最终路径经 `EvalSymlinks` 后仍必须位于 skill 的真实 base directory 内；
- skill 正文不改变工具表和 permission mode；
- description 在进入 prompt 前折叠换行，防止伪造新的索引项或 XML 结构；
- 模型不可调用 `disable-model-invocation` skill；
- reload 失败时保留上一份有效快照。

## 测试与验收

单元测试覆盖：

- frontmatter、CRLF、字段默认值和坏文件降级；
- Git 边界、目录优先级、同名覆盖、稳定排序和真实路径去重；
- include/ignore；
- `skill://` 正常资源、`..`、绝对路径和 symlink 越界；
- model/user invocation gate 与参数传递；
- skill 工具保持 TierRead；
- system prompt 只有索引、没有正文；
- reload 刷新 system-prefix；
- 只读子 agent 仍拥有 skill 工具；
- `go test ./...` 全量回归。

## 后续阶段

1. `autoload_skills` 子 agent frontmatter；
2. scope-aware install/uninstall；
3. session_init 记录 skill 名称、路径和摘要指纹，检测恢复时的磁盘漂移；
4. 为剪枝后的 skill 工具结果保留 `skill://name` 重载提示；
5. fake-model eval：匹配任务必须先调用 skill，非匹配任务不得加载。
