# Seeding & config: bootstrap, slack (config.yaml), env, agents, and `murtaugh cfg`

Two files live on disk under the workspace (`~/.config/murtaugh` by default):
`config.yaml` (only `oauth:` + `database:`) and `.env` (all secrets). Everything
else — agents, mcp_servers, jobs, chat, access, defaults, journal, troubleshoot,
workflow/unfurl rules — lives in the **config database** and is managed with
`murtaugh cfg …`. The `setup_*` tools below write the two files and seed the
store; `cfg` does the rest.

> **CLI flag spelling.** Setup-tool `Arg` columns name the schema fields
> (snake_case), which is what you pass over MCP; on the **CLI** each is a
> kebab-case flag carrying a value (`app_token` → `--app-token`). `cfg` flags are
> already written in their CLI kebab form below. Every flag needs a value (there
> are no bare switches — booleans included: `--enabled true`), and array flags
> repeat (`--tools files --tools terminal`). Run `murtaugh help setup <tool>` or
> `murtaugh help cfg <group>` (or `--help` on any command) for the full
> per-command reference. Over MCP, `cfg` verbs are the `cfg.*` tools.

## `setup_bootstrap` — seed the workspace & store

*Seed the Murtaugh config directory with embedded defaults and create the config
store.*

Takes **no arguments**. It runs automatically on every Murtaugh start (and you
can run it by hand). What it touches:

- `config.yaml` and `templates/` — **created once, then preserved**: your tokens
  and edits are never overwritten. A fresh `config.yaml` carries just the two
  blocks — `oauth:` (Slack tokens via `${VAR}`) and `database:` (`backend: sqlite`
  by default; the store is `config.db` beside `config.yaml` unless you set
  `sqlite.path`).
- The **config database** itself — created empty if absent. This is the source of
  truth for everything except OAuth and the DB pointer.
- `.agents/skills/` (the home for your **bespoke** skills) plus a `.claude/skills`
  symlink to it — **created if absent**. The bundled `murtaugh-*` skills are
  served in-binary and are **not** written here; an agent's `export_skills_to_fs`
  is what mirrors chosen ones into a workdir (see the `murtaugh-agents` skill).
  Skills you add yourself are left alone.

> **Upgrading auto-migrates.** On the first run of a new binary against an old
> YAML tree (`agents.yaml`, `jobs.yaml`, `journal.yaml`, `workflow-rules.yaml`,
> `unfurl-rules.yaml`, `troubleshoot.yaml`), Murtaugh migrates the whole tree into
> SQLite, slims `config.yaml` to `oauth:`+`database:`, and archives the old
> siblings to `~/.config/murtaugh/migrated-<timestamp>/`. It's validated and safe
> to re-run; the archived copies are left for you to inspect or delete.

Returns a report of which files were **created**, **updated** (refreshed), and
**preserved**. Run it first on a fresh install; safe to re-run any time.

## `setup_slack` — write config.yaml `oauth:` + `.env`

*Write `config.yaml`'s `oauth:` block (tokens via `${VAR}`) and the token values
into `.env`.*

| Arg | Required | Meaning |
|---|---|---|
| `app_token` | yes | Slack app-level token; must start with `xapp-`. Stored in `.env`; `oauth:` references it by `${VAR}`. |
| `bot_token` | yes | Slack bot token; must start with `xoxb-`. Stored in `.env`; `oauth:` references it by `${VAR}`. |

Validates the token prefixes, writes the `oauth:` block into `config.yaml` at
`0600` (backing up any existing file), and upserts the token values into `.env`.
Re-run to rotate tokens. The **admin user** and **default chat agent** are no
longer written here — set them in the database with `cfg access set` and
`cfg chat set` (below).

```bash
murtaugh setup slack --app-token xapp-… --bot-token xoxb-…
```

## `setup_env` — upsert .env secrets

*Upsert `KEY=VALUE` secrets into `~/.config/murtaugh/.env` (other entries
preserved).*

`.env` holds **all secrets**: Slack tokens (written by `setup_slack`), **LLM
provider keys**, MCP server tokens, and a Postgres DSN. A native agent references
its key by variable name (`api_key_env`); the value lives only here, never in the
database.

| Arg | Required | Meaning |
|---|---|---|
| `set` | yes | A `KEY=VALUE` pair to upsert. **Repeatable** — pass `--set` once per pair. The value is written verbatim. |

Merges into the existing `.env`: keys you pass are added or updated, every other
entry is left untouched. Writes `0600` and backs up any existing file. The result
reports key **names** only — never the secret values.

```bash
# Native agents can't authenticate until their key is in .env:
murtaugh setup env --set GEMINI_API_KEY=AIza… --set ANTHROPIC_API_KEY=sk-ant-…
# Postgres DSN, referenced later by `cfg db migrate --dsn-env MURTAUGH_DB_DSN`:
murtaugh setup env --set MURTAUGH_DB_DSN=postgres://…
```

## `setup_agents` — create an agent in the database

*Create the first agent (native, ACP, or claude_code) in the config store.*

The **type is inferred** when you don't pass `--type`: a `--provider` ⇒ **native**,
a `--command` ⇒ **acp**. Passing neither leaves chat disabled; passing agent flags
that can't determine a type is rejected rather than silently skipped. This tool is
a thin wrapper over `cfg agent create` for the install flow — for later edits and
additional agents, use `cfg agent …` directly (see below and the `murtaugh-agents`
skill).

Shared:

| Arg | Required | Meaning |
|---|---|---|
| `agent_name` | no | Name to register the agent under. Defaults to `default`. |
| `type` | no | `native` (default), `acp`, or `claude_code`. Inferred from the flags when omitted. |

**Native** (`--type native`, or just pass `--provider`):

| Arg | Required | Meaning |
|---|---|---|
| `provider` | yes | Provider family: `gemini`, `anthropic`, or `openai`. |
| `model` | yes | Provider model id (e.g. `gemini-2.5-pro`). |
| `api_key_env` | yes | Name of the `.env` variable holding the API key (write the value with `setup_env`). |
| `base_url` | no | Endpoint override for a compat provider (Z.ai/DeepSeek/Kimi). |
| `tools` | no | Tool allowlist. **Repeatable** — `--tools files --tools terminal …`. |
| `mcp_servers` | no | Names of `mcp` entries to attach. **Repeatable**. |
| `system_prompt_file` | no | Path (relative to the config dir) to the system prompt file. |
| `context_limit` | no | Token budget for compaction (integer). `0` uses a per-family default. |
| `compaction` | no | `truncate` (default) or `summarize`. |
| `cache_retention` | no | Prompt-cache TTL: `5m` (default), `1h`, or `off`. |

**ACP / claude_code** (`--type acp` / `--type claude_code`, or just pass `--command`):

| Arg | Required | Meaning |
|---|---|---|
| `command` | yes | Path to the agent binary. |
| `args` | no | Arguments for the agent command. **Repeatable** — once per argument. |

Writes the agent row to the store (re-validated; an invalid config rolls back). To
enable the chat surface you still need `cfg chat set --enabled true` and a routed
agent (see below); delegation (jobs, workflow rules, unfurls) runs whenever the
agent is defined, regardless of that gate.

```bash
# Native agent (default type): the key value already went into .env via setup_env.
murtaugh setup agents --provider gemini --model gemini-2.5-pro \
  --api-key-env GEMINI_API_KEY \
  --tools files --tools terminal --tools skills --tools ask --tools present_plan

# A claude_code (or ACP) agent. --args is repeatable; values become argv in order:
murtaugh setup agents --agent-name claude --type claude_code \
  --command /usr/local/bin/claude-code-acp --args --stdio --args --verbose
```

## `murtaugh cfg …` — everything else in the store

After the two files and the first agent, the rest of the config is `cfg`. Each
mutation re-validates the whole config and rolls back an invalid change; **restart
the gateway** for a change to take effect (config loads once). Full surface in the
`murtaugh-operations`, `murtaugh-agents`, `murtaugh-jobs`, and `murtaugh-slack`
skills — the install-flow essentials:

- **Access** (who can interact):
  ```bash
  murtaugh cfg access set --admin-user @you --allowed-users U0AAA --allowed-users U0BBB
  ```
- **Chat** (turn the surface on, pick agents):
  ```bash
  murtaugh cfg chat set --enabled true --default-agent default --dm-agent default \
    --reply-on-thread true
  ```
- **MCP servers** (attached by name from an agent's `--mcp-servers`):
  ```bash
  murtaugh cfg mcp set --name vaultre --command vaultre-mcp --arg --stdio \
    --env VAULTRE_TOKEN=${VAULTRE_TOKEN}
  ```
- **Jobs** (`cfg job set …` — see `murtaugh-jobs`), **workflow / unfurl rules**
  (`cfg workflow-rule set --from-file …` / `cfg unfurl-rule set --from-file …` —
  see `murtaugh-slack`).

Store-wide helpers: `cfg show` (whole config), `cfg validate`, `cfg export
[--file]` / `cfg import --file` (back up / restore), and `cfg db migrate --to
<postgres|sqlite> [--dsn-env <VAR>|--sqlite-path <path>]` to move the store
between backends (e.g. `--to postgres --dsn-env MURTAUGH_DB_DSN`).
