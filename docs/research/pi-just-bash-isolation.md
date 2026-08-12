# just-bash 对 Pi Runtime 隔离的适用性评估

状态：只读技术评估，不是实现方案或生产能力声明
日期：2026-08-12
范围：CodexLoom 当前 Pi Runtime、`vercel-labs/just-bash`、现有 OpenShell #32 原型

## 结论

**不推荐用 just-bash 替代 OpenShell，也不能据此把 Pi Runtime 标记为
Sandbox。** just-bash 适合限制模型提交给一个虚拟 `bash` 工具的脚本：它用命令注册表而非
宿主 shell 执行命令，可只暴露给定文件系统，网络默认关闭，并提供执行资源上限。
但它明确在 Pi 的 Node.js 进程内运行、没有 VM 隔离；Pi 本身、任意 Pi 扩展、其他内置工具、
扩展自行发起的网络/文件/进程操作，以及 Pi 已继承的环境变量都不进入该边界。
[just-bash Security Model](https://github.com/vercel-labs/just-bash/blob/1fbde341d74ff7f933d9cead9a390a6ab65b5df3/packages/just-bash/README.md#L564-L590)
[just-bash trust assumptions](https://github.com/vercel-labs/just-bash/blob/1fbde341d74ff7f933d9cead9a390a6ab65b5df3/THREAT_MODEL.md#L26-L31)
[Pi security](https://github.com/earendil-works/pi-mono/blob/534bcbffb7e1e7551d9ee3572dfeb278e203e493/packages/coding-agent/docs/security.md#L23-L40)

因此：

- 对 **model-generated bash/script isolation**：有用，但只能诚实命名为
  `virtual bash` 或 `restricted shell`；若其他 Pi 工具仍可直接访问宿主，它甚至不是完整的
  model-tool isolation。
- 对 **whole-process Pi isolation**：不能满足，也不解除
  [#32](https://github.com/kassol/agentloom/issues/32) 的阻塞。
- 对真实项目开发维护：不适合作为默认执行后端，因为内置命令表没有 `git`、`go`、
  `make`、`npm` 等任意宿主工具链；官方也建议需要任意二进制时换成真正的 VM Sandbox。
  [just-bash command registry](https://github.com/vercel-labs/just-bash/blob/1fbde341d74ff7f933d9cead9a390a6ab65b5df3/packages/just-bash/src/commands/registry.ts#L16-L114)
  [just-bash Sandbox guidance](https://github.com/vercel-labs/just-bash/blob/1fbde341d74ff7f933d9cead9a390a6ab65b5df3/packages/just-bash/README.md#L466-L492)

## 必须区分的两个边界

| 边界 | just-bash 能否提供 | 实际覆盖 |
| --- | --- | --- |
| 模型生成的 bash 脚本 | 可以，边界有限 | 脚本只能调用注册命令；只见配置的虚拟/受限文件系统；网络默认关闭；循环、输出和部分内存有上限。 |
| Pi 暴露给模型的全部工具 | 默认不能 | 仅替换 `bash` 时，`read`、`write`、`edit`、`grep`、`find`、`ls` 仍可走宿主实现；必须禁用或逐一替换才覆盖工具调用面。 |
| 整个 Pi Runtime | 不能 | Pi 进程、扩展 TypeScript、语言服务器、包安装、扩展网络请求及其他宿主进程仍以 Pi 用户权限运行。 |
| OS/容器/VM 权限边界 | 不能 | just-bash 明确无 VM 隔离；强内存约束和任意二进制执行需要进程、容器或 VM。 |

Pi 官方允许扩展覆盖 `bash`、`read`、`write`、`edit`、`grep`、`find`、`ls`，也可以用
`--no-builtin-tools` 去掉宿主内置工具，因此工具级接入在 API 上可行。
[Pi tool overrides](https://github.com/earendil-works/pi-mono/blob/534bcbffb7e1e7551d9ee3572dfeb278e203e493/packages/coding-agent/docs/extensions.md#L1835-L1861)
Pi 的 `BashOperations` 也提供了可替换的 `exec` 接缝。
[Pi BashOperations](https://github.com/earendil-works/pi-mono/blob/534bcbffb7e1e7551d9ee3572dfeb278e203e493/packages/coding-agent/src/core/tools/bash.ts#L53-L75)
但 API 可接入不等于获得整进程边界：Pi 明确说明扩展与内置工具都使用 Pi 进程权限，真正
隔离必须来自 OS、容器或虚拟化。
[Pi no built-in sandbox](https://github.com/earendil-works/pi-mono/blob/534bcbffb7e1e7551d9ee3572dfeb278e203e493/packages/coding-agent/docs/security.md#L23-L30)

## just-bash 实际承诺与限制

### 有价值的限制

- 默认 `InMemoryFs` 不访问磁盘；`OverlayFs` 可从一个真实目录读取、把写入留在内存；
  `ReadWriteFs` 才会直接修改指定真实目录。
  [filesystem modes](https://github.com/vercel-labs/just-bash/blob/1fbde341d74ff7f933d9cead9a390a6ab65b5df3/packages/just-bash/README.md#L169-L218)
- 网络默认关闭；开启后可按 origin、路径前缀和 HTTP 方法限制，并在请求边界注入 header。
  [network policy](https://github.com/vercel-labs/just-bash/blob/1fbde341d74ff7f933d9cead9a390a6ab65b5df3/packages/just-bash/README.md#L254-L302)
- bash 命令来自固定注册表，没有从解释器到 `child_process` 的正常执行路径；其威胁模型把
  这一点列为主防线。
  [defense layers](https://github.com/vercel-labs/just-bash/blob/1fbde341d74ff7f933d9cead9a390a6ab65b5df3/THREAT_MODEL.md#L287-L301)
- 有解析、循环、命令数量、输出、文件系统和执行时间等限制，适合低权限脚本计算或文本处理。
  [execution protection](https://github.com/vercel-labs/just-bash/blob/1fbde341d74ff7f933d9cead9a390a6ab65b5df3/packages/just-bash/README.md#L538-L563)

### 不能跨越的限制

- just-bash 把宿主提供的 `fs`、`fetch`、custom commands、transform plugins，以及
  Node/OS 都视为可信；它保护的是不可信脚本，不是不可信宿主或扩展。
  [trust assumptions](https://github.com/vercel-labs/just-bash/blob/1fbde341d74ff7f933d9cead9a390a6ab65b5df3/THREAT_MODEL.md#L26-L31)
- 自定义命令默认是可信宿主代码；官方提醒 JavaScript 无法强停任意宿主代码，要求硬保证的
  外部副作用必须放到可终止 worker/process。
  [custom command boundary](https://github.com/vercel-labs/just-bash/blob/1fbde341d74ff7f933d9cead9a390a6ab65b5df3/packages/just-bash/README.md#L48-L69)
- 启用 Python/JavaScript 增加攻击面；官方明确说强内存约束仍需进程/容器隔离。
  [Security Model](https://github.com/vercel-labs/just-bash/blob/1fbde341d74ff7f933d9cead9a390a6ab65b5df3/packages/just-bash/README.md#L564-L590)
- `ReadWriteFs` 只把真实磁盘访问缩到一个 root，并没有隔离运行 just-bash 的 Pi 进程或扩展。
  `OverlayFs` 更安全，但写入不落盘，不能承担正常开发结果。
  [filesystem modes](https://github.com/vercel-labs/just-bash/blob/1fbde341d74ff7f933d9cead9a390a6ab65b5df3/packages/just-bash/README.md#L187-L218)
- 项目当前标记为 beta；即使上游成熟，边界类型仍是解释器级而非整进程级。
  [package status](https://github.com/vercel-labs/just-bash/blob/1fbde341d74ff7f933d9cead9a390a6ab65b5df3/packages/just-bash/README.md#L1-L6)

## 与当前 CodexLoom Pi 集成的关系

当前 Loom 在宿主上启动 `pi --mode rpc`，为 Agent 传入宿主工作目录，并显式加载 Loom
扩展；Pi 子进程先继承 Hub 的完整环境，再覆盖少量 Loom 变量。
[Pi Runtime start](../../internal/hub/pi_agent_runtime.go#L133-L168)
[Pi RPC environment](../../internal/pi/rpc.go#L70-L92)
Loom 扩展本身读取 `process.env`、直接 `fetch` Loom HTTP API，并可注册其他工具。
[Loom Pi extension](../../internal/pi/loom_extension.ts#L26-L59)

把 just-bash 放进这个扩展后，最外层仍是：

```text
host Pi process (full Owner permissions)
├── Loom / third-party extensions (host TypeScript)
├── Pi read/write/edit/... tools (host, unless all replaced)
└── just-bash interpreter
    └── model-generated virtual bash script (restricted)
```

所以它能缩小一个工具槽位的权限，却不能改变 Pi Runtime 的宿主权限。现有 ADR 也明确把
审批扩展与 Sandbox 分开：审批能阻止识别到的工具调用，但不声称提供 Pi 隔离。
[ADR 0005](../adr/0005-use-one-pi-extension-for-loom-tools-and-approvals.md)

## 对 #32 验收条件的逐项判断

| #32 条件 | just-bash | 判断依据 |
| --- | --- | --- |
| Pi、扩展、内置工具、后代进程在同一边界 | 不满足 | 只解释虚拟 bash；Pi 和扩展仍在宿主。 |
| 长连接 Pi RPC、stderr、interrupt、close、进程树终止 | 不提供 | 不监管 Pi RPC 或 Pi 进程树。 |
| 宿主 home、Loom 数据、兄弟目录不可访问 | 部分 | 对 just-bash 脚本可通过 FS 配置限制；宿主 Pi 工具和扩展不受限。 |
| 未列网络目的地和本地服务不可访问 | 部分 | 对 just-bash `curl` 可限制；Pi/扩展的 `fetch` 与其他进程不受限。 |
| 原始凭证不进入边界 | 不满足 | 脚本环境可以不传凭证，但当前 Pi 进程已继承 Hub 环境。 |
| scoped Loom relay | 不提供 | 当前扩展仍直连 Loom API；just-bash 不改变控制面授权。 |
| 重启保持 Agent/Thread/Session/history | 不提供 | 不管理 Sandbox 身份、存储或恢复。 |
| 模型、图片、Approval、协作、历史一致 | 无帮助 | 这些是 Pi RPC/Runtime conformance，不是虚拟 shell 能力。 |
| macOS/Linux 同一整进程语义 | 不满足 | TypeScript 解释器可跨平台，但没有共同 OS/VM policy boundary。 |
| 失败不回退宿主 Pi | 不满足 | Pi 从一开始就在宿主；没有隔离 Runtime launcher。 |

仓库已有研究要求整进程 OpenShell 边界，并明确拒绝只包装 `bash` 作为 Pi Sandbox。
[Pi whole-process isolation research](./pi-execution-isolation.md)
当前 #32 原型也只证明固定镜像、扩展加载和基础 RPC，真实 OpenShell 边界仍因缺少 CLI/gateway
而阻塞。
[OpenShell prototype status](../../prototype/pi-openshell/README.md)

## 最小可行接入（仅在需要“受限虚拟 shell”时）

如果产品需要一个**只读分析/文本处理模式**，最小且诚实的实现是：

1. 在现有 Loom Pi 扩展中覆盖 `bash`，让 `BashOperations.exec` 调用一个 Agent 级共享的
   just-bash `Bash` 实例；Pi 已提供该扩展接缝。
2. 使用 `OverlayFs` 限定到 Agent cwd，网络、Python、JavaScript 全部关闭，启用 hardened
   execution limits；结果命名为 `virtual-bash`，绝不映射成 Runtime `sandbox` capability。
3. UI/能力事实明确展示“bash-only、写入不落盘、其他 Pi 工具仍在宿主”。若要覆盖全部
   model-facing 文件工具，则用 `--no-builtin-tools` 并重新注册同一虚拟 FS 上的
   `read/write/edit/grep/find/ls`；在此之前不得声称 tool isolation。
4. 不为 `git`、`go`、`npm`、`make` 添加会直接 spawn 宿主进程的 custom commands；这样做会
   穿透该边界。真正需要项目工具链时继续使用宿主 Pi（Owner 监督）或整进程 Sandbox。

这一路径适合安全地运行小段模型生成的文本处理脚本、读取仓库快照或测试命令生成逻辑；
它不适合替代 CodexLoom 当前真实开发 Runtime。由于当前产品目标是实际项目开发，而且 #32
要求整进程隔离，**现在不应引入 just-bash 依赖或业务集成**。等出现明确的只读虚拟 shell
需求再做一个独立、可删除的 spike 即可。

## 最终建议

- **生产 Pi Sandbox：不推荐 just-bash；继续保持 unsupported。** 它不解除 #32，也不替代
  OpenShell/容器/VM。
- **真实项目开发：继续用当前宿主 Pi + Owner 审批/独立 worktree；不把它描述为恶意代码
  隔离。** 对无人值守或不可信仓库，仍需整进程容器、VM、micro-VM 或策略 Sandbox。
  [Pi untrusted-work guidance](https://github.com/earendil-works/pi-mono/blob/534bcbffb7e1e7551d9ee3572dfeb278e203e493/packages/coding-agent/docs/security.md#L28-L40)
- **可选窄用途：推荐作为 `virtual-bash`。** 仅当需要受限脚本/文本处理而不需要真实工具链
  或持久写入时采用，并把能力边界显示给 Owner。

本评估没有安装 just-bash、没有修改 Runtime 代码，也没有改变 #32/OpenShell 原型状态。
