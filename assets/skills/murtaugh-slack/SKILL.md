---
name: murtaugh-slack
description: Everything Slack on Murtaugh — post/update/read messages and reactions, ask the user and block for the answer, compose Block Kit, read/edit canvas documents, and (operator) wire reactive buttons and link previews. Read only the row your task needs.
requires: [slack, ask, present_plan, manage]
templated: true
files:
  reference/formatting.md:     { requires: [slack],            summary: "the two dialects — standard Markdown vs Slack mrkdwn, and mentions" }
  reference/messaging.md:      { requires: [slack],            summary: "post, update, and read messages & reactions" }
  reference/canvas.md:         { requires: [slack],            summary: "read & edit a Slack canvas document (Markdown in and out)" }
  reference/asking.md:         { requires: [ask, present_plan], summary: "ask the user a question / get plan sign-off and block for the answer" }
  reference/blocks.md:         { requires: [slack, manage],    summary: "compose Block Kit (sections, actions, plan, card)" }
  reference/automations.md:    { requires: [manage],           summary: "conventions for scheduled clock-tick scripts that post to Slack" }
  reference/workflow-rules.md: { requires: [manage],           summary: "wire what happens on a button click via cfg workflow-rule set" }
  reference/unfurl.md:         { requires: [manage],           summary: "turn posted links into rich previews via cfg unfurl-rule set" }
  examples/unfurl/:            { requires: [manage] }
---

# Skill: Murtaugh Slack

Everything Slack flows through Murtaugh — posting and reading messages, asking the
user, composing Block Kit, and (for operators) wiring reactive rules. Read only
the file your task needs:

{{FILES}}

> If a task needs something not listed above, it's often an operator **config
> change**. Config now lives in the config database and is changed with
> `murtaugh cfg …` (chat routing, access, agents, jobs, workflow/unfurl rules) —
> those commands re-validate the whole config and roll back a bad change, so you
> **may** make the change that way (e.g. `cfg chat set`, `cfg access set`,
> `cfg workflow-rule set --from-file`, `cfg unfurl-rule set --from-file`), then
> note that a **gateway restart** is needed to apply it. The exceptions are
> **secrets and `config.yaml`** (Slack tokens / provider keys in `.env`, the
> `oauth:`/`database:` blocks) — defer those to the operator.

## Guidelines (defaults — follow unless the user says otherwise)

- **One message per entity** — post once, then `update-msg` in place against the
  stored `ts`; use a thread reply for follow-ups. Don't repost on every tick.
- **No secrets in a message** — never put tokens or PII in `action_id`,
  `block_id`, or a button `value`; they travel inside the message and are
  readable by anyone who can see it.
- **A channel post is visible to every member of the channel.** The allowlist
  gates *who can act*, not *who can see* — for single-recipient delivery use an
  ephemeral message or a DM.
- **To *ask*, don't post.** A `send-msg` is fire-and-forget; to get an answer
  back use `ask` / `present_plan`, which block the turn.
