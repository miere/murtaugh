# Configuration

Murtaugh keeps almost nothing on disk. Two files live in
`~/.config/murtaugh/` (override the gateway path with
`--config /path/to/config.yaml`); **everything else lives in a database** and is
managed with the `murtaugh cfg …` admin CLI.

| Where | Holds | Reference |
|---|---|---|
| `.env` | **All secrets** — Slack tokens, provider API keys, the Postgres DSN. Mode `0600`. | [below](#env--secrets) |
| `config.yaml` | Two blocks only: `oauth:` (Slack tokens) and `database:` (the config-store backend). | [below](#configyaml) |
| the **config store** (SQLite/Postgres) | Agents, MCP servers, jobs, chat routing, access control, runtime defaults, journal, troubleshoot providers, workflow-rules, unfurl-rules. | [The `cfg` surface](#the-murtaugh-cfg-surface) |

> **Golden rule:** secrets live **only** in `.env`. `config.yaml` and the config
> store reference them as `${VAR}`. This is what lets `murtaugh troubleshoot`
> bundle your configuration for sharing without leaking credentials — the bundler
> never collects `.env`.

The old sibling files — `agents.yaml`, `jobs.yaml`, `journal.yaml`,
`workflow-rules.yaml`, `unfurl-rules.yaml`, `troubleshoot.yaml` — **no longer
exist** as the source of truth. Their contents are now records in the config
store, edited with `murtaugh cfg …` (see below). If you are upgrading from a
YAML-tree install, the [auto-migration](#upgrading-from-the-yaml-tree) moves them
into the store for you.

---

## `.env` — secrets

```sh
# ~/.config/murtaugh/.env   (mode 0600 — keep it secret)

# --- Slack (required to run the gateway) ---
SLACK_APP_TOKEN=xapp-replace-me
SLACK_BOT_TOKEN=xoxb-replace-me
# Optional: the admin's own user token (xoxp-…). Enables `slack send-msg --as
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

A value exported in the real environment overrides the one here. Write provider
keys with `murtaugh setup env --provider gemini --key ...` or by editing the file
directly.

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
                                  # Enables `slack send-msg --as admin` (posts
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
[`murtaugh cfg db migrate`](#switching-the-store-backend), which copies the whole
store and rewrites this block for you.

Everything that used to live in the sibling YAMLs — agents, jobs, chat routing,
access control, and the rest — is now read from this store. Edit it with
[`murtaugh cfg …`](#the-murtaugh-cfg-surface).

### Slash commands

Slash commands (`/murtaugh`, `/stop`) are registered in the **Slack app
manifest**, not here. Murtaugh recognises the verbs `chat`, `stop`,
`troubleshoot`, `restart`, and `help` (e.g. `/murtaugh stop`, or a standalone
`/stop`).

---

## The `murtaugh cfg` surface

`murtaugh cfg …` is the admin CLI for the config store. Every command is **also**
an MCP tool with the same name (dots for spaces — e.g. `cfg.agent.create`), so an
agent can reconfigure Murtaugh the same way you can.

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
murtaugh cfg agent create --name emily --type native \
  --workdir '${HOME}/work/emily' \
  --tools files --tools terminal --tools skills --tools slack \
  --provider gemini --model gemini-2.5-pro --api-key-env GEMINI_API_KEY

murtaugh cfg agent update --name emily --max-turns 40
murtaugh cfg agent list
murtaugh cfg agent show --name emily
murtaugh cfg agent delete --name emily
```

`--type` is one of `native`, `acp`, or `claude_code`. See
[Agent chat](agents.md) for the full flag reference (native vs ACP/claude_code
flags, tools, approval).

### MCP servers

```sh
murtaugh cfg mcp set --name vaultre \
  --command vaultre-mcp --arg --stdio --env VAULTRE_TOKEN=${VAULTRE_TOKEN}
murtaugh cfg mcp set --name data-api --url https://data-api.internal/mcp
murtaugh cfg mcp list
murtaugh cfg mcp show --name vaultre
murtaugh cfg mcp delete --name vaultre
```

Each server uses exactly one transport: a stdio child process (`--command` +
repeatable `--arg`/`--env`) or a remote endpoint (`--url`). Attach a server to an
agent with `cfg agent … --mcp-servers <name>` (repeatable).

### Jobs

```sh
murtaugh cfg job set --name nightly-backup \
  --command /usr/local/bin/backup.sh --schedule "0 2 * * *"
murtaugh cfg job set --name code-review-job \
  --agent default --prompt 'Review PR {{ 1 }} in {{ 2 }}.'
murtaugh cfg job list
murtaugh cfg job show --name nightly-backup
murtaugh cfg job delete --name nightly-backup
```

See [Jobs](jobs.md) for command vs agent jobs, scheduling, and the run-time
tools.

### Chat routing

```sh
murtaugh cfg chat set --enabled true --default-agent default
murtaugh cfg chat set --dm-agent support --reply-on-thread true
murtaugh cfg chat show
```

`--enabled` gates **only** the DM + `@mention` chat surface. When enabled,
`--default-agent` is required and every routed agent name must exist, or the
store rejects the change. See [Slack → chat routing](slack.md).

### Access control

```sh
murtaugh cfg access set --admin-user your-slack-handle \
  --allowed-users U0123ABC --allowed-users alice --debug false
murtaugh cfg access show
```

Access is **fail-closed**: only `--admin-user` plus everyone in
`--allowed-users` may interact. The admin is always implicitly allowed; an empty
allowed-users list keeps the bot admin-only. Entries may be Slack user IDs
(`U0123ABC`) or handles (`alice`, `@alice`); handles are resolved to IDs at
startup and **the gateway refuses to start if any entry can't be resolved**.

> Access gates *who can act*, not *who can see*. A message posted to a channel is
> visible to every member — use a DM or an ephemeral message for private replies.

### Workflow rules and unfurl rules

These carry richer nested structure, so they are set from a YAML fragment on
disk rather than a flat flag list:

```sh
murtaugh cfg workflow-rule set --name code-review-approval --from-file rule.yaml
murtaugh cfg workflow-rule list
murtaugh cfg workflow-rule show --name code-review-approval
murtaugh cfg workflow-rule delete --name code-review-approval

murtaugh cfg unfurl-rule set --name github-pr --from-file unfurl.yaml
murtaugh cfg unfurl-rule list
murtaugh cfg unfurl-rule show --name github-pr
murtaugh cfg unfurl-rule delete --name github-pr
```

See [Slack → workflow rules](slack.md#workflow-rules) and
[Slack → link unfurling](slack.md#link-unfurling) for the fragment shape.

### Read-only views

Some blocks are read-only from the CLI — inspect them with a `show`:

```sh
murtaugh cfg defaults show      # runtime defaults (session, rendering, acp, approval)
murtaugh cfg journal show       # journal streams and retention
murtaugh cfg troubleshoot show  # troubleshoot providers
```

Runtime defaults are covered in [Agent chat → Runtime defaults](agents.md#runtime-defaults);
journal tuning in [Gateway Debug Mode](journal.md).

### Store-wide operations

```sh
murtaugh cfg show               # dump the whole config as JSON
murtaugh cfg validate           # re-validate the store without changing it
murtaugh cfg export --file cfg.json   # export the whole store (to stdout if no --file)
murtaugh cfg import --file cfg.json   # replace the store from an export
murtaugh cfg db migrate --to postgres --dsn-env MURTAUGH_DB_DSN
```

`cfg export` / `cfg import` move a complete configuration between hosts. `cfg
validate` is the same whole-config check every mutation runs, on demand.

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
murtaugh cfg db migrate --to postgres --dsn-env MURTAUGH_DB_DSN
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

From then on, edit configuration with `murtaugh cfg …`. See
[Operations](operations.md#applying-config-changes) for the operational view.

---

## Applying changes

The gateway loads config **once at startup** — it never hot-reloads. After any
`murtaugh cfg …` change, restart the gateway. When the store changes the running
daemon *suggests* a restart (via an admin-only button) but applies nothing until
you do. See [Operations](operations.md#applying-config-changes).

## Reference assets

The repository's `assets/` directory ships a fully-commented `config.yaml` and
`env.example` starter, plus default Block Kit templates. `setup_bootstrap` seeds
copies into your config directory and initialises an empty config store; you can
also read the templates in-tree as the canonical reference.
