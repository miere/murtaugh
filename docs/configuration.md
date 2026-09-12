# Configuration

Murtaugh keeps almost nothing on disk. Two files live in a **configuration
root** (override the path with `--config /path/to/config.yaml`); **everything
else lives in a database** and is managed with the `cfg …` admin CLI.

**Each binary has its own root, and its own half of `cfg`.** The gateway's root
is `~/.config/murtaugh`, a node's is `~/.config/murtaugh/node` — a directory of
its own, so the two roles never share a `.env`, a store or a schema migration. A
node's backend need not match the gateway's: a laptop node on SQLite attaching
to a Firestore-backed gateway is ordinary and supported.

| Owner | Root | Carries | `cfg` groups |
|---|---|---|---|
| `murtaugh-gateway` | `~/.config/murtaugh` | Slack tokens, access, election, jobs, workflow/unfurl rules, node token hashes | `access`, `job`, `election`, `workflow_rule`, `unfurl_rule`, `node split` |
| `murtaugh-runtime` | `~/.config/murtaugh/node` | its node credential, the gateway seed address, agent profiles, MCP servers, provider keys | `agent`, `mcp`, `defaults`, `node set\|show` |

`chat`, `show`, `export`, `import`, `db migrate`, `validate`, `migrate` and
`launchd` are on **both**, acting on that binary's own configuration. Asking a
binary for the other half's command is an unknown command, not a permission
error — the role is implied by which file you ran, so nothing takes a `--role`.

The examples below are written `murtaugh-gateway …` or `murtaugh-runtime …`
according to which one owns the setting.

| Where | Holds | Reference |
|---|---|---|
| `.env` | **All secrets** — Slack tokens, provider API keys, the Postgres DSN. Mode `0600`. | [below](#env--secrets) |
| `config.yaml` | Two blocks only: `oauth:` (Slack tokens) and `database:` (the config-store backend). | [below](#configyaml) |
| the **config store** (SQLite/Postgres) | Agents, MCP servers, jobs, chat routing, access control, runtime defaults, journal, troubleshoot providers, workflow-rules, unfurl-rules. | [The `cfg` surface](#the-cfg-surface) |

> **Golden rule:** secrets live **only** in `.env`. `config.yaml` and the config
> store reference them as `${VAR}`. This is what lets `murtaugh troubleshoot`
> bundle your configuration for sharing without leaking credentials — the bundler
> never collects `.env`.

The old sibling files — `agents.yaml`, `jobs.yaml`, `journal.yaml`,
`workflow-rules.yaml`, `unfurl-rules.yaml`, `troubleshoot.yaml` — **no longer
exist** as the source of truth. Their contents are now records in the config
store, edited with `cfg …` (see below). If you are upgrading from a
YAML-tree install, the [auto-migration](#upgrading-from-the-yaml-tree) moves them
into the store for you.

---

## `.env` — secrets

```sh
# ~/.config/murtaugh/.env   (mode 0600 — keep it secret)

# --- Slack (required to run the gateway) ---
SLACK_APP_TOKEN=xapp-replace-me
SLACK_BOT_TOKEN=xoxb-replace-me
# Optional: the admin's own user token (xoxp-…). Enables `slack send_msg --as
# admin`, which posts under the admin's real identity. Leave unset to disable.
# SLACK_USER_TOKEN=xoxp-replace-me

# --- LLM providers (only the ones your native agents use) ---
# The variable NAME is what an agent's `--api-key-env` points at.
GEMINI_API_KEY=
ANTHROPIC_API_KEY=
OPENAI_API_KEY=

# --- Postgres config store (only if you switch off the default SQLite) ---
# MURTAUGH_DB_DSN=postgres://murtaugh:secret@localhost:5432/murtaugh?sslmode=disable

# --- External MCP servers (optional) ---
# VAULTRE_TOKEN=
```

A value exported in the real environment overrides the one here. **Edit the file
directly** — `setup env` is gone.

This is the **gateway's** `.env`. The provider keys belong in the *node's*
(`~/.config/murtaugh/node/.env`), because that is where agents run; a node's
`.env` deliberately holds no `SLACK_*` variables at all. Both files are seeded as
commented templates the first time a binary runs against their root.

---

## `config.yaml`

Slimmed to two blocks: how the gateway authenticates to Slack, and which
config-store backend it reads everything else from.

```yaml
# ~/.config/murtaugh/config.yaml
oauth:
  app_token: ${SLACK_APP_TOKEN}   # xapp-… Socket Mode token
  bot_token: ${SLACK_BOT_TOKEN}   # xoxb-… bot token
  user_token: ${SLACK_USER_TOKEN} # xoxp-… admin user token; optional.
                                  # Enables `slack send_msg --as admin` (posts
                                  # under the admin's real identity). Omit to disable.

database:
  backend: sqlite                 # sqlite (default) | postgres
  # sqlite:
  #   path: /custom/config.db     # default: config.db beside this file
  # postgres:
  #   dsn: ${MURTAUGH_DB_DSN}     # DSN lives in .env; referenced as ${VAR}
```

### The config store

`database:` selects where the rest of the configuration lives:

- **`backend: sqlite`** (default) — a single file beside the bootstrap file,
  named after it: `config.yaml` → `config.db`, `slack-nurturecloud.yaml` →
  `slack-nurturecloud.db` (override with `sqlite.path`). Because the filename
  follows the config, several configs can live in one directory without sharing
  a store. Zero setup; ideal for one host.
- **`backend: postgres`** — `postgres.dsn`, referenced as `${VAR}` so the real
  DSN stays in `.env`. Use this to share one config store across hosts.

You rarely hand-edit `database:`. Switch backends with
[`cfg db migrate`](#switching-the-store-backend), which copies the whole
store and rewrites this block for you.

Everything that used to live in the sibling YAMLs — agents, jobs, chat routing,
access control, and the rest — is now read from this store. Edit it with
[`cfg …`](#the-cfg-surface).

### Slash commands

Slash commands (`/murtaugh`, `/stop`) are registered in the **Slack app
manifest**, not here. Murtaugh recognises the verbs `chat`, `stop`,
`troubleshoot`, `restart`, and `help` (e.g. `/murtaugh stop`, or a standalone
`/stop`).

---

## The `cfg` surface

`cfg …` is the admin CLI for the config store, carried by both binaries over
their own half of the surface. Every command is **also** an MCP tool, named with
underscores where the CLI uses spaces — `murtaugh-runtime cfg agent create`
publishes as `cfg_agent_create` — so an agent can reconfigure Murtaugh the same
way you can.

Two rules apply to every mutation:

- **Whole-config re-validation.** Each `cfg` change re-validates the *entire*
  configuration and **rejects + rolls back** anything that would leave the store
  invalid — a bad reference or a malformed value is caught immediately, not at the
  next restart.
- **Load-once runtime.** The gateway still reads config **once at startup**. After
  any `cfg` change, **restart the gateway to apply it** (see
  [Applying changes](#applying-changes)).

Conventions: booleans are **explicit** (`--enabled true`, never a bare
`--enabled`); repeatable flags build arrays (`--tools files --tools terminal`);
all flags are `--kebab-case value`.

### Agents

```sh
murtaugh-runtime cfg agent create --name emily --type native \
  --workdir '${HOME}/work/emily' \
  --tools files --tools terminal --tools skills \
  --provider gemini --model gemini-2.5-pro --api-key-env GEMINI_API_KEY

murtaugh-runtime cfg agent update --name emily --max-turns 40
murtaugh-runtime cfg agent list
murtaugh-runtime cfg agent show --name emily
murtaugh-runtime cfg agent delete --name emily
```

`--type` is one of `native`, `acp`, or `claude_code`. See
[Agent chat](agents.md) for the full flag reference (native vs ACP/claude_code
flags, tools, approval).

### MCP servers

```sh
murtaugh-runtime cfg mcp set --name vaultre \
  --command vaultre-mcp --arg --stdio --env VAULTRE_TOKEN=${VAULTRE_TOKEN}
murtaugh-runtime cfg mcp set --name data-api --url https://data-api.internal/mcp
murtaugh-runtime cfg mcp list
murtaugh-runtime cfg mcp show --name vaultre
murtaugh-runtime cfg mcp delete --name vaultre
```

Each server uses exactly one transport: a stdio child process (`--command` +
repeatable `--arg`/`--env`) or a remote endpoint (`--url`). Attach a server to an
agent with `cfg agent … --mcp-servers <name>` (repeatable).

### Jobs

```sh
murtaugh-gateway cfg job set --name nightly-backup \
  --command /usr/local/bin/backup.sh --schedule "0 2 * * *"
murtaugh-gateway cfg job set --name code-review-job \
  --agent default --prompt 'Review PR {{ 1 }} in {{ 2 }}.'
murtaugh-gateway cfg job list
murtaugh-gateway cfg job show --name nightly-backup
murtaugh-gateway cfg job delete --name nightly-backup
```

See [Jobs](jobs.md) for command vs agent jobs, scheduling, and the run-time
tools.

### Chat routing

```sh
murtaugh-gateway cfg chat set --enabled true --default-agent default
murtaugh-gateway cfg chat set --dm-agent support --reply-on-thread true
murtaugh-gateway cfg chat show
```

`chat` is one of the blocks **both** binaries carry, and it means a different
thing on each. On the gateway it is the Slack surface: `--enabled` gates DMs and
`@mentions`, and the routing below decides which agent NAME a conversation asks
for. On a node it decides which of that node's own agents the process serves.

`--enabled` gates **only** the DM + `@mention` chat surface. A routed agent name
is checked against a profile BODY, so **where** you set it decides when the
check happens: `murtaugh-runtime cfg chat set --default-agent typo` is rejected
on the spot, while the gateway holds no bodies and accepts the name, resolving
it at **connect time** against the profiles the connected nodes advertise. A
name nothing serves is journalled (`stream=gateway kind=node
state=unservable`) and is a warning, never fatal — a gateway that refused to
start with an empty node registry could never start at all. See
[Slack → chat routing](slack.md).

Per-channel routing lives in `chat.channels`, an **ordered list** where the
**first matching rule wins**:

```yaml
chat:
  channels:
    - match: "nc-*"          # channel ID, exact name, or a `*` glob on the name
      agent: coder
      allow_anyone: true
    - match: "mt-*"
      agent: admin
```

Order is the precedence — there is no specificity scoring, so a narrow rule must
be listed **above** the broader one it needs to beat:

```yaml
    - match: "nc-secrets"    # stays closed…
      agent: admin
    - match: "nc-*"          # …even though this rule would have opened it
      agent: coder
      allow_anyone: true
```

A rule with no `agent` still matches and falls back to `chat.defaults.agent`,
so a rule can override the reply strategy or access alone. Duplicate `match`
values are rejected: past the first, they could never be reached.

> The older map shape (`channels: {"nc-*": {agent: coder}}`) still loads and keeps
> its previous precedence — exact IDs, then exact names, then longest-glob-prefix.
> It is rewritten to the ordered list the next time the chat config is saved.

### Access control

```sh
murtaugh-gateway cfg access set --admin-user your-slack-handle \
  --allowed-users U0123ABC --allowed-users alice --debug false
murtaugh-gateway cfg access show
```

Access is **fail-closed**: only `--admin-user` plus everyone in
`--allowed-users` may interact. The admin is always implicitly allowed; an empty
allowed-users list keeps the bot admin-only. Entries may be Slack user IDs
(`U0123ABC`) or handles (`alice`, `@alice`); handles are resolved to IDs at
startup and **the gateway refuses to start if any entry can't be resolved**.

> Access gates *who can act*, not *who can see*. A message posted to a channel is
> visible to every member — use a DM or an ephemeral message for private replies.

#### Opening a channel to everyone

A `chat.channels` rule may set `allow_anyone: true` to waive the allowlist for
that channel, letting any workspace user who can post there talk to the routed
agent. The typical pairing is a channel whose agent carries a deliberately narrow
toolset:

```yaml
chat:
  channels:
    - match: "nc-*"          # anyone can use the coding agent here
      agent: coder
      allow_anyone: true
    - match: "mt-*"          # admin-only, full toolset
      agent: admin
```

Authority follows the same ladder everywhere: `admin_user` → `allowed_users` →
the matched channel rule's `allow_anyone`. A guest admitted by the third rung
may do two things in that channel, and nothing else:

- **talk to the routed agent** — `@mentions` and plain messages;
- **answer the prompts that agent raises** — `ask` questions and **tool-approval
  buttons**. A guest who may ask the agent to act may also answer what it asks
  back; otherwise their turn would stall on a button they cannot click.

The waiver does **not**:

- waive the `@mention` requirement (that is `chat.no_mention`, kept separate so
  an opened channel does not have the bot answering every message in it);
- extend to slash commands, including `/murtaugh troubleshoot`, whose bundle can
  carry sensitive data;
- extend to **workflow rules**, whose configured actions can run commands and
  delegate to other agents — their blast radius is not bounded by the channel
  agent's toolset, so they stay allowlist-only;
- extend to DMs, restart, or the App Home controls.

Because a guest's approval authority is scoped to the channel, what an opened
channel's agent may be *talked into* doing is bounded by that agent's `tools`,
`mcp_servers`, and `approval` policy. Scope those deliberately: they, not the
allowlist, are what limits an opened channel.

> Approval is by **channel**, not by turn: anyone admitted in the channel can
> answer a prompt raised there, including one from someone else's turn.

> An opened channel is a spend and resource surface: anyone who joins can start
> agent turns. There is no per-user rate limit yet — scope the glob accordingly.

#### Node grants

`access.node_grants` records who, besides its owner, may have a conversation
delegated to a runtime node. The key is a **node id**; the value is the Slack
user IDs holding a grant on it:

```yaml
access:
  node_grants:
    laptop-mira: ["U0ALEX", "U0SAM"]
```

A conversation is served by the initiating user's **own** connected nodes, or —
only when they have none connected — by the nodes they hold a grant on. Never a
mixture: a user with a node of their own never lands on somebody else's, however
many grants they hold.

Two limits while this is manual configuration. Keys and values are Slack IDs, not
handles — nothing rewrites a handle here the way it does in `allowed_users`, so a
handle silently matches nobody. And there is no ownership check, because the only
writer is the gateway admin.

#### The main node

`access.main_node` names the node id that serves **headless** work — scheduled
jobs, workflow triggers and link unfurling:

```yaml
access:
  main_node: laptop-mira
```

None of those has a user to choose a node for. A cron at 03:00 has nobody, and an
unfurl's user is whoever pasted the link — usually somebody who owns no node and
holds no grant. So they do not go through delegation at all: they go to the one
node the gateway admin designated.

It is written **here**, on the gateway, and never on the node's own
configuration. Being main is the right to serve every user's unfurls and every
scheduled job, which is the largest grant this gateway makes, and a node that
could declare itself main would be granting it to itself.

Set it with `murtaugh-gateway cfg access set --main-node <node-id>`; pass an empty string
to clear it. With none set — or with the designated node not attached — every
headless surface **refuses and says so**, in the log and in the journal (gateway
stream, kind `headless`). Nothing is borrowed from whichever node happens to be
connected. A job whose node is asleep does not run and is not replayed, which is
the same policy that already applies to a missed occurrence.

Grants apply only to the runtime-node split (`murtaugh-gateway -node-listen`).
They have nothing to do with the tool-approval "always allow" grants a user
accumulates in a conversation.

### Workflow rules and unfurl rules

These carry richer nested structure, so they are set from a YAML fragment on
disk rather than a flat flag list:

```sh
murtaugh-gateway cfg workflow_rule set --name code-review-approval --from-file rule.yaml
murtaugh-gateway cfg workflow_rule list
murtaugh-gateway cfg workflow_rule show --name code-review-approval
murtaugh-gateway cfg workflow_rule delete --name code-review-approval

murtaugh-gateway cfg unfurl_rule set --name github-pr --from-file unfurl.yaml
murtaugh-gateway cfg unfurl_rule list
murtaugh-gateway cfg unfurl_rule show --name github-pr
murtaugh-gateway cfg unfurl_rule delete --name github-pr
```

See [Slack → workflow rules](slack.md#workflow-rules) and
[Slack → link unfurling](slack.md#link-unfurling) for the fragment shape.

### Read-only views

Some blocks are read-only from the CLI — inspect them with a `show`:

```sh
murtaugh-runtime cfg defaults show      # runtime defaults (session, rendering, acp, approval)
murtaugh-gateway cfg journal show       # journal streams and retention
murtaugh-gateway cfg troubleshoot show  # troubleshoot providers
```

`troubleshoot.providers` has no setter: it is a manual knob whose **empty
default means every provider Murtaugh knows how to collect diagnostics for**
(today `goose` and `claude-code`). To pin a narrower list, edit a `cfg export`
snapshot and `cfg import` it back — see
[Operations → Ship a diagnostics bundle](operations.md#ship-a-diagnostics-bundle).

Runtime defaults are covered in [Agent chat → Runtime defaults](agents.md#runtime-defaults);
journal tuning in [Gateway Debug Mode](journal.md).

### Store-wide operations

```sh
murtaugh-gateway cfg show               # dump the whole config as JSON
murtaugh-gateway cfg validate           # re-validate the store without changing it
murtaugh-gateway cfg export --file cfg.json   # export the whole store (to stdout if no --file)
murtaugh-gateway cfg import --file cfg.json   # replace the store from an export
murtaugh-gateway cfg db migrate --to postgres --dsn-env MURTAUGH_DB_DSN
```

These act on **that binary's own** configuration; run them on the runtime binary
to reach a node's. `cfg export` / `cfg import` move a complete configuration
between hosts. `cfg validate` is the same whole-config check every mutation
runs, on demand, judged as the role the binary implies — there is no `--role`.

Two more are installer-shaped and also on both:

```sh
murtaugh-gateway cfg migrate                 # bring the config DIRECTORY to this version's schema
murtaugh-gateway cfg launchd [--alias <a>] [--update-existing true]
```

`cfg migrate` and `cfg db migrate` are different: the first brings the
configuration *directory* up to the schema this version expects (which the
daemons also do at startup), the second moves the store's *content* between
backends. `cfg launchd` is covered in
[Operations](operations.md#as-a-daemon-macos).

---

## Database backends

### SQLite (default)

Nothing to set up. The store is a single file — `config.db` in the config
directory (beside `config.yaml`) by default; set `database.sqlite.path` to move
it elsewhere. This is the right choice for a single host.

### Postgres

Use Postgres to share one config store across hosts. Put the DSN in `.env`, then
migrate:

```sh
# 1. add the DSN to ~/.config/murtaugh/.env
#    MURTAUGH_DB_DSN=postgres://murtaugh:secret@host:5432/murtaugh?sslmode=disable

# 2. copy the whole store into Postgres and rewrite config.yaml
murtaugh-gateway cfg db migrate --to postgres --dsn-env MURTAUGH_DB_DSN
```

`cfg db migrate` copies everything and **rewrites `config.yaml`'s `database:`
block for you** — you don't hand-edit it. Migrate back to a file with
`--to sqlite --sqlite-path ~/.config/murtaugh/config.db`.

---

## Upgrading from the YAML tree

If you are upgrading a host that still has the old sibling YAMLs, the **first run
of the new binary auto-migrates them** — no action required, and it's idempotent:

1. The existing YAML tree (`agents.yaml`, `jobs.yaml`, `journal.yaml`,
   `workflow-rules.yaml`, `unfurl-rules.yaml`, `troubleshoot.yaml`, plus the
   `access`/`chat` blocks that used to live in `config.yaml`) is read and
   imported into a SQLite config store at `~/.config/murtaugh/config.db`.
2. `config.yaml` is rewritten down to just the `oauth:` and `database:` blocks.
3. The old sibling YAMLs are **moved** (never deleted) into
   `~/.config/murtaugh/migrated-<timestamp>/`, so the originals stay recoverable.

From then on, edit configuration with `cfg …`. See
[Operations](operations.md#applying-config-changes) for the operational view.

---

## Applying changes

The gateway loads config **once at startup** — it never hot-reloads. After any
`cfg …` change, restart the daemon that reads it. When the store changes the running
daemon *suggests* a restart (via an admin-only button) but applies nothing until
you do. See [Operations](operations.md#applying-config-changes).

## Reference assets

The repository's `assets/` directory ships fully-commented `config.yaml` /
`env.example` starters for a gateway and `node-config.yaml` / `node-env.example`
for a node, plus default Block Kit templates. The first command a binary runs
against a root copies the right pair in and initialises an empty config store;
you can also read the templates in-tree as the canonical reference.
