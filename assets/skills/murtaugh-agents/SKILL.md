---
name: murtaugh-agents
description: Configure Murtaugh's agent chat with `murtaugh cfg agent` (stored in the config database) — native, claude_code, and ACP profiles, which tools an agent may call, the defaults block, and which agent answers DMs vs each channel.
requires: [manage]
files:
  reference/agents-yaml.md: { requires: [manage], summary: "define agents via cfg agent (provider/model/tools/approval, the defaults block) or a command-based ACP/claude_code agent" }
  reference/routing.md:      { requires: [manage], summary: "wire which agent answers DMs vs each channel" }
  reference/interaction.md:  { requires: [manage], summary: "chat triggers, streaming, interrupts/stop, warmup (how chat behaves)" }
---

# Skill: Murtaugh Agent Chat

Murtaugh can route Slack **DMs and @-mentions to an AI agent**, stream the reply
back into the thread, and let a follow-up interrupt an in-flight response. Use
this whenever a task involves configuring which agent answers, tuning
streaming/timeouts, or understanding the `/chat` and `/stop` behavior.

## Three agent backends

Agents live in the **config database**, managed with `murtaugh cfg agent …` (also
`cfg.*` over MCP). The backend is the agent's `--type`:

- **native** (the **default**) — Murtaugh runs the LLM loop in-process and
  talks to a provider (`gemini` / `anthropic` / `openai`, plus compatible third
  parties via `base_url`) directly. A native agent authenticates with an API key
  read from `~/.config/murtaugh/.env` — its profile names the variable via
  `api_key_env`, and the key value lives only in that `.env`, never in the store.
- **claude_code** — Murtaugh drives the `claude` CLI directly over its
  stream-json protocol (one process per conversation). Type `claude_code` with a
  `--command` naming the `claude` binary and an optional `--model`. Like ACP, it
  reaches Murtaugh's own tools (`slack.*`, `jobs`, …) through the tool bridge,
  gated by the same `approval` policy.
- **ACP** (legacy) — Murtaugh drives an external agent process over ACP (the
  Agent Client Protocol). Type `acp` with a `--command` (+ repeatable `--arg`).

## Turning it on

1. **Define an agent** — `cfg agent create`. For a **native** agent pass
   `--provider` + `--model` + `--api-key-env`; for an **acp**/**claude_code** agent
   pass `--command` (+ `--arg …`). → `reference/agents-yaml.md`
2. **Turn chat on** — `cfg chat set --enabled true --default-agent <name>` (and
   optionally `--dm-agent <name>`). → `reference/routing.md`

Then **restart the gateway** (config loads once). With chat disabled (the
default), DMs and mentions are ignored. Note that `chat.enabled` gates **only** the
Slack chat surface (DMs + @mentions); agent delegation (jobs, workflow rules,
unfurls) runs whenever the target agent is defined, regardless of this flag.

```bash
murtaugh cfg agent create --name default --type native \
  --provider gemini --model gemini-2.5-pro --api-key-env GEMINI_API_KEY \
  --tools files --tools terminal --tools skills --tools ask --tools present_plan
murtaugh cfg chat set --enabled true --default-agent default
```

> The runtime tuning **`defaults` block** (grouped by `session`, `rendering`,
> `acp`, `approval`) governs native, claude_code, and ACP agents alike. It's
> stored config — inspect it with `cfg defaults show`; per-agent overrides are
> `cfg agent` flags (`--progress-display`, `--approval-terminal`, …). →
> `reference/agents-yaml.md`

## The flow (mental model)

1. A user **DMs** the bot or **@-mentions** it in a channel (or runs
   `/murtaugh chat …`).
2. Murtaugh **resolves which agent** handles it (DM vs channel routing). →
   `reference/routing.md`
3. The agent's reply **streams** into the thread, updated as chunks arrive. A
   native agent may pause mid-turn to **ask you** (`ask`), get **sign-off on a
   plan** (`present_plan`), or seek **approval to run a command** (the terminal
   gate), waiting for your click before continuing. → `reference/interaction.md`
4. A new message on the same conversation **interrupts** the previous reply (if
   the agent supports it); `/stop` cancels on demand. → `reference/interaction.md`

## Read the right file (don't load everything)

| When you're… | Read |
|---|---|
| Defining agents (native `provider`/`model`/`tools`/`approval` via `cfg agent`, or a command-based ACP/claude_code agent) and the `defaults` block (timeouts, streaming, sessions) | `reference/agents-yaml.md` |
| Wiring which agent answers DMs vs each channel | `reference/routing.md` |
| Understanding `/chat`, `/stop`, interrupts, streaming, and warmup (how chat behaves) | `reference/interaction.md` |
| How an agent *uses* `ask` / `present_plan` / the approval gate (vs *enabling* them here) | the `murtaugh-slack` skill (`reference/asking.md`) |
| Wanting worked `cfg agent` / `cfg chat` examples | `examples/` |

## Global guidelines (defaults — follow unless the user says otherwise)

- **Native is the default backend.** The backend is the agent's `--type`:
  `native` (default), `acp`, or `claude_code`. Native needs `--provider` +
  `--model` + `--api-key-env`; the command-based types (`acp`/`claude_code`) need
  `--command`. Set them with `cfg agent create` / `cfg agent update`.
- **Native agents authenticate via `~/.config/murtaugh/.env`.** The profile names
  the variable with `--api-key-env`; write the value there with `setup_env` (see
  the `murtaugh-setup` skill). The key never goes in the config store.
- **`cfg chat set --default-agent` is required when chat is enabled**, and every
  routed agent name must exist (`cfg agent list`), or the gateway refuses to start
  (fail-closed).
- **Per-channel routing is keyed by channel ID** (e.g. `C0ENG1`) or a channel-name
  glob (`feature-*`), not a `#name`; each entry picks an agent and/or a
  thread-reply setting. → `reference/routing.md`
- **`--reply-on-thread` (`cfg chat set`) picks thread vs in-channel replies** —
  default `true` (threaded). `false` replies directly in the channel and treats it
  as one rolling conversation.
- **Leave `interruptible` unset** (ACP only) and let Murtaugh probe the agent at
  startup; only pin it (`true`/`false`) when the probe is wrong or you want to
  skip it. Native agents don't probe.
- An agent's **`workdir` defaults to the workspace** (`~/.config/murtaugh`) when
  unset, so it starts where the bundled skills/templates live.
