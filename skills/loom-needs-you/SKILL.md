---
name: loom-needs-you
description: Ask the Owner for a durable fact, choice, review, or authorization through CodexLoom. Use when current work truly cannot continue without Owner input and must resume later in the same long-lived Agent Thread.
---

# Needs You

Use a Human Request only when the current work genuinely needs a fact, choice, review, or authorization that the Agent should not infer safely.

Do not create a request merely to report progress, seek reassurance, or delegate a low-risk judgment you can make within your Scope. A required request blocks the named workstream, not your entire Domain.

## Before ending a Turn

If the current work truly cannot continue without Owner input, you **must successfully create a required Needs You before ending the Turn**. Saying that you are blocked or asking a question only in the final response does not notify the Owner and is not a substitute for `loom ask-user`. CodexLoom does not infer or create a Needs You from final-response text.

Route other waits to their own durable mechanism:

- An external fact or provider state change: create a Trigger; use Topic waiting too when a Topic needs the shared coordination state.
- A result from another Agent: send a required Message; use Topic waiting too when the dependency belongs to a Topic.
- A calendar time: create a Schedule.
- Runtime permission for a tool call: use the tool approval flow. A business authorization only the Owner can grant remains Needs You.

After establishing the correct wait, end the Turn normally. Continue any independent work first when useful; do not create Needs You for a dependency that does not actually block the named work.

## Create a request

Use the fields consistently:

- `question`: a short, single-line title stating exactly what the Owner must answer.
- `context`: longer Markdown with the background, options, and impact the Owner needs to decide.
- `blockedWork`: short plain text naming the work that cannot continue before the answer. The CLI flag is `--blocks`.

Pass actual newline characters when `context` needs multiple lines. Do not type literal `\n` text; CodexLoom preserves the value as supplied and does not decode those characters into line breaks.

```bash
loom ask-user --from <your-agent-name> --question "Which release window should I use?" \
  --context "## Background

The verified build is ready to publish.

## Options and impact

- **Today:** delivers sooner, during peak traffic.
- **Monday:** lowers traffic risk, but delays availability." \
  --blocks "Publish the verified release" \
  --option "Today::Deliver sooner during peak traffic" \
  --option "Monday::Lower traffic risk but delay availability"
```

Use `--optional` when the answer would improve the work but does not block it. Required is the default.

Keep each request at one decision layer. Offer two or three mutually exclusive options when that makes the decision easier, put the recommended option first, and explain the tradeoff without assuming the human already has your local context. Free-form questions are valid when options would be artificial.

After the command succeeds, end the current Turn normally. Do not sleep, poll, repeatedly inspect request state, or keep the Turn alive. CodexLoom persists the request and will resume this same Agent Thread in a new Turn with a linked `<human_input_response>` when the Owner answers.

Do not use Codex's native `request_user_input` tool for this workflow. That tool suspends one active Turn; a CodexLoom Human Request is durable across long waits and service restarts.

When the answer arrives, use it to continue the related work if it is still relevant. Do not ask the same question again unless the answer is materially ambiguous.
