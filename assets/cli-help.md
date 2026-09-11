# murtaugh — command-line reference

Murtaugh ships as a single binary with three frontends over one shared tool
registry:

- **CLI** — direct invocation: `murtaugh <command> [flags...]`.
- **MCP** — JSON-RPC stdio server (`murtaugh mcp`) exposing every tool below to
  AI clients. The MCP tool name is the registry name with every dot replaced by
  an underscore (e.g. `jobs_run`, `slack_send_msg`) — some providers (e.g.
  Gemini) reject a `.` in a function name, so it is normalised at the MCP
  boundary. The dotted form (`jobs.run`) remains the registry key, and the CLI
  spells the same tool with a space (`jobs run`, `slack send_msg`).
- **Slack gateway** — the long-running Socket Mode daemon
  (`murtaugh slack gateway`).

```
Usage: murtaugh [--config PATH] <command> [flags...]
```

Run `murtaugh help` for this full document, or `murtaugh help <command>`
(e.g. `murtaugh help slack send_msg`) for a single command. `murtaugh <command>
--help` works too. Agents reach the same reference through the `help` tool
rather than shelling out for it.

**This file is half of the reference.** Every flag table you see in the
rendered output is generated from the owning tool's `InputSchema` at render
time — flag names, types, requiredness, enum values and repeatability all come
from the code, so they cannot drift from what the binary accepts. A tool with
no section here still gets a complete generated one appended.

What this file contributes is everything a schema cannot say: worked examples,
the consequence of a flag, which changes need a daemon restart, how two flags
interact. **Editing a flag table below has no effect** — the renderer replaces
it. Change the tool's schema instead, and put the reasoning here.

# Global conventions

These rules apply to **every** CLI command. Read them once; the per-command
sections below assume them.

- **Global flag `--config PATH`** — path to `config.yaml`. Default
  `~/.config/murtaugh/config.yaml`. Accepts `--config PATH` or `--config=PATH`.
  `config.yaml` is slimmed to two blocks — `oauth:` (Slack tokens, referenced
  as `${VAR}`) and `database:` (the config-store backend) — plus its sibling
  `.env` (all secrets). Everything else — agents, MCP servers, jobs, chat
  routing, access, journal, troubleshoot, workflow/unfurl rules, runtime
  defaults — lives in the **config store** (a SQLite DB by default at
  `~/.config/murtaugh/config.db`, or Postgres), managed with the
  `murtaugh cfg …` commands (also exposed over MCP as `cfg.*` tools). The
  store's default filename follows the config file's own name — `config.yaml`
  uses `config.db` and `config-journal.db`, while `--config
  slack-nurturecloud.yaml` uses `slack-nurturecloud.db` and
  `slack-nurturecloud-journal.db` — so several configs can share a directory
  without sharing a store. The old
  hand-edited siblings (`agents.yaml`, `jobs.yaml`, `journal.yaml`,
  `workflow-rules.yaml`, `unfurl-rules.yaml`, `troubleshoot.yaml`) no longer
  exist; on upgrade an existing set is auto-migrated into the store on first
  run and archived to `~/.config/murtaugh/migrated-<timestamp>/`.
- **Flags take values; there are no positional arguments.** Every flag is
  `--flag value`. There is no `--flag=value` form for tool flags (only the
  global `--config` accepts `=`).
- **Booleans require an explicit value.** Write `--load true`, `--force false`.
  A bare `--load` is rejected with `flag --load requires a value`. (Over MCP,
  pass a real JSON boolean instead.)
- **Flags are kebab-case.** A schema field `attachment_type` is the flag
  `--attachment-type`; `binary_path` → `--binary-path`; `app_token` →
  `--app-token`. The MCP argument names are the snake_case originals.
- **Repeatable (array) flags** accumulate when repeated:
  `--args one --args two` yields `["one", "two"]`.
- **Output goes to stdout; diagnostics/warnings go to stderr.** CLI results are
  rendered as human-readable lines; the MCP frontend returns the same data as
  JSON.
- **Unknown flags are errors.** An unrecognised `--flag` fails fast rather than
  being ignored.

## Slack target & identifier resolution

Several Slack tools accept channels, users, threads, and timestamps. Every
tool resolves them through one resolver, so the same value works everywhere —
in particular, a `channel` returned by `send_msg` can be handed straight to
`fetch_msgs`, `fetch_reactions` or `update_msg`.

- **A conversation** (`--to` on send_msg; `--channel` on fetch_msgs,
  fetch_reactions and update_msg) accepts any of:
  - `#channel-name`, or the bare name without `#`;
  - a channel, group or DM ID — `C…`, `G…`, `D…` — or Slack's escaped
    `<#C…>` / `<#C…|name>`;
  - a person — `@handle`, a user ID `U…`/`W…`, `@U…`, or Slack's escaped
    `<@U…>` / `<@U…|name>` — which resolves to the bot's DM with them,
    opening it if needed.

  IDs are used as given, without a lookup: a DM or a private channel never
  appears in `conversations.list`. Only a name is looked up there, and a
  private channel is only visible to that lookup once the bot is invited.
- **A person** (`--from` on fetch_reactions, `--invite` on create_channel)
  accepts `@handle`, a bare handle, a user ID, or `<@U…>`. A handle is matched
  case-insensitively against legacy username, then display name, then real
  name. An ID is told apart from an all-caps handle by its digits: `U0B20G0ET9T`
  is an ID, `QA` is a name.
- **`@mentions` inside `--body`** are auto-expanded to `<@USERID>` Slack mention
  syntax. An unresolvable `@handle` is left as plain text and a warning is
  printed to stderr.
- **Thread / message timestamps (`--thread`, `--ts`)** are Slack `ts` strings
  like `1716950455.123456`.
- **`--since`** is a wall-clock datetime `YYYY-MM-DD HH:mm:ss` interpreted in
  **Sydney (Australia/Sydney) time**. Default when omitted: 24 hours ago.

## Block Kit blocks (`--blocks`)

`--blocks` accepts **either** an inline JSON string (the value's first
non-whitespace character is `[` or `{`) **or** a filesystem path to a `.json`
file. Either way the content must be valid JSON or the command fails with
`Error parsing blocks JSON: …`. `--blocks` is mutually exclusive with
`--attachment` on `send_msg`.

# Commands

## murtaugh ping

Health check. Returns `pong`. Takes no flags.

```
murtaugh ping
```

## murtaugh jobs run

Run a job previously defined in the config store, by name. The job's command
runs with its configured args/workdir/timeout; child stdout/stderr stream to
your terminal (and are captured into the JSON result over MCP).

| Flag     | Required | Type   | Notes                                       |
|----------|----------|--------|---------------------------------------------|
| `--name` | yes      | string | Job key as registered in the config store.  |

- Default timeout is **10 minutes** when the job has no `timeout` set.
- Exit code is reported in the result; a non-zero exit is **not** a CLI error
  (the job ran), but the scheduler treats it as a failed run.
- Fails if the job name is not found or the job has no `command`.
- An agent job's final reply comes back in the result (`reply`). `jobs run`
  keeps none of it in the journal, and running it by hand never posts to
  `report_to`: only a scheduled run on the gateway reports or keeps a reply.

```
murtaugh jobs run --name nightly-backup
```

## murtaugh jobs define

Register a new job, or update an existing one, in the config store. Does **not**
run the job — use `jobs run` for that. Unrelated jobs are preserved. (`cfg job
set` is the equivalent under the unified `cfg` surface.)

| Flag         | Required | Type            | Notes                                                                 |
|--------------|----------|-----------------|-----------------------------------------------------------------------|
| `--name`     | yes      | string          | Job key. Must be non-empty.                                           |
| `--command`  | yes      | string          | Absolute path or PATH-resolved binary to execute.                     |
| `--args`     | no       | string (repeat) | Positional arguments for the command. Repeat the flag per arg.        |
| `--workdir`  | no       | string          | Working directory for the command.                                    |
| `--timeout`  | no       | duration        | Go duration (e.g. `30s`, `5m`). Defaults to `10m` at run time.        |
| `--schedule` | no       | cron            | 5-field cron (e.g. `0 2 * * *`) for automatic runs by the gateway.    |
| `--every`    | no       | duration        | Go duration (e.g. `1h`) for fixed-interval runs by the gateway.       |

- `--schedule` and `--every` are **mutually exclusive**; set at most one.
- A scheduled job only fires while `murtaugh slack gateway` is running.
- `--timeout` and `--every` must be valid Go durations; `--every` must be > 0.
- Every write stamps the entry `confirmed: false`, so a new **or edited** job is
  held: the scheduler asks the admin to approve its next run before executing it.
  Approving persists (a restart does not re-ask); the next edit re-arms the gate.
  `cfg job set` behaves identically.

```
murtaugh jobs define --name nightly-backup \
  --command /usr/local/bin/backup --args --full --args /data \
  --workdir /srv --timeout 30m --schedule "0 2 * * *"
```

## murtaugh journal query

Read structured events back out of the **event journal** — the queryable record
of what Murtaugh did (gateway interactions, workflow rules, link unfurls, job
runs). This is how Gateway Debug Mode answers "why did this interaction
misbehave?". Distinct from the daemon's stderr logs: the journal is for filtered,
correlated inspection. Configure it in the config store (`cfg journal show` to
inspect the current settings).

All filters are optional and ANDed; results are most-recent-first.

| Flag        | Required | Type     | Notes                                                                 |
|-------------|----------|----------|-----------------------------------------------------------------------|
| `--stream`  | no       | string   | `gateway`, `job`, or `acp_session`.                                   |
| `--kind`    | no       | string   | Exact event kind, e.g. `workflow.trigger`, `unfurl.render`, `job.run`.|
| `--level`   | no       | string   | Minimum severity (at least): `debug`, `info`, `warn`, `error`.        |
| `--channel` | no       | string   | Slack channel ID.                                                     |
| `--user`    | no       | string   | Slack user ID.                                                        |
| `--session` | no       | string   | ACP session ID.                                                       |
| `--corr-id` | no       | string   | Correlation id — every event from one interaction shares it.          |
| `--rule`    | no       | string   | Workflow or unfurl rule name.                                         |
| `--since`   | no       | string   | Lower time bound: a Go duration ago (`2h`) or an RFC3339 timestamp.   |
| `--until`   | no       | string   | Upper time bound: a Go duration ago (`5m`) or an RFC3339 timestamp.   |
| `--limit`   | no       | integer  | Max events (default `50`, capped at `500`).                           |

- The typical flow: filter by `--channel`/`--since`/`--level error` to find a
  failure, then re-query with `--corr-id` to see that whole interaction.
- Each event's `payload` carries the detail (template path, render error,
  non-JSON agent output, command error, …).

```
murtaugh journal query --stream gateway --channel C0REVIEWS --since 1h --level warn
murtaugh journal query --corr-id gw_3f9c2b1a
```

## murtaugh journal stats

Summarise the journal: row count and oldest/newest timestamp per stream. Every
known stream is listed, including empty ones — a `0` count on `gateway` means
Gateway Debug Mode is not recording (check `cfg journal show` and restart the
daemon). Takes no flags.

```
murtaugh journal stats
```

## murtaugh journal prune

Delete events older than each stream's configured retention (from the config
store's journal settings) — a manual run of the sweep the gateway daemon
performs automatically (on startup and every `sweep.every`). Takes no flags;
uses the configured retention.

```
murtaugh journal prune
```

## murtaugh slack send_msg

Post a message (or upload a file) to a Slack channel or user. By default the
message is posted as the app, using the bot token from `oauth.bot_token` in
`config.yaml`. Pass `--as admin` to post as the human admin instead (see
`--as` below).

| Flag                | Required | Type   | Notes                                                                       |
|---------------------|----------|--------|-----------------------------------------------------------------------------|
| `--body`            | yes      | string | Message text. Also the notification fallback when `--blocks` is set. `@mentions` are expanded. |
| `--to`              | yes      | string | Destination: `#channel`, `@user`, or `C…/G…/D…` ID. See target resolution.  |
| `--thread`          | no       | string | Parent message `ts` to reply in-thread.                                     |
| `--attachment`      | no       | string | Path to a file to upload. Mutually exclusive with `--blocks`.               |
| `--attachment-type` | no       | enum   | Snippet type for the attachment. Only value: `markdown`.                    |
| `--blocks`          | no       | string | Block Kit JSON (inline string or file path). Mutually exclusive with `--attachment`. |
| `--as`              | no       | enum   | Sender identity: `bot` (default) posts as the app; `admin` posts as the human admin via their Slack user token. Requires `oauth.user_token` — errors if unset, never silently falls back to the bot. |

`--as admin` posts with the admin's **real Slack identity** — the message is
indistinguishable from one the admin typed by hand, and it is not marked as
app-generated. Use it deliberately, and only where speaking as the human is
intended.

```
murtaugh slack send_msg --to "#deploys" --body "Build green :white_check_mark:"
murtaugh slack send_msg --to "@miere" --body "ping" --thread 1716950455.123456
murtaugh slack send_msg --to "#status" --body "Status" --blocks ./status-blocks.json
murtaugh slack send_msg --to "#team" --body "Approving this — go ahead" --as admin
```

## murtaugh slack create_channel

Create a public or private Slack channel, optionally inviting users and setting
a topic/purpose. Uses the bot token from `oauth.bot_token` in `config.yaml`. The
bot needs the `channels:manage` scope for public channels and `groups:write`
for private ones (those scopes also cover the invites).

| Flag        | Required | Type    | Notes                                                              |
|-------------|----------|---------|--------------------------------------------------------------------|
| `--name`    | yes      | string  | Channel name (a leading `#` is stripped). Slack lowercases it and replaces spaces with hyphens. |
| `--private` | no       | boolean | Create a private channel instead of a public one.                  |
| `--invite`  | no       | array   | Users to invite: `@handle` mentions or raw `U…/W…` user IDs. Unresolvable handles are skipped with a warning; per-user failures don't abort. |
| `--topic`   | no       | string  | Channel topic to set after creation.                               |
| `--purpose` | no       | string  | Channel purpose/description to set after creation.                 |

```
murtaugh slack create_channel --name launch-2026 --topic "Launch coordination"
murtaugh slack create_channel --name incident-42 --private true --invite @miere --invite U07ABCDE
```

## murtaugh slack fetch_msgs

Fetch messages from a channel or thread, oldest-first. Capped at 100 messages.

| Flag        | Required | Type   | Notes                                                              |
|-------------|----------|--------|--------------------------------------------------------------------|
| `--channel` | yes      | string | Channel name (with or without `#`) or channel ID.                  |
| `--thread`  | no       | string | A thread `ts`; fetches that thread's replies instead of history.   |
| `--since`   | no       | string | `YYYY-MM-DD HH:mm:ss` Sydney time. Excludes older messages. Default 24h ago. |

```
murtaugh slack fetch_msgs --channel deploys
murtaugh slack fetch_msgs --channel deploys --since "2026-06-12 09:00:00"
murtaugh slack fetch_msgs --channel C0123456789 --thread 1716950455.123456
```

## murtaugh slack fetch_reactions

Fetch the messages in a channel that a specific user reacted to with a specific
emoji. Scans up to 100 recent messages and filters them. Output is oldest-first.

| Flag        | Required | Type   | Notes                                                              |
|-------------|----------|--------|--------------------------------------------------------------------|
| `--from`    | yes      | string | User handle, with or without `@`.                                  |
| `--emoji`   | yes      | string | Emoji name, with or without colons (`thumbsup` or `:thumbsup:`).   |
| `--channel` | yes      | string | Channel name (with or without `#`) or channel ID.                  |
| `--since`   | no       | string | `YYYY-MM-DD HH:mm:ss` Sydney time. Default 24h ago.                |

```
murtaugh slack fetch_reactions --from @miere --emoji eyes --channel triage
```

## murtaugh slack update_msg

Update an existing message in a channel, optionally rewriting its Block Kit
blocks.

| Flag        | Required | Type   | Notes                                                                          |
|-------------|----------|--------|--------------------------------------------------------------------------------|
| `--channel` | yes      | string | Channel ID, **or** a channel name with a leading `#`. A name without `#` is treated as a raw ID. |
| `--ts`      | yes      | string | Timestamp of the message to update.                                            |
| `--body`    | no       | string | Fallback text for the update. Defaults to `Message updated`.                   |
| `--blocks`  | no       | string | Block Kit JSON (inline string or file path).                                   |

```
murtaugh slack update_msg --channel "#deploys" --ts 1716950455.123456 \
  --body "Build finished" --blocks ./done-blocks.json
```

## murtaugh slack gateway

Start the Slack gateway: the long-running Socket Mode daemon. It responds to
slash commands, runs YAML workflow rules against interactive payloads, bridges
Slack conversations to an ACP agent with live streaming, renders custom link
unfurls, and fires scheduled jobs. Configuration comes entirely from `config.yaml`
(`oauth:` + `database:`), its sibling `.env`, and the config store the database
block points at (agents, jobs, rules, chat routing, access, …); there are no
tool flags. Stop it with SIGINT/SIGTERM. Normally run under launchd (see `setup
launchd`).

```
murtaugh slack gateway
murtaugh --config /etc/murtaugh/config.yaml slack gateway
```

## murtaugh mcp

Start the MCP stdio server. Serves every registered tool to an MCP client over
JSON-RPC on stdin/stdout. stdout is reserved for protocol traffic — do not run
this interactively expecting human output. Register it with a client via
`setup mcp_register`.

```
murtaugh mcp
```

## murtaugh cfg

Manage the **config store** — the SQLite (default) or Postgres database that
holds everything except the Slack tokens and the store connection itself
(agents, MCP servers, jobs, chat routing, access, runtime defaults, journal,
troubleshoot, and workflow/unfurl rules). Every mutation writes the store and
then re-validates the **whole** assembled config; a change that would produce an
invalid config is rejected and rolled back, so the store is never left broken.
The same commands are exposed over MCP as `cfg.*` tools. The runtime loads
config once at startup — **restart the daemon to apply** any `cfg` change.

Grouped below by entity. Collection entities (`agent`, `mcp`, `job`,
`workflow-rule`, `unfurl-rule`) share the `set`/`list`/`show`/`delete` verbs
keyed by `--name`; singletons (`chat`, `access`, `defaults`, `journal`,
`troubleshoot`) are edited/read in place.

### Agents (`cfg agent`)

```
murtaugh cfg agent create --name <n> --type <native|acp|claude_code> [flags]
murtaugh cfg agent update --name <n> [flags]
murtaugh cfg agent list
murtaugh cfg agent show   --name <n>
murtaugh cfg agent delete --name <n>
```

Same backend fields as `setup agents` (provider/model/api-key-env/tools/…
for native; command/args for acp). `--type claude_code` selects the direct
Claude Code stream-json backend.

### MCP servers (`cfg mcp`)

```
murtaugh cfg mcp set    --name <n> [...]
murtaugh cfg mcp list
murtaugh cfg mcp show   --name <n>
murtaugh cfg mcp delete --name <n>
```

### Jobs (`cfg job`)

```
murtaugh cfg job set    --name <n> [--command --arg --workdir --timeout | --agent --prompt --report-to] [--schedule|--every]
murtaugh cfg job list
murtaugh cfg job show   --name <n>
murtaugh cfg job delete --name <n>
```

`cfg job set` is the store-native equivalent of `jobs define`; `jobs run`
executes a job by name. Like `jobs define`, `cfg job set` stamps every entry it
writes `confirmed: false`, holding a new or edited job until the admin approves
its next scheduled run.

`--report-to` has the gateway post an agent job's final reply after each
scheduled run, as the bot. The gateway keeps or posts a reply only when the node
that ran the job belongs to the gateway admin (or the agent ran inside the
gateway itself). For anyone else's node the reply is neither posted nor kept,
with or without `--report-to`; the journal notes only that it was withheld, and
the admin is told by DM when a report was withheld. It takes effect on the next
gateway restart, like every job change.

### Chat routing (`cfg chat`) — singleton

```
murtaugh cfg chat set  [--enabled --default-agent --dm-agent --reply-on-thread]
murtaugh cfg chat show
```

### Access (`cfg access`) — singleton

```
murtaugh cfg access set  [--admin-user --allowed-users <u> (repeat) --debug
                          --main-node <node-id>]
murtaugh cfg access show
```

`--main-node` designates the node that serves HEADLESS work: scheduled jobs,
workflow triggers and link unfurling. None of those has a user whose fleet could
be chosen from, so they go to this one node rather than through delegation. Pass
an empty string to clear it. With none designated — or with the designated node
not attached — those surfaces refuse and say which of the two it was, in the log
and in the journal (`journal.query --stream gateway`, kind `headless`); nothing
is borrowed from whichever node happens to be connected. It lives on the GATEWAY
because a node that could declare itself main would be granting itself the right
to serve every user's unfurls and every job.

### Workflow & unfurl rules (`cfg workflow_rule`, `cfg unfurl_rule`)

Rules are authored as YAML and loaded whole from a file (the rule shape is
unchanged; only its home moved into the store).

```
murtaugh cfg workflow_rule set    --name <n> --from-file <rule.yaml>
murtaugh cfg workflow_rule list
murtaugh cfg workflow_rule show   --name <n>
murtaugh cfg workflow_rule delete --name <n>

murtaugh cfg unfurl_rule set    --name <n> --from-file <rule.yaml>
murtaugh cfg unfurl_rule list
murtaugh cfg unfurl_rule show   --name <n>
murtaugh cfg unfurl_rule delete --name <n>
```

### Read-only views (singletons)

```
murtaugh cfg defaults show       # runtime defaults
murtaugh cfg journal show        # journal streams, retention, sweep cadence
murtaugh cfg troubleshoot show   # default diagnostics providers
```

### Store-wide operations

```
murtaugh cfg show                # the whole assembled config
murtaugh cfg validate            # validate the store's config without changing it
murtaugh cfg export [--file <path>]   # dump the store (stdout, or a file)
murtaugh cfg import --file <path>     # load a previously exported store
murtaugh cfg db migrate --to <postgres|sqlite> [--dsn-env <VAR>|--sqlite-path <path>]
murtaugh cfg node split [--dest <path>] [--gateway wss://host:port]
murtaugh cfg node set --gateway wss://host:port   # on a node: where it dials
murtaugh cfg node show
```

`cfg db migrate` copies the current store into the target backend and rewrites
the `database:` block of `config.yaml` to point at it. For Postgres, pass
`--dsn-env` naming the `.env` variable that holds the DSN (e.g.
`--dsn-env MURTAUGH_DB_DSN`); the DSN itself is never written to YAML. For
SQLite, `--sqlite-path` overrides the default `config.db` location.

`cfg node split` gives the runtime-node half of a combined installation its own
configuration root (default `~/.config/murtaugh/node/config.yaml`, a directory
of its own so the two roles never share a `.env`, a store or a schema
migration). Agent profiles, MCP servers, jobs, `chat` and `defaults` are
**copied** there; access, election, grants, workflow and unfurl rules stay here,
and node token hashes and conversation pins never travel. Nothing here is
deleted or changed, so it is safe to run against a live gateway and safe to run
twice — the gateway keeps answering in-process until you start
`murtaugh-gateway` instead, which is a choice of binary rather than a
consequence of this command.

A node's backend does not have to match the gateway's: a laptop node on SQLite
attaching to a Firestore-backed gateway is ordinary and supported.

The destination flag is `--dest` and not `--config`. `--config` is the GLOBAL
flag naming the configuration this command runs against — the gateway's — and it
is stripped from the whole command line before any tool sees it, so a second one
would silently retarget the whole invocation instead of naming the destination:

```
murtaugh --config ~/.config/murtaugh/config.yaml \
  cfg node split --dest ~/.config/murtaugh/node/config.yaml
```

**One behaviour changes when profile bodies live on nodes.** The default agent
NAME is checked against a profile BODY at write time today — `cfg chat set
--default-agent typo` is refused. A gateway running as the broker half holds no
bodies, so it cannot make that check, and it moves to **connect time**: when a
node attaches, the gateway resolves the names its configuration uses against the
profiles that user's fleet advertises and journals the ones nothing serves
(`stream=gateway kind=node state=unservable`). "Is my configuration valid" now
depends partly on who is online. It is a warning and never fatal — a gateway
that refused to start with an empty node registry could never start at all.

```
murtaugh cfg agent create --name default --type native --provider gemini \
  --model gemini-2.5-pro --api-key-env GEMINI_API_KEY --tools files --tools terminal
murtaugh cfg access set --admin-user @miere --allowed-users @alex --allowed-users @sam
murtaugh cfg workflow_rule set --name deploy-approve --from-file ./rules/deploy.yaml
murtaugh cfg db migrate --to postgres --dsn-env MURTAUGH_DB_DSN
```

## murtaugh setup bootstrap

Seed the Murtaugh config directory with embedded defaults (`config.yaml` with
its `oauth:` + `database:` blocks, `.env`, `system-prompt.md`, Block Kit
templates, bundled skills). The config store itself is seeded on first run;
everything else (agents, jobs, rules, …) is created there via `cfg …` and the
`setup` tools, not as YAML siblings. Runs on every Murtaugh start, not just the
first.

| Flag      | Required | Type    | Notes                                                              |
|-----------|----------|---------|-------------------------------------------------------------------|
| `--force` | no       | boolean | Refresh the bundled default `system-prompt.md` to the shipped version. |

- **`config.yaml`, `.env`, templates** (`templates/`) and **`AGENTS.md`** (the
  agent's identity) are created once and then **preserved** — your tokens,
  edits, and chosen persona are never overwritten, even with `--force`.
- **`system-prompt.md`** (the default base prompt) is created once and preserved,
  but `--force` refreshes it to the version shipped with the binary.
- **Bundled skills** (`.agents/skills/`) are **refreshed** to the shipped version
  on every run, so the workspace tracks upgrades. A skill directory you add
  yourself is left untouched; an edit to a skill Murtaugh ships is overwritten —
  add a new skill instead of editing a shipped one.

The report lists which files were created, updated (refreshed), and preserved.

```
murtaugh setup bootstrap
murtaugh setup bootstrap --force true   # refresh the default system prompt
```

## murtaugh setup slack

Write the `oauth:` block of `config.yaml` (preserving its `database:` block) and
store the Slack tokens in `~/.config/murtaugh/.env`; the admin user and chat
routing go into the **config store** (`access` + `chat`). The YAML references the
tokens as `${SLACK_APP_TOKEN}` / `${SLACK_BOT_TOKEN}`, so they never live in a
file the troubleshoot bundler collects. Both `config.yaml` and `.env` are backed
up before being replaced/merged.

| Flag              | Required | Type   | Notes                                                       |
|-------------------|----------|--------|-------------------------------------------------------------|
| `--app-token`     | yes      | string | Slack app-level token; must start with `xapp-`. Stored in `.env`. |
| `--bot-token`     | yes      | string | Slack bot OAuth token; must start with `xoxb-`. Stored in `.env`. |
| `--admin-user`    | yes      | string | Admin handle (`@name`) or user ID (`U…`). Written to the store's `access`. |
| `--default-agent` | no       | string | Store agent key to wire into the store's `chat.defaults.agent`. |

```
murtaugh setup slack --app-token xapp-… --bot-token xoxb-… --admin-user @miere
```

## murtaugh setup env

Upsert `KEY=VALUE` secrets into `~/.config/murtaugh/.env`, preserving existing
entries and comments. This is where all secrets live — LLM provider API keys
(store agents reference them by name via `api_key_env`), Slack tokens, and the
Postgres DSN. The file is backed up before being merged. Output reports key
**names** only — never the secret values.

| Flag    | Required | Type            | Notes                                   |
|---------|----------|-----------------|-----------------------------------------|
| `--set` | yes      | string (repeat) | A `KEY=VALUE` pair. Repeat for several. |

```
murtaugh setup env --set GEMINI_API_KEY=AIza… --set VAULTRE_TOKEN=…
```

## murtaugh setup agents

Write the runtime `defaults` and a single named agent into the **config store**.
Supports both backends: a **native** LLM agent (the default — Murtaugh talks to
the model directly) and an external **ACP** agent. The backend is inferred from
the flags when `--kind` is omitted: `--provider` ⇒ native, `--command` ⇒ acp.
With no agent flags chat is left disabled. Secrets are never written to the
store — a native agent records `--api-key-env` (the `.env` variable name); set
the value with `setup env`. (`cfg agent create|update` is the equivalent under
the unified `cfg` surface.)

| Flag                   | Required | Type            | Notes                                                             |
|------------------------|----------|-----------------|------------------------------------------------------------------|
| `--agent-name`         | no       | string          | Key the agent is registered under. Defaults to `default`.         |
| `--kind`               | no       | enum            | `native` or `acp`. Inferred from the other flags when omitted.    |
| `--command`            | acp      | string          | ACP: absolute path to the ACP-speaking binary.                    |
| `--args`               | no       | string (repeat) | ACP: arguments passed to the command.                             |
| `--provider`           | native   | enum            | `gemini`, `anthropic`, or `openai` (compat via `--base-url`).     |
| `--model`              | native   | string          | Provider model id (e.g. `gemini-2.5-pro`).                        |
| `--api-key-env`        | native   | string          | Name of the `.env` variable holding the API key.                  |
| `--base-url`           | no       | string          | Native: endpoint override for compat providers.                   |
| `--tools`              | no       | string (repeat) | Native: tool allowlist (`files`, `terminal`, `skills`, namespaces).|
| `--mcp-servers`        | no       | string (repeat) | Native: `mcp_servers` entries to attach.                          |
| `--system-prompt-file` | no       | string          | Native: path (relative to config dir) to the system prompt.       |
| `--soul-file`          | no       | string          | Persona override; default searches workdir then workspace SOUL.md.|
| `--context-limit`      | no       | integer         | Native: token budget for compaction. 0 = per-family default.      |
| `--compaction`         | no       | enum            | Native: `truncate` (default) or `summarize`.                      |
| `--cache-retention`    | no       | enum            | Native: prompt-cache TTL — `5m` (default), `1h`, or `off`.        |

- For ACP, supplying `--args` without `--command` is an error.

```
murtaugh setup agents --provider gemini --model gemini-2.5-pro \
  --api-key-env GEMINI_API_KEY --tools files --tools terminal --tools skills
murtaugh setup agents --kind acp --agent-name goose --command /usr/local/bin/goose --args acp
```

## murtaugh setup mcp_register

Register Murtaugh as an MCP server in a downstream AI client's config, merging
into the existing file (other keys preserved) and backing it up first.

| Flag            | Required | Type   | Notes                                                          |
|-----------------|----------|--------|----------------------------------------------------------------|
| `--client`      | yes      | enum   | One of `opencode`, `auggie`, `goose`.                          |
| `--binary-path` | yes      | string | Absolute path to the `murtaugh` binary used as the MCP command.|

Target files: `opencode` → `~/.config/opencode/opencode.json`; `auggie` →
`~/.augment/settings.json`; `goose` → `~/.config/goose/config.yaml`.

When the client is also a provider Murtaugh can collect diagnostics for (today
`goose`), it is recorded in the config store's `troubleshoot` settings so
`troubleshoot bundle` and `/murtaugh troubleshoot` include that provider's
sessions/logs **by default** (no `--include` needed). Recording is best-effort —
a failure there only adds a warning, it does not fail the client registration.

```
murtaugh setup mcp_register --client opencode --binary-path /usr/local/bin/murtaugh
murtaugh setup mcp_register --client goose --binary-path /usr/local/bin/murtaugh
```

## murtaugh setup launchd

Write the `dev.murtaugh` LaunchAgent plist (macOS only) and optionally load it
via launchctl. On non-macOS hosts it returns a clean "unsupported on <os>"
error. An existing plist is backed up first.

| Flag            | Required | Type    | Notes                                                              |
|-----------------|----------|---------|--------------------------------------------------------------------|
| `--binary-path` | yes      | string  | Absolute path to the `murtaugh` binary.                            |
| `--load`        | no       | boolean | `true` runs `launchctl bootout`+`bootstrap`+`kickstart` after writing. Remember booleans need a value: `--load true`. |

```
murtaugh setup launchd --binary-path /usr/local/bin/murtaugh --load true
```

## murtaugh setup update

Replace the running Murtaugh binary with the matching asset from a GitHub
release. The fetched asset is verified before the swap; the previous binary is
backed up.

| Flag                 | Required | Type    | Notes                                                            |
|----------------------|----------|---------|------------------------------------------------------------------|
| `--version`          | no       | string  | Release tag to install. Default: latest release.                 |
| `--force`            | no       | boolean | Update even when the current build is `dev` or already current. `--force true`. |
| `--release-json-url` | no       | string  | Override the release-metadata URL. Mainly for tests/fixtures.    |

- A `dev` build is refused unless `--force true` (it is likely a local checkout).
- An already-current install short-circuits with a "nothing to do" result.

```
murtaugh setup update
murtaugh setup update --version v0.5.0 --force true
```

## murtaugh troubleshoot bundle

Assemble a self-contained **diagnostics bundle** (a zip) for investigating a
problem: a consistent snapshot of the event journal, the ACP transcripts, the
daemon logs (tail-truncated), the **redacted** config files, optional
downstream-provider artifacts (e.g. Goose sessions + logs), a `manifest.json`,
and an `INSTRUCTIONS.md` telling an AI agent how to read it. Deterministic — it
never asks an agent to gather the files.

| Flag             | Required | Type            | Notes                                                                          |
|------------------|----------|-----------------|--------------------------------------------------------------------------------|
| `--note`         | no       | string          | Symptom description; recorded in the manifest.                                 |
| `--include`      | no       | string (repeat) | Provider whose on-disk diagnostics to add (known: `goose`). Repeat per provider. Defaults to the providers in the store's `troubleshoot` settings (written by `setup mcp_register`), else all known providers.|
| `--out`          | no       | string          | Output path for the zip. Defaults to a timestamped file in the temp dir.       |
| `--max-log-bytes`| no       | integer         | Tail cap per log file in bytes. Defaults to 5 MiB.                             |
| `--redact`       | no       | boolean         | Redact known secrets. Defaults to `true`; only set `false` for local-only use. |

- Redaction removes Slack tokens (`xoxb-`/`xapp-`/`xoxp-`) and the values of
  obviously-secret config keys. It **cannot** scrub secrets inside conversation
  transcripts or binary `*.db` files — treat the bundle as sensitive.
- The same capability is exposed over MCP as `troubleshoot_bundle`, and in Slack
  as `/murtaugh troubleshoot <symptoms>` (which DMs the bundle to the admin).
- Missing sources (e.g. logs on a non-macOS host, or a provider that isn't
  installed) are skipped and noted in `manifest.json`, never fatal.

```
murtaugh troubleshoot bundle --note "bot goes silent on action requests" --include goose
murtaugh troubleshoot bundle --out /tmp/murtaugh-diag.zip --max-log-bytes 1048576
```

## murtaugh auth request

Request credentials the agent does not have, and block until the configured
admin grants them. The command never authenticates anything itself: it posts a
card to the admin's DM and waits for them to complete the sign-in, deny it, or
let it expire.

Two people are involved. Whoever triggered the turn gets a short notice in their
own thread ("your admin has been notified") and nothing else — no buttons, no
command output. The admin gets the card that does the work. When the requester
*is* the admin, or there is no thread at all (CLI/MCP), the two collapse into a
single card.

Flags:

- `--tool` (required) — the capability that needs authentication, named as the
  user knows it (e.g. `gcp-mcp`, `postgres-mcp`). Name the **directly affected**
  tool, not the helper binary it shells out to underneath: an admin approving
  "the tool 'gcloud' requires authentication" has no idea what they are
  approving. Name the helper only when it is being invoked directly.
- `--profile` (required) — which workflow to run:
  - `gcloud` — `gcloud auth login`, signing in the user credential. Finishes
    with a verification code.
  - `gcloud-adc` — `gcloud auth application-default login`, writing the
    application-default credentials that client libraries and MCP servers
    usually read. Finishes with a verification code.
  - `custom` — run `--command` in the background. For flows Murtaugh does not
    ship a profile for.
  - (`aws` is named in the design but not implemented yet, and is rejected.)
- `--command` — only with `--profile custom`: the command line to run. Passing
  it alongside a built-in profile is an error rather than a silent no-op.
- `--needs-code` — only with `--profile custom`: `true` when the flow completes
  by pasting a verification code back, `false` when the whole exchange happens
  in the browser. Defaults to `false`. Booleans need an explicit value on the
  CLI (`--needs-code true`).

The card layout follows from the flow. A code flow shows **Enter Code** as the
primary button with **Open In Browser** beside it; a browser-only flow shows
**Open In Browser** as the primary and no code button. The primary button is a
single attempt: clicking it retires the whole button bar and reveals the footer,
and the request then runs to completion on its own.

Fails closed, always. A denial, a timeout, a failed sign-in, no configured
admin, or an undeliverable card all return an error — never a partial success —
so the caller stops rather than retrying the call that lacked credentials.

Requires `configuration.admin_user` to be set; with no admin nobody can approve,
and the request is refused before anything is posted.

```
murtaugh auth request --tool gcp-mcp --profile gcloud-adc
murtaugh auth request --tool vendor-mcp --profile custom --command "vendor-cli login --headless" --needs-code false
```

## murtaugh node token mint

Issue a bearer token for a **runtime node** — a host that runs agents on the
gateway's behalf. The token is displayed **once**: only its hash is stored, so
it cannot be recovered afterwards, and a lost one is replaced by minting another
rather than looked up.

A node never tells the gateway who it is. It presents the token, and the gateway
resolves which node and which user that token was minted for — which is why
`--node` and `--user` are recorded here and are not something a node can assert
later.

| Flag           | Required | Type     | Notes                                                                                  |
|----------------|----------|----------|----------------------------------------------------------------------------------------|
| `--node`       | yes      | string   | Node id this credential identifies.                                                    |
| `--user`       | yes      | string   | Murtaugh user the node acts for.                                                       |
| `--label`      | no       | string   | Free-text note about where the credential lives (`mac mini`, `rotation 2026-09`).      |
| `--expires-in` | no       | duration | Go duration (e.g. `720h`). Omitted means the credential lasts until it is revoked.      |
| `--token-file` | no       | string   | Write the token to this file (mode `0600`) instead of printing it. Refuses to overwrite.|

- Tokens carry the prefix `mrtg_node_`, so a leaked one is greppable in a log and
  matchable by a secret scanner. `troubleshoot bundle` redacts on that prefix.
- Put the file on the node, mode `0600`. A seatbelt-confined agent is denied it
  unconditionally — reads and writes both, wherever the file sits — but the
  sandbox is off by default and exists only on macOS. **Where it is off, nothing
  protects the token from the agents this node runs**: they run as the daemon's
  own uid, so `0600` does not exclude them, and the default agent workdir is the
  very directory the token lives in. The sandbox is the only mitigation there is.
- Node credentials live in a side table of the configured database, alongside
  job runs and leader locks. They are **not** part of the config, so `cfg show`
  never prints them and `cfg db migrate` does not carry them: after switching
  backends, re-enrol the nodes.
- Minting is an approval-gated tool, so an agent that has been given it still
  needs a human's yes.

```
murtaugh node token mint --node mac-mini --user U012ABCDEF --label "office mac"
murtaugh node token mint --node ci-box --user U012ABCDEF --expires-in 720h --token-file /etc/murtaugh/node-token
```

## murtaugh node token list

List issued node credentials with their state (`live`, `expired`, `revoked`).
Never shows a token — only a hash is stored — and does not show the hash either;
credentials are addressed by their **selector**, the public half printed here.

| Flag     | Required | Type   | Notes                                                     |
|----------|----------|--------|-----------------------------------------------------------|
| `--node` | no       | string | Only this node's credentials. Omitted lists every node's.  |

```
murtaugh node token list
murtaugh node token list --node mac-mini
```

## murtaugh node token revoke

Withdraw a credential. Revoke one by selector during a rotation, or every
credential a node holds when the node itself is compromised.

| Flag         | Required | Type   | Notes                                                              |
|--------------|----------|--------|--------------------------------------------------------------------|
| `--selector` | one of   | string | The credential to revoke, from `node token list`.                  |
| `--node`     | one of   | string | Revoke every live credential this node holds.                      |

- `--selector` and `--node` are **mutually exclusive**, and one is required: a
  revoke that guessed would be worse than one that refuses.
- Two credentials may be live for one node at once, which is what makes rotation
  need no downtime: mint the replacement, install it, then revoke the old one.
- A revoked credential stops verifying immediately, but **a connection already
  authenticated with it stays open** until the gateway learns to close it (#193).
  Revoking is not yet the same as disconnecting.

```
murtaugh node token revoke --selector 1a2b3c4d5e6f7a8b
murtaugh node token revoke --node mac-mini
```

## murtaugh version

Print the binary's version string (e.g. `v0.4.1` or `dev`). Takes no flags.

```
murtaugh version
```

## murtaugh help

Print this reference. With no argument, prints the full document; with a command
argument, prints just that command's section.

```
murtaugh help
murtaugh help slack send_msg
murtaugh help jobs define
```
