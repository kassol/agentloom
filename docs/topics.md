# Topics：跨 Turn、跨 Agent 的持续事项

Topic（中文界面称“事项”）是 CodexLoom 中一条有完成边界的薄共享协调记录。它解决的是：一件工作跨越多个 Turn、数天和多个长期 Agent 后，Owner 与参与 Agent 仍能快速知道“这是什么、谁负责、现在到哪、在等什么、证据在哪里”。

Topic 不执行工作。专业过程继续发生在每个 Agent 自己的 Codex Thread 中；Topic 只保存跨域所需的当前 Brief、责任、等待条件、关键证据和因果活动。

## 产品边界

- 一个 Topic 只有一个 Responsible Agent，可以有多个 Participants。
- Responsible 维护 purpose、completion boundary、versioned brief、waiting_on、参与关系和最终收口。
- Participant 只承担明确的 topic-scoped responsibility，在自己的 Thread 工作，并把结果、限制和上下文缺口返回 Responsible。
- Owner 在 Topic 中的普通输入始终发给 Responsible。Responsible 理解整体语境后再通过带 Topic ID 的 Agent Message 路由工作。
- 只有 Responsible 发布的阶段结果进入 Owner 的 Results Ready。
- Topic 没有自己的 Codex Thread、Turn、busy 状态或 reservation。
- Topic 不是 Goal、共享 Session、群聊、任务树、看板、项目管理系统或外部事实的第二真相源。

`active / waiting / resolved / archived` 只表示协调记录状态。`resolved` 不会自动结束 Goal、取消 Trigger、关闭 Message 或 Needs You，也不会 interrupt 任何 Turn。

## 什么时候使用

适合创建 Topic：

- 工作有明确阶段或完成边界；
- 需要跨多个 Turn 或数天恢复；
- 多个 Agent 需要稳定共享当前结论，但不应共享彼此完整 Thread；
- 工作在等待 GitHub、部署、人工授权或其他外部条件；
- Owner 不应靠重新阅读几条 Agent Thread 拼接全局状态。

不适合创建 Topic：

- 一次 Turn 能完成的任务；
- 一个 Agent 的永久 Domain 或 Profile；
- 仅需复用方法的 Skill；
- 单个 Agent 的长程执行生命周期，这属于 Goal；
- 没有完成边界的永久“项目名”或“业务域”。

## 创建与恢复

Web 中可以从 Agent 输入框旁的 **Track this work as a Topic** 创建，当前 Agent 会预填为 Responsible。创建时只要求用户确认最小组织假设：标题、purpose、completion boundary、Responsible 和当前必要的 Participants。

CLI 示例：

```sh
loom topic create \
  --title "Parall Clip 0.2.0｜staging / release 收口" \
  --responsible parall-dev-lead \
  --purpose "协调本阶段跨 Lead、Edge 与 Platform 的验证和交付。" \
  --completion "Responsible 发布经核验的阶段结果并说明后续边界。" \
  --summary "当前候选与环境事实需要重新核验。" \
  --participant 'parall-edge-dev::负责 packaged client 的真实验收。' \
  --participant 'parall-platform-dev::负责部署与环境证据。'
```

恢复工作时，Agent 收到的 `<loom_topic_context>` 只包含：

- Topic ID、标题、purpose 与 completion boundary；
- 当前 versioned brief；
- waiting_on；
- 自己是 Responsible 还是 Participant，以及自己的责任；
- 当自己是 Responsible 时，当前 Participants 及其 bounded responsibilities；普通 Participant 不会收到其他 Participant 的责任；
- 少量关键 links；
- 自该 Agent 上次收到 Topic context 之后的有界 activity delta。

它不会携带其他 Agent 的完整 Session 或 Topic 全历史。Agent 仍需读取当前 provider、代码、Artifact 或自己的 Thread 来核验事实。

## 协作与因果关联

Responsible 派发 topic-scoped work：

```sh
loom msg parall-edge-dev --from parall-dev-lead --topic tpc_xxx \
  --subject "核验当前 packaged candidate" \
  --body "请读取当前候选并返回证据、限制或阻塞条件。"
```

Participant 用相同 Topic ID 向 Responsible 返回结果。Message reply 会继承原 Topic，不能把 Topic 工作发送给未加入的 Agent。

以下入口支持直接关联 Topic：

```sh
loom msg AGENT --from SELF --topic tpc_xxx ...
loom ask-user --from SELF --topic tpc_xxx ...
loom trigger add ... --from SELF --topic tpc_xxx ...
loom topic link tpc_xxx artifact art_xxx --from SELF --label "验收报告"
```

Message、Needs You、Trigger 与其后续 Turn 会沿确定因果继承 Topic。既有 Goal（建议使用 `<thread-id>@<goal-created-at>` 标识具体 Goal 版本）、外部 Inbox / Outbox、PR、Artifact 或其他证据使用 `topic link` 显式加入。不要自动回填整个历史，也不要把旧摘要当权威事实。

## Brief、等待与结果

Responsible 使用乐观版本号更新 Brief，避免多个恢复 Turn 静默覆盖：

```sh
loom topic get tpc_xxx
loom topic update tpc_xxx --from parall-dev-lead --if-version 4 \
  --summary "跨域结论" \
  --state "当前已核验事实" \
  --next "下一项明确动作" \
  --limitations "仍未确认的边界"
```

等待外部条件：

```sh
loom topic update tpc_xxx --from parall-dev-lead \
  --waiting "等待目标 PR 进入 main。" \
  --waiting-kind github-pr \
  --waiting-ref parall-hq/parall-mono#1970 \
  --resume-action "重新读取 main 并核验预期契约。"
```

如果创建 Trigger，应同时传 `--topic tpc_xxx`。Trigger 唤醒后只是要求 Agent 回源核验；当前事实满足真实条件后，Responsible 才清除 waiting 并更新 Brief。

Responsible 发布阶段结果：

```sh
loom topic update tpc_xxx --from parall-dev-lead --if-version 5 \
  --summary "本阶段核验结果" --state "Ready for Owner review" --result
loom topic resolve tpc_xxx --from parall-dev-lead
```

Owner 打开结果后，Loom 记录已读版本。普通 Participant 输出不会直接进入 Results Ready，避免内部协作量变成 Owner 注意力负担。

## Owner 下钻与 Turn 干预

范围、优先级、验收条件和分工变化应通过 `topic send` 发给 Responsible：

```sh
loom topic send tpc_xxx "验收范围增加 iPad packaged smoke，请重新拆解并同步 Participants。"
```

当问题只发生在某个 Participant 正在执行的具体 Turn，Owner 可以从 Topic 的 **Active work** 打开该 Agent Thread，并对精确 active Turn 做指导或终止：

```sh
loom topic intervene tpc_xxx --agent parall-edge-dev \
  --action steer --text "请核验当前 HEAD，不要继续使用旧候选。"

loom topic intervene tpc_xxx --agent parall-edge-dev \
  --action interrupt --reason "候选已被替代。"
```

底层控制对象仍是 Agent Session 中的 active Turn。Topic 只记录 intervention event，并通知 Responsible。干预不会自动暂停或结束 Topic，也不会自动重新分派工作；若局部纠正改变了整体范围，Responsible 必须重新规划并更新 Brief。

## Web 与 CLI 入口

- Web 左侧 **Topics**：按 For you、Active、Waiting、Resolved、All 查看事项。
- `For you`：仅包含关联的开放 Needs You 或 Responsible 已发布但 Owner 尚未读取的结果。
- Topic 详情：Brief、waiting、Participants、active Turns、shared activity 和 evidence。Owner 在创建时确认初始 Participants；创建后的协调变化通过底部输入框交给 Responsible。
- `loom topic list [--status STATUS] [--agent AGENT]`
- `loom topic get TOPIC_ID`
- `loom topic send TOPIC_ID ...`
- `loom topic update|resolve|archive ...`
- `loom topic participant add|remove ...`
- `loom topic link ...`
- `loom topic intervene ...`
- `loom topic read TOPIC_ID`

## 第一版限制

- 一个 Agent 仍只有一个主要 Codex Thread；Topic 不创建 topic-scoped Thread。
- Brief、waiting 和创建后的 Participants 由 Responsible 显式维护；Owner 通过 Topic 输入提出范围和分工变化。系统不自动总结所有 Agent 历史。
- Topic 不自动判断组织拆分、任务优先级或完成质量。
- Owner intervention 只作用于当前精确 active Turn；没有 active Topic Turn 时会拒绝。
- Topic 不提供子任务、依赖图、排期、工时、自动派单或项目看板。
