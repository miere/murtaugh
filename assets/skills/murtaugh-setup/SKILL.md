---
name: murtaugh-setup
description: Install and configure Murtaugh from scratch with the idempotent setup_* tools and the murtaugh cfg admin CLI — binary on PATH, config dir, config.yaml (oauth + database), .env secrets, the config database (agents/chat/jobs/…), the macOS daemon, and self-update.
requires: [setup]
files:
  reference/config-tools.md:       { requires: [setup], summary: "seed config / write config.yaml (oauth+database) / .env secrets / seed the config DB via murtaugh cfg" }
  reference/daemon-and-clients.md: { requires: [setup], summary: "install the daemon, register an MCP client, self-update" }
  reference/mcp-server.md:         { requires: [setup], summary: "run Murtaugh as an MCP server for another tool" }
---

# Skill: Murtaugh Setup & Install

How to install and configure Murtaugh from scratch using the `setup_*` tools and
the `murtaugh cfg …` admin CLI. This is **operator-facing**: getting the binary
in place, writing the two on-disk files, seeding the config **database**, and
(on macOS) installing the daemon. For *running and debugging* the daemon
afterward, see the `murtaugh-operations` skill.

**Where config lives now.** Only **two files** sit on disk, both under
`~/.config/murtaugh`:

- **`config.yaml`** — slimmed to two blocks: `oauth:` (Slack tokens via `${VAR}`)
  and `database:` (`backend: sqlite` [default, `sqlite.path`] or `postgres`
  [`postgres.dsn: ${VAR}`]).
- **`.env`** — **all secrets**: Slack tokens, provider API keys, a Postgres DSN.

**Everything else lives in the config database** — agents, mcp_servers, jobs,
chat routing, access, runtime defaults, journal, troubleshoot, workflow/unfurl
rules — and is managed with `murtaugh cfg …` (also exposed over MCP as `cfg.*`
tools). The old sibling YAMLs (`agents.yaml`, `jobs.yaml`, `journal.yaml`,
`workflow-rules.yaml`, `unfurl-rules.yaml`, `troubleshoot.yaml`) are **gone** as
the source of truth. The default store is SQLite at
`~/.config/murtaugh/config.db` (beside `config.yaml`; override with `database.sqlite.path`).

**Upgrading auto-migrates.** On the first run of a new binary against an old YAML
tree, Murtaugh migrates the whole tree into SQLite, slims `config.yaml` down to
`oauth:`+`database:`, and archives the old siblings to
`~/.config/murtaugh/migrated-<timestamp>/`. Move to Postgres later with
`murtaugh cfg db migrate --to postgres --dsn-env MURTAUGH_DB_DSN`.

Every `setup_*` tool is idempotent, so re-running is safe. The file writers
(`setup_slack`, `setup_env`, `setup_launchd`, `setup_mcp-register`) back up any
file they replace (`<file>.bak.<timestamp>`); `setup_agents` writes the config
database. `setup_bootstrap` seeds the workspace and is safe to re-run.
Every `cfg` mutation **re-validates the whole config** and rolls back an invalid
change. The bundled agent skills are served in-binary (not written to disk), so
there's no on-disk skill copy to keep in sync — see `reference/config-tools.md`.

## Install order (the workflow)

1. **Get the binary** on `PATH` (download a release, or `go build`).
2. **`setup_bootstrap`** — seed the config dir and create the store (must run
   first, so later steps write real files/rows). → `reference/config-tools.md`
3. **`setup_slack`** — write `config.yaml` `oauth:` (tokens via `${VAR}`) and the
   token values into `.env`.
4. **`setup_env`** — upsert provider keys into `.env` (a native agent can't
   authenticate without its key here; run before/with `setup_agents`).
5. **`setup_agents`** — create an agent **in the database** (native, ACP, or
   claude_code), or leave chat disabled.
6. **`murtaugh cfg …`** — everything else in the DB: `cfg access set` (admin +
   allowed users), `cfg chat set` (turn chat on, pick agents), `cfg job set`,
   `cfg mcp set`, `cfg workflow-rule set` / `cfg unfurl-rule set`.
   → `reference/config-tools.md`
7. **`setup_launchd`** *(macOS, optional)* — install the daemon as a LaunchAgent.
   → `reference/daemon-and-clients.md`
8. **`setup_mcp-register`** *(optional)* — register Murtaugh in an MCP client.

Later: **`setup_update`** self-updates the binary from a GitHub release (and the
next start auto-migrates any old YAML tree, as above).

## Read the right file (don't load everything)

| When you're… | Read |
|---|---|
| Seeding config / writing config.yaml (oauth+database) / .env secrets / seeding the DB with `cfg` | `reference/config-tools.md` |
| Installing the daemon, registering an MCP client, or self-updating | `reference/daemon-and-clients.md` |
| Running Murtaugh as an MCP server for another tool | `reference/mcp-server.md` |
| Wanting a copy-paste install sequence | `examples/install-sequence.sh` |

## Global guidelines (defaults — follow unless the user says otherwise)

- **`setup_bootstrap` first.** It creates the workspace (`~/.config/murtaugh`),
  the config store, and templates/skills; the other tools write files/rows that
  must already exist.
- **`config.yaml` and `.env` hold secrets** — they're written `0600`. Slack
  tokens and provider API keys live in `.env`; `config.yaml` `oauth:` and a
  Postgres DSN reference them by `${VAR}`. Agents reference their key by variable
  name via `api_key_env`. Don't commit them or echo tokens into logs.
- **Restart to apply.** The runtime still loads config **once** at startup. After
  any `cfg` change (or a file edit), restart the gateway for it to take effect.
- **`setup_launchd` is macOS-only**; on other platforms run the gateway under
  your own supervisor (`murtaugh slack gateway`).
- Tools run as `murtaugh setup <tool> …` / `murtaugh cfg <group> <verb> …` on the
  CLI, and as `setup_<tool>` / `cfg.*` over MCP. Setup tools work **before** a
  valid config exists (they create it).
- **CLI flags always carry a value — booleans included.** Write `--load true`,
  `--force true`, `--enabled true`; a bare `--load` is rejected. Arrays repeat the
  flag (`--tools files --tools terminal`). snake_case arg names map to kebab flags
  (`binary_path` → `--binary-path`, `app_token` → `--app-token`).
- **When in doubt, ask the binary.** `murtaugh help` lists every command;
  `murtaugh help setup <tool>` / `murtaugh help cfg <group>` (or `--help` on any)
  prints that command's full flag reference — required/optional, types, defaults,
  examples.
