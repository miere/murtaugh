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
  the Slack tools under the `slack` namespace (`murtaugh slack send_msg`, …).
- **MCP** — JSON-RPC stdio server (`murtaugh mcp`) that exposes every
  registered tool to AI clients.

Module path: `github.com/miere/murtaugh`.

## High-level architecture

```
       ┌──────────────┐        ┌──────────────────────┐
       │ cmd/murtaugh │        │ cmd/murtaugh-gateway │
       └──────┬───────┘        └──────────┬───────────┘
              │  app.Agents{local}        │  app.Agents{} (empty)
              └────────────┬──────────────┘
                    ┌──────▼───────┐
                    │ internal/app │   ← composition root
                    └──────┬───────┘
            builds Registry + selects Mode
   ┌───────────────────────┼───────────────────────┐
   │                       │                       │
┌──▼─────────────┐  ┌──────▼──────────┐  ┌─────────▼──────────┐
│ frontends/cli  │  │ frontends/mcp   │  │ slack/gateway      │
└──┬─────────────┘  └──────┬──────────┘  └────────────────────┘
   │                       │
   └──────► Tool ◄─────────┘
             (internal/tools/*)
```

- `internal/app` is the only place tools are wired into the registry.
- CLI and MCP frontends know nothing about each other and reach tools only
  through `tools.Tool`.
- The Slack gateway does not use the Tool registry today; it runs side-by-side
  as a third frontend selected by mode (`ModeGateway`). The `slack.*` tools,
  by contrast, are ordinary registry tools shared by the CLI and MCP frontends.
- **The agent machinery is injected, not imported.** `internal/app` names no
  agent backend; each entry point hands it an `app.Agents` (a pair of
  constructors). `cmd/murtaugh` passes `internal/agentruntime/local`, so the CLI
  and today's `murtaugh slack gateway` behave exactly as before.
  `cmd/murtaugh-gateway` passes either the zero value or a **broker** over
  attached runtime nodes, and therefore links nothing that can run a model
  either way — CI proves it (see "The gateway reachability rule"). A third
  binary, `cmd/murtaugh-runtime`, is the runtime-node entry point: it dials a
  gateway, holds the connection open and serves one agent over it.

## Repository layout

```
cmd/murtaugh/         Entry point: flag parsing, mode selection, signal handling.
                      The one binary that ships today; keeps its local agent.
cmd/murtaugh-gateway/ The Slack gateway alone, linking no agent machinery.
                      `-node-listen ADDR` opens the node endpoint; empty (the
                      default) accepts none and the process binds nothing.
cmd/murtaugh-runtime/ The runtime-node entry point. May reach the agent
                      packages. `-gateway URL` is required: it dials in.
internal/app/         Composition root + Registry wiring. Names no agent
                      backend: `app.Agents` is injected by the entry point.
internal/frontends/   CLI and MCP adapters over the Tool registry.
  cli/                Human frontend: kebab → snake flag mapping, render dispatch.
  mcp/                MCP stdio adapter wrapping the same Registry.
internal/tools/       Shared Tool interface + one package per tool.
  tool.go             Tool interface + Registry.
  ping/               Health-check tool (the canonical example).
  jobs/run/           Tool `jobs.run`: execute a job stored in the config store.
  jobs/define/        Tool `jobs.define`: register a job in the config store.
  journal/            Tools `journal.query`/`.stats`/`.prune`: inspect the event journal.
  slack/              Slack tools: send_msg, fetch_msgs, fetch_reactions, update_msg.
  cfg/                Tools `cfg.*`: read/write the config store (agents, mcp,
                      jobs, chat, access, rules, defaults, db migrate) with
                      validate-and-rollback on every mutation.
  node/               Tools `node.token.mint`/`.list`/`.revoke`: issue and
                      withdraw the bearer credentials runtime nodes present.
internal/nodetoken/   Node credentials: mint, hash at rest, constant-time
                      verify, and the on-disk credential file's location/mode.
internal/nodesocket/  The WebSocket transport under a link: one dialler (node),
                      one upgrader (gateway), a write deadline per write.
internal/nodehost/    The gateway's inbound edge: the accept endpoint, the one
                      nodetoken.Verify on a serving path, the registry of
                      connected nodes and what each claims, and the runtime
                      builder that brokers to them.
internal/nodeserve/   The node's half: answers the six requests over one link,
                      streams a turn's events back, gates native tool calls
                      through the gateway's approver, proxies Murtaugh's own
                      tools back to the gateway one call at a time, and holds
                      what this node advertises.
internal/nodeclaim/   The node's claim: what its configuration means as an
                      advertisement, and the poller that notices an edit.
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
  approvalcard/       The approval card — a gated tool call, or a held job's
                      first run — from templates/approval/*.json.
                      Rendering only — the interaction broker keeps the
                      lifecycle and calls in through its CardRenderer hook.
  alertcard/          The non-interactive alerts Murtaugh sends about itself
                      (error / warn / info), from templates/alert/alert.json.
                      Always collapsed; renders to blocks or to plain text.
  configcard/         The configuration-change approval card: a YAML diff plus
                      Apply / Rollback, from templates/config/update.json.
  agentcard/          The "no agent configured" prompt and the button that opens
                      the setup form, from templates/agent/setup.json.
internal/agent/       Agent backend interface, session manager, protocol types,
                      and the shared tool watcher / execution ceiling.
  native/             In-process LLM agent loop (kind: native): conversation,
                      turn loop, system prompt, recovery.
  acp/                External ACP agent over a subprocess (kind: acp).
  claudecode/         Claude Code stream-json backend (kind: claude_code).
  remote/             agent.Client over a link to a runtime node. Runs no
                      model; nothing selects it yet.
internal/agentbuild/  Kind-aware backend builder (native / ACP / claude_code).
internal/agentruntime/ The seam between the gateway and the machinery that
                      builds and runs agents: the types both ends name, and
                      nothing that can run a model.
  local/              The in-process implementation: backends, session managers,
                      the delegation runner and the MCP aggregator. The one
                      package that decides whether a binary can run an agent.
internal/agentwire/   The serialisable form of the agent event abstraction: the
                      protocol a runtime node speaks to the gateway, plus the
                      Encoder/Decoder that translate to and from agent.Event,
                      and the request direction (the six methods and their
                      answers).
internal/nodelink/    The envelope that wraps those payloads: sequencing,
                      acknowledgement, backpressure, loss detection. Imports
                      nothing of ours.
internal/election/    Leader election: the lifecycle runner, the suspend-safe
                      gate, and the journal of promotions and stand-downs.
internal/onboarding/  The agent-setup form's domain: provider catalogue, model
                      discovery, the tool-family and confinement catalogues, and
                      the two profiles a completed form produces.
internal/jsontemplate/ JSON-document templating with JSON-safe escaping funcs.
                      The single renderer behind every Block Kit template.
internal/llm/         Provider-agnostic LLM boundary over litellm (gemini /
                      anthropic-compat / openai-compat), and the mapping from
                      litellm's error onto the providerfail vocabulary.
internal/providerfail/ What a provider failure is reduced to (kind, provider,
                      status, message, retryable) and how it is worded for a
                      human. A leaf: no litellm, nothing of ours.
internal/claudeauth/  Recognises, from its prose, a Claude Code failure a
                      re-authentication would fix. Read by the gateway's
                      credential-repair path, not by the backend.
internal/toolset/     Per-agent toolset resolver (native tools + registry + MCP).
internal/mcpclient/   External MCP client: remote tools as tools.Tool.
internal/agentdelegate/ One-shot isolated agent runner (delegate-to-agent).
internal/workflow/    Workflow engine, command runner, template rendering.
internal/unfurl/      Link matcher and Block Kit attachment renderer.
assets/               Embedded reference config, JSON templates, agent skills.
```

`internal/*` is private to this module. Cross-package dependencies flow in one
direction: `slack/gateway` orchestrates `config`, `agentruntime`, `workflow`,
`unfurl`, `slack/interaction`, and `slack/authcard`; those packages do not
import `slack/gateway`. It reaches no agent backend and no delegation runner:
what it has is an `agentruntime.Runtime` handed to `New`, whose session
managers, delegator and tool surface it consumes without knowing what built
them. `jsontemplate` sits at the bottom — it knows nothing of Slack and is
imported by every template renderer above it. The `agentdelegate` runner builds
on `agentbuild` and backs every delegate-to-agent surface (the `workflow`
engine, the `unfurl` handler, and the `jobs.run` tool consume it through small
local interfaces). Tool packages depend on `config` where they need shared types
(e.g. `JobProfile`), never the other way around.

### The gateway reachability rule

`cmd/murtaugh-gateway` must not be able to reach `internal/agentbuild`,
`internal/llm` or `internal/agent/{acp,native,claudecode}` **by any import
path** (#170 Change E: the gateway is *incapable* of serving an AI-backed
request, not merely disinclined to). That is a property of the whole import
closure, so it is not a `go/analysis` pass — those run per package. It is a
`go list -deps` grep, run in CI ("Gateway reachability rule") and mirrored by
`internal/archtest/reachability`, whose decay test fails if the two copies of
the forbidden list drift apart.

`cmd/murtaugh` is the positive control and always violates the pattern: the CLI
keeps its local agent on purpose, so `murtaugh jobs run x` works with no gateway
and no node. A check that cannot demonstrate a failure is not a check.

Three consequences are load-bearing and easy to undo by accident:

- The gateway wants the *words* for a provider failure without the machinery
  that produces one, so the vocabulary lives in `internal/providerfail` and
  `internal/llm` keeps only the litellm mapping. `internal/agent/native`
  classifies at the point of failure (`eventError` → `llm.CarryFailure`) and
  every reader downstream — the alert card, the wire encoder — reads the carried
  classification. Classifying at the reader would work in-process and silently
  stop working across a node link, where no `*providers.LiteLLMError` survives.
- Deciding whether a Claude Code failure warrants re-authentication is gateway
  policy, so the prose matcher lives in `internal/claudeauth`, outside the
  backend it describes.
- `agentdelegate.ErrNonJSONOutput` moved to `internal/agent`, because a caller
  branching on it (the workflow engine) would otherwise reach `agentbuild` three
  hops down for one sentinel.

`agentwire` is a top-level sibling of `agent` for the same reason `agentbuild`
and `agentdelegate` are: everything **under** `internal/agent/` is a backend
implementing `agent.Client` or backend support, and a codec is neither. It
imports `agent` and `providerfail` and no backend, and no provider client — an
error's backend-specific structure reaches it through an interface it declares
(`RPCFaulter`) that the backend satisfies structurally, and a provider failure
reaches it already classified. That is what makes it importable by a gateway
that may not link litellm; before the vocabulary was split out of `llm`, this
package's own stated goal was one it did not meet. Its types **derive from**
`agent.Event` rather than being
it: two types with an explicit translation, so renaming a field on `agent.Event`
stays a refactor instead of becoming a wire-compatibility event.

`nodelink` is the envelope that carries those payloads, and it imports **nothing**
of ours — a test over its direct imports fails the build if that ever changes.
That is what makes "sequencing and acknowledgement live outside the payload" a
property rather than a convention: `agentwire` can be proven to preserve meaning
with no transport in the room, and `nodelink` can be proven to deliver with no
idea what it is delivering. `internal/agent/remote` is the one place they meet.

`internal/agent/remote` sits **under** `internal/agent/` even though it runs no
model, because it is an `agent.Client` implementation and that is what lives
there. That placement is compatible with #170's proposed gateway reachability
rule — **not yet implemented**: there is no gateway binary to check and
`cmd/archcheck` carries only the workdir, renderclock and nodetoken passes —
because the
rule is specified to enumerate `internal/agent/{acp,native,claudecode}` rather
than to ban the whole subtree. When it is built it must be written that way, or
a gateway that imports `remote` will be read as reaching a backend.

`internal/nodetoken` mints, hashes and verifies the bearer credential a runtime
node presents (#190). It imports nothing of ours but `internal/config`, for the
store contract alone. `Verify` is now called on the serving path, exactly once,
by `nodehost` at the handshake and **before** the socket is upgraded — an
upgrade first would hand an unauthenticated peer a connection to hold and turn
the refusal into a close frame instead of an HTTP status. Its one arch rule — a digest is never compared with `==` — is a
`go/analysis` pass rather than a test, because a constant-time comparison and a
leaky one return identical verdicts and only a static check can tell them apart.

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
  `murtaugh jobs run` to the registered name `jobs.run`. The MCP frontend
  sanitises it to `[A-Za-z0-9_-]` for the LLM-facing id (`jobs.run` →
  `jobs_run`), because stricter providers reject a dotted name outright.
- A backend may publish a tool under an alias: the name of one of its own
  built-ins that it disables. `claudecode.ToolAliases` publishes `ask` as
  `AskUserQuestion` to Claude Code agents only, because Claude Code's built-in
  of that name needs a terminal UI and fails headlessly; `ask`'s `InputSchema`
  is that built-in's payload, field for field. Every other agent, and the
  standalone `murtaugh mcp` server, sees `ask`. Aliases are sanitised and
  collision-checked like any other name.
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
  `murtaugh help slack send_msg`) and `murtaugh <command> --help` print a single
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
   blocks until the context is cancelled or the frontend returns. The last
   argument is the `app.Agents` this binary is willing to link — for
   `cmd/murtaugh`, `internal/agentruntime/local`.

## Configuration (`internal/config` + `internal/config/store`)

Configuration is split between a slim on-disk **bootstrap file** and a
**config store** (a database). Only the credentials and the store connection
live on disk; everything else lives in the store.

**On disk** — `~/.config/murtaugh/config.yaml`, two blocks only:

- `oauth:` — Slack tokens (`app_token`/`bot_token`/`user_token`), each a
  `${VAR}` reference resolved from the sibling `.env`.
- `database:` — the config-store backend (`config.DatabaseConfig`): `backend:
  sqlite` (default; `sqlite.path`, defaulting to
  `~/.config/murtaugh/config.db`), `backend: postgres` (`postgres.dsn`,
  a `${VAR}` reference into `.env`), or `backend: firestore`
  (`firestore.project_id` / `database_id` / `collection` /
  `credentials_file`, all optional).

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
- **Firestore** (`firestore.go`) is a *separate* implementation, not a third
  dialect: the SQL store is written against `database/sql` and Firestore shares
  none of that. What it does share is the data's shape — the config store is
  already a document store — so the mapping is direct and there is no schema to
  migrate. Documents live in `<collection>_items` (keyed `section~name`) and
  `<collection>_singletons`; `updated_at` is a **server** timestamp, because
  writes arrive from several machines whose clocks need not agree.
  Authentication is Google ADC, so a node on GKE / Cloud Run / a Compute Engine
  VM needs nothing but `backend: firestore`; `credentials_file` overrides it
  with an explicit key file for hosts ADC cannot serve.
  Firestore exists because it is the backend a *distributed* deployment can
  actually reach: a Postgres store good enough for leader election would have to
  be reachable from every node, which for a workstation node means a tunnel to
  production — infrastructure the deployment does not otherwise need.
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

**Configuration is hot-reloaded, under admin approval.** The runtime no longer
loads config once and keeps it: leader election made that untenable, since a
standby can sit for a week before promotion, so "the config this process booted
with" and "the config the operator believes is live" drift arbitrarily far
apart — and the moment that matters is the moment nobody is watching.

The leader polls the store every 30s (`internal/app/config_watch.go`; polling
because no two backends offer the same change feed). A difference is rendered as
YAML, diffed, and DM'd to the admin as a `configcard` container block with
**Apply Modifications** / **Rollback**. Approval reloads; rejection — and
timeout, and an unreachable admin — writes the running config back over the
edit, so the next poll is quiet either way.

Three properties are load-bearing:

- **The rendering is deterministic**, and an empty singleton renders as absent.
  Instability would show as a phantom diff and train the admin to approve
  without reading; treating a written `{}` as present made the daemon re-detect
  its own rollback and prompt forever.
- **Rollback is `config.RevertToSnapshot`, not `Store.Restore`.** Restore
  upserts, so it would leave a newly-added agent standing while telling the
  admin their rejection was applied.
- **The click is admin-only**, re-checked in the handler. The router admits any
  allowlisted user to built-ins, and approving adopts whatever is in the store —
  possibly an edit that widens the allowlist itself.

Applying performs a **soft reload** (`Application.reloadConfig`): stop serving,
tear the gateway down, rebuild from the new config, start serving — all while
holding the leader lease, so the cluster sees no gap to fail over into. It is
not hot in the swap-a-value sense and cannot be: agents own backend process
trees decided at construction. The card says so in as many words. What it buys
over a process restart is narrow but real — no launchd round trip, no re-exec,
and leadership is never released. Everything that outlives a single gateway
reaches the current one through `gatewayHolder` rather than a captured pointer.

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

Authorship is **not** part of that ladder. Another app that @-mentions Murtaugh
is admitted or refused by `allowed_users` on exactly the same terms as a human,
so a bot earns access by being on the list and nothing else — there is no second
allowlist to keep in sync. The one authorship question the gateway does ask is
`isSelfAuthored`: an event we wrote ourselves is always dropped, because a reply
whose text contains our own `<@id>` re-enters as an `app_mention` and would reply
again without end. Identity comes from `auth.test` at construction; when that
call fails the check degrades to refusing every app-authored event, which loses
messages but cannot recurse.

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

**Delegation runs at chat parity.** Every delegate-to-agent surface (jobs,
workflow triggers, unfurls) shares ONE `agentdelegate.Runner`, built by
`agentruntime/local` with the same `agentbuild.Deps` a chat agent gets —
registry, MCP servers, workspace, and the MCP aggregator — and handed to the
gateway on the `Runtime` as a `Delegator` interface. The aggregator matters because it is
the only route by which an `acp`/`claude_code` agent reaches Murtaugh's tools:
without it a scheduled agent job starts happily and then cannot post its own
result. Two deps are withheld on purpose, both because no human is in a thread:
the `Approver` (an approval card nobody can answer would hang the turn until the
idle watchdog kills it — the agent's own `approval` policy is the only gate) and
the background-events sink. `ask`/`present_plan` fail for the same reason, by
design. The aggregator lives in the daemon, so a delegation fired straight from
the CLI still runs on the backend's own built-ins.

## Slack gateway (`internal/slack/gateway`)

`Gateway` owns the `*slack.Client`, the `*socketmode.Client`, and the four
subsystems (`handler`, `workflow`, `chat`, `unfurl`). `New()` wires them:

- `chat` is built **only if** `acp.enabled` is true.
- `unfurl` is built **only if** `unfurl-rules` is non-empty (a bad matcher logs
  and disables the feature rather than crashing).
- `workflow.NewEngine` always exists; with no rules it simply matches nothing.
  The ping → pong self-test is **not** a workflow rule: it is owned by the
  gateway (`internal/slack/gateway/ping.go` + `internal/slack/pingcard`), handled
  before the engine, so it cannot be redirected by config or template edits. Its
  button lives in the App Home control row, next to Restart.

### Leader election and failover (`internal/election` + `slack/gateway/leader.go`)

Always on, and not configurable off. Election is a property of the
configuration backend rather than a feature a deployment opts into: every
backend supplies a lock, and only its *scope* varies — SQLite gives one gateway
per machine (an OS advisory lock, released by the kernel on process death),
Postgres and Firestore give one per cluster (a renewed lease). The `election:`
block in the config store carries only the timings. A node that served without
contending would be the duplicate gateway the mechanism exists to prevent, so
every setup failure here is fatal rather than degraded.

`Gateway.Run` branches once: with no elector it calls `serve` and behaves
exactly as it always has; with one, the elector owns the loop and
`StartServing` / `StopServing` run beneath it on promotion and demotion. The
gateway depends on a two-method `LeaderElector` interface, not on the election
package — the composition root (`internal/app/leader.go`) is the only place that
knows about both halves, and it is where the promote/demote callbacks are built.

Three decisions carry the safety argument:

- **The lock is keyed on `team_id` + `bot_id` from `auth.test`**, resolved
  before contending. Not on the bot token: a token-derived key changes on
  rotation, so the incumbent would keep the old key's lock while a new node took
  a different one, and both would serve the same Slack app.
- **The outbound gate is an `http.RoundTripper`** shared by every Slack client
  in the daemon (`outbound_gate.go`), not a check at the socket. Slack's Web API
  is stateless HTTP independent of the socket, and chat turns deliberately run on
  background contexts, so a demoted node with a closed socket can still post.
  `auth.test` and `apps.connections.open` are exempt: they establish leadership
  rather than exercise it.
- **Demotion order is gate → disconnect → drain**, not drain first. The elector
  clears leadership before calling `OnDemote`, so nothing during the teardown can
  reach Slack; in-flight agent turns then get up to `drainTimeout` to finish
  writing files before being cancelled. The danger of a demoted node was never
  that its work continued, but that its work replied.

Session managers survive demotion — a standby may be promoted again, and a torn
down agent backend cannot be revived. They are closed only on process exit.

**The MCP aggregator must therefore be restartable.** `startBridge` runs on
every promotion, on that promotion's serve context, and a demotion cancels it.
Because the session managers and their agents survive, the aggregator has to
come back on the *same socket path* — no agent restart is involved. So
`mcpbridge.Server` numbers each run: a run's context watcher tears down only the
listener it started, and unlinks the socket only if it is still the current run
when it gets there. Both halves matter, because the watcher goroutine is not
waited on by `StopServing`, so a fast demote/promote genuinely does run an old
watcher after a new bind. (For the same reason the listener is created with
`SetUnlinkOnClose(false)`: Go's default would have a closing listener remove
whatever file is at its path, which after a re-promotion is its successor's.)
A failure to bind is journalled at error level under kind `bridge.start`, not
just logged — the symptom is an agent that answers normally and then cannot post
its result hours later.

A new leader announces itself to the admin DM with hostname, local and public
IP, version, PID, and the leadership epoch, plus whether it is the first leader
or took over from a predecessor.

**The election is journalled** to the `gateway` stream under kind `election`, so
`murtaugh journal query --stream gateway --kind election` reconstructs a
failover after the fact. Four states are recorded — `promoted`, `renew_failed`,
`stood_down`, `lock_unreachable` — each carrying the epoch (which totally orders
events across nodes whose logs interleave arbitrarily) and, on a failure, the
store's own error.

This matters most for the case that is otherwise invisible: a node whose lock
credentials lapse mid-flight stands down correctly and *silently*. It cannot say
so in Slack, because the outbound gate shuts before the demote callback runs;
and its successor knows a lease expired but not why. The journal is the only
place that sequence survives. Healthy renewals record nothing, so the stream
stays readable.

**Scheduled runs are claimed, not just scheduled.** Election stops two nodes
firing a job simultaneously, but `gocron` counts from when *its* scheduler
started, which is per-process — so a leader that restarts mid-interval starts a
fresh interval and fires again far too soon, and a promoted standby has no idea
what its predecessor already ran. Every fire therefore takes a claim in the
shared store first (`config.JobRunStore`), keyed by job name plus an occurrence
slot: wall-clock time truncated to the job's resolution (a minute for cron, the
period for `every`). Truncation is against a fixed origin, not node uptime —
otherwise each node computes its own grid and each wins its own claim. The
insert's primary key *is* the mutual exclusion, so there is no read before the
write and no window between checking and claiming. A claim that errors skips the
run: a missed occurrence is visible and recovers, a duplicate deploy may not.

Not implemented: **catch-up for missed occurrences**. If no leader exists when a
cron is due, that run is skipped entirely rather than replayed on the next
promotion — replaying raises policy questions (should a 03:00 backup run at
09:00?) that want an explicit answer.

**Node credentials are a third side store** (`config.NodeTokenStore`, #190),
sitting beside the leader lock and the run claim for the same reason: whichever
gateway a node's connection lands on must resolve that node's token, and the
store their shared configuration came from is the only place they already agree.
Like those two it is deliberately **not** a config section — absent from
`AllSections`/`AllSingletons`, so records never enter the `Config` a process
loads, never appear in `cfg show`, and are **not carried by Snapshot/Restore**.
The consequence is worth stating rather than discovering: `cfg db migrate` does
not move node credentials, so nodes are re-enrolled after a backend change. One
row per **token**, keyed by the token's public selector, which is what makes two
live credentials for one node — a rotation in flight — two ordinary rows.

### Event loop

`Run` launches `socket.RunContext` in a goroutine, warms the ACP client, then
selects over `socket.Events`, dispatching each to `handleEvent`:

| Socket event            | Handler                | Behaviour                                            |
|-------------------------|------------------------|------------------------------------------------------|
| `Connected`             | `notifyConnected`      | Greets once: resumes a pending restart (edits the notice into the back-online card) **or** sends the startup card — never both. |
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
the choice is made, shared by `agentruntime/local` and the `agentdelegate` runner:

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
`Warm`) and reuses sessions so a Slack thread maps to one persistent agent
conversation.

Eviction (`evictLocked`, run lazily on each new-session request) counts in-flight
turns per session, and its governing rule is that **a session running a turn is
not idle**:

- `idle_timeout` (30m) applies only to a session with no turn in flight, and its
  clock restarts when a turn *ends* — so a three-hour turn is followed by a full
  idle window, not by instant eligibility.
- `busy_timeout` (18h) is the runaway guard, and the only thing that can take a
  working session. Long overnight work is expected; this catches a wedged one.
- `max_sessions` evicts the least-recently-used **idle** session to make room.
  When every slot is busy there is nothing to take, so `Prompt` returns
  `*agent.CapacityError` and the user gets a "busy" warning card — the refusal is
  deliberate, because the alternative is killing somebody's live turn.

Every eviction names its reason in the log and on the journal's `acp_session`
stream (`session.evicted`), via the manager's eviction observer. Before this the
sweep was blind to in-flight work and silent about what it dropped, which is how
an ordinary message in one channel came to kill a 45-minute turn in another.

`ChatHandler.Handle` builds the key + `SessionMetadata`, sets the assistant
status to `is thinking...`, then ranges over the prompt's event channel.

### The link to a runtime node (`internal/nodelink`, `internal/agent/remote`)

The request direction is `agent.Client`'s five methods plus `session.close`,
expressed as `agentwire` messages: `initialize`, `session.new`, `prompt`,
`cancel`, `session.close`, `close`. `prompt`'s answer is an **acceptance**, not
the turn's outcome — both consumers read `Prompt`'s error synchronously before
any rendering starts — and the turn's events follow as event frames on the same
request id, ending with one that closes the consumer's Go channel.

`nodelink` wraps every payload in an envelope that owns delivery and nothing
else: a per-direction sequence from 1, a cumulative acknowledgement of the
highest **contiguous** frame the consumer has taken (acks piggyback, and never
consume a sequence number of their own), a byte-measured send window that
restores the pacing a Go channel gave for free, and a retransmit buffer pruned
by the peer's acknowledgement. A gap is fatal: on an ordered transport it means
the framing is broken, so the link fails with `ErrSequenceGap` and every open
turn ends with that error rather than with a hole in a sentence. A duplicate is
dropped silently, which is legal only after a resume replayed a suffix.

**The remote client is inserted at `agent.Client`, under the existing
`SessionManager` — not in place of it.** Six optional capability surfaces are
type-asserted on this path and only two are asserted on the client
(`CloseSession`, `SupportsCancel`); the other four are asserted on the *manager*
from the gateway (`Warm`, `Discard`, `Interruptible`, `io.Closer`), three of
which fail silently when unsatisfied. Placing the client underneath keeps those
four answered by `*SessionManager` unchanged.

`CloseSession` is answered rather than degraded — on `acp` and `claude_code` a
session owns a real process, so a no-op leaks one per evicted conversation — and
it is **enqueued, never awaited**, because it is called while
`SessionManager.mu` is held and has no context and no error return.
`SupportsCancel` is folded into the `initialize` answer as a *pointer*: absent
means unknown and degrades to interruptible, with a warning, because a plain
bool would decode to `false` and silently disable interrupting a live turn.

### The session channel end to end (`internal/nodesocket`, `internal/nodehost`, `internal/nodeserve`)

The node dials; the gateway never dials a node. `nodehost` is the daemon's
**first inbound listener** — nothing in Murtaugh had ever bound a port — so it
is reached only from `cmd/murtaugh-gateway -node-listen`, never from
`murtaugh slack gateway`, which remains the shipping default and binds nothing.
There is deliberately no configuration key: a port must not be acquirable by
editing a file the default daemon also reads.

`nodesocket` is `nodelink.Conn` over gorilla/websocket, and its two rules are
measured rather than defensive. A deaf peer — one that upgrades and never reads
— absorbs about **549 KB** before `WriteMessage` blocks, which is
`SO_SNDBUF + SO_RCVBUF` and nothing else: there is no library queue, so the
pacing signal survives the hop. But the block is permanent, and gorilla's own
`SetWriteDeadline` cannot rescue a write already in flight, so the deadline is
set immediately before every write and a timeout **tears the link down** rather
than failing one frame — gorilla latches the first write error and returns it
for every write afterwards.

That measurement is why `DefaultWindowBytes` is 256 KiB. `nodelink`'s own 4 MiB
default is larger than the socket buffers, so the socket would fill first and
the block would land in `Link.write`, which takes no context; under a 256 KiB
window it lands in `awaitRoom`, which honours one. Both are asserted against a
real deaf peer, because `nodelink.Pipe` cannot pose "the peer is alive and not
reading" at all.

The same measurement produced a fix in `nodelink`: acknowledgements now trigger
on **consumed bytes as well as consumed frames**. A byte-measured window and a
frame-counted ack policy deadlock each other — four 64 KiB attachment chunks
fill a 256 KiB window three frames short of the threshold, and the ack that
would open it can only come from consuming more frames.

`nodeserve` runs the node's half. Requests are dispatched off the read loop
(a prompt must not hold up the cancel that stops it) while events stay inline,
so the ack-after-the-handler-returns rule keeps applying backpressure in the
direction that has volume. An attachment's chunks are sent **before** the event
that references them, because the gateway materialises an attachment from
inside its own read loop and a deliverer that pulled chunks arriving on that
loop would deadlock it.

**Approvals cross as one frame in each direction, and the note comes back.**
The native backend — the default one — never raises a permission event: it calls
an `Approver` inline. `nodeserve.ToolGate` is what that call reaches on a node;
it raises a `GateTool` request on the current turn's stream and returns the
`(allowed, note)` pair the loop expects. The note is not diagnostics — for a
native tool call it IS the result string handed to the model — so the gateway
answers with a full `PermissionResponse` rather than a bare option id, and the
decoded request carries its `Gate` so a tool approval is never routed to the
agent-harness asker. A tool approval is answered off the turn's stream and is
never rendered, exactly as in process.

**A background stretch's events cross too, addressed by session rather than by
turn.** A `claude_code` session emits after its turn's `result` — a subagent
finishing, an auto-continue completing minutes later — and in process those go
to the gateway's `backgroundEventsRouter`, which is what renders the "went
quiet" notice. Across a link they have no stream to ride, so they travel as
`agentwire.BackgroundEvent`, keyed by the session id. The node's end is
`nodeserve.BackgroundSink`, bound to the serving connection the way the tool
gate is, because a backend captures its sink when its process starts and the
connection comes and goes; unbound it drops, since no gateway is attached and
the session id then names nothing. A node built without one drops the events at
the backend, one hop before the protocol could carry them — with the gateway
side fully plumbed, which makes it look like a routing bug on the only side
anyone would think to debug.

A link is still an agent: the protocol carries no agent name, so every
configured agent name resolves to a node and a node serves one profile. What
ended with #170 item 9 is the single slot — see the registry below.

### The node registry and what a node advertises (`internal/nodehost`, `internal/nodeclaim`)

The gateway keeps a registry of connected nodes and what each one claims,
updated **at connect and on every node-side configuration change**. Identity
comes from the credential a connection presented, never from anything the node
said: there is no `node_id` anywhere in the wire format, because inside a fleet a
node that announces who it is can announce somebody else.

**Push, not poll.** A node's claim rides the `initialize` answer at connect and
arrives as a `node.advertise` request on every later change; the gateway matches
locally against the cached copy. Asking every node at delegation time would put
an N-way fan-out on the first message of every conversation, where one wedged
node adds a timeout to every delegation in the workspace. The cost is a
staleness window one configuration edit wide, and its worst outcome is one
conversation delegated to the wrong node.

**The connect-time claim rides the handshake answer for an ordering reason.**
The gateway builds the registry entry the instant `initialize` returns, so a
claim carried on that answer is in hand exactly when there is somewhere to put
it. A node pushing its opening claim as a request instead would race the
gateway's own bookkeeping. The two paths converge in `remote.Client`, which is
the only place that can tell them apart, and where the newest claim wins
whichever way it arrived.

**The registry is in memory, and that is a decision.** #170's table puts it in
the *gateway holds* column, which states ownership rather than storage. An entry
is a live socket plus a claim and both die with the process; only the elected
leader accepts node connections, so a persisted registry read by a standby is
guaranteed stale. The half that outlives a process is the conversation **pin**,
which item 10 stores as a side store in the `NodeTokenStore` family.

**Keyed per connection, enumerated per node.** Rotation means two credentials
are valid at once, and `CloseCredential` closes by selector so revoking the old
one does not drop the node — so a map keyed by node id would make the second
connection evict the first. `Nodes()` collapses back to one entry per node id,
because delegation must never see one machine twice and round-robin it against
itself. Choosing between them is item 10: a new conversation still opens on the
most recently attached node.

**A session is bound to the connection that minted it.** The choice waits for
item 10; the binding cannot. Session ids are minted per node, so once a second
node can be connected, sending a conversation's next turn to "whichever node is
newest" hands a machine an id it never issued — the prompt fails in front of the
user, and a `Cancel` is answered as success while the turn runs on and the
gateway blocks draining a stream that will never close. Every prompt, cancel and
close therefore goes back to the session's own connection, and a session whose
connection has gone is `agent.ErrSessionGone` — distinct from `ErrNoNode`
because it means *this session* cannot run while a new one could. Re-opening it
elsewhere, and telling the model it moved, is item 10.

**`allow_anyone` deliberately does not cross**, and neither does
`reply_on_thread`. The first waives the gateway's own access list for a
channel's chat surface; the second decides the conversation key a pin is keyed
by. Both are written by a node admin — possibly a guest holding a grant — so
accepting either would let a node owner make a gateway decision by editing a
file on their laptop. Same rule as the tool partition: enforcement is
gateway-side because a node cannot be trusted to filter itself.

**A node advertises what it SERVES, not what it has configured.**
`internal/nodeclaim` derives the claim from the node's own configuration and the
profile names the process actually serves, and drops any channel rule routing to
a profile it does not serve. Advertising the rest would be a claim the gateway
could act on and the node could not honour.

**The node's watcher is the only poll in the design, and it is not a reload.**
`nodeclaim.Watcher` compares `config.Store.Snapshot` renderings and re-reads the
CLAIM SET on a change — applied unconditionally, because a node admin editing
their own node is the authority and there is no Slack surface on a node to ask
through. It deliberately does not rebuild the agent: both backend families latch
their toolset and the redial loop reuses the client captured at startup, so
rebuilding under a live connection would strand every open session. While the
configuration split (item 12) is unbuilt a gateway and node on one machine share
one store, so a "node-side" edit on a `--role both` box is also seen by the
gateway's approval-card watcher and may be rolled back.

**A sleeping node stays connected.** `nodesocket` sets no read deadline and
there is no ping/pong, so a laptop that sleeps without a FIN remains in the
registry until a write fails at the transport's write timeout or TCP gives up.

**Attach, detach, revocation and re-advertisement are journalled, not
announced** — gateway stream, kind `node`. A laptop sleeping at six o'clock
disconnects every evening, and a nightly message trains the admin to ignore the
one that matters.

### The tool channel (`internal/toolset` partition, `internal/nodehost`, `internal/nodeserve`)

Murtaugh's own tools live on the gateway and hold the gateway's credentials. A
node's agent reaches them over the connection the node already dialled — the
gateway still never dials out — as `tool.list` and `tool.call`, the only
direction a node initiates a call in.

**It is a per-call proxy, not a tunnel of the MCP stream, and that is what makes
it small.** #170 Concern 5 describes the hard version: move the MCP byte stream
across and the gateway end becomes a server session whose entire state IS that
stream, so a reconnected pipe is dead rather than degraded and the node has to
own initialise replay and request-id rewriting. Keeping the MCP session local —
between the agent's own subprocess and the node's aggregator socket — removes
that problem rather than solving it. It also reaches the backend a tunnel would
have missed: the **native** backend, the default one, never touches the
aggregator at all, so a tunnel would have restored tools for `acp` and
`claude_code` and left native silently tool-less.

**All three backends reach the channel, and getting the other two there took a
change to the aggregator.** The proxy alone is not enough: `acp` and
`claude_code` reach the registry through `agentbuild`'s aggregator, which used to
resolve the agent's built-ins when the agent was *constructed*. On a node that is
before any gateway connection exists, so it snapshotted an empty registry and
served nothing for the life of the process — the very regression this channel is
for, in the two backends it was written for, while `native` looked fine because
it resolves inside its own `Initialize`. The aggregator now resolves on its first
session instead, which is after the handshake on a node and makes no difference
in the gateway process, where the registry is built once in `app.New` before any
agent.

**The partition is a table in `internal/toolset` and is enforced in exactly one
place, gateway-side.** `Reach` has three values and its zero value denies, so an
unclassified family is refused rather than assumed harmless. A node reaches
`ping`, `version`, `ask` and `present_plan`; `slack`, `jobs`, `cfg`, `setup`,
`node`, `journal`, `troubleshoot` and `restart` are the gateway's alone; the
workdir-rooted native groups and `auth.request` are meaningless remotely —
`auth.request` because it writes the granted credential into the environment of
the process that asked, which gateway-side is the wrong process. `nodehost`
applies it on **both** verbs: `tool.list` never names a tool a node may not have,
and `tool.call` re-checks before looking anything up, because a node admin owns
their node's configuration and a node cannot be trusted to filter itself. The
drift guard is in `internal/app`, the only package that can build the real
registry.

**Two things a proxied call must carry that an in-process one gets for free.**
The turn's `agent.TurnLocation` is re-injected gateway-side from the stream id
the call names — without it the approval gate short-circuits to *allowed* with no
card and `ask`/`present_plan` go non-interactive, a silent ungating rather than a
failure. And the descriptor carries the tool's `MCPName` override, so `ask`
republishes as `AskUserQuestion` and a Claude Code agent still reaches for it.
What does **not** cross is `agent.TurnEnv`: a gateway-executed tool runs with the
gateway's environment.

Only a **native** call names its stream today. An `acp`/`claude_code` call
reaches the proxy through the node's local MCP aggregator, whose context is
decorated once per session and cannot name a turn, so those two backends call
with an empty stream: `ping` and `version` are unaffected, `ask` and
`present_plan` fail gateway-side for want of a thread. Carrying a per-turn id
onto the aggregator's per-call context is its own item; it is written down here
rather than left to be discovered as "`ask` works on one backend".

**The abort policy is forced by an absence, and it is enforced by cancellation
rather than by wording.** Nothing in the tool surface says whether a call may be
retried — `tools.Tool` is `Name`/`Description`/`InputSchema`/`Invoke` — so when a
link dies, every in-flight call fails and none is retried. What makes that
stick is that the node ends every turn before it fails that turn's calls, so an
agent whose call was dropped is already cancelled; the note it would have read —
the action may or may not have taken effect, do not retry — is the backstop for
a caller whose context is not a turn's. The gateway cancels its side too, so a
drop mid-approval cannot fire a side effect for an agent that has gone. That gap
is closed **on this path only**: the in-process MCP frontend cannot be fixed the
same way, because the MCP SDK wraps a connection's context in a type whose
`Done()` returns nil forever, so no ancestor context reaches a tool handler. See
`internal/nodehost`'s package doc.

The tool set is fetched at the handshake, immediately before the node's agent is
initialised, because both backend families latch: `native` on its first
`Initialize`, `acp`/`claude_code` when their aggregator registers its first
session. A gateway that cannot answer fails the handshake; the node's redial loop
retries. The set is then frozen for the process — descriptions and schemas
refresh on reconnect, membership does not.

### The two translations (`chat_request_translator.go`, `chat_event_translator.go`)

A turn crosses two named boundaries, one per direction. They exist because the
gateway/runtime-node split (#170) puts a network between them, and they are built
first so the seam is explicit before anything is threaded through it.

`requestTranslator` resolves the Slack side of a turn — the triggering message,
its uploads, the routing decision, the thread — into the request the agent layer
takes: `ConversationKey`, `SessionMetadata`, `PromptRequest`, and the timestamp
the reply is posted at. It produces agent types rather than wire types because
that direction is already wire-shaped (`SessionMetadata` carries JSON tags);
framing them is the transport's job.

`eventTranslator` runs the other way: one `agent.Event` at a time (event-at-a-time
rather than a channel loop, so the push-driven `backgroundEventsRouter` can adopt
it) into `chatRenderer` calls, plus the decision of how the turn ends —
`Finish` / `Fail` / `Interrupted` / `EnsureStopped`. It reads `agent.Event`, not
`agentwire.Event`: `agentwire.Decoder` already owns that hop, so the full inbound
chain is `agentwire.Event → Decoder → agent.Event → eventTranslator →
chatRenderer` and the remote and in-process paths share the last two links.

It decides the terminal but carries out none of the policy that goes with one —
dropping a wedged session, starting a credential repair, substituting the error
the user is shown, recording the journal row. That is why `Event` returns the
step and `Settle` renders it as two separate calls: the gap between them is where
the caller's policy runs.

**`eventTranslator` owns the liveness window; `chatRenderer` has no clock.** The
window resets on every event before the kind is examined, so a status heartbeat
keeps a turn alive while rendering nothing. `internal/archtest/renderclockanalyzer`
enforces the renderer half in CI. See AGENTS.md, "The renderer sees no clock",
for the precise statement and its limits.

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

`internal/slack/approvalcard` is the other shape: **rendering without a
lifecycle**. The interaction broker already posted, correlated, timed out and
optionally deleted the pre-card approval prompts for all three gates and for the
scheduler's first-run hold, so the card package owns none of that — it implements
the broker's `CardRenderer` hook (`Pending`/`Resolved`/`Fallback`) and the broker
calls in at the two points where blocks are needed. Reach for this shape when a
lifecycle already exists and only the looks are changing; reach for authcard's
when there is no lifecycle yet.

Its `Spec.Subject` picks the card's voice. A gate asks about a tool an agent
wants to use; the scheduler asks about a held job whose first run has come due,
where "the agent wants to" would be untrue — a timer asked, not an agent. Adding
a third asker means adding a `Subject`, not a second card package.

`internal/slack/alertcard` is the third shape: **rendering with neither a
lifecycle nor a click**. An alert is a statement, so there is nothing to
correlate and no terminal state to rewrite — a caller renders and posts. Reach
for it when Murtaugh needs to say something about itself.

Three things it does that the other two do not:

- **It always arrives collapsed.** Title and subtitle stay visible while the body
  is folded, so the alert costs one line. That is what makes it affordable to put
  the whole provider error in `Detail` instead of truncating it — the wall of JSON
  is one click away rather than pasted into the conversation.
- **It renders two ways.** `Render` gives blocks; `PlainText` gives the same
  content as mrkdwn, for the surfaces a container cannot reach (text appended to
  an in-flight stream, or a post that failed). `FallbackText` is the third, shorter
  form: title and subtitle only, for the notification field. Callers that can post
  blocks post blocks — the others are degradation paths, not styles.
- **Its `Level` is the journal's vocabulary** (`error`/`warn`/`info`), so an alert
  and the event recorded for it use the same word.

Its scope is the alerts that were bare text. The cards that already have routing
contracts — approval, restart, ask, auth — keep their own rendering; folding them
in would re-plumb click routers for no visual gain.

Two consequences worth knowing before editing either of the first two:

- The broker mints the `action_id` and the click `value`, and hands them to the
  card as `CardOption`. A card template **must** emit both verbatim; a button that
  rewrites either one is a dead button. Slack does report `action_id` for a button
  nested inside a `container`'s `child_blocks` — both the ask and approval cards
  depend on this.
- The button row's `block_id` is passed *into* the renderer rather than fixed in
  it, because it is the gateway router's constant, not the card's.

### The two approval paths

There are two, and they are not interchangeable:

- **Murtaugh's gate** (`GateApprover`) — tool calls Murtaugh can see: the native
  loop's, and the registry tools an ACP/claude_code agent reaches over the MCP
  bridge (`agentbuild` wraps the same approver as `mcpApprover`). Murtaugh owns
  the options, so it offers "Approve & always allow". Governed by
  `approval.terminal`/`approval.allow`.
- **Reflecting the agent's intent** (`PermissionGate`) — the agent asks about one
  of *its own* tools. The options are the agent's: it will only understand an
  `optionId` it declared, so Murtaugh renders them and adds none of its own.
  Governed by `approval.requests`, which is a routing policy for an inbound
  question, not a gate Murtaugh imposes.

A backend with no options of its own sets `PermissionRequest.PolicyOwned` instead
of inventing some, and `PermissionGate` then supplies Murtaugh's own set —
approve / approve & always allow / deny — making that request a Murtaugh gate
decision wearing the reflection path's plumbing. Claude Code's `can_use_tool` is
the case that needs it: a bare allow/deny with no option list. The gate maps its
own option ids back to `agent.PermissionAllow`/`PermissionDeny` before answering,
so a delegating backend's vocabulary stays allow, deny and "nobody chose", and
nothing about always-allow crosses the boundary.

The limit is worth stating: Murtaugh can only gate what it is told about.
Anything an agent's harness auto-approves internally never produces a request,
and no setting here changes that.

`Grants` is the one thing they share — an always-allow set built per agent and
handed to both. Only the gate can create a grant; the reflection path reads it,
so a command already allowed through Murtaugh's own tools is not asked about
again when the agent runs it itself. `GrantKey` keys a shell call on the command
line alone precisely so it crosses the two paths, where the same command arrives
under different tool names (`terminal`, `execute`, `Bash`).

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
Test-communication button is built in Go (`internal/slack/pingcard` supplies its
ids and label; `app_home.go` renders it), not a template, so no config or
template edit can shadow the self-test. Its reply, and Murtaugh's other
lifecycle messages, are ordinary info alert cards. Bundled agent skills
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
