# Microsandbox 对 Pi Runtime 整进程隔离的适用性评估

状态：候选方案研究；不是实现方案或生产能力声明
日期：2026-08-13
范围：CodexLoom 当前支持的 macOS 与 Linux、本地 Microsandbox backend、Pi
`--mode rpc` 整进程隔离
上游基线：[Microsandbox v0.6.8](https://github.com/superradcompany/microsandbox/releases/tag/v0.6.8)
（commit [`7958cdde`](https://github.com/superradcompany/microsandbox/tree/7958cddeff2fce10dbcfd0c0d6a32dec93a9d5ea)）

## 结论

**Microsandbox 值得作为 #32 的第二个整进程隔离原型候选，而且从产品形态看比
just-bash 更接近 CodexLoom 的真实需求；但当前仍不能直接替换 OpenShell，也不能据此把
生产 Pi Runtime 标记为 Sandbox。**

它提供真正的 microVM 边界：每个 Sandbox 有自己的 Linux kernel、内存和 vCPU，Linux
使用 KVM，macOS 使用 Hypervisor.framework；Pi、Loom 扩展、第三方扩展、内置工具、shell
及所有后代进程都可装进同一 VM。其本地 Go SDK 可以嵌入 Loom，VM 作为子进程启动，不要求
常驻 gateway/daemon；这比当前被 OpenShell CLI/gateway 环境阻塞的原型更容易在 Loom 内做
一次本机 spike。
[Security model](https://github.com/superradcompany/microsandbox/blob/7958cddeff2fce10dbcfd0c0d6a32dec93a9d5ea/docs/security/overview.mdx)
[Isolation boundary](https://github.com/superradcompany/microsandbox/blob/7958cddeff2fce10dbcfd0c0d6a32dec93a9d5ea/docs/security/isolation.mdx)
[Go SDK architecture](https://github.com/superradcompany/microsandbox/blob/7958cddeff2fce10dbcfd0c0d6a32dec93a9d5ea/sdk/go/README.md#L14-L41)

但有五个生产 no-go：

1. **上游仍明确标为 beta**，警告 breaking changes、missing features 和 rough edges；而且
   `v0.6.5` 曾因 patch release 引入 breaking wire changes 被撤回。这比 OpenShell 的 alpha
   状态更积极，但仍不是可直接发布的成熟度证据。
   [README requirements and beta warning](https://github.com/superradcompany/microsandbox/blob/7958cddeff2fce10dbcfd0c0d6a32dec93a9d5ea/README.md#L117-L123)
   [Retracted v0.6.5](https://github.com/superradcompany/microsandbox/blob/7958cddeff2fce10dbcfd0c0d6a32dec93a9d5ea/sdk/go/go.mod#L1-L7)
2. **长连接 RPC 的恢复语义尚不满足 #32。** Microsandbox 的 streamed exec 支持独立
   stdin/stdout/stderr 和 signal，但 runtime relay 在 SDK client 断开时会向该 client 的所有
   active exec session 发送 `SIGKILL`。所以 detached VM 能跨 Hub 进程存活，不代表以
   `exec_stream` 启动的 `pi --mode rpc` 能跨 Hub 重启存活或重连。
   [Command streaming and stdin](https://github.com/superradcompany/microsandbox/blob/7958cddeff2fce10dbcfd0c0d6a32dec93a9d5ea/docs/sandboxes/commands.mdx#L138-L235)
   [Exec signal API](https://github.com/superradcompany/microsandbox/blob/7958cddeff2fce10dbcfd0c0d6a32dec93a9d5ea/sdk/rust/lib/sandbox/exec.rs#L430-L469)
   [Disconnect cleanup sends SIGKILL](https://github.com/superradcompany/microsandbox/blob/7958cddeff2fce10dbcfd0c0d6a32dec93a9d5ea/crates/runtime/lib/relay.rs#L1023-L1064)
3. **Go SDK 的原始 secret 值会持久化明文到 Sandbox config。** 凭据在运行时可只以
   placeholder 进入 VM，并只在允许的 TLS 目的地由宿主替换；但官方同时明确，SDK 的
   `.value(...)` 路径会把真实值原样写进持久配置。CLI 可保存宿主环境变量 source reference，
   当前文档中的 create-time SDK 示例则使用 raw value。Go v0.6.8 的 create option 只有 raw
   `SecretEntry`，但 modify API 已能表达 host env/store source reference；原型应验证“无
   secret 创建 → source-reference modify → restart”，而不是直接假定 Go 完全不能安全配置
   凭据。Loom 不能把当前 Hub 环境直接继承给 Pi，也不能接受凭据明文落到普通 Sandbox 配置；
   若上述路径不成立，才需要一个不落盘的宿主侧凭据适配层。
   [Secret handling guarantee and limits](https://github.com/superradcompany/microsandbox/blob/7958cddeff2fce10dbcfd0c0d6a32dec93a9d5ea/docs/security/secrets.mdx)
   [Raw SDK values are persisted](https://github.com/superradcompany/microsandbox/blob/7958cddeff2fce10dbcfd0c0d6a32dec93a9d5ea/docs/sandboxes/secrets.mdx#L91-L104)
4. **它不能覆盖 CodexLoom 当前完整的宿主矩阵。** Microsandbox 本地 backend 在 macOS
   只支持 Apple Silicon；CodexLoom 当前还认证 `darwin-x64`。因此它可以覆盖
   `darwin-arm64`、`linux-arm64` 和 `linux-x64`，但在不缩小产品支持范围或增加另一个后端前，
   不能成为唯一的跨平台 Pi Sandbox 实现。
   [Microsandbox host requirements](https://github.com/superradcompany/microsandbox/blob/7958cddeff2fce10dbcfd0c0d6a32dec93a9d5ea/README.md#L112-L123)
   [CodexLoom four-platform matrix](../claude-runtime-certification.md#four-row-preview-result-contract)
5. **v0.6.8 Go SDK 没有暴露 guest `rlimit` / `RLIMIT_NPROC` 配置。** 上游 Rust/CLI
   已有按 Sandbox 和 exec 设置资源限制的能力，但 Loom 若按 Go SDK 直接嵌入，在选定 release
   的公开 Go option surface 中不能配置它。vCPU 和内存上限仍由 VMM 强制，但 #32 要求的
   fork/process-tree hostile gate 对这条集成路径仍是 partial，不能从底层实现能力推定已满足。
   [v0.6.8 Go options](https://github.com/superradcompany/microsandbox/blob/7958cddeff2fce10dbcfd0c0d6a32dec93a9d5ea/sdk/go/options.go)
   [Rust exec rlimits](https://github.com/superradcompany/microsandbox/blob/7958cddeff2fce10dbcfd0c0d6a32dec93a9d5ea/sdk/rust/lib/sandbox/exec.rs)

因此当前建议是：**保留 OpenShell 的既有研究结论，同时做一个有界的 Microsandbox
spike；在 RPC 断连恢复、无明文凭据持久化、macOS/Linux 实机一致性和 upstream beta
状态解除前，生产能力继续为 `unsupported`，且绝不回退宿主 Pi。**

## 边界与架构

Microsandbox 的 trusted side 是 Loom/SDK、每 Sandbox 一个宿主进程，以及该进程内的 VMM、
用户态网络栈、文件系统 broker 和 secret substitution；untrusted side 是 VM 内的 Linux
kernel、PID 1 `agentd`、Pi、扩展和所有工作负载。VM 仅通过固定的 virtio-console、virtio-net、
virtio-fs、virtio-blk、virtio-rng 设备与宿主交互。宿主 Sandbox 进程以启动 Loom 的同一用户
运行，不需要 root；Linux 需要 `/dev/kvm` 权限，macOS binary 需要 hypervisor entitlement。
[Isolation boundary](https://github.com/superradcompany/microsandbox/blob/7958cddeff2fce10dbcfd0c0d6a32dec93a9d5ea/docs/security/isolation.mdx)

这符合 CodexLoom 的 Owner trust principle：它不是防御恶意 Owner 或 same-UID 宿主进程，
而是保护 Owner 主机免受 VM 内 Pi、模型错误、prompt injection、不可信依赖脚本和扩展行为
影响。Microsandbox 官方也明确把 compromised host、hypervisor/CPU 漏洞、被允许的远端目的地
及 image provenance 放在边界外。
[Security scope](https://github.com/superradcompany/microsandbox/blob/7958cddeff2fce10dbcfd0c0d6a32dec93a9d5ea/docs/security/overview.mdx#L45-L83)

建议的 Loom 形态：

```text
CodexLoom Hub (trusted host process)
└── Microsandbox Go SDK / FFI
    └── per-Agent host sandbox process
        ├── libkrun + KVM / Hypervisor.framework
        ├── host-side network policy + secret substitution
        ├── private rootfs / explicit workspace transfer
        └── guest Linux VM (untrusted)
            ├── agentd PID 1
            └── pi --mode rpc
                ├── Loom extension
                ├── third-party extensions
                └── tools and descendant processes
```

与 just-bash 相比，这是决定性差异：just-bash 只限制一个解释器工具槽位；Microsandbox 可把
整个 Pi Runtime 装进硬件虚拟化边界。因此它满足
[现有整进程隔离研究](./pi-execution-isolation.md) 的 boundary 类型要求，而不是缩小后的
“virtual bash”能力。

## 平台支持与集成成本

| 项目 | macOS | Linux | 对 Loom 的含义 |
| --- | --- | --- | --- |
| Host | Apple Silicon | x86_64 / ARM64，KVM enabled | `darwin-x64` 明确 unsupported；Linux CI/用户主机必须先验证 KVM。 |
| Guest | Linux microVM | Linux microVM | 两端可使用同一 Pi image、同一工具链和同一隔离语义。 |
| Loom API | Go SDK + CGO FFI | Go SDK + CGO FFI | Loom 可直接嵌入；但发布物需包含/安装匹配版本的 `msb` 与 `libkrunfw`。 |
| Runtime lifecycle | 每 Sandbox 一个宿主子进程，可 detached | 同左 | 无常驻 gateway，但 Loom 仍需监督、列举、清理和恢复这些进程。 |

官方 Go SDK 支持 macOS ARM64、Linux x86_64/ARM64，要求 Go 1.22+、CGO 和 C toolchain；Go
binary 只嵌入 FFI library，匹配版本的 `msb` 和 `libkrunfw` 仍会安装到
`~/.microsandbox/`。`EnsureInstalled` 默认会从 GitHub Releases 下载它们，因此生产集成需要
Loom 自己固定版本、校验资产并把安装/升级作为明确 Owner action，不能在普通 Agent start
路径静默联网安装。
[Go SDK supported platforms](https://github.com/superradcompany/microsandbox/blob/7958cddeff2fce10dbcfd0c0d6a32dec93a9d5ea/sdk/go/README.md#L26-L49)
[Pinned SDK/runtime downloader](https://github.com/superradcompany/microsandbox/blob/7958cddeff2fce10dbcfd0c0d6a32dec93a9d5ea/sdk/go/setup.go#L20-L116)

## 文件系统与 workspace

默认 guest 只看见 OCI image、独立的 writable root layer 和显式 mount；不会隐式看见 host
filesystem、环境变量或凭据。每个 Sandbox 的写层独立，stop/start 后仍可用，remove 才删除。
[Filesystem privacy](https://github.com/superradcompany/microsandbox/blob/7958cddeff2fce10dbcfd0c0d6a32dec93a9d5ea/docs/security/filesystem.mdx#L7-L31)

Microsandbox 同时提供 filesystem copy API、bind mounts、named volumes、disk volumes 和
snapshots。对首个 Pi 原型，建议继续采用 #32 已定义的 **copy-in/copy-out workspace**：启动前
把工作区复制进 private root/named volume，结束后只导出明确结果；不挂载 host home、Loom
data、`~/.pi`、SSH/cloud config 或 Docker socket。
[Filesystem API](https://github.com/superradcompany/microsandbox/blob/7958cddeff2fce10dbcfd0c0d6a32dec93a9d5ea/docs/sandboxes/filesystem.mdx)
[Volume types and mount defaults](https://github.com/superradcompany/microsandbox/blob/7958cddeff2fce10dbcfd0c0d6a32dec93a9d5ea/docs/sandboxes/volumes.mdx#L1-L31)

不把 writable bind mount 作为默认有两个原因：它本来就授权 guest 直接修改对应 host path；
且官方说明 Linux 5.6+ 使用 `openat2(RESOLVE_BENEATH)` 做最强的原子 containment，而旧 Linux
和 macOS 回退到 `O_NOFOLLOW`，对 crafted `..` 的保护更弱。若未来提供 direct workspace
模式，应明确它是性能/便利交换，并优先只读。
[Bind-mount containment limits](https://github.com/superradcompany/microsandbox/blob/7958cddeff2fce10dbcfd0c0d6a32dec93a9d5ea/docs/security/filesystem.mdx#L33-L48)

Snapshots 只包含 writable filesystem 与 pinned image identity，不包含 memory、running
process 或 network state；只能对 stopped/crashed Sandbox 创建。因此它适合保存预装依赖的
磁盘基线或灾难恢复点，**不能恢复正在运行的 Pi RPC session**。
[Snapshot semantics](https://github.com/superradcompany/microsandbox/blob/7958cddeff2fce10dbcfd0c0d6a32dec93a9d5ea/docs/sandboxes/snapshots.mdx#L1-L31)

## 网络与 Loom 控制面

所有 guest 流量进入宿主进程中的用户态 network stack，经 policy 检查后才出站，不走 host
kernel NAT。默认策略允许 public internet 和 gateway DNS，但拒绝 private、loopback、
link-local、cloud metadata 和 host；published ports 默认只绑定 host `127.0.0.1`。
[Network defenses](https://github.com/superradcompany/microsandbox/blob/7958cddeff2fce10dbcfd0c0d6a32dec93a9d5ea/docs/security/network.mdx#L7-L36)

这比 plain Docker 更接近 #32，但默认仍不够窄。Pi 原型必须显式改成 deny-by-default，只允许：

- Pi 模型/provider 所需的精确域名与端口；
- 一个专用 Loom relay 的精确 host endpoint；
- gateway DNS；以及
- 若 workspace/bootstrap 确需包下载，再逐项增加 registry/package host。

Microsandbox 支持按 direction、protocol、port、IP/CIDR、domain/domain suffix 和目的地组配置
first-match-wins policy，也可完全禁网。Domain 规则配合 observed-DNS pin、SNI 和 HTTP
authority 校验，防止 DNS rebinding、硬编码 IP + 假 SNI 和 domain fronting。
[Networking policy model](https://github.com/superradcompany/microsandbox/blob/7958cddeff2fce10dbcfd0c0d6a32dec93a9d5ea/docs/networking/overview.mdx)
[DNS/SNI defenses](https://github.com/superradcompany/microsandbox/blob/7958cddeff2fce10dbcfd0c0d6a32dec93a9d5ea/docs/security/network.mdx#L63-L111)

不能直接把当前 unrestricted Loom local HTTP API 暴露给 VM。VM 是新出现的可达不可信边界，
仍须实现 #32 的 per-Agent capability token + narrow relay，只允许该 Agent 的
Message/Topic/Needs You 等既定操作。Microsandbox 能 enforcement “只能到这个 host/port”，
不能替 Loom 决定 “这个 Agent 可以调用哪些 API”。

## 凭据

Microsandbox 的 runtime credential 机制很强：guest 环境只得到 placeholder，真实 secret 保持
在 host network layer；只有 allowed-host、DNS pin、TLS identity 和 HTTP authority 全部通过
时才替换。guest memory、disk 和 snapshot 不含真实值；不符合条件或不支持安全替换的请求会
保留 placeholder 或被阻止。
[Secret substitution gates](https://github.com/superradcompany/microsandbox/blob/7958cddeff2fce10dbcfd0c0d6a32dec93a9d5ea/docs/security/secrets.mdx#L9-L55)

需要诚实记录四个限制：

- allowed endpoint 最终会收到真实 secret；它不是请求级授权或防止允许端误用；
- 默认保证依赖 TLS interception，绕过 interception 或特殊 body encoding 可能无法替换，
  这时官方语义是 fail closed；
- secret 在 Sandbox 存活期间以普通字符串存在 host process memory，未专门 zeroize；
- **SDK raw value 会明文持久化到 Sandbox config**，与“真实值不进 VM/snapshot”是两个不同
  命题。Loom 原型不得忽略后者。

因此 credential gate 必须同时验证 provider API、OAuth/refresh、streaming response、图片输入、
模型切换和错误路径；不能只以一次 OpenAI/Anthropic HTTPS 请求成功作为完成证据。

## 生命周期、RPC 与恢复

Microsandbox 的生命周期比一个普通容器 wrapper 丰富：attached Sandbox 随 Owner process
退出而停止；detached Sandbox 可继续运行并由名称重新获取；支持 graceful stop、force kill、
drain、ping/touch、idle timeout、max duration、list/inspect 和 persisted restart。
[Sandbox lifecycle](https://github.com/superradcompany/microsandbox/blob/7958cddeff2fce10dbcfd0c0d6a32dec93a9d5ea/docs/sandboxes/lifecycle.mdx)

Pi RPC 的基础管道看起来可行：streamed exec 分离 stdout/stderr、支持 stdin pipe、exit event
和向 guest process group 发送 Unix signals。agentd 的 process manager 还针对 PID reuse 做了
generation 检查，并对 process group 发 signal，适合验证 interrupt/close 清理后代进程。
[Exec wire protocol](https://github.com/superradcompany/microsandbox/blob/7958cddeff2fce10dbcfd0c0d6a32dec93a9d5ea/crates/protocol/lib/exec.rs)
[Process-group signalling](https://github.com/superradcompany/microsandbox/blob/7958cddeff2fce10dbcfd0c0d6a32dec93a9d5ea/crates/agentd/lib/process.rs#L152-L186)

但恢复模型存在关键断点：

```text
Hub / SDK client disconnects
  └── host relay removes that client
      └── sends SIGKILL to every active exec session owned by it
          └── pi --mode rpc dies, even if the VM itself is detached
```

官方命令文档说 interactive detach 后进程继续、可用 session ID reattach，但 v0.6.8 的公开
SDK/source 中没有发现可把新的 SDK connection 重新绑定到旧 streamed exec 的 API；相反，
relay source 明确执行上述 disconnect cleanup。故本评估采用更保守、可由源码证实的结论：
**`exec_stream` 不能被假定为可恢复 Pi transport。** 若用 guest 内 supervisor/background
service 保住 Pi，再通过 guest TCP/SSH/自建 relay 重接 RPC，会新增协议桥接和授权面，必须作为
独立设计审查，不能在 spike 中悄悄扩大范围。
[Documented detach claim](https://github.com/superradcompany/microsandbox/blob/7958cddeff2fce10dbcfd0c0d6a32dec93a9d5ea/docs/sandboxes/commands.mdx#L243-L266)
[Relay disconnect behavior](https://github.com/superradcompany/microsandbox/blob/7958cddeff2fce10dbcfd0c0d6a32dec93a9d5ea/crates/runtime/lib/relay.rs#L1023-L1064)

## 与 OpenShell 及 #32 的对照

| 维度 | Microsandbox | OpenShell 既有结论 | #32 判断 |
| --- | --- | --- | --- |
| 隔离边界 | 每 Agent Linux microVM，整进程覆盖 | whole-process Sandbox；driver 可为 container/MicroVM | 满足边界类型，需实机攻击性验证。 |
| macOS/Linux | Apple Silicon + Hypervisor.framework；Linux KVM | Docker/Podman/MicroVM drivers | 只覆盖当前四平台矩阵中的三行；`darwin-x64` 是产品级阻塞，其他行仍需实机探测。 |
| Loom 集成 | Go SDK/CGO FFI，可嵌入，无常驻 gateway | CLI + local gateway，当前机器缺 prerequisite | Microsandbox 更易做本机 spike。 |
| 网络 | 宿主用户态网络栈，细粒度 policy；默认允许 public | gateway deny-by-default proxy | 必须显式改为 deny-by-default。 |
| 凭据 | host-side placeholder substitution；raw SDK value 可能明文持久化 | provider/inference routing，raw key 不进 Agent | 能力方向合适，但 Go/source-reference gate 未通过。 |
| Workspace | private root、copy API、volumes、bind | transfer workspace；host mounts 默认关 | 采用 copy-in/out 可满足。 |
| 进程与资源 | VMM 限制 vCPU/内存；底层支持 process-group signal 与 rlimit | process/fork policy | Go v0.6.8 无 rlimit option，Loom 集成路径仍是 partial。 |
| 长生命周期 | detached VM、stop/start、persistent disk | persistent Sandbox | VM 生命周期满足，Pi exec 生命周期未满足。 |
| Hub 重启恢复 | active exec 随 client disconnect 被杀；snapshot 不含 process | 原型同样尚未证明 RPC reconnect | 仍阻塞 #32。 |
| 上游成熟度 | beta；已有撤回的 breaking patch | alpha，官方不建议 production | Microsandbox 信号更好，但仍是生产 no-go。 |
| Pi 专属指导 | 无；需自建 pinned image 和 RPC adapter | Pi 官方明确给出 whole-process OpenShell pattern | OpenShell 的 Pi 路径更直接。 |

因此 Microsandbox **不解除** [#32](https://github.com/kassol/agentloom/issues/32) 当前任何已记录
的真实隔离验收项；它提供了一个可能更易落地的 runtime primitive。原有
[Pi whole-process isolation](./pi-execution-isolation.md) 的威胁模型、no-fallback 原则、scoped
Loom relay 和 conformance stories 全部继续适用。

### #32 验收矩阵

| #32 条件 | 当前判断 | 依据 |
| --- | --- | --- |
| Pinned complete Pi、Loom extension、tools、custom code、descendants 在一个边界 | **Partial** | OCI + whole microVM 的结构满足，但 exact Pi image 和完整路径未运行。 |
| 持续 full-duplex LF-JSONL、ordering、stderr、interrupt、close | **Partial** | byte-faithful stream、stdin、独立 stdout/stderr、signal 已有；Pi 多轮与背压未测。 |
| Host home/Loom/sibling/local service/raw credential/unlisted egress 不可达 | **Partial** | 私有 root、显式 mounts、network policy、secret substitution 方向符合；hostile probe 未跑。 |
| Process tree 与资源限制 | **Partial / Blocked** | process-group signal 与 VM kill 存在；Go v0.6.8 缺 rlimit option，完整 descendant/fork gate 未证实。 |
| Copy workspace 与 Owner review/apply boundary | **Partial** | copy API 可用；Git metadata、失败恢复和吞吐未测。 |
| Agent-scoped Loom relay | **Blocked** | 网络/virtio 可承载窄 relay，但 Loom relay 与 authority 尚不存在。 |
| Hub/Pi/VMM/host restart identity 与 history | **No-go for direct ExecStream** | SDK client 断开会 kill active Pi exec；disk snapshot 不保存 process/memory。 |
| Model、image、Approval、collaboration、history conformance | **Blocked** | 尚未运行 Loom Runtime Contract stories。 |
| 当前 macOS/Linux 平台一致性 | **No-go** | `darwin-x64` 不受支持；其余三行也未实测。 |
| Failure never falls back to host Pi | **Blocked** | Microsandbox 本身不回退，但 Loom launcher/probe 尚不存在。 |
| Inspector disclosure 与 unavailable reason | **Blocked** | 还没有产品状态或 UX。 |

## 建议的有界 spike

不先引入生产 Runtime；做一个独立、可删除的 prototype，固定 v0.6.8（或开始实施时最新的
明确版本）并验证：

1. **Prerequisite / fail closed**：macOS entitlement/Apple Silicon 与 Linux KVM 探测；缺失、
   VM boot 失败或 runtime version 不匹配时返回 `unavailable`，绝不启动 host Pi。
2. **Pinned Pi image**：复用 #32 的 Pi 版本、Loom extension 和 probe extension；镜像按 digest
   pin，验证上游只做 digest consistency、不验证签名/provenance 的限制。
3. **RPC baseline**：以无 TTY streamed exec 启动 `pi --mode rpc`，验证持续双向 LF-JSONL、
   ordering、独立 stderr、stdin backpressure、exit code、SIGINT/SIGTERM/SIGKILL。
4. **Whole-process probes**：Pi 内置工具、自定义扩展和 shell descendant 均无法读 host home、
   Loom data、兄弟仓库，无法访问未允许网络、本地服务和 metadata。
5. **Workspace**：只用 copy-in/out；stop/start 后 git/worktree 状态保留；export 失败不得自动
   删除 Sandbox。
6. **Credentials**：证明真实 provider secret 不在 guest env/files/argv/log/snapshot，且不明文
   落 durable config；若 Go SDK 没有 source-reference 路径，立即判该 gate blocked，不以 raw
   SDK value 继续。
7. **Scoped Loom relay**：只开放 per-Agent relay；越权 Agent ID、未列 API 和复用 token 全部
   失败并留下可审计结果。
8. **Process/resource gate**：在最终选定的 Go integration surface 上设置并攻击测试
   `RLIMIT_NPROC`、CPU、memory、output 和 file-descriptor limits；若只能绕过 Go SDK 调 Rust/CLI，
   先记录新的包装与版本兼容成本，不把底层已有实现算作 Loom 已支持。
9. **Failure/recovery**：分别杀 Pi、Hub、Sandbox host process 和整台 host；记录 detached VM、
   Pi session、Loom Agent/Turn/session binding 的真实结果。重点证明 Hub disconnect 的
   `SIGKILL` 行为，并决定是接受“Hub restart 明确终止 Turn”还是另开 transport 设计，而非
   假装 resume。
10. **平台与性能**：至少实测 `darwin-arm64`、`linux-arm64`、`linux-x64`；`darwin-x64`
    必须返回有解释的 `unsupported`，并在 ADR 中决定是否接受该产品缺口。测 cold create、
    warm start、copy throughput、build I/O、idle memory 和 concurrent Agents；阈值由产品
    目标决定，不能从 README 的 “under 100 ms” 宣传值推导。

## 采用门槛

只有以下条件全部成立，才值得把 Microsandbox 提升为生产候选：

- upstream 不再把当前版本标为 beta，或 Loom 明确接受并隔离其版本/迁移风险；
- macOS 与 Linux 通过同一套 whole-process、filesystem、network、credential、process-tree
  conformance stories；
- `darwin-x64` 有明确的产品决策和替代路径，而不是从支持矩阵中静默消失；
- Loom 实际使用的 Go/FFI surface 能执行 process/fork limits，或有经过审查且固定版本的
  最小适配层；
- Pi RPC transport 对 Hub restart 给出明确、可测试的产品语义，且不会静默复制 Agent、Turn
  或 Pi session；
- Go 集成可引用宿主 secret source，真实值既不进 VM，也不明文进入 durable config；
- scoped Loom relay 完成，VM 不可访问 unrestricted local API；
- installer/update、image/runtime pin、restart 和删除均是显式 Owner action，有清晰失败和
  recovery UX；以及
- 所有 prerequisite/runtime failure 都 fail closed，无 host fallback。

在此之前，产品 Inspector 最多显示 `Microsandbox prototype unavailable/experimental`，不能
显示已具备生产 Sandbox。

## 最终建议

- **值得原型。** 它是目前研究中第一个同时具备硬件 microVM、跨 macOS/Linux、本地嵌入式
  Go API、细粒度网络策略、宿主侧凭据替换和持久 lifecycle 的候选，明显比 just-bash 更符合
  Pi whole-process isolation。
- **暂不替换 OpenShell 决策。** OpenShell 有 Pi 官方推荐路径和更直接的 inference/provider
  broker；Microsandbox 的优势是本地嵌入与 VM 边界，短板是 Pi RPC disconnect recovery、
  Go secret persistence 和 beta 成熟度。
- **把 #32 拆出一个 Microsandbox spike 即可，不开生产实现票。** spike 若证明 RPC/凭据
  gate 可解，再基于 macOS/Linux 实测结果在 OpenShell 与 Microsandbox 之间做最终 ADR；若
  gate 不可解，维持 Sandbox `unsupported`，不增加兼容层。

本研究只使用 Microsandbox 官方 repository、docs、source 和 Releases；没有安装 runtime、
没有修改 Loom Runtime 代码，也没有改变 #32 的状态。
