# Claude Max OAuth 本地开发验证流程

状态：本地开发研究；不改变生产认证或发布口径
日期：2026-08-13
范围：同一 Owner 在自己的 macOS 上，使用 Claude Pro/Max 登录验证 Claude Agent SDK Runtime

## 结论

Claude Agent SDK 可以在个人、本地开发中使用 Owner 自己的 Claude Pro/Max 订阅 OAuth。
CodexLoom 当前拒绝 OAuth 是自身的产品认证策略，不是 SDK 的技术限制。正确边界应当是：

- 允许同一 Owner 在显式 opt-in 的本地开发 smoke 中复用自己的 Claude Code 登录；
- 继续禁止在正常产品配置中向其他用户提供 Claude.ai 登录，或代用户路由订阅凭据；
- 不把本地 OAuth smoke 计入 API key/cloud/gateway 的生产认证或四平台发布结果。

Anthropic 的当前说明支持这一区分：

- Pro/Max 的正常个人用量包括 Claude Code 和 Agent SDK；Agent SDK、`claude -p` 与第三方 App
  当前仍可消耗订阅额度：
  [Use the Claude Agent SDK with your Claude plan](https://support.claude.com/en/articles/15036540-use-the-claude-agent-sdk-with-your-claude-plan)
- Claude Code 认证顺序包括 `CLAUDE_CODE_OAUTH_TOKEN` 和 `/login` 保存的订阅 OAuth；macOS
  登录凭据保存在加密 Keychain：
  [Authentication](https://code.claude.com/docs/en/authentication)
- 商业边界禁止第三方开发者向自己的产品用户提供 Claude.ai 登录，或代表用户路由订阅凭据；
  它不等于禁止开发者本人在本机使用自己的订阅：
  [Legal and compliance](https://code.claude.com/docs/en/legal-and-compliance)、
  [Agent SDK overview](https://code.claude.com/docs/en/agent-sdk/overview)

这是对公开规则和 CodexLoom 的 local-first、single-owner 边界的解释，不是 Anthropic 对
CodexLoom 的专项法律批准。

## 已完成的本机探针

在仓库外的临时目录中对固定版本 `@anthropic-ai/claude-agent-sdk@0.3.228` 做了只读探针。
该包内嵌 Claude Code `2.1.228`，与当前 managed generation 完全一致。

结果：

1. `query(...).accountInfo()` 可以通过默认 Claude config/Keychain 看到本机 first-party 身份
   通道，不需要 `ANTHROPIC_API_KEY` 或 `CLAUDE_CODE_OAUTH_TOKEN`。
2. `settingSources: []` 仍能读取身份，证明认证存储与 User/Project/Local settings layer 分离。
3. 把 `CLAUDE_CONFIG_DIR` 指向全新的临时目录后身份消失，证明复用点确实是 Owner 的默认
   Claude 本机登录，而不是项目设置。
4. SDK 类型中的 `AccountInfo.apiKeySource` 包含 `oauth`；运行时还提供 OAuth 登录控制方法。
5. 首次探针时登录已经过期。`claude auth status --json` 返回 `loggedIn:false`；一次
   `tools:[]`、`maxTurns:1`、`haiku` 的最小 Turn 在产生 token 或费用前失败，结果为 0 token、
   `$0`，Claude Code `2.1.228` 的 init evidence 为 `apiKeySource:"none"`。
6. Owner 于同日重新登录后再次验证成功：CLI 报告 `loggedIn:true`、`authMethod:"claude.ai"`、
   `subscriptionType:"max"`；固定 SDK/Claude Code `0.3.228/2.1.228` 在 `tools:[]`、
   `maxTurns:1`、`haiku`、`maxBudgetUsd:0.05`、`persistSession:false` 下准确返回约定文本，结果
   `success`，实际费用 `$0.00123`。
7. 成功登录时 SDK `accountInfo()` 返回 `apiProvider:"firstParty"`、
   `subscriptionType:"Claude Max"`，但 `apiKeySource` 和 `tokenSource` 都是 null；init 中的
   `apiKeySource` 仍为 `"none"`。因此 `apiKeySource === "oauth"` 不是这个固定版本上可靠的
   Max 判据。

因此，流程可行，但真实 E2E 的第一个前置步骤是重新登录：

```sh
claude auth login --claudeai
claude auth status --json
```

不要根据 Keychain 文件存在与否判断登录有效；必须看到 `loggedIn:true`，随后再用真实最小 Turn
确认 refresh 和推理都成功。

## 当前 Loom 的阻断点

当前实现把产品发布约束直接应用到了所有本地运行：

- `internal/hub/runtime_configuration.go` 只声明并接受 `console/api_key`、cloud 与 gateway；
- `internal/claudegen/assets/bridge.mjs` 只恢复被选认证源对应的环境变量，并永不恢复
  `CLAUDE_CODE_OAUTH_TOKEN`；
- bridge 自建的 `verifyAuthentication()` 对 `console/api_key` 明确拒绝
  `apiKeySource === "oauth"`。

Bridge 已保留 `HOME` 和默认 `CLAUDE_CONFIG_DIR` 行为。因此，要复用同一 Owner 的 Keychain
登录，不需要读取、复制或传递 token；只需要增加一个诚实的、开发专用认证来源，并停止把它
误判为 API key。

## 推荐的临时验证流程

### 阶段 A：零产品改动的认证预检

先在 `mktemp -d` 中安装固定 SDK `0.3.228`，不传任何 auth env，使用：

```js
query({
  prompt,
  options: {
    persistSession: false,
    settingSources: [],
    tools: [],
    model: "haiku",
    maxTurns: 1
  }
})
```

先读安全 evidence，要求 `accountInfo()` 表明 `apiProvider === "firstParty"` 且
`subscriptionType` 是已识别的 Claude 订阅；随后只发送“回复固定字符串”的一个最小 Turn，以
实际成功结果证明 credential 可刷新。不要输出 email、organization、token 或 OAuth URL。
此阶段证明固定 SDK/CLI 能复用本机登录，不证明 Loom 集成。

### 阶段 B：增加明确的本机 Owner 认证源

增加显式认证源 `subscription/claude_ai`，UI 文案为
`Local Claude.ai login (OAuth)`。CodexLoom 是 local-first、single-owner 应用，因此该来源可在
WebUI 中直接可见；选择它只是声明复用当前 Owner 的现有本机登录，不提供 OAuth 登录流程，也
不把订阅认证升级为 production-ready 认证。它必须满足：

1. 不成为默认值；旧配置仍迁移为原有 `console/api_key`，不会静默换到订阅；
2. 只复用同 UID、默认 Claude config/Keychain，不读取、复制、记录或返回 token；
3. 不恢复 `CLAUDE_CODE_OAUTH_TOKEN`，也不新增 Claude.ai 登录 UI；
4. 清除或拒绝优先级更高的 API key、gateway 和 cloud auth，避免实际来源与声明不一致；
5. 验证不能只要求 `apiKeySource === "oauth"`：固定版本在有效 Max 登录下仍返回 null/none。
   最小可靠组合是 first-party provider、明确的 subscription type，以及一次无工具低预算
   preflight Turn 成功；仅有 `firstParty + null/none` 不足以接受，因为过期登录也会出现该
   provider；
6. 现有 API key/cloud/gateway 契约、release smoke 和 `productionReady:false` 均不变。

不要用 `console/api_key` 配置却在 bridge 内悄悄放过 OAuth。这样虽然改动更小，但配置 evidence
会说谎，也容易把开发例外泄漏到发布路径。

### 阶段 C：隔离的真实 Loom E2E

现有 `loom dev canary` 是被动只读模式，不能创建 Agent 或发送 Turn。真实浏览器 E2E 应使用
当前构建启动一个可写的临时 data dir，而不是 Owner 的 `~/.codex-loom`：

```sh
TEST_ROOT="$(mktemp -d)"
./bin/codex-loom -data "$TEST_ROOT/data" -port 0
```

实际执行前还需安装、验证并激活当前 exact managed generation；安装条款仍必须由 Owner 显式
接受，不应由认证源选择隐式代替。测试 Agent 使用新的空工作目录、`ApprovalPolicy=never`、
`tools=[]`、`maxTurns=1`、`maxBudgetUSD=0.05`、`persistSession=false`。

浏览器 E2E 顺序：

1. System 页确认 generation 为 `0.3.228 / 2.1.228` 且 `productionReady:false`；
2. 创建临时 Claude Agent，认证源选择 `Claude subscription / Local Claude.ai login (OAuth)`；
3. 发送一个只要求固定文本、不使用工具、不读写文件的 Turn；
4. 验证 owner message、running、单一 assistant final、usage 和 idle；
5. 再发送一个长纯文本 Turn 并立即 Stop，验证 interrupt、terminal 与回到 idle；
6. 重启这个临时 Hub，验证 Canonical Ledger 中 completed/interrupted Turn 仍在，且旧 bridge
   process tree 已清理；
7. 停止 Hub 后删除临时 data/workspace。

可直接复用 `TestClaudeRuntimeManagedGenerationRealSmoke` 已有的无工具、低预算、interrupt、
history reopen 和 process cleanup 断言，但应新增独立的 local-OAuth opt-in smoke；不要改变现有
只接受 `CLAUDE_REAL_API_KEY` 的发布认证测试。

## 最小测试矩阵

| 场景 | 预期 |
| --- | --- |
| 提交未知 subscription source | HTTP/Go normalize 拒绝 |
| 选择 `subscription/claude_ai`，Max 登录有效 | first-party + subscription evidence，真实 Turn 可执行 |
| 选择订阅，登录过期或 subscription evidence 缺失 | fail closed |
| 选择订阅，同时存在 API key/cloud env | 不恢复这些变量，不能静默换源 |
| OAuth account evidence 返回身份字段 | 只保留 category/source/validation，不泄露身份 |
| 发布 smoke / CI | 仍只接受 product-safe API key/cloud/gateway |
| E2E 完成 | 临时数据删除、productionReady 仍为 false |

## 决策

建议实现独立的本机 Owner `subscription/claude_ai` 认证源，再进行一次固定 generation 的本机 E2E。
这既恢复了官方允许的 Max 本地开发体验，也保持了生产分发和认证证据的边界。当前唯一需要
Owner 先完成的外部步骤是 `claude auth login --claudeai`；其余实现和验证可以自动化。
