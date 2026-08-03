# Agent chat

Murtaugh can route Slack **DMs and `@mentions` to an AI agent**, stream the
reply back into the thread, and let a follow-up interrupt an in-flight response.
This page covers the two agent backends, the tools an agent may call, routing,
and how a turn behaves.

Two `cfg` commands turn chat on:

1. **`murtaugh cfg agent create …`** — define at least one agent.
2. **`murtaugh cfg chat set --enabled true --default-agent <name>`** — turn on
   the chat surface and point it at one of those agents.

With chat disabled (the default), DMs and mentions are ignored — but agents are
still available to [jobs](jobs.md), [workflow rules](slack.md#workflow-rules),
and [unfurls](slack.md#link-unfurling).

Every `cfg` change re-validates the whole config store and takes effect on the
next gateway restart (config is loaded once at startup).

---

## Three backends: native, ACP, and claude_code

An agent's backend is chosen with **`--type`** on `cfg agent create`:

| | **native** (default) | **ACP** / **claude_code** |
|---|---|---|
| `--type` | `native` | `acp` / `claude_code` |
| What runs | Murtaugh runs the LLM loop in-process | An external agent binary, driven over its protocol |
| Needs | `--provider` + `--model` + `--api-key-env` | a `--command` |
| Auth | API key from `.env` (named by `--api-key-env`) | the child process's own env |

`claude_code` drives a Claude Code process; it shares the ACP command flags
(`--command`, `--arg`, `--env`) and additionally accepts `--model`.

### A native agent

```sh
murtaugh cfg agent create --name emily --type native \
  --workdir '${HOME}/work/emily' \
  --tools files --tools terminal --tools skills --tools slack \
  --tools jobs --tools ask --tools present_plan --tools attach \
  --approval-terminal allowlist \
  --approval-allow kubectl --approval-allow 'docker ps' \
  --provider gemini --model gemini-2.5-pro --api-key-env GEMINI_API_KEY \
  --system-prompt-file prompts/emily.md \
  --max-turns 40 --context-limit 1000000 \
  --compaction truncate --cache-retention 5m
```

- `--workdir` roots the files/terminal tools.
- `--tools <group>` is repeatable (see the tool table below).
- `--approval-terminal` and `--approval-allow` set the approval gate (below).
- `--provider` is `gemini`, `anthropic`, or `openai`; `--api-key-env` names the
  `.env` variable that holds the key.
- `--base-url` targets Z.ai / DeepSeek / Kimi compat endpoints; `--system-prompt`
  inlines the prompt instead of `--system-prompt-file`.

The API key value **never** lives in the config store — `--api-key-env` names a
variable in `~/.config/murtaugh/.env`. Write it with `murtaugh setup env`. GLM,
DeepSeek, and Kimi ride the `anthropic`/`openai`-compatible families via a
`--base-url` override. A workspace `AGENTS.md` in the agent's `--workdir` is
auto-loaded into the system prompt as project guidelines.

Change a field later with `cfg agent update`:

```sh
murtaugh cfg agent update --name emily --max-turns 60 --cache-retention 1h
```

### An ACP or claude_code agent

```sh
murtaugh cfg agent create --name default --type acp \
  --workdir /path/to/workspace \
  --command /path/to/acp-agent --arg --stdio \
  --env ANTHROPIC_API_KEY=${ANTHROPIC_API_KEY}
```

`--arg` and `--env KEY=VALUE` are repeatable. Murtaugh probes the agent's cancel
support at startup, so you don't declare interruptibility. Native agents don't
probe. A `claude_code` agent is created the same way with `--type claude_code`
and may add `--model`.

---

## What an agent can do: tools

The repeatable `--tools <group>` flag (shared by all backends) controls which
capabilities an agent may call. Values are native tool **groups** plus registry
**namespaces**:

| Tool / group | Lets the agent… |
|---|---|
| `files` | Read and write files under its `workdir`. |
| `terminal` | Run shell commands (subject to the approval gate, below). |
| `skills` | Load Murtaugh's bundled skills for how-to knowledge. |
| `slack` | Post, update, and read Slack messages and reactions. |
| `jobs` | Define and run [jobs](jobs.md). |
| `ask` | Put a question with options to you as clickable buttons, and **wait** for the answer. |
| `present_plan` | Show a plan with Proceed / Revise / Cancel and **wait** for sign-off. |
| `attach` | Return a workspace file (report, image, export) as a real Slack upload; confined to `workdir`. |

`ask` and `present_plan` are recommended — they let the agent get a real answer
instead of guessing. See [Slack → Asking the user](slack.md#asking-the-user).

### The approval gate

`--approval-terminal` governs whether a native agent's `terminal` commands need
your sign-off in Slack **during live chat**:

- `allowlist` (default) — auto-run recognised read-only commands (`ls`, `cat`,
  `grep`, `git status`, …); ask before anything else.
- `prompt` — ask before *every* command.
- `off` — never ask.

`--approval-allow <cmd>` (repeatable) extends the auto-run set with your own
commands — an argv0 like `kubectl`, or a `binary subcommand` like `docker ps`.
The gate is **only** active in live chat; scheduled and delegated runs are never
gated. For an ACP agent, `--approval-requests <ask|auto-allow|auto-deny>` decides
how the agent's own permission prompts are answered.

### External MCP servers

A native agent can attach to external MCP servers, defined once with
`cfg mcp set` and referenced by name:

```sh
murtaugh cfg mcp set --name vaultre \
  --command vaultre-mcp --arg --stdio --env VAULTRE_TOKEN=${VAULTRE_TOKEN}
murtaugh cfg mcp set --name data-api --url https://data-api.internal/mcp

murtaugh cfg agent update --name emily \
  --mcp-servers vaultre --mcp-servers data-api   # additive on top of the global set
```

Each server uses exactly one transport: a stdio child process (`--command` with
repeatable `--arg`/`--env`) or a remote endpoint (`--url`). `--mcp-servers` on an
agent is repeatable and additive on top of the global set.

### Sandboxing an agent process (macOS)

An `acp` or `claude_code` agent runs an external process on your machine with
your permissions. `sandbox.mode: seatbelt` confines it — and every process it
spawns — with the macOS kernel sandbox:

```sh
murtaugh cfg agent update --name code --sandbox-mode seatbelt
```

```yaml
agents:
  code:
    workdir: ~/Development/miere/murtaugh
    claude_code:
      command: claude
    sandbox:
      mode: seatbelt        # off (default) | seatbelt
```

Every other key is optional. The defaults are the point — `mode: seatbelt` alone
gives you a working box.

**Writes are deny-by-default.** The agent can write to its `workdir`, `$TMPDIR`,
`~/.claude` (Claude Code's session state) and the MCP bridge socket. Nothing
else — not your other repos, not your dotfiles, not Murtaugh's own config, so a
boxed agent cannot widen its own permissions for the next restart. Add more with
repeatable `--sandbox-write <path>`.

**Reads are allow-by-default, minus the credential stores.** `~/.ssh`, `~/.aws`,
`~/.config/gcloud`, `~/.config/gh` and `~/.netrc` are blinded unless you replace
the list with `--sandbox-deny-read <path>`. This asymmetry is deliberate: a read
*allowlist* breaks a real coding agent in a dozen small ways (toolchains, caches,
git objects, system headers), so the write boundary is the one doing the work.

**The environment is reduced to an allowlist**: `PATH`, `HOME`, `TMPDIR`, `USER`,
`LANG`, `SHELL`. Everything else — every cloud credential and API token in the
daemon's environment — stops at the boundary. Add to the set with repeatable
`--sandbox-env <NAME>`; it is additive, so you cannot accidentally drop `PATH`.

You do **not** need to forward a credential for Claude Code: on macOS it
authenticates through the login Keychain, which is reached over IPC to a system
service outside the box. An agent that needs API-key auth instead should carry it
in the backend's own `--env KEY=VALUE`, which is applied after the filter.

Two limits worth knowing:

- **Network is all-or-nothing.** The agent needs `api.anthropic.com`, and the
  macOS sandbox cannot filter by host. A confined agent can still reach the
  network, so confinement prevents damage to your disk, not exfiltration.
- **macOS only, and it fails closed.** `mode: seatbelt` on a non-macOS host is a
  startup error, never a silent downgrade to an unconfined process. Check the
  posture at a glance in the startup log: `startup routing: agent … sandbox=…`.

---

## Routing: which agent answers

```sh
murtaugh cfg chat set --enabled true \
  --default-agent default \        # DMs and any unrouted channel
  --dm-agent support \             # optional: a different agent for DMs
  --reply-on-thread true           # optional: global reply strategy (default true)

murtaugh cfg chat show             # inspect the current routing
```

- `--default-agent` handles DMs and any channel without a more specific route.
- `--reply-on-thread` (global default `true`) picks where the bot replies to a
  top-level channel message: `true` roots a thread; `false` replies directly in
  the channel and treats it as one rolling conversation. A message already in a
  thread is always answered in-thread.
- Every routed agent name must exist, or the `cfg chat set` change is rejected on
  the spot (fail-closed) — and the gateway refuses to start against an invalid
  store.

| Entry point | Session scope |
|---|---|
| DM the bot | one session per DM channel |
| `@mention` in a channel (threaded reply) | one session per Slack thread |
| `@mention` in a channel (`reply_on_thread: false`) | one rolling session per channel |
| `/murtaugh chat <prompt>` | one session per thread |

Threaded conversations are bound to their thread and never shared across threads;
a channel in off-thread mode is one shared rolling session (reset with `/clear`).

---

## How a turn behaves

**Streaming.** The reply streams into the thread using Slack's native streaming
APIs, updated as chunks arrive — no polling. The cadence
(`stream_append_interval`, `stream_min_chunk_chars`) comes from the runtime
defaults (below). How tool progress renders is a per-agent choice:
`--progress-display simplified` (the default one-line status) or `tasks` (the
full plan cards).

**Pausing for you.** Mid-turn, a native agent may stop to **ask you** (`ask`),
get **sign-off on a plan** (`present_plan`), or seek **approval to run a command**
(the terminal gate). The turn waits for your click. A quiet turn may be *waiting*
on you, not hung.

**Interrupts and stop.** A new message in the same DM or thread automatically
interrupts the previous reply (if the agent supports it): Murtaugh cancels the
in-flight prompt, waits `defaults.acp.cancel_grace_period` (default `2s`) for
trailing chunks, then hard-cancels. The interrupted reply is sealed with an
`_interrupted_` marker so partial output stays visible. To stop without sending a
follow-up, run `/stop` (or `/murtaugh stop`) from the thread or DM.

---

## Runtime defaults

The **defaults** block in the config store tunes **all** backends — session
timeouts, streaming cadence, ACP child-process lifecycle, and the global approval
default that per-agent flags override. Inspect it with:

```sh
murtaugh cfg defaults show
```

```
session:
  idle_timeout: 30m
  request_timeout: 10m       # idle-bounded: reset by each agent event, not total wall-clock
  max_concurrent: 100
rendering:
  progress_display: simplified
  stream_min_chunk_chars: 96
  stream_append_interval: 750ms
acp:                         # ACP child-process lifecycle (native ignores these)
  startup_timeout: 10s
  cancel_grace_period: 2s
approval:                    # global default, overridden per agent by --approval-* flags
  terminal: allowlist
  requests: ask              # how an ACP agent's own permission prompts are answered
```

> `request_timeout` is **idle-bounded** — it's reset by every agent event, so a
> long-but-active turn won't time out; only a genuinely stalled one will.

An agent's `--workdir` defaults to the workspace (`~/.config/murtaugh`) when
unset, so it starts where the bundled skills and templates live.
