# Architecture

This document is the orientation guide for anyone — human or AI agent — changing
the Murtaugh Dev Toolkit. It describes what each package does, how data flows
between them, and the conventions you must respect so changes stay consistent.

## What this service is

A Go toolkit that ships a single binary with **three frontends** backed by a
shared Tool registry:

- **Slack gateway** — Socket Mode daemon (`murtaugh slack gateway`) that
  responds to slash commands, runs YAML-defined **workflow rules** against
  interactive payloads (Block Kit buttons, etc.), bridges Slack conversations
  to an **agent** — either the in-process native LLM loop (`kind: native`, the
  default) or an external **ACP** (Agent Communication Protocol) agent
  (`kind: acp`) — with live response streaming, and renders **custom link
  unfurls** for shared URLs.
- **CLI** — human-facing direct invocation (`murtaugh <tool> [...]`), including
  the Slack tools under the `slack` namespace (`murtaugh slack send-msg`, …).
- **MCP** — JSON-RPC stdio server (`murtaugh mcp`) that exposes every
  registered tool to AI clients.

Module path: `github.com/miere/murtaugh`.

## High-level architecture

```
                          ┌──────────────┐
                          │ cmd/murtaugh │
                          └──────┬───────┘
                                 │
                          ┌──────▼───────┐
                          │ internal/app │   ← composition root
                          └──────┬───────┘
                  builds Registry + selects Mode
        ┌────────────────────────┼────────────────────────┐
        │                        │                        │
┌───────▼────────┐      ┌────────▼────────┐      ┌──────────▼─────────┐
│ frontends/cli  │      │ frontends/mcp   │      │ slack/gateway      │
└───────┬────────┘      └────────┬────────┘      └────────────────────┘
        │                        │
        └──────► Tool ◄──────────┘
                  (internal/tools/*)
```

- `internal/app` is the only place tools are wired into the registry.
- CLI and MCP frontends know nothing about each other and reach tools only
  through `tools.Tool`.
- The Slack gateway does not use the Tool registry today; it runs side-by-side
  as a third frontend selected by mode (`ModeGateway`). The `slack.*` tools,
  by contrast, are ordinary registry tools shared by the CLI and MCP frontends.

## Repository layout

```
cmd/murtaugh/         Entry point: flag parsing, mode selection, signal handling.
internal/app/         Composition root + Registry wiring.
internal/frontends/   CLI and MCP adapters over the Tool registry.
  cli/                Human frontend: kebab → snake flag mapping, render dispatch.
  mcp/                MCP stdio adapter wrapping the same Registry.
internal/tools/       Shared Tool interface + one package per tool.
  tool.go             Tool interface + Registry.
  ping/               Health-check tool (the canonical example).
  jobs/run/           Tool `jobs.run`: execute a job stored in the config store.
  jobs/define/        Tool `jobs.define`: register a job in the config store.
  journal/            Tools `journal.query`/`.stats`/`.prune`: inspect the event journal.
  slack/              Slack tools: send-msg, fetch-msgs, fetch-reactions, update-msg.
  cfg/                Tools `cfg.*`: read/write the config store (agents, mcp,
                      jobs, chat, access, rules, defaults, db migrate) with
                      validate-and-rollback on every mutation.
internal/config/      Config schema, validation, bootstrap-file loader, and the
                      config-store seam.
  store/              SQLite/Postgres store implementation + YAML→DB migration.
internal/journal/     Event journal: SQLite store, async recorder, query/stats/prune.
internal/slack/       Slack subsystem:
  gateway/            Socket Mode gateway, event loop, all Slack event handlers.
  client/             Slack Web API client wrapper used by the slack.* tools.
  interaction/        Human-in-the-loop broker: the single-choice option buttons
                      behind a quick `ask` and the tool-approval gates.
  askcard/            The `ask` tool's multi-question card: inline inputs,
                      answered in place, from templates/ask/*.json.
  authcard/           Two-party authentication card (requester + admin),
                      rendered from templates/auth/*.json.
internal/agent/       Agent backend interface, session manager, protocol types,
                      and the shared tool watcher / execution ceiling.
  native/             In-process LLM agent loop (kind: native): conversation,
                      turn loop, system prompt, recovery.
  acp/                External ACP agent over a subprocess (kind: acp).
  claudecode/         Claude Code stream-json backend (kind: claude_code).
internal/agentbuild/  Kind-aware backend builder (native / ACP / claude_code).
internal/jsontemplate/ JSON-document templating with JSON-safe escaping funcs.
                      The single renderer behind every Block Kit template.
internal/llm/         Provider-agnostic LLM boundary over litellm (gemini /
                      anthropic-compat / openai-compat).
internal/toolset/     Per-agent toolset resolver (native tools + registry + MCP).
internal/mcpclient/   External MCP client: remote tools as tools.Tool.
internal/agentdelegate/ One-shot isolated agent runner (delegate-to-agent).
internal/workflow/    Workflow engine, command runner, template rendering.
internal/unfurl/      Link matcher and Block Kit attachment renderer.
assets/               Embedded reference config, JSON templates, agent skills.
```

`internal/*` is private to this module. Cross-package dependencies flow in one
direction: `slack/gateway` orchestrates `config`, `acp`, `agentdelegate`,
`workflow`, `unfurl`, `slack/interaction`, and `slack/authcard`; those packages
do not import `slack/gateway`. `jsontemplate` sits at the bottom — it knows
nothing of Slack and is imported by every template renderer above it. The
`agentdelegate` runner builds on `acp` and backs every delegate-to-agent
surface (the `workflow` engine, the `unfurl` handler, and the `jobs.run`
tool consume it through small local interfaces). Tool packages depend on
`config` where they need shared types (e.g. `JobProfile`), never the other way
around.

## The Tool contract

```go
type Tool interface {
    Name() string
    Description() string
    InputSchema() *jsonschema.Schema
    Invoke(ctx context.Context, args map[string]any) (any, error)
}
```

- `Name` is the registry key. A `.`-separated name (e.g. `jobs.run`) declares
  the tool as belonging to a namespace; the CLI frontend resolves
  `murtaugh jobs run` to the registered name `jobs.run`.
- `Description` is a one-line human-readable hint used by MCP clients.
- `InputSchema` returns the JSON Schema that documents and validates the
  tool's parameters. Returning `nil` means the tool takes no parameters.
- `Invoke` receives args keyed by the JSON-Schema property names declared on
  `InputSchema`. The CLI frontend maps `--kebab-case` flags to `snake_case`
  keys before invocation; array-typed properties accumulate when the flag is
  repeated.

### Frontend conventions

- **CLI** writes tool output to stdout and diagnostics to stderr. Tool
  results dispatch by type via `cli.Render`: `string`, `[]string`,
  `fmt.Stringer`, and finally `%v` fallback. Tools that own a rich result
  struct provide a `String()` method for the CLI representation and let the
  MCP frontend JSON-marshal the same struct.
- **MCP** wraps every registered tool's result in a single `TextContent`
  block. Strings pass through; everything else is JSON-marshalled. Tool
  errors map to `CallToolResult{IsError: true, ...}`.

### Adding a new tool

1. Create `internal/tools/<flat>/` or `internal/tools/<ns>/<sub>/` and
   implement the `Tool` interface.
2. Register the new tool in `internal/app/application.go::buildRegistry`.
3. Add a `_test.go` covering happy-path invocation and schema declarations.
4. **Document the command in `assets/cli-help.md`** (see "CLI/MCP command
   reference" below) and add it to the command list in
   `internal/help/help_test.go::TestEveryCommandDocumented`.
5. Note the command in `README.md`.

The same rule applies when you **change or remove** a tool: every flag rename,
new flag, changed default, new enum value, or deleted command must be reflected
in `assets/cli-help.md` in the same change. `go test ./internal/help/...` fails
if a registered command has no help section, but it cannot catch a stale flag
description — keep the prose honest by hand.

### CLI/MCP command reference (`assets/cli-help.md`)

`assets/cli-help.md` is the **single source of truth** for what every command
does and which flags it takes. It is embedded into the binary (via the
`assets` `go:embed` directive) and surfaced by the `internal/help` package:

- `murtaugh help` prints the whole document; `murtaugh help <command>` (e.g.
  `murtaugh help slack send-msg`) and `murtaugh <command> --help` print a single
  command's section.
- CLI usage errors and the bare-invocation usage line point users at
  `murtaugh help`.

The document is plain Markdown with one parser contract: each command's section
begins with a level-2 header of the exact form `## murtaugh <invocation>`
(e.g. `## murtaugh jobs run`). `help.Section` returns everything from that header
up to the next `## ` or `# ` line, and accepts both the spaced CLI form
(`jobs run`) and the dotted registry form (`jobs.run`). Top-of-file overview
material uses level-1 (`# `) headers so it is never mistaken for a command
section. When you add a command, follow that header convention or `help.Section`
will not find it.

Because tool flag descriptions also live in each tool's `InputSchema`, keep the
two in agreement: the schema `Description` is the one-line MCP hint; the
`cli-help.md` section is the full human/agent-facing manual (required/optional,
defaults, mutual exclusions, the boolean-needs-a-value CLI quirk, examples).

## Lifecycle (entrypoint → shutdown)

`cmd/murtaugh/main.go`:

1. Extracts the global `--config` flag from `os.Args` (supports
   `--config PATH` and `--config=PATH`).
2. Resolves the config path (`config.DefaultPath()` →
   `~/.config/murtaugh/config.yaml`, overridable with `--config`).
3. Selects the mode: `slack gateway` → `ModeGateway`, `mcp` → `ModeMCP`, or
   any other tokens (including `slack <tool>`) → `ModeCLI`. No subcommand, or
   a bare `slack`, prints usage rather than launching anything.
4. `config.Bootstrap(path)` seeds the config directory on first run
   (`config.yaml` + `.env` + templates; the former YAML siblings are no longer
   seeded). Then `store.Bootstrap(ctx, path, setup)` resolves the running
   config: it parses the slim bootstrap file (`config.LoadBootstrap` →
   `oauth:` + `database:`), auto-migrates a legacy YAML tree into a fresh store
   on first upgrade, opens the `config.Store`, and (outside setup invocations)
   loads + validates the whole config from it. It returns the assembled
   `config.Config` and the open store, which `main` closes on shutdown.
5. Builds an `slog.Logger` (text handler; debug level when
   `configuration.debug: true`; warn level for CLI mode so tool output
   dominates the terminal).
6. Creates a `signal.NotifyContext` for `SIGINT`/`SIGTERM`.
7. `app.New(...)` builds the Registry and the chosen frontend; `Run(ctx)`
   blocks until the context is cancelled or the frontend returns.

## Configuration (`internal/config` + `internal/config/store`)

Configuration is split between a slim on-disk **bootstrap file** and a
**config store** (a database). Only the credentials and the store connection
live on disk; everything else lives in the store.

**On disk** — `~/.config/murtaugh/config.yaml`, two blocks only:

- `oauth:` — Slack tokens (`app_token`/`bot_token`/`user_token`), each a
  `${VAR}` reference resolved from the sibling `.env`.
- `database:` — the config-store backend (`config.DatabaseConfig`): `backend:
  sqlite` (default; `sqlite.path`, defaulting to
  `~/.config/murtaugh/config.db`) or `backend: postgres` (`postgres.dsn`,
  a `${VAR}` reference into `.env`).

`config.LoadBootstrap(path)` parses *only* these two blocks, loads `.env`, and
expands the `${VAR}` references. It does **not** read the store or validate — it
is the minimal step that yields the credentials + the store connection. The
returned `Config` carries `OAuth`, `Database`, and `BaseDir`; every other
section is left zero for the store to fill.

**In the store** — the bulk of configuration: agents, MCP servers, jobs, chat
routing, access (admin/allowed users), runtime defaults, journal, troubleshoot,
and workflow/unfurl rules. `Config` is still the root struct these assemble
into; the store-backed sections carry `yaml:"-"` tags because they are no longer
parsed from YAML. Collection entities (agent, mcp, job, workflow-rule,
unfurl-rule) are `config_items` rows keyed by `(section, name)`; the rest are
singletons.

**The store seam** (`config.Store`, implemented in `internal/config/store`):

- `store.Open(ctx, config.DatabaseConfig)` opens the backend and returns a
  `config.Store`. A `Dialect` (`internal/config/store/dialect.go`) abstracts the
  SQLite vs Postgres SQL differences (placeholders, JSON column type, `now()`),
  so one store implementation serves both.
- `store.Bootstrap(ctx, configPath, setup)` is the single startup entrypoint
  that replaced the old `config.Load`: parse the bootstrap file, migrate a
  legacy YAML tree on first upgrade (below), open the store, and — unless
  `setup` is true — load + validate the whole config from it.
- `config.AssembleFromRows(base, items, singletons)` is the **validated core**:
  it merges the bootstrap `Config` with the store rows into a full `Config` and
  runs `Config.Validate()`. `Store.Load` calls it; so does every `cfg` mutation.
  Validation covers required Slack tokens, agent routing references, durations,
  and per-rule checks — a single place, regardless of where the rows came from.

**YAML → DB auto-migration.** A bootstrap file predating this feature has no
`database:` block (`DatabaseConfig.IsZero()`). On the first non-setup run,
`store.Bootstrap` calls `migrateFilesToStore`: it loads and validates the full
legacy config from the on-disk siblings (`agents.yaml`, `jobs.yaml`,
`journal.yaml`, `workflow-rules.yaml`, `unfurl-rules.yaml`, `troubleshoot.yaml`),
writes every non-credential section into a fresh SQLite store, rewrites
`config.yaml` down to `oauth:` + `database:` (keeping the `${VAR}` references,
never the secrets), and **archives** the now-migrated siblings to
`~/.config/murtaugh/migrated-<timestamp>/` (moved, never deleted). It then
re-reads the rewritten bootstrap so it points at the new store.

**`cfg` tools** (`internal/tools/cfg`) are the store's write surface, exposed on
both the CLI (`murtaugh cfg …`) and MCP (`cfg.*`). Every mutation is
**validate-and-rollback**: it upserts the row, re-loads and validates the whole
assembled config via `AssembleFromRows`, and on failure restores the prior row
(`upsertItemValidated`/`putSingletonValidated` in `internal/tools/cfg/deps.go`),
so a bad edit can never leave the store in an unloadable state. `cfg db migrate`
copies the store into the other backend and rewrites the `database:` block.

The runtime loads config **once** at startup — a `cfg` change requires a daemon
restart to take effect.

### Multi-agent routing

Agent profiles and the `chat` routing config now live in the store (agents as
`config_items`, `chat` as a singleton). Routing is unchanged:

1.  **Direct Messages**: Use `chat.defaults.dm_agent` if set, otherwise
    `chat.defaults.agent`.
2.  **Channels**: `chat.channels` is an **ordered** rule list; the **first** rule
    whose `match` selects the channel wins. Use its `agent` if set, otherwise
    `chat.defaults.agent`. The matched rule's `reply_on_thread` (falling back to
    `chat.defaults.reply_on_thread`, default `true`) decides whether the reply is
    threaded or posted directly in the channel, and its `allow_anyone` decides
    whether the channel's chat surface is open to non-allowlisted users.

`chat.defaults.agent` is required when `chat.enabled: true`. The legacy map shape
for `chat.channels` still decodes — `config.ChannelRules` converts it to the list
reproducing the old precedence — and is rewritten as a list on the next save.

Channel admission (both the `allow_anyone` waiver and the `no_mention` waiver)
runs **off the socket goroutine**, because judging a non-allowlisted author needs
the channel NAME and resolving an uncached one costs a `conversations.info` call.
The socket goroutine keeps only cheap work; dedup stays after the authz checks so
a message that is about to be dropped never consumes the slot its twin needs.

Interactive callbacks share that authority ladder via `interactionAdmission`:
`admissionAllowlisted` (admin/allowed_users) reaches the whole surface, while
`admissionChannelGuest` (admitted only by a channel's `allow_anyone`) reaches
just the broker prompts — the `ask` tool and the tool-approval gates, which both
post through `internal/slack/interaction`. Built-in controls and the workflow
engine stay allowlisted-only. The allowlisted case dispatches inline so the
`ask` modal's short-lived `trigger_id` is never spent on a name lookup.

### Triggers and actions

`TriggerConfig` has a **custom `UnmarshalYAML`** that requires a mapping with
exactly one key — `reply-to-slack` (→ `ReplyToSlackTriggerConfig`), `run`
(→ `RunTriggerConfig`), or `delegate-to-agent` (→ `DelegateToAgentConfig`). Any
other key is rejected at parse time.

- `ReplyToSlackTriggerConfig` — exactly one of `template` (path), a nested
  `run`, or `delegate-to-agent`.
- `RunTriggerConfig` — `cmd`, `args`, `timeout`, `workdir`.
- `DelegateToAgentConfig` — `agent` (must exist as a stored agent) + `prompt`.
  Nested in `reply-to-slack` or an unfurl action it captures JSON output; as a
  top-level trigger it is fire-and-forget.

`UnfurlRuleConfig` = `Match` (`channels`, `domain`, `url_prefix`, `url_pattern`)
+ `Unfurl` (exactly one of `template`, `run`, or `delegate-to-agent`).
`validateUnfurlRule` requires at least one content condition, a compilable
`url_pattern`, non-blank channel entries, and exactly one action.

`JobProfile` runs **either** a `command` **or** an agent (`agent` + `prompt`,
mutually exclusive). An agent job is fire-and-forget; its prompt supports
positional `{{ N }}` placeholders filled from the run-time/configured args.

## Slack gateway (`internal/slack/gateway`)

`Gateway` owns the `*slack.Client`, the `*socketmode.Client`, and the four
subsystems (`handler`, `workflow`, `chat`, `unfurl`). `New()` wires them:

- `chat` is built **only if** `acp.enabled` is true.
- `unfurl` is built **only if** `unfurl-rules` is non-empty (a bad matcher logs
  and disables the feature rather than crashing).
- `workflow.NewEngine` always exists; with no rules it simply matches nothing.
  The ping → pong self-test is **not** a workflow rule: it is owned by the
  gateway (`internal/slack/gateway/ping.go` + `internal/slack/pingcard`), handled
  before the engine, so it cannot be redirected by config or template edits.

### Event loop

`Run` launches `socket.RunContext` in a goroutine, warms the ACP client, then
selects over `socket.Events`, dispatching each to `handleEvent`:

| Socket event            | Handler                | Behaviour                                            |
|-------------------------|------------------------|------------------------------------------------------|
| `Connected`             | `notifyConnected`      | Greets once: resumes a pending restart (edits the notice into the back-online ping card) **or** sends the startup ping — never both. |
| `SlashCommand`          | `handleSlashCommand`   | `/...  chat` → ACP chat; otherwise the default handler acks. |
| `Interactive`           | `handleInteractive`    | Acks, then runs `workflow.Execute` in a goroutine (5 min).   |
| `EventsAPI`             | `handleEventsAPI`      | Routes inner events (below).                          |

Inner Events API events:

- `LinkSharedEvent` → `handleLinkShared` (unfurl subsystem).
- `AppMentionEvent` → `startChat` (skipped if chat disabled or sender is a bot).
- `MessageEvent` → `startChat` **only** for direct messages
  (`ChannelType == "im"`, no bot/subtype).

Handlers **ack first, then work asynchronously** in a goroutine with a bounded
context. Long work must never block the event loop.

## Agent chat (`internal/agent` + `slack/gateway/chat_handler.go`)

`agent.Client` is the backend interface (`Initialize`, `NewSession`, `Prompt`,
`Cancel`, `Close`). There are **two implementations**, selected per agent by
each stored agent's `kind:` (default `native`); `agentbuild.Client` is the single place
the choice is made, shared by the gateway and the `agentdelegate` runner:

- **`agent.ProcessClient`** (`kind: acp`) drives an **external** agent process by
  speaking **JSON-RPC over its stdio** (NDJSON): requests carry an incrementing
  id, responses are matched via a `pending` map, and `session/update`
  notifications fan out to per-session subscriber channels as `Event`s.
- **`agent/native.Client`** (`kind: native`) runs the agent loop **in-process**:
  it owns the provider conversation array (`internal/llm` over `litellm` —
  gemini/anthropic-compat/openai-compat), executes tools itself, and emits the
  same `Event`s. Its tools are resolved per-agent by `internal/toolset` from the
  `tools:` allowlist (native `files`/`terminal`/`skills` rooted at the agent's
  workdir + registry namespaces) plus any attached `mcp_servers:` (external MCP
  servers via `internal/mcpclient`).
  - **Prompt layout (caching-aware).** The **system prompt is static**: the base
    prompt + a stable skills index (the allowlisted skills' name + description,
    so the agent knows what it can load). Static so providers cache the
    system+tools prefix across turns and conversations. The **volatile per-turn
    context** (time, cwd, Slack channel/thread) is folded into the *current user
    message* instead — never a standalone message, so `native.Conversation`
    (which exposes no API to do otherwise) and `assertNoConsecutiveUserAfterTool`
    keep the array clean. That single design serves both goals: the structural
    fix for the consecutive-`user` empty-completion bug, and a cacheable prefix.
    Caching is requested via `llm.Request.CacheRetention` (default `5m`), gated to
    Anthropic/OpenAI in the provider layer (Gemini rejects the extra and caches a
    static prefix implicitly).
  - Provider credentials come from `~/.config/murtaugh/.env` (`api_key_env` names
    the variable); secrets never live in YAML.

Both implementations satisfy the same interface, so `SessionManager`, the Slack
`ChatHandler`, streaming, and the journal are identical across backends.

**Context-window management (native).** A native session's conversation would
otherwise grow unbounded across turns. `native/compaction.go` keeps it within a
per-agent token budget (`context_limit`, defaulting per provider family via
`llm.DefaultContextLimit`). Before each provider turn the loop compares the
estimated prompt size — and the provider-reported input-token count from the
prior turn, which is authoritative — against a high-water mark (¾ of budget) and,
when over, compacts down toward a low-water mark (½). Two strategies, set per
agent by `compaction:`: **truncate** (default, always-on safety net) drops the
oldest whole turn-groups, cutting only on user-message boundaries so tool
pairings stay intact and the array still starts with a user message and never
violates `assertNoConsecutiveUserAfterTool`; **summarize** LLM-compresses the
oldest groups into a `<conversation-summary>` message, falling back to truncation
if the summary call fails. The token count is tracked per-`Conversation` (not the
`Loop`, which is shared across a client's sessions).

`SessionManager` caches sessions keyed by `ConversationKey`
(`TeamID`/`ChannelID`/`ThreadTS`/`DM`). It initializes the client lazily (or via
`Warm`), evicts on idle timeout / `max_sessions`, and reuses sessions so a Slack
thread maps to one persistent agent conversation.

`ChatHandler.Handle` builds the key + `SessionMetadata`, sets the assistant
status to `is thinking...`, then ranges over the prompt's event channel.

## Streaming (`slack/gateway/stream_api.go` + stream writer)

`StreamAPI` abstracts the Slack streaming surface so the chat handler is
testable: `StartStreamContext`, `AppendStreamContext`, `StopStreamContext`, and
`SetAssistantThreadsStatusContext`. `*slack.Client` satisfies it in production.

The stream writer is **lazy**: the live message is only started on the **first
non-empty text chunk**, appends are batched by `stream_append_interval` /
`stream_min_chunk_chars`, and the stream is stopped on `complete` (or `Fail`ed on
error). Streaming requires a source message timestamp; without one `Handle`
returns an error. A `ConversationKey` requires a thread timestamp for replies.

## Workflow engine (`internal/workflow`)

`Engine` holds rules sorted by name (deterministic, first-match-wins), a
`ResponsePoster`, a `CommandRunner`, and the template search roots. `Execute`
marshals the `slack.InteractionCallback` to both a `map[string]any` (for
matching) and JSON (for `run` stdin), finds the first `interactive` rule whose
`match` is a subset of the payload, and runs its triggers in order:

- `reply-to-slack` → render JSON (template or nested `run`) and POST it to the
  interaction's `response_url`.
- `run` → execute the command with the payload JSON on stdin.

**Template rendering** uses Go `text/template` with `Option("missingkey=error")`,
so every field referenced in a template must exist in the data
(`{"Payload": payload}`). Output must be valid JSON (`validJSON`).

`CommandRunner.Run(ctx, RunTriggerConfig, stdin []byte) ([]byte, error)` is the
external-process contract. `OSCommandRunner` enforces a timeout (default 30s),
pipes stdin in, and captures stdout. **Convention: handlers read a JSON object on
stdin and print a single JSON object on stdout.**

## Block Kit rendering (`internal/jsontemplate` + `slack/client.DecodeBlocks`)

**Never build a Slack message's blocks with slack-go's typed builders when the
payload uses a block type newer than the pinned release** — today that means
`container`, `card`, `child_blocks`, `callout`, and `rich_text`. Render a JSON
template instead and let the client pass the bytes through untouched.

This is not a style preference. slack-go decodes recognised block types into
typed structs, and `encoding/json` silently discards any field those structs do
not declare. A payload using a newer Block Kit feature therefore loses those
fields on the way out **with no error at all** — the message simply arrives
missing pieces. (Wholly unrecognised block *types* are safe: slack-go keeps them
verbatim in `UnknownBlock`. It is the partially-known types that lose data.) The
rationale is restated at the `rawBlock` declaration in `slack/client/blocks.go`.

The path is:

```
assets/templates/<area>/<state>.json     ← the document, structure and all
        │  jsontemplate.Renderer.Render  ← config dir first, then assets.FS
        ▼
   rendered []byte
        │  slacklib.PostMessageParams.Blocks / UpdateMessageParams.Blocks
        ▼
   client.DecodeBlocks → rawBlock        ← marshals back byte-identical
```

Conventions that fall out of it:

- **The template owns the structure**, including conditionals and repetition.
  `templates/auth/admin.json` is the reference: nested `{{ if }}` emitting
  comma-correct JSON, `{{ json .Field }}` for whole values. Do not assemble
  `child_blocks` in Go and inject them as one blob — the point of a template is
  that an operator can restyle the card without a rebuild.
- **Every interpolated value goes through `json` or `jsonstr`.** `text/template`
  performs no escaping, so a bare `{{ .Field }}` inside a JSON string literal
  lets a crafted value close the string and append sibling blocks — producing
  *valid* JSON with attacker-chosen structure that no validity check can catch.
  Agent-supplied text (tool names, questions, option labels) is untrusted.
- Templates parse with `missingkey=error`, so a typo'd placeholder fails loudly
  rather than rendering a half-built document.
- **Modals are the exception**: `views.open` takes a typed `ModalViewRequest`, a
  different API surface from the raw-blocks message path, so those stay
  slack-go-typed (see `authcard.CodeModal`).

`internal/slack/authcard` is the worked example of a card package: a `Renderer`
over `jsontemplate`, a `State` enum whose terminal values stop further edits,
correlation carried in the buttons' `action_id` namespace, and a `Flow` that
posts, blocks on a rendezvous, and rewrites the card to its terminal state.

## Custom link unfurling (`internal/unfurl` + `slack/gateway/link_unfurl_handler.go`)

- `Matcher` compiles rules once (sorted-key order). `Match(url, domain, channel)`
  returns the first rule whose optional channel allowlist, domain (exact or
  subdomain suffix), `url_prefix`, and RE2 `url_pattern` all match. Named regex
  groups are returned as `Captures`.
- `Renderer` turns a Block Kit JSON template into a `slack.Attachment`. Lookup,
  escaping and execution are delegated to `internal/jsontemplate` (config dir
  first, then the embedded `assets.FS`); this package only decodes the result.
  The template/`run` data is the exported `Data` struct (`URL`, `Domain`,
  `Channel`, `User`, `MessageTS`, `ThreadTS`, `TeamID`, `Captures`).
  `ParseAttachment` rejects non-JSON output — that decode *is* the validity
  check, which is why `jsontemplate.Render` returns unvalidated bytes.
- `LinkUnfurlHandler.Handle` skips composer-mode events (non-numeric timestamp)
  and the bot's own links, dedupes URLs, caps at 10, builds each preview
  (`run` → JSON stdin/stdout, or `template` → render), isolates per-link
  failures, and posts one `chat.unfurl` (`UnfurlMessageContext`).

Each `match.domain` must be registered in the Slack app's **App Unfurl Domains**
list (max 5) or no `link_shared` event is delivered.

## Event journal (`internal/journal`)

The journal is the **agent-facing** record of what Murtaugh did — a structured,
queryable event store backing Gateway Debug Mode (and, later, persistent ACP
session logs). It is deliberately distinct from `slog`: `slog` → stderr is for a
human tailing the daemon; the journal is for an AI agent (or admin) issuing
filtered queries. Two lanes, never conflated.

- **Store** (`store.go`) — SQLite via the pure-Go `modernc.org/sqlite` driver
  (no CGo). One `events` table partitioned **logically** by a `stream` column
  (`gateway`, `job`, `acp_session`); WAL + `busy_timeout` so the CLI/MCP reader
  processes never block the daemon's single writer. Exposes `Query`, `Stats`,
  and `Prune` (age-based, per the retention passed at `Open`).
- **Recorder** (`recorder.go`) — the write seam every domain package depends on.
  `AsyncRecorder` never blocks the caller: `Record` enqueues onto a bounded
  buffer and a single goroutine drains it in batched transactions (the
  single-writer model SQLite wants). A disabled stream or a full buffer **drops**
  (surfacing the count) rather than waiting — observability must not backpressure
  a Slack turn. `NopRecorder` backs disabled streams so call sites never branch.
- **Correlation** (`context.go`) — the gateway mints a `corr_id` at interaction
  ingress and carries it on the context; the workflow engine and unfurl handler
  stamp it on their events, so every event from one interaction ties together.
- **Recording points** — `workflow.Engine` (match / no-match / per-trigger
  outcome), the unfurl handler (`unfurl.*`), the gateway ingress (`slash.command`,
  `interactive.received`), `jobs.run` (`job.run`, the `job` stream), and the
  `ChatHandler` (`session.turn`, the `acp_session` stream). All take a
  `journal.Recorder`, defaulting to no-op.
- **Transcripts & blobs** (`blob.go`) — ACP chat is full-fidelity: each turn's
  prompt and response go to a per-session NDJSON transcript file under
  `blob_dir`, referenced from the row's `blob_ref`; the row keeps only the
  queryable envelope + metrics. `BlobStore` does the appends; the gateway's
  `sessionLogger` (`session_log.go`, wired only when `acp_session` is enabled)
  ties the transcript write to the row. `Store.Prune` removes a transcript file
  once its last referencing row ages out (`WithBlobDir`).
- **Lifecycle & sweeper** — `main` opens the store and builds the recorder for
  non-setup invocations (fail-soft: a store that can't open degrades to no-op and
  logs a warning), draining on shutdown. The daemon (single writer) runs the
  retention **sweeper** in `Gateway.startJournalSweeper` — once at startup and
  every configured `sweep.every` — reusing that same store; `journal.prune`
  is the manual equivalent.
- **Config** — the journal settings are a singleton in the config store (read
  with `cfg journal show`): per-stream `enabled` (a `*bool` so streams default
  on and opt out with `enabled: false`) and `retention`, plus the DB `path` and
  `sweep` cadence. The `JournalConfig` type still lives in
  `internal/config/journal.go`. Note this is the **event journal's** SQLite DB,
  a separate database from the config store.

## Assets and embedding (`internal/../assets`)

`assets/assets.go` embeds reference files via
`//go:embed config.yaml env.example system-prompt.md AGENTS.md cli-help.md templates skills troubleshoot`.
The embedded `config.yaml` is the slim bootstrap default (`oauth:` +
`database:`); the former YAML siblings are no longer embedded or seeded, since
that configuration now lives in the config store.
Block Kit templates live under `templates/` (`unfurl/`, `auth/`, `ask/`) — see "Block Kit
rendering" above for why a card is a template rather than Go builders. The
ping → pong card is built in Go (`internal/slack/pingcard`), not a template:
its blocks are all types the pinned slack-go models. Bundled agent skills
live under `skills/`, each a `SKILL.md` + `reference/` + `examples/` tree.
`cli-help.md` is the canonical command reference (see "CLI/MCP command
reference" above). The `templates` and `skills` directories are embedded
recursively.

The embedded FS is the **fallback** template source: the workflow engine and unfurl renderer both
look in the config directory first, then `assets.FS` — so a config template path
like `templates/unfurl/github-pr.json` resolves to `<workspace>/templates/unfurl/github-pr.json`
on disk, falling back to the same path inside `assets.FS`. **If you add a new asset
directory, the recursive `templates`/`skills` embeds cover nested files, but a new
top-level asset must be added to the `go:embed` directive** or it will not ship in the binary.

`config.Bootstrap` runs on every start. It mirrors `templates/` into
`<workspace>/templates/` and `skills/` into `<workspace>/.agents/skills/`, then
symlinks `<workspace>/.claude/skills` to `.agents/skills` so both ACP and
Claude-based agents discover the bundled skills. The two trees use different
copy policies (`copyPolicy` in `bootstrap.go`): **config files and `templates/`
are `preserveExisting`** (seeded once, never overwritten, since they hold the
user's tokens/edits), while **`skills/` is `refreshFromAssets`** — shipped skill
files are rewritten in place when their content drifts from the embedded copy,
so a binary upgrade keeps the workspace skills current. Bootstrap only ever
writes files it embeds, so user-added skills are never deleted; an unchanged
file is a no-op (no mtime churn). The `BootstrapReport` buckets each path as
Created, Updated, or Preserved.

## Testing conventions

- Tests are standard `go test` table tests colocated with each package.
- Interfaces (`StreamAPI`, `ChatSessionManager`, `Unfurler`, `CommandRunner`,
  `ResponsePoster`, `acp.Client`) exist primarily so handlers can be driven by
  **fakes/stubs** (`fakeStreamAPI`, `fakeChatSessions`, fake `Unfurler`,
  `stubRunner`). Add a fake rather than reaching for real Slack/process I/O.
- Use `discardLogger()` (a `slog.Logger` over `io.Discard`) in tests.
- Config behaviour is verified by parsing YAML strings through `Parse` and
  asserting on both success and the specific validation error.

## Guidelines for making changes

1. **Work in a dedicated git worktree** under `ignore/worktrees/`, branched off
   the up-to-date local `HEAD`. Never push without explicit permission.
2. **Keep config changes complete.** A new config field means: struct field +
   tag, `Validate()` coverage (run through `AssembleFromRows`), store read/write
   support in `internal/config/store` (and a `cfg` flag if it's user-editable),
   migration coverage for the legacy-YAML importer, a README note, and tests in
   `config_test.go`.
2b. **Keep command docs complete.** A new/changed/removed tool or flag means
   updating its section in `assets/cli-help.md` (and the command list in
   `internal/help/help_test.go`). See "Adding a new tool".
3. **Trace downstream effects.** New Slack events need a `handleEvent` /
   `handleEventsAPI` case; new ACP event types need handling in `ChatHandler`.
4. **Respect the JSON contracts.** Templates and `run` handlers must emit valid
   JSON; `missingkey=error` means every referenced template field must be present.
5. **Embed new assets** by updating the `go:embed` directive.
6. **Validate before finishing:** `go build ./...`, `go vet ./...`, and
   `go test ./...` must all pass. Add or update tests for the behaviour you change.
7. **Match the surrounding style** — small, focused types; constructor functions
   that default optional dependencies; bounded contexts for async work.
