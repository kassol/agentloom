---
name: loom-needs-you
description: Ask the human operator for a durable decision, missing fact, or authorization through CodexLoom. Use when work genuinely needs human input and should resume later in the same long-lived Agent Thread.
---

# Needs You

Use a Human Request only when the current work genuinely needs a decision, fact, preference, review, or authorization that the Agent should not infer safely.

Do not create a request merely to report progress, seek reassurance, or delegate a low-risk judgment you can make within your Scope. A required request blocks the named workstream, not your entire Domain.

## Create a request

Use `question` as a short, single-line title. Put the long background and
explanation in `context`, which supports Markdown. Keep `blocks` as a short,
plain-text description of the work that is waiting on the answer. When a field
needs multiple lines, pass real newline characters; do not write the literal
two-character sequence `\n`.

```bash
loom ask-user --from <your-agent-name> --question "Which release window should I use?" \
  --context "Two windows are available.

**Tomorrow morning:** lower risk, with a complete support window.
**Tonight:** faster, but with a shorter support window." \
  --blocks "Arrange and publish the migration notice" \
  --option "Recommended choice::Impact or tradeoff" \
  --option "Alternative::Impact or tradeoff"
```

Use `--optional` when the answer would improve the work but does not block it. Required is the default.

Keep each request at one decision layer. Offer two or three mutually exclusive options when that makes the decision easier, put the recommended option first, and explain the tradeoff without assuming the human already has your local context. Free-form questions are valid when options would be artificial.

After the command succeeds, end the current Turn normally. Do not sleep, poll, repeatedly inspect request state, or keep the Turn alive. CodexLoom persists the request and will resume this same Agent Thread in a new Turn with a linked `<human_input_response>` when the human answers.

Do not use Codex's native `request_user_input` tool for this workflow. That tool suspends one active Turn; a CodexLoom Human Request is durable across long waits and service restarts.

When the answer arrives, use it to continue the related work if it is still relevant. Do not ask the same question again unless the answer is materially ambiguous.
