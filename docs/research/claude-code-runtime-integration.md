# CodexLoom Claude Runtime：Claude Code CLI 与 Claude Agent SDK 选型

状态：只读技术评估，不是实现或依赖引入
日期：2026-08-12
范围：在现有 Runtime Contract v2、Codex Driver、Pi Driver 架构中新增 Claude Runtime

> 后续结论：本报告的 CLI-first 路径已由
> [#36 的 direct CLI No-Go](https://github.com/kassol/agentloom/issues/36) 取代；当前方向是
> [#41 的薄 TypeScript Agent SDK bridge](https://github.com/kassol/agentloom/issues/41)。
> 以下内容保留为当时的选型证据与失败门槛。

## 决策

**第一版直接监督 Claude Code CLI，不引入 Claude Agent SDK。** 使用一个长生命周期的
`claude -p --input-format stream-json --output-format stream-json --verbose`
子进程作为每个 Loom Agent 的 native host，并像 Pi Driver 一样在 Go 内完成 native
event → Runtime Contract v2 的翻译。

这不是因为 Agent SDK 能力较弱；相反，它提供更完整的类型化 interrupt、permission
callback、hooks、MCP、session inspection 和外部 SessionStore。选择 CLI 的原因是：

1. Loom 是 Go 应用，Anthropic 官方明确说 Agent SDK 只有 TypeScript/Python；其他语言应以
   `-p` 方式监督 CLI 子进程；
2. Agent SDK 本身仍然启动 Claude Code CLI，并在其 stream-json 上增加控制协议、回调路由和
   类型层，因此在 Go 与 CLI 之间再加 Node/Python sidecar 并不会消除 CLI 生命周期和版本风险；
3. Loom 已经拥有 agent loop 之外的持久控制面、审批、历史投影、Agent/Turn 因果与 Host
   supervision，不需要为了 SDK 的应用框架再引入第二个常驻语言运行时；
4. CLI 的公开能力已经覆盖第一版核心：长连接 streaming input、partial output、session ID /
   resume、模型与权限启动参数、MCP、额外目录、usage/cost、hooks 与 subagent stream。

[Agent SDK overview](https://code.claude.com/docs/en/agent-sdk/overview)
[Claude Code CLI reference](https://code.claude.com/docs/en/cli-usage)
[Headless/stream-json guide](https://code.claude.com/docs/en/headless)

但该决策有三个硬门槛：**interrupt receipt、Loom Approval 往返、冷态 native history**。
先做一个可删除的 CLI probe；只要其中一项必须依赖未公开 wire schema、无法行为验证，第一版
就改为一个薄的 **TypeScript Agent SDK bridge**。不要自行复制 SDK 的私有 control protocol。

## 两条路径实际是什么

### 直接 Claude Code CLI

Go Hub 启动官方 `claude` 二进制，写 stdin 的 stream-json user messages，逐行读取 stdout
的 system、assistant、user、stream_event、result 等记录，stderr 单独留作诊断。官方文档说明
`stream-json` 是换行 JSON，最后一个 result 包含最终文本、成本和 session metadata；开启
partial messages 后可获得 token/tool deltas。`system/init` 还提供 model、tools、MCP、plugins
及 feature capability 名称，调用方应做 capability detection 而不是只比较版本。

[Programmatic streaming](https://code.claude.com/docs/en/headless#stream-responses)
[Streaming output](https://code.claude.com/docs/en/agent-sdk/streaming-output)

这与当前 Pi 路径最相似：每 Agent 一个受监督进程，Go adapter 保有 pending request、Turn
correlation、事件映射、失败和 close 语义。区别是 Pi 有本仓库已锁定的官方 RPC 命令响应，
Claude Code CLI 的公开 headless 文档更偏事件流；不要假设 SDK 内部的 `control_request` JSON
也是承诺给第三方的稳定协议。

### Claude Agent SDK

Agent SDK 不是独立模型 Runtime。官方定义是“以 Python/TypeScript library 运行 Claude Code
agent loop”；Python 官方源码的 `ClaudeSDKClient` 最终构造
`claude --output-format stream-json --verbose` 子进程，再在 stdin/stdout 上管理 initialize、
interrupt、set_model、permission callback、hooks 和 SDK MCP 请求。

[Python SDK client source at evaluated commit](https://github.com/anthropics/claude-agent-sdk-python/blob/be2d0dfbd9ee884ff43efd44e5a3158aa09a6a34/src/claude_agent_sdk/client.py)
[Python SDK subprocess transport](https://github.com/anthropics/claude-agent-sdk-python/blob/be2d0dfbd9ee884ff43efd44e5a3158aa09a6a34/src/claude_agent_sdk/_internal/transport/subprocess_cli.py)
[Python SDK control layer](https://github.com/anthropics/claude-agent-sdk-python/blob/be2d0dfbd9ee884ff43efd44e5a3158aa09a6a34/src/claude_agent_sdk/_internal/query.py)

因此 Loom 使用 SDK 必须新增一个 Node 或 Python bridge，再由 Go 监督 bridge，bridge 再监督
Claude CLI。它购买的是官方类型和高级控制面，不是少一个进程，也不是更强的 Sandbox。

## Runtime Contract v2 逐项适配

| Loom 需求 | 直接 CLI | Agent SDK | 判断 |
| --- | --- | --- | --- |
| Create/Resume Binding | `--session-id` 创建，result 捕获 session ID，`--resume` 恢复 | `session_id`、`resume`、`continue`、fork 均有类型化 option | 两者可行；Loom 必须保存明确 session ID，不能用 cwd 的“最近会话”。 |
| 长连接输入 | `--input-format stream-json` 官方支持，运行中输入会排队成后续 Turn | streaming input 是官方推荐模式 | 两者可行；CLI 少一层。 |
| partial streaming | `--include-partial-messages` 输出 `stream_event` deltas | 类型化 `StreamEvent` 后再给 bridge | 两者同源；CLI adapter 需保留未知事件为 diagnostic。 |
| Turn correlation | 输入可启用 `--replay-user-messages`；输出含 session/message/tool IDs | SDK message 类型和 `origin` 更丰富 | CLI 需要实测并建立 Loom Turn fence，不能只按“下一个 result”猜测。 |
| Interrupt | 文档暴露 capability names，但 CLI reference 没有给 Go 调用方一个稳定的 interrupt API | `ClaudeSDKClient.interrupt()` / query interrupt 是公开 SDK API | **SDK 明显更强；CLI probe 的第一硬门槛。** 不通过就采用 SDK。 |
| Approval / Needs You | `--permission-prompt-tool` 可把非交互审批交给 MCP tool | `canUseTool` / `AskUserQuestion` callback 直接暂停并返回 allow/deny/modified input | CLI 可复用一个最小 Loom MCP relay；SDK 更自然。不得用 `--dangerously-skip-permissions` 冒充 Approval。 |
| Hooks | CLI 加载 settings/plugin 中的 command、HTTP、MCP 等 hooks，并可输出 hook events | programmatic callback hooks，另可加载相同 filesystem hooks | Loom 只需审计/治理时 CLI 足够；需要动态 in-process callback 时 SDK 更适合。 |
| MCP | `--mcp-config`、`--strict-mcp-config`，init 报告 server status/errors | 外部 MCP + in-process SDK MCP server | Loom 控制面适合一个显式、Agent-scoped MCP server；不要加载用户全局 MCP 后再声称严格策略。 |
| Subagents | stream 中以 `parent_tool_use_id` 关联；`--forward-subagent-text` 可重建嵌套文本 | programmatic agent definitions、hooks、session helpers 更完整 | CLI 足以把 subagent activity 作为 native diagnostic/内容；Loom 第一版不应把 Claude subagent 升格为 Loom Agent。 |
| History | session 自动持久并可按 ID resume；直接被动读取完整历史会涉及本地 JSONL 格式 | `listSessions`、`getSessionMessages`、subagent helpers、SessionStore 有正式 API | **SDK 明显更强；CLI probe 的第三硬门槛。** 第一版可只投影 Loom 已见事件，但必须通过 restart/history conformance。 |
| Usage | assistant 和 final result 均有 usage；result 含 per-model usage/cost | 相同数据的类型化对象 | 两者可行。需按 message ID 去重；subagent 全树应读 model usage，不能只累加顶层 usage。 |
| Model | `--model`/`--fallback-model` 是 session 启动参数；init 报告实际 model | option 之外还公开 `set_model()` 动态控制 | CLI 第一版可在 idle 时 close + `--resume --model`；若实测破坏同一 session 语义则 SDK 胜出。 |
| Close/failure | Go 直接监管一个 process tree | Go 监管 bridge，bridge 再监管 CLI | CLI failure domain 更小。两条路径都必须把超时后未知结果记为 indeterminate。 |

[CLI input, permission, model and session flags](https://code.claude.com/docs/en/cli-usage)
[SDK permissions](https://code.claude.com/docs/en/agent-sdk/permissions)
[SDK sessions](https://code.claude.com/docs/en/agent-sdk/sessions)
[SDK subagents](https://code.claude.com/docs/en/agent-sdk/subagents)
[SDK usage](https://code.claude.com/docs/en/agent-sdk/cost-tracking)

### History is the main architectural caveat

Claude sessions persist conversation rather than workspace state。默认 transcript 位于
`~/.claude/projects/`；Agent SDK 的 SessionStore 也是“本地先写、再 best-effort mirror”，
mirror 失败最多重试后丢批次而不中止 agent。`getSessionMessages` 在 compaction 后只返回恢复
链，不等于所有原始记录；需要完整审计必须读 store raw entries。

[Session storage contract](https://code.claude.com/docs/en/agent-sdk/session-storage)

这与 Loom 的原则一致：Loom 继续是产品历史、Agent/Turn 因果和状态的唯一权威，Claude
transcript 只作为 native evidence。第一版 CLI 可以把运行期间收到的完整事件规范化并持久到
Loom；但 cold `ReadHistory`、崩溃窗口及 adoption 不能靠臆测。如果必须可靠查询未被 Loom
观察的 native history，Agent SDK SessionStore/API 是升级的最充分理由。

## Approval、hooks 与安全边界

Claude 的 `allowedTools` 只表示自动批准，不表示工具白名单；未列工具仍可落到 permission
mode/callback。要做锁定模式，应显式选择 tool surface/deny rules，并以 `dontAsk` 拒绝未批准
请求。`bypassPermissions` 会批准所有走到该阶段的工具，不能和一个短 allow list 组合后声称
受限。必须覆盖每个工具调用时，用 PreToolUse hook，而不是只依赖可能被 allow rule 绕过的
callback。

[Permission evaluation order](https://code.claude.com/docs/en/agent-sdk/permissions)
[Approval and user input](https://code.claude.com/docs/en/agent-sdk/user-input)

对 Loom：

- 第一版 CLI 用 `--permission-prompt-tool` 指向一个仅提供 approval/question 的 Loom MCP
  endpoint，并通过 `--strict-mcp-config` 控制这部分 MCP 配置；
- MCP 请求必须携带 Loom Agent/Turn 关联，但 native session ID 仍只在 adapter 私域；
- Owner 决定转成 Contract v2 `ApprovalDecision`，超时/断线不得自动批准；
- Claude 原生 subagent 继承 permission mode；它们仍属于同一 Runtime binding 和相同权限边界；
- hooks/permissions 是治理，不是隔离。CLI 或 SDK 仍拥有宿主用户进程权限，Sandbox 问题另行
  解决。

## 凭证与 Claude 订阅兼容

这里必须区分“本地 Owner 自用”与“对外产品认证”。

- Claude Code CLI 支持个人 claude.ai 登录、Team/Enterprise、Console API key 和云厂商；
  本地 Owner 运行 Loom 时，CLI 可以复用 Owner 已登录的 Claude Code 凭证。
- 但 Anthropic 明确规定：第三方开发者不得在自己的产品中提供 claude.ai 登录，也不得代用户
  路由 Free/Pro/Max 凭证；包括 Agent SDK 在内的产品/服务集成应使用 Console API key 或受支持
  的云提供商。
- `ANTHROPIC_API_KEY` 存在时可能优先于 subscription。Loom preflight/smoke 应报告“凭证来源
  类别和可用性”，不能打印 token，也不能悄悄把失效 env key 当作已登录订阅。

[Claude Code authentication](https://code.claude.com/docs/en/authentication)
[Authentication and credential-use terms](https://code.claude.com/docs/en/legal-and-compliance#authentication-and-credential-use)

因此，**CLI 与 Agent SDK 都不能让 Loom 合法地把用户 Claude 订阅包装成第三方 SaaS
能力**。本项目当前 local-first、single-owner 边界下，可以把现有 Owner Claude Code 登录
视作本机 Runtime prerequisite；如果未来提供远程多用户服务，必须转成 API/cloud credential
模式并重新审查条款。

## 部署、版本锁定和维护风险

### CLI 路径

- Preflight 解析一个显式 `claude` 路径，读取并验证版本；不要依赖 launchd/service 的 PATH。
- 官方 `claude install <version>` 支持安装指定版本。生产配置锁精确版本，并以 init
  `capabilities` 再做行为协商；不能自动 `claude update`。
- Go 只监管一个额外进程层，错误、stderr、exit code、process tree 和重启归属清楚。
- 风险是 stream-json 新事件持续扩展，且公开 CLI 对 interrupt/passive history 的低层契约不如
  SDK。parser 必须接收未知 message/block 并保留 diagnostic，不能因新增字段让整个 Runtime
  死亡。

### Agent SDK 路径

- 官方 SDK 只有 TS/Python。TypeScript 能力通常更早、更全；若升级，优先一个极薄 TS
  bridge，不在 bridge 中复制 Loom 状态机。
- Python SDK 当前源码同时记录 package version 和 bundled CLI version；评估 commit
  `be2d0df...` 为 SDK `0.2.136`、bundled CLI `2.1.228`。这说明必须同时锁 SDK 和 bundled CLI，
  不能只锁一个包版本。
- Python package 标为 Alpha；TS SDK 使用 Anthropic Commercial Terms，而非普通开源库许可。
- bridge 增加 Node/Python runtime、IPC schema、两级 shutdown、两级 stderr、打包和升级测试。
  但它把 Claude 私有 control protocol 留在 Anthropic SDK 内，并获得 typed callbacks、dynamic
  controls 和 session APIs，若 CLI 三个硬门槛失败，这些复杂度是合理成本。

[Evaluated Python SDK metadata](https://github.com/anthropics/claude-agent-sdk-python/blob/be2d0dfbd9ee884ff43efd44e5a3158aa09a6a34/pyproject.toml)
[Evaluated bundled CLI version](https://github.com/anthropics/claude-agent-sdk-python/blob/be2d0dfbd9ee884ff43efd44e5a3158aa09a6a34/src/claude_agent_sdk/_cli_version.py)
[TypeScript SDK license](https://github.com/anthropics/claude-agent-sdk-typescript/blob/0a2639d6b561af90342d4a98c93f9cc807d0e5ce/LICENSE.md)

## 与现有 Codex/Pi Driver 的最小落点

不要抽象出新的通用 sidecar framework。现有 `RuntimeHostDriver`、`AgentHost`、
`runtimecontract.Contract` 和 conformance 已是正确复用点。

第一版只新增：

1. `internal/claudecode`：二进制解析、version/preflight、一个 LF-JSON stream process client；
2. `claudeRuntimeHostDriver`：每 Agent 一个进程/handle，明确 failure domain；
3. `claudeRuntimeContract`：Binding、Turn correlation、event/history/usage/model/approval 映射；
4. 最小 Loom MCP approval relay（只有 CLI probe 证明需要且现有 HTTP relay 不能复用时）。

复用 Pi 的“每 Agent 进程”监督形态，但不要复制 Pi native DTO 或 RPC 假设。复用 Codex 的
未知事件 diagnostic、Turn fence、indeterminate 和 capability snapshot 处理。Claude 原生
subagent/MCP/hook 不新增 Loom 核心接口；先作为 adapter 私有事件和 resource inventory。

第一版应诚实标记：

- `goal`、`remote`、`sandbox_configuration`：unavailable；
- `manual_compaction`：直到找到并验证公开 headless 入口前 unavailable；
- `resource_inventory/policy`：可先 unavailable，避免解析 `.claude` 私有布局；
- `native rename/archive`：只有 CLI/SDK 正式 API 通过 conformance 后才 available。

## 必须先通过的 CLI probe

不改生产 Runtime，写一个独立 probe，锁定 Claude Code 版本后验证：

1. 一个进程接收两个 ordered Loom Turn，partial text/tool events、user replay 和 result 均可唯一
   归属；运行中追加输入不会误并入当前 Turn。
2. interrupt 后收到可证明的 receipt/terminal event；interrupt、result、process exit 竞态能映射
   为 interrupted/completed/indeterminate，且不会重复执行 workspace 副作用。
3. `--permission-prompt-tool` 的 approve、deny、超时、Hub 断线全部 fail closed；
   `AskUserQuestion` 能落入 Loom Needs You，而不是永久挂住进程。
4. kill Hub、kill CLI、重启并 `--resume <id>`；验证同一 session、历史、Turn 因果、usage、
   compact boundary 和未完成 Turn 恢复。
5. idle 时切换 model：close + resume + model 是否保持 session identity 和历史；否则 CLI
   `model_configuration` 标 unavailable 或升级 SDK。
6. MCP/plugin 加载错误必须从 init 结构化检测；不存在时不能继续宣称 capability available。
7. subagent 并行、嵌套和取消不会让主 Turn 提前 terminal；usage 使用 whole-tree model usage。
8. 精确版本在 macOS/Linux 各跑只读真实模型 smoke；不记录任何凭证值。

Go/no-go：第 2、3、4 项任一无法只靠公开 CLI 行为稳定完成，停止 CLI adapter，改用薄 TS
Agent SDK bridge。不要以解析 SDK 私有 JSON、信号猜测或直接读取未版本化 transcript 来绕过。

## 何时一开始就应选 Agent SDK

以下需求已经确定时，不必先走 CLI：

- Loom 必须在进程不退出的情况下使用公开 API 动态 interrupt/setModel/setPermissionMode；
- 必须以 callback 形式实现丰富的 Approval、AskUserQuestion 或动态 hooks；
- 必须列出、查询、fork、rename、tag、delete 或跨主机恢复 native sessions；
- 必须使用外部 SessionStore、查询 subagent transcripts，或把 in-process custom tools 暴露成
  SDK MCP；
- 团队愿意将一个 TS bridge 及其 CLI 双版本矩阵作为长期维护组件。

否则，SDK 大部分能力会与 Loom 已有控制面重叠。先采用 CLI 是更小、可删除、可行为验证的
实现。

## 最终建议

- **当前选择：直接 Claude Code CLI。** 它最符合 Go Host Driver 和现有 Pi 进程监督架构，
  也是 Anthropic 对非 Python/TypeScript host 的官方路径。
- **Agent SDK 作为明确 fallback，而非同时建设。** 三个硬门槛失败时，用薄 TS bridge，不写
  自己的 SDK clone。
- **认证：本地 Owner 可复用自己的 Claude Code 登录；对外产品必须使用 Console API key /
  cloud provider，不得提供 claude.ai subscription passthrough。**
- **能力声明：只以 Contract v2 conformance 和真实 smoke 为准。** CLI 文档中存在或 SDK
  类型中存在，都不等于 Loom adapter 已经可用。

本评估没有安装 Claude Code/Agent SDK，没有修改 Runtime 业务代码，也没有 commit 或 push。
