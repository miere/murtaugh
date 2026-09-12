# Agent chat

Murtaugh can route Slack **DMs and `@mentions` to an AI agent**, stream the
reply back into the thread, and let a follow-up interrupt an in-flight response.
This page covers the two agent backends, the tools an agent may call, routing,
and how a turn behaves.

**Agents live on a node.** Every command on this page is a `murtaugh-runtime`
command, run against a node's own configuration root
(`~/.config/murtaugh/node` by default). The gateway binary cannot run an agent —
it cannot even reach the packages that would let it, and CI enforces that — so
`murtaugh-gateway cfg agent create` is an unknown command, not a permission
error.

Two `cfg` commands turn chat on:

1. **`murtaugh-runtime cfg agent create …`** — define at least one agent.
2. **`murtaugh-runtime cfg chat set --enabled true --default-agent <name>`** —
   turn on the chat surface and point it at one of those agents.

A node serves **one** agent per process: the one named by `chat.defaults.agent`,
or the only one it holds, or the one you name with `-agent`. A node with several
agents and no default refuses to start rather than pick.

With chat disabled (the default), DMs and mentions are ignored — but agents are
still available to [jobs](jobs.md), [workflow rules](slack.md#workflow-rules),
and [unfurls](slack.md#link-unfurling).

Every `cfg` change re-validates the whole config store and takes effect on the
next restart of the daemon that reads it (config is loaded once at startup).

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
murtaugh-runtime cfg agent create --name emily --type native \
  --workdir '${HOME}/work/emily' \
  --tools files --tools terminal --tools skills \
  --tools ask --tools present_plan --tools attach \
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
variable in the **node's** `.env` (`~/.config/murtaugh/node/.env` by default).
Write it there by hand; `setup env` no longer exists. GLM,
DeepSeek, and Kimi ride the `anthropic`/`openai`-compatible families via a
`--base-url` override. A workspace `AGENTS.md` in the agent's `--workdir` is
auto-loaded into the system prompt as project guidelines.

## The persona (SOUL.md)

An agent's voice lives in `SOUL.md`, and it is the same file for a `native` and a
`claude_code` agent — that is what keeps the two reading as the same character.
Murtaugh resolves it in this order:

1. `--soul-file` on the agent profile — the only knob that separates two
   profiles sharing one workdir.
2. `<workdir>/SOUL.md` — the per-workdir override, so an agent's voice can follow
   the context it works in. This is what the onboarding flow writes.
3. `<workspace>/SOUL.md` — the node default, seeded by bootstrap.

The file carries YAML frontmatter because it doubles as a Claude Code **output
style**: bootstrap links it to `.claude/output-styles/murtaugh.md`, so an admin
working in that directory from the interactive `claude` CLI can select the
agent's voice from their style list. The frontmatter is stripped before the prose
is injected, and its `name:` key is a binding key — Claude Code's `outputStyle`
setting matches on it, so renaming it breaks style selection. The name an agent
answers to belongs in the prose, not the frontmatter.

How it reaches the model differs by backend, because their system prompts do:

| Backend | Persona delivery |
|---|---|
| `native` | A `<persona>` block appended after the base system prompt, inside the cacheable static prefix. |
| `claude_code` | A `<persona>` block passed as `--append-system-prompt` (omitted when there is no persona). The CLI owns the rest of its system prompt; `system-prompt.md` is deliberately not passed. |
| `acp` | None. An ACP adapter is bespoke and brings its own harness, so its voice is the admin's to configure there. |

A freshly seeded `SOUL.md` is frontmatter only, which resolves to no persona at
all — the pre-onboarding state that makes the seeded `AGENTS.md`'s "I don't have
a name or personality yet" true rather than immediately contradicted.

Change a field later with `cfg agent update`:

```sh
murtaugh-runtime cfg agent update --name emily --max-turns 60 --cache-retention 1h
```

### An ACP or claude_code agent

```sh
murtaugh-runtime cfg agent create --name default --type acp \
  --workdir /path/to/workspace \
  --command /path/to/acp-agent --arg --stdio \
  --env ANTHROPIC_API_KEY=${ANTHROPIC_API_KEY}
```

`--arg` and `--env KEY=VALUE` are repeatable. Murtaugh probes the agent's cancel
support at startup, so you don't declare interruptibility. Native agents don't
probe. A `claude_code` agent is created the same way with `--type claude_code`
and may add `--model`.

---

## ACP agents are the node admin's job

Murtaugh used to register itself into opencode, auggie and goose for you, and
to guess the rest. It no longer does: **an ACP agent is configured by the person
who runs the node**, deliberately, because most of the investment has gone into
`claude_code` and pretending otherwise moves the surprise to runtime. This
section is what that person needs.

### Configuring one, end to end

Everything below runs on the **node**, against the node's own root. There is no
combined install to do it from.

```sh
# 1. Where the node dials, and the credential the gateway minted for it.
murtaugh-runtime cfg node set --gateway wss://gateway.example.com:8443
#    …and put the minted token at ~/.config/murtaugh/node/node-token (0600).

# 2. The agent profile. --command is what makes it an ACP agent.
murtaugh-runtime cfg agent create --name coder --type acp \
  --workdir "$HOME/work/coder" \
  --command "$HOME/.local/bin/claude-code-acp" --arg --stdio \
  --env ANTHROPIC_API_KEY='${ANTHROPIC_API_KEY}' \
  --tools ask --tools present_plan --tools attach \
  --approval-requests ask \
  --sandbox-mode seatbelt

# 3. Any external MCP servers it should reach, by name.
murtaugh-runtime cfg mcp set --name vaultre --command vaultre-mcp --arg --stdio
murtaugh-runtime cfg agent update --name coder --mcp-servers vaultre

# 4. Make it the agent this node serves, then check the whole store.
murtaugh-runtime cfg chat set --enabled true --default-agent coder
murtaugh-runtime cfg validate
murtaugh-runtime cfg agent show --name coder
```

The adapter's own credentials are **yours**: `--env` is applied to the child
process after the sandbox's environment filter, and the value is a `${VAR}`
reference resolved from the node's `.env`. Murtaugh downloads no third-party
agent and authenticates none.

### Murtaugh's tools reach the agent by themselves

You do **not** wire Murtaugh into the adapter to give it Murtaugh's tools. For
every session it opens, the node hands the agent one stdio MCP server in
`session/new`, named `murtaugh`, whose command is the runtime binary's internal
`mcp-bridge` subcommand with a single-use socket path and token in its
environment. Behind that socket the node serves exactly this agent's resolved
toolset — its own groups, minus the native-only ones, plus the proxied tools of
every MCP server it attaches — gated by the same approval broker a native agent
goes through. Third-party MCP credentials never leave the node's process.

The consequence for you: an ACP agent's tool surface is `--tools` and
`--mcp-servers` on the profile, and nothing else. `files`, `terminal` and
`skills` are stripped from it whether or not you list them — the adapter brings
its own — while `attach`, `ask`, `present_plan` and `auth.request` are served
when you list them, because they deliver something to the person in the thread.
(`auth.request` is spelled `--tools auth` or `--tools auth.request`; a namespace
or an exact name both match.)

### Registering Murtaugh inside opencode / auggie / goose — manual

That is the *other* direction: making Murtaugh's tools available when you drive
opencode, auggie or goose **yourself**, outside Murtaugh. `setup mcp_register`
used to write these files; it is gone, and they are yours to edit.

The entry is always the same command — the runtime binary, the `--config` of the
node root it should act on, and the `mcp` subcommand. `--config` is a global
flag, so it comes **before** `mcp`. Use absolute paths; these clients do not run
under your login shell.

**opencode** — `~/.config/opencode/opencode.json`:

```json
{
  "$schema": "https://opencode.ai/config.json",
  "mcp": {
    "murtaugh": {
      "type": "local",
      "command": ["/usr/local/bin/murtaugh-runtime", "--config", "/Users/you/.config/murtaugh/node/config.yaml", "mcp"],
      "enabled": true
    }
  }
}
```

**auggie** — `~/.augment/settings.json`:

```json
{
  "mcpServers": {
    "murtaugh": {
      "command": "/usr/local/bin/murtaugh-runtime",
      "args": ["--config", "/Users/you/.config/murtaugh/node/config.yaml", "mcp"]
    }
  }
}
```

**goose** — `~/.config/goose/config.yaml`:

```yaml
extensions:
  murtaugh:
    name: murtaugh
    type: stdio
    enabled: true
    cmd: /usr/local/bin/murtaugh-runtime
    args: ["--config", "/Users/you/.config/murtaugh/node/config.yaml", "mcp"]
    timeout: 300
```

Merge it into whatever the file already holds. What the client gets is the
**node's** tool surface — `cfg_agent_*`, `cfg_mcp_*`, `cfg_node_*`, `cfg_show`,
`cfg_validate`, `jobs_run`, `ping`, `version`, `help`, `setup_update`, and
`ask` / `present_plan` / `auth_request`. There are no `slack_*` tools: only the
gateway talks to Slack. Names use underscores, not dots
(`cfg.agent.create` → `cfg_agent_create`), because some providers reject a `.`
in a function name. See [CLI & MCP server](cli-and-mcp.md).

> Do this for standalone use of the client, **not** to wire up a Murtaugh agent.
> If the adapter you registered it in is also the `--command` of an ACP profile,
> that agent now sees two copies of Murtaugh: the per-session bridge above, and
> this one — which carries no approval gate and no turn context. Point the entry
> at a different node root, or leave it out.

### `troubleshoot.providers` is a manual knob

`troubleshoot bundle` (and `/murtaugh troubleshoot`) can fold a downstream
provider's own sessions and logs into the bundle. `setup mcp_register` used to
append to that list when it registered a client that was also a known provider;
nothing appends to it now.

Nothing is lost. The list lives on the **gateway** and an **empty one — the
default — means every provider Murtaugh knows how to collect diagnostics for**,
today `goose` and `claude-code`. Missing files are skipped at collection time,
so the all-known fallback is safe on a machine that runs only some of them.

```sh
murtaugh-gateway cfg troubleshoot show                     # what is pinned, if anything
murtaugh-gateway troubleshoot bundle --include goose       # narrow one bundle
```

There is no `cfg troubleshoot set`: to pin a narrower default list, edit a
`cfg export` snapshot and `cfg import` it back.

### The honest state of ACP, next to Claude Code

Both backends implement one `agent.Client` and feed one event stream into the
same Slack renderer, so streaming, task cards, interrupts, approval prompts and
attachments look the same either way. That parity is a hard constraint, not a
claim about how well any given adapter behaves. Where they genuinely differ:

| | `claude_code` | `acp` |
|---|---|---|
| Persona (`SOUL.md`) | Passed as `--append-system-prompt`. | **None.** An adapter brings its own harness, so its voice is yours to configure there. |
| `--model` | Honoured. | **Silently ignored** — the flag exists on `cfg agent create` for the other two backends and is not written to an `acp:` block. Pick the model in the adapter. |
| Credential upkeep | The warden refreshes the Claude Code login before it lapses and DMs the node's owner when it fails. | **Not covered.** An ACP command may be Claude Code behind an adapter, but the adapter's name is arbitrary, so guessing would fail quietly. Yours to keep alive. |
| Approvals | `--approval-requests ask\|auto-allow\|auto-deny`, answered on the agent's own permission requests. | The same flag, same default (`ask`) — but only for the requests the adapter actually raises. An adapter that gates nothing is never gated. |
| Cancellation | Supported. | **Probed at startup** and logged. An adapter that does not support cancel cannot be interrupted; a follow-up message waits instead. |
| Session resume | Resumes its on-disk session (`--resume`), falling back to a fresh one. | **None.** One process per conversation, and the adapter's advertised `loadSession` is logged at startup but not acted on. |
| MCP transport | n/a — the bridge is stdio. | Stdio only. HTTP/SSE are read from the adapter's capabilities but not used, and at least one shipping adapter advertises HTTP while silently dropping such servers. |
| Tested against | Exercised throughout the suite. | One live smoke test, against the **Claude Code ACP adapter** only, behind the `acplive` build tag — so it never runs in CI. |

Read that last row as the summary: **`acp` is a supported shape, not a
supported list of agents.** Adapters vary in what they implement, Murtaugh
cannot tell which parts yours skips, and a gap shows up as a feature that
quietly does nothing rather than as an error. If you want the path with the most
road behind it, use `--type claude_code`. Choose `acp` when you specifically
want that adapter, and expect to verify each surface yourself.

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
| `slack` | Post, update, and read Slack messages and reactions. **Not available to an agent on a node** — see the note below. |
| `jobs` | Define and run [jobs](jobs.md). **Not available to an agent on a node** — see the note below. |
| `ask` | Put a question with options to you as clickable buttons, and **wait** for the answer. |
| `present_plan` | Show a plan with Proceed / Revise / Cancel and **wait** for sign-off. |
| `attach` | Return a workspace file (report, image, export) as a real Slack upload; confined to `workdir`. |
| `auth.request` | Sign in for credentials the agent lacks, on the machine it runs on, and **wait** until that machine's **owner** completes or refuses it. |

`ask` and `present_plan` are recommended — they let the agent get a real answer
instead of guessing. See [Slack → Asking the user](slack.md#asking-the-user).

> **What a group resolves to depends on where the agent runs.** `files`,
> `terminal`, `skills` and `attach` are synthesized per agent, rooted at its
> `workdir`, and always available. The rest are *registry namespaces*, and the
> registry an agent on a node sees holds exactly `ask`, `present_plan`,
> `auth.request`, `ping`, `version` and `help`. A `slack` or `jobs` entry
> therefore contributes no tool there — only the gateway talks to Slack, and an
> agent reaches a person through the display requests above instead. An
> unmatched entry contributes nothing rather than being rejected, so a group
> with no home on a node is a quiet no-op, not a startup error.

`auth.request` is the one tool that does **not** ask the person in the thread.
Credentials belong to the machine the agent runs on, so the sign-in runs there,
in that agent's environment, and the machine's owner completes it: for a runtime
node, the Slack user its token was minted for; for an agent running inside the
gateway, `configuration.admin_user`. The owner gets the card by DM and must be
allowed to use the gateway; the thread only sees a notice that a DM was sent. If
the owner loses access while the card is open, their answer is refused and the
sign-in is stopped, and a node that is revoked or disconnects stops its sign-in
too. The `custom` profile runs a command the agent chose, so its card shows the
owner that exact command and nothing runs until they approve it. It fails
closed — a refusal, a timeout or a failed sign-in all return an error — so the
agent stops rather than retrying a call it still has no credentials for.
Outside a conversation it only works on a runtime node: a scheduled job there
still reaches the node's owner by DM, with no thread notice, while an agent
inside the gateway or a CLI call is refused before anything runs. See
`murtaugh-runtime help auth request`.

Its built-in profiles are `gcloud`, `gcloud-adc`, `claude-code`, and `custom`.
The `claude-code` profile re-authenticates the Claude Code CLI itself — the
credential every `claude_code` agent runs on — by driving
`claude auth login --claudeai`.

An agent can ask for it like any other profile, but usually will not have to: a
`claude_code` agent whose credential has been rejected cannot run at all, so
there is no turn left from which to call `auth.request`. The machine the agent
runs on detects that case and starts the sign-in on the agent's behalf: a
runtime node sends the card to its owner, a gateway running its own agents to
the admin, and the user is told their turn is waiting on it. `/murtaugh auth
login` starts one pre-emptively on the node the conversation is pinned to, or
on a node you name where none is. See
[Operations → Claude Code credentials](operations.md#claude-code-credentials),
which also covers how Murtaugh keeps that credential from lapsing in the first
place.

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

By default a settled approval card clears itself a few seconds after the
decision, so a long run doesn't bury the thread in spent approvals. Pass
`--approval-keep-resolved` to leave it in place instead — collapsed, naming the
tool and who decided — for agents where who-allowed-what is worth keeping. It
applies to every outcome, including denials and timeouts.

### External MCP servers

A native agent can attach to external MCP servers, defined once with
`cfg mcp set` and referenced by name:

```sh
murtaugh-runtime cfg mcp set --name vaultre \
  --command vaultre-mcp --arg --stdio --env VAULTRE_TOKEN=${VAULTRE_TOKEN}
murtaugh-runtime cfg mcp set --name data-api --url https://data-api.internal/mcp

murtaugh-runtime cfg agent update --name emily \
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
murtaugh-runtime cfg agent update --name code --sandbox-mode seatbelt
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

`chat` is one of the few blocks **both** binaries carry, and it means a
different thing on each. Slack-side routing — which agent NAME a conversation
asks for — is the **gateway's**:

```sh
murtaugh-gateway cfg chat set --enabled true \
  --default-agent default \        # DMs and any unrouted channel
  --dm-agent support \             # optional: a different agent for DMs
  --reply-on-thread true           # optional: global reply strategy (default true)

murtaugh-gateway cfg chat show             # inspect the current routing
```

On a **node**, the same block answers a smaller question: which of that node's
own agents this process serves (`murtaugh-runtime cfg chat set --default-agent
coder`).

- `--default-agent` handles DMs and any channel without a more specific route.
- `--reply-on-thread` (global default `true`) picks where the bot replies to a
  top-level channel message: `true` roots a thread; `false` replies directly in
  the channel and treats it as one rolling conversation. A message already in a
  thread is always answered in-thread.
- **Where you set the name decides when it is checked.** A node holds the
  profile bodies, so `murtaugh-runtime cfg chat set --default-agent typo` is
  rejected on the spot. The gateway holds none, so it accepts the name and
  resolves it at **connect time** against what the connected nodes advertise; a
  name nothing serves is journalled (`stream=gateway kind=node
  state=unservable`) as a warning, never a fatal error.

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
defaults (below). Tool progress always renders as task cards, in their own
message alongside the reply, for every agent.

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

An interrupted turn is never reported as a failure or as an empty reply — you
asked for it, and your follow-up is already being answered below it. The
"finished without a reply" note is reserved for a turn that genuinely ran to the
end with nothing to say.

---

## Runtime defaults

The **defaults** block in the config store tunes **all** backends — session
timeouts, streaming cadence, ACP child-process lifecycle, and the global approval
default that per-agent flags override. Inspect it with:

```sh
murtaugh-runtime cfg defaults show
```

```
session:
  idle_timeout: 30m
  request_timeout: 10m       # idle-bounded: reset by each agent event, not total wall-clock
  background_idle_timeout: 15m  # same, for work that lands after the turn ended
  max_concurrent: 100
rendering:
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

> `background_idle_timeout` is the same idea for a background completion — a
> Claude Code subagent finishing after its turn ended, rendered into the thread
> with no turn loop watching it. If that stream goes quiet for the whole window,
> the message is sealed with a short "closed this out" note rather than left
> streaming — a notice, not an error, because nothing is known to have failed. It
> never cancels the agent: expiry ends a Slack message, and later output simply
> opens a new one. It defaults higher than `request_timeout` because the tool heartbeat
> that keeps a turn's window alive is scoped to a turn and never reaches this one.

An agent's `--workdir` defaults to the workspace (`~/.config/murtaugh`) when
unset, so it starts where the bundled skills and templates live.
