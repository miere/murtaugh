# Asking: the agent puts a decision in front of you

A native agent doesn't just stream a reply — it can pause mid-turn and put a
**decision in front of the user in the same thread**, then block until they
respond. This is the human-in-the-loop surface. Use these instead of hand-wiring
a button + `workflow-rules` (see `workflow-rules.md`): they go through Murtaugh's
native interaction broker, post the buttons/modal, **block the turn** until the
user answers, and hand the choice straight back to the agent — no rule wiring,
and no secrets travelling in a button `value`.

> Enabling vs using. Whether an agent *has* these tools is set in `agents.yaml`
> (`tools: [… ask, present_plan]`) and the terminal gate's `approval:` block —
> see the `murtaugh-agents` skill. This file is how the agent *uses* them.

## The three surfaces

- **`ask`** — the agent asks a question and **WAITS** for the answer instead of
  assuming one. A single quick choice renders as clickable Slack buttons
  (`question` + `options`); several questions at once, or multi-select / free-text
  answers, render as a form behind an *Answer* button (`questions`). The agent
  gets back exactly what was chosen or typed — or a note that the user didn't
  respond (which it must **not** treat as approval).
- **`present_plan`** — the agent lays out a concrete plan and **WAITS** for
  sign-off via **Proceed / Revise / Cancel** buttons before starting multi-step
  work. *Proceed* greenlights it as presented; *Revise* sends it back for changes
  (it doesn't start); *Cancel* stops it. (Distinct from the `plan` *block* in
  `blocks.md`, which is just a static checklist with no sign-off.)
- **The terminal approval gate** — when a native agent wants to run a
  side-effecting shell command, Murtaugh posts it to the thread with
  **Approve / Deny** buttons and waits. Whether a given command is gated depends
  on the agent's `approval:` block (`allowlist` auto-runs recognized read-only
  commands and asks for the rest; `prompt` asks for every command; `off` never
  asks — see the `murtaugh-agents` skill). The gate is only active in **live
  chat** — scheduled jobs and delegated agents are never gated.

All three only work **inside a Slack conversation**: they target the same thread
the agent is talking in. Outside a chat turn (a CLI/MCP call, a headless job) the
interactive tools return an error rather than blocking.

## Why this exists — the consent posture

These tools encode Murtaugh's house rule that **consent is explicit or it isn't
consent**. Before anything that changes code, runs a side-effecting command, or
spans several steps, the agent says what it intends and waits for an explicit
go-ahead; read-only information-gathering is the only thing it does on its own.
Silence, a non-answer, or a changed subject are **not** approval — if the agent
asked a question (via `ask`/`present_plan`), it does not get to answer it by
acting. And **approval covers only what was agreed**: if the approved path fails
or needs a workaround — a different command, installing something — that's a new
decision and the agent stops to ask again.
