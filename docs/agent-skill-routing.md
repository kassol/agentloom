# Runtime-native Resources

Agent Inspector 的 **Resources** 视图只展示所选 Runtime 自己报告的资源。
这些资源具有 **Runtime-specific semantics**：名称、路径、scope 与 source 可用于检查
当前 Runtime，但不承诺能跨 Runtime 移植。

## Inventory

`resource_inventory` 是可选 Runtime Contract capability：

- Codex adapter 针对 Agent 的 CWD 直接调用 app-server `skills/list`。它不从
  CodexLoom 的全局 CodexHost catalog 投影 per-Agent inventory。
- Pi adapter 调用 Pi RPC `get_commands`，将原生来源分类为 `skill`、`prompt`、
  `extension`，并过滤 built-in/TUI command。
- 不支持 inventory 的 Runtime 返回明确的 reason 与 alternative；消费者不按
  `runtimeKind` 猜测能力。

全局 Codex Skill 安装和 `loom skills reload` 仍是 CodexHost integration，和
per-Agent Runtime inventory 分离。

## Policy

`resource_policy` 是比 inventory 更窄的可选 capability。目前 Codex 支持按 Agent
禁用绝对 `SKILL.md` 路径；Pi 明确不支持，CodexLoom 不写 `~/.pi`，也不伪装成
已经 hot-reload。Pi 的 reason/alternative 会引导用户使用 Pi 原生设置。

Codex 的 desired policy 存在：

```text
<data-dir>/agent-skill-config.json
```

CodexLoom 把它编译进 `thread/start` 或 cold `thread/resume` 的 SessionFlags：

```json
{"skills":{"config":[{"path":"/absolute/path/to/SKILL.md","enabled":false}]}}
```

写入顺序是 native-first：只有 cold binding lifecycle 原生确认了精确 policy，且
adapter 返回 `effective=true` 与 native evidence 后，CodexLoom 才持久化。原生应用、
持久化或最终竞态检查失败时，内存 desired state 会回滚，并 fence 已受影响的 Runtime。
已加载 binding 无法安全应用 SessionFlags，因此在持久化前返回 `409`，要求重启后重新
打开 Resources，而不是宣称 hot apply 成功。

重启产生新的 Runtime generation。持久化 policy 最初只表示 desired state；binding
重新 start/resume 后必须再次取得精确 native evidence，才会显示为 effective。

## HTTP and concurrency

```text
GET   /api/agents/{agent}/skills
PATCH /api/agents/{agent}/skills/config
```

GET 返回一个原子 `resources` snapshot，包含 inventory、policy、evidence 和
`revision`。PATCH 必须提交该 snapshot 的 revision：

```json
{
  "path": "/absolute/path/to/SKILL.md",
  "enabled": false,
  "expectedRevision": "resources:..."
}
```

stale revision、binding、CWD 或 Host generation，以及 running/loaded/edge Agent，
都会在持久化前拒绝。成功后发布 `loom/runtime-resources-updated` SSE event；CLI 与
Inspector 都重新读取完整 snapshot，而不是合并局部猜测。

## CLI

```sh
loom skills agent AGENT
loom skills agent AGENT disable /absolute/path/to/SKILL.md
loom skills agent AGENT enable /absolute/path/to/SKILL.md
```

CLI 和 Web 使用同一个 typed snapshot，不包含 `runtimeKind` 分支。Native binding
reference 和内部 diagnostics 不出现在该公开响应中。
