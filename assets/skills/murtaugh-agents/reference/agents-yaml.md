# Agent configuration via `cfg agent`

Agents live in the **config database**, not a YAML file. You manage them with
`murtaugh cfg agent …` (also `cfg.*` over MCP):

- `cfg agent create --name <n> --type <native|acp|claude_code> [flags]`
- `cfg agent update --name <n> [flags]`
- `cfg agent list` · `cfg agent show --name <n>` · `cfg agent delete --name <n>`

Every mutation re-validates the whole config and rolls back an invalid change;
**restart the gateway** to apply (config loads once). Two parts shape agent
behaviour: a **`defaults` block** of runtime tuning that applies to **all**
backends (viewable with `cfg defaults show`), and the **per-agent profiles** you
create. Each profile has one of three types:

- **native** (the **default**) — Murtaugh runs the LLM loop in-process: it talks
  to the provider directly and owns the conversation. This is what you almost
  always want.
- **claude_code** — Murtaugh drives the `claude` CLI directly over its
  stream-json protocol (one process per conversation), bypassing the ACP adapter.
  Command-based (`--command` + `--arg`), with an optional `--model`.
- **acp** (legacy) — Murtaugh drives an external agent process over ACP (the Agent
  Client Protocol). Command-based (`--command` + `--arg`).

The type is set explicitly with `--type` (native/acp/claude_code). Shared knobs
(`--workdir`, `--tools`, `--mcp-servers`, `--approval-*`, `--progress-display`,
`--export-skills-to-fs`) apply to every type; the provider knobs are native-only;
`--command`/`--arg`/`--env` are for the command-based types.

```bash
# Native (the default) — provider loop in-process.
murtaugh cfg agent create --name default --type native \
  --workdir '${HOME}/work' \
  --tools files --tools terminal --tools skills --tools slack \
  --tools jobs --tools ask --tools present_plan --tools attach \
  --mcp-servers vaultre \
  --approval-terminal allowlist --approval-allow kubectl --approval-allow "docker ps" \
  --provider gemini --model gemini-2.5-pro --api-key-env GEMINI_API_KEY \
  --system-prompt-file prompts/default.md \
  --max-turns 40 --context-limit 1000000 --compaction truncate --cache-retention 5m
```

## The `defaults` block — runtime tuning (all backends)

`defaults` is the runtime block, grouped by the concern each knob serves:
`session`, `rendering`, `acp`, and an optional `approval` global default. The
knobs apply to native, claude_code, and ACP agents alike. It's **seeded** with
tuned values at install; inspect the effective values with `cfg defaults show`.
The per-agent `--progress-display` and `--approval-*` flags override the matching
defaults for one agent; to retune the rest, `cfg export` the config, edit the
`defaults` block, and `cfg import` it back.

| Field | Default if omitted | Controls |
|---|---|---|
| `session.idle_timeout` | `30m` | How long an idle session is kept before teardown. |
| `session.request_timeout` | `10m` | Idle timeout per chat turn: max time with **no agent activity** before the turn is treated as stalled. Resets on every chunk/task update, so a long but progressing response is never cut off. |
| `session.long_running_tool_timeout` | `1h` | Total-duration cap on a single tool call (ACP agents). While a tool runs a heartbeat keeps the turn alive so `request_timeout` never trips; this bounds a genuinely wedged tool. Past it the turn fails naming the tool and the session is dropped. |
| `session.max_concurrent` | `100` | Concurrent session cap per agent. |
| `rendering.progress_display` | `simplified` | How tool/step progress renders while a turn streams: `simplified` (one small context-line message — "Reading file…" — that updates in place and resolves to "✓ Done thinking" when the turn ends) or `tasks` (the full multi-card plan woven into the reply). Override per agent with `--progress-display`. |
| `rendering.stream_min_chunk_chars` | `24` | Minimum characters before a chunk is flushed (avoids choppy edits). |
| `rendering.stream_append_interval` | `250ms` | How often buffered chunks are flushed to Slack. |
| `acp.startup_timeout` | `10s` | Budget for the agent warmup probe at daemon start (ACP agents). |
| `acp.cancel_grace_period` | `2s` | After asking the agent to cancel, how long to let trailing chunks flush before hard-cancelling. |
| `approval.terminal` | `allowlist` | Global default terminal-tool gate (see `--approval-terminal` below); per-agent `--approval-terminal` overrides it. |
| `approval.requests` | `ask` | Global default for how an ACP agent's own permission prompts are answered; per-agent `--approval-requests` overrides it. |
| `approval.keep_resolved` | `false` | Global default for whether a settled approval card stays in the thread; per-agent `--approval-keep-resolved` overrides it either way. |

> The chat on/off switch is **not** here — it's `cfg chat set --enabled`, which
> gates only the Slack chat surface (DMs + @mentions). Agent delegation (jobs,
> workflow rules, unfurls) runs whenever the target agent is defined.

## Native profiles (the default — `--type native`)

A native agent needs a **provider**, a **model**, and an **api-key-env**; the key
value itself never lives in the store (it comes from `~/.config/murtaugh/.env` —
see the `murtaugh-setup` skill's `setup_env`).

| Flag | Scope | Required | Meaning |
|---|---|---|---|
| `--provider` | native | yes | Provider family: `gemini`, `anthropic` (Anthropic-compatible), or `openai` (OpenAI-compatible). GLM/Z.ai, DeepSeek and Kimi ride the `anthropic` or `openai` family via `--base-url`. |
| `--model` | native + command types | yes | Provider model id (e.g. `gemini-2.5-pro`, `glm-4.6`). |
| `--api-key-env` | native | yes | Name of the `.env` variable holding the API key (e.g. `GEMINI_API_KEY`). The credential never appears in the store. |
| `--base-url` | native | no | Endpoint override for a compatible third party (Z.ai, DeepSeek, Kimi, self-hosted). Empty uses the provider default. |
| `--system-prompt` | native | no | Inline system prompt. Mutually exclusive with `--system-prompt-file`; when both are empty a built-in default is used. |
| `--system-prompt-file` | native | no | Path (resolved against the config dir) to a file holding the system prompt. Mutually exclusive with `--system-prompt`. |
| `--max-turns` | native | no | Tool-call iterations allowed in a single prompt. `0` uses a default. |
| `--context-limit` | native | no | Conversation token budget that drives compaction. `0` uses a per-provider-family default. |
| `--compaction` | native | no | How the conversation is kept within `context-limit`: `truncate` (default — drop oldest turn-groups) or `summarize` (LLM-compress the oldest groups, truncation as the fallback). |
| `--cache-retention` | native | no | Prompt-cache TTL: `5m` (default) or `1h`; `off`/`none` disables. Applied for Anthropic/OpenAI; Gemini caches a static prefix implicitly regardless. |

### Shared flags (every type)

| Flag | Required | Meaning |
|---|---|---|
| `--workdir` | no | Working directory that roots the files/terminal/attach tools. Defaults to the workspace (`~/.config/murtaugh`) when unset. |
| `--tools` | no | Allowlist of tool groups the agent may use — **repeat the flag**. Native groups (`files`, `terminal`, `attach`) plus registry namespaces (`skills`, `slack`, `jobs`, `ask`, `present_plan`, …) and the `manage` skills-visibility grant. `attach` lets the agent return a workspace file (report, image, export) to the user as a real downloadable upload, confined to `workdir` like the files tools. Empty means only the always-on set. |
| `--export-skills-to-fs` | no | Bundled (`murtaugh-*`) skills to write into this agent's `workdir` so a filesystem-discovering backend (e.g. a Claude Code agent) can load them. **Repeat the flag**; `all` exports every bundled skill. Empty (default) keeps the bundled skills in-binary only — readable solely through the gated `skills` tool, never by `files`/`terminal`. See below. |
| `--mcp-servers` | no | Names of `cfg mcp` entries to attach — **repeat the flag**. Each contributes its remote tools. |
| `--approval-terminal` / `--approval-allow` / `--approval-requests` | no | Human-approval gate for side-effecting tool calls (see below). Defaults to gating on (`allowlist`). |
| `--approval-keep-resolved` | no | Keep settled approval cards in the thread instead of clearing them a few seconds after the decision. Defaults to clearing. |
| `--progress-display` | no | Override `defaults.rendering.progress_display` for this agent (`simplified` / `tasks`). |

A workspace `AGENTS.md` (in the agent's `workdir`) is auto-loaded into the system
prompt as project guidelines — no config needed. The agent's **name and voice**
are conventionally set there.

### `--export-skills-to-fs` — making bundled skills filesystem-discoverable

The bundled `murtaugh-*` skills live **in the binary** and are served only
through the gated `skills` tool — they never touch disk, so the `files`/`terminal`
tools (and a shell) can't read them. That's the default and the secure posture.

Some backends discover skills from the **filesystem** instead (a Claude Code agent
reads `.claude/skills/`). For those, list the skills to mirror into the agent's
`workdir`:

```bash
murtaugh cfg agent create --name claude --type claude_code \
  --workdir '${HOME}/work/claude' \
  --export-skills-to-fs all \
  --command claude-code-acp
# or e.g. --export-skills-to-fs murtaugh-slack --export-skills-to-fs murtaugh-jobs
```

- The list is the **source of truth**, reconciled on every gateway start: listed
  skills are (re)written, and any `murtaugh-*` skill no longer listed is removed —
  so upgrades and edits self-heal. **Bespoke skills are never touched.**
- Names are validated **fail-closed**: an unknown skill name (anything other than a
  bundled `murtaugh-*` name or `all`) makes the gateway refuse to start.
- **Exporting a skill opts it out of the in-binary blind for that agent** — once on
  disk it's readable by that agent's file/terminal tools. Export only what a
  filesystem-discovering backend actually needs.
- Agents that **share a `workdir`** should agree on their export lists; they
  reconcile the same `.agents/skills`, so the last one wins. Give agents with
  different export needs distinct `workdir`s.

### The approval gate (`--approval-terminal` / `--approval-allow` / `--approval-requests`)

The unified, per-agent human-approval gate, with a matching global default under
`defaults.approval`:

- `--approval-terminal <allowlist|prompt|off>` — the native terminal gate.
- `--approval-allow <key>` — extra read-only command keys for the terminal gate
  (**repeat the flag**).
- `--approval-requests <ask|auto-allow|auto-deny>` — how an ACP agent's own
  permission prompts are answered.

**Native terminal gate** (`--approval-terminal` + `--approval-allow`) gates a
native agent's side-effecting **terminal** tool calls behind a human **Approve /
Deny** in Slack (the terminal is the only tool that can act outside the rooted
workspace — the files tools are confined to `workdir`):

- `allowlist` (**default**) — auto-run a recognized **read-only** command (`ls`,
  `cat`, `grep`, `git status`, …); **ask** for anything else (fail-closed).
- `prompt` — ask before **every** terminal command.
- `off` — never ask (the pre-gate behaviour).
- `--approval-allow` extends the built-in read-only allowlist with extra command
  keys: an argv0 (`kubectl`) or a `binary subcommand` pair (`"docker ps"`).

**ACP permission** (`--approval-requests`) decides how an ACP agent's own
permission prompts are answered: `ask` (default — surface them to the user),
`auto-allow`, or `auto-deny`.

The terminal gate is only active in a **live Slack chat** (where there's a human
to ask); headless runs — scheduled jobs and delegated agents — are never gated.

**Keeping the card** (`--approval-keep-resolved`) decides what happens to an
approval card after it settles. By default the card rewrites itself to its
outcome, waits a few seconds, and deletes itself — so a long run doesn't bury the
thread in spent approvals. Set the flag and the settled card stays put instead,
collapsed, naming the tool and who decided: a durable record of who allowed what.

It applies to **every** settled state, not just approved. A denial or a timeout is
equally part of that record, and a flag that kept some outcomes and swept others
would leave a misleading trail.

Because it is three-state (`unset` / `true` / `false`), an agent can opt out of a
`defaults.approval.keep_resolved: true` as well as into it.

## Claude Code profiles (`--type claude_code`)

Murtaugh drives the `claude` CLI directly over its stream-json protocol — one
process per conversation, no ACP adapter in between. Like an ACP agent, it
reaches Murtaugh's own tools (`slack.*`, `jobs`, `ask`, …) through the tool
bridge, gated by the same `--approval-*` policy. The command-based knobs live on
their own flags; the shared knobs (`--workdir`, `--tools`, `--approval-*`,
`--progress-display`, `--export-skills-to-fs`) apply as above.

| Flag | Required | Meaning |
|---|---|---|
| `--command` | yes | Path to the `claude` binary to launch. |
| `--arg` (repeatable) | no | Override the default stream-json launch flags. Empty uses the built-in defaults (headless bidirectional stream-json). |
| `--model` | no | Passed as `--model` (e.g. `claude-opus-4-8`). Empty lets the binary resolve its own (e.g. via an `ANTHROPIC_MODEL` entry in `--env`). |
| `--env KEY=VALUE` (repeatable) | no | Extra environment variables for the process; expanded and layered on the inherited environment exactly like an ACP agent's env. |

```bash
murtaugh cfg agent create --name coder --type claude_code \
  --command claude --model claude-opus-4-8 \
  --workdir '${HOME}/work/coder' \
  --tools files --tools terminal --tools skills --tools slack \
  --tools jobs --tools ask --tools present_plan \
  --approval-terminal allowlist --approval-allow kubectl --approval-allow "docker ps"
```

Because claude_code discovers skills from the **filesystem** (like a Claude-based
ACP agent), use `--export-skills-to-fs` to mirror the bundled `murtaugh-*` skills
into its `workdir` — see that section above.

## ACP profiles (legacy — `--type acp`)

Murtaugh drives an external agent process. The command-based knobs live on their
own flags; the shared knobs (`--workdir`, `--tools`, `--approval-*`,
`--progress-display`, `--export-skills-to-fs`) apply as above.

| Flag | Required | Meaning |
|---|---|---|
| `--command` | yes | Path to the agent executable (ACP-speaking, or a Claude Code binary). |
| `--arg` | no | A CLI argument for the process — **repeat the flag**, once per argument (commonly `--arg --stdio`). |
| `--env` | no | Extra environment for the process — **repeat** `--env KEY=VALUE`. Values are expanded against Murtaugh's own environment and layered on top of the inherited one. |
| `--model` | no | Model id, shared with native (some backends honour it). |

```bash
murtaugh cfg agent create --name legacy --type acp \
  --workdir /path/to/workspace \
  --command /path/to/acp-agent --arg --stdio
```

### Interruptibility (ACP)

Whether a follow-up can interrupt an in-flight reply is determined by a **warmup
probe**: at daemon start Murtaugh probes an ACP agent for session/cancel support
and logs the verdict. If the agent can't be interrupted, a follow-up that arrives
mid-reply is **deferred** (it waits for the current reply to finish) rather than
cutting it off with a misleading `_interrupted_`. Native agents don't probe the
same way, so this applies only to the process-driven types.

## External MCP servers (`cfg mcp`)

The servers a native agent attaches to via `--mcp-servers` are defined with
`cfg mcp set` (see the `murtaugh-slack`/`murtaugh-setup` skills). Each uses exactly
one transport — a stdio child process (`--command`/`--arg`/`--env`) or a remote
endpoint (`--url`). Secrets in `--env` come from `~/.config/murtaugh/.env` via
`${VAR}`:

```bash
murtaugh cfg mcp set --name vaultre --command vaultre-mcp --arg --stdio \
  --env VAULTRE_TOKEN=${VAULTRE_TOKEN}
```
