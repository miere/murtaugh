# Seeding & config: the two files, and `cfg`

Each binary has its own configuration **root**: the gateway's is
`~/.config/murtaugh`, a node's is `~/.config/murtaugh/node` (override either
with the global `--config <root>/config.yaml`). Two files live in a root —
`config.yaml` and `.env` — and everything else lives in the **config database**
beside them, managed with `cfg …`.

There is no `setup_bootstrap`, `setup_slack`, `setup_env` or `setup_agents` any
more. The first two files you edit by hand; the rest is `cfg`.

> **CLI flag spelling.** `cfg` flags are written in their CLI kebab form below;
> over MCP the same tool takes the snake_case schema field, and the tool name is
> the registry name with dots turned into underscores (`cfg.agent.create` →
> `cfg_agent_create`). Every flag needs a value (there are no bare switches —
> booleans included: `--enabled true`), and array flags repeat (`--tools files
> --tools terminal`). Run `murtaugh-gateway help cfg <group>` /
> `murtaugh-runtime help cfg <group>` (or `--help` on any command) for the full
> per-command reference.

## Seeding a root

Nothing to install: **run any command against the root and it seeds itself.**

```bash
murtaugh-gateway cfg validate
murtaugh-runtime --config ~/.config/murtaugh/node/config.yaml cfg validate
```

A gateway's very first run seeds the root and *then* reports that
`oauth.app_token` and `oauth.bot_token` are missing — that error is the expected
first-run output, not a failure to seed. What appears:

- `config.yaml` — **created once, then preserved**: your edits are never
  overwritten. A fresh gateway `config.yaml` carries `oauth:` (Slack tokens via
  `${VAR}`) and `database:`; a fresh node's carries `database:` only, because a
  node has no Slack connection and must never hold the workspace's tokens.
- `.env` — a commented template, `0600`. **You fill this in by hand.**
- The **config database** — created empty. Source of truth for everything except
  OAuth and the database pointer. SQLite by default, named after the config file
  (`config.yaml` → `config.db`), so the gateway root and the node root never
  share a store.
- `templates/`, `AGENTS.md`, `SOUL.md`, `.agents/skills/` (the home for your
  **bespoke** skills) plus a `.claude/skills` symlink to it. The bundled
  `murtaugh-*` skills are served in-binary and are **not** written here; an
  agent's `export_skills_to_fs` is what mirrors chosen ones into a workdir (see
  the `murtaugh-agents` skill).

`cfg migrate` brings an existing root up to the schema this version
expects; both daemons run the same pass at startup, so you rarely call it.

## `.env` — the secrets, by hand

`.env` holds **all secrets**, and which secrets depends on the role.

**Gateway root** (`~/.config/murtaugh/.env`):

```dotenv
SLACK_APP_TOKEN=xapp-…
SLACK_BOT_TOKEN=xoxb-…
# SLACK_USER_TOKEN=xoxp-…        # optional: `slack send_msg --as admin`
# MURTAUGH_DB_DSN=postgres://…   # only if you move the store to Postgres
```

**Node root** (`~/.config/murtaugh/node/.env`): the **LLM provider keys**, because
agents run on the node. A native agent references its key by variable name
(`api_key_env`); the value lives only here, never in the database.

```dotenv
GEMINI_API_KEY=…
ANTHROPIC_API_KEY=…
```

There are deliberately no `SLACK_*` variables in a node's `.env`. A node reaches
Slack only through the gateway it dials.

The node's own credential is not in `.env` either. It is a **file** —
`node-token` beside `config.yaml`, mode `0600` — so the sandboxed model the node
runs cannot read it out of the environment. Mint it on the gateway:

```bash
murtaugh-gateway node token mint --node mac-mini --user U012ABCDEF \
  --label "office mac" --token-file /tmp/node-token   # printed once; only its hash is stored
```

A value exported in the real environment overrides the one in `.env`.

## `cfg …` — everything else in the store

Each mutation re-validates the whole config and rolls back an invalid change;
**restart the daemon** for a change to take effect (config loads once). Full
surface in the `murtaugh-operations`, `murtaugh-agents`, `murtaugh-jobs`, and
`murtaugh-slack` skills — the install-flow essentials, split by which binary
carries them:

**Gateway (`murtaugh-gateway cfg …`)**

- **Access** (who can interact). An unclaimed gateway adopts the first person to
  DM it; set it explicitly if you would rather not:
  ```bash
  murtaugh-gateway cfg access set --admin-user @you --allowed-users U0AAA --allowed-users U0BBB
  ```
- **Main node** (who serves headless work — jobs, workflow triggers, unfurls):
  ```bash
  murtaugh-gateway cfg access set --main-node mac-mini
  ```
- **Jobs** (`cfg job set …` — see `murtaugh-jobs`), **workflow / unfurl rules**
  (`cfg workflow_rule set --from-file …` / `cfg unfurl_rule set --from-file …` —
  see `murtaugh-slack`), and **election** timings (`cfg election set`).

**Node (`murtaugh-runtime cfg …`)**

- **Where it dials** (repeatable; replaces the list):
  ```bash
  murtaugh-runtime cfg node set --gateway wss://gateway.example.com:8443
  murtaugh-runtime cfg node show
  ```
- **Agents** — native, `acp`, or `claude_code`:
  ```bash
  murtaugh-runtime cfg agent create --name default --type native \
    --provider gemini --model gemini-2.5-pro --api-key-env GEMINI_API_KEY \
    --tools files --tools terminal --tools skills --tools ask --tools present_plan
  ```
- **MCP servers** (attached by name from an agent's `--mcp-servers`):
  ```bash
  murtaugh-runtime cfg mcp set --name vaultre --command vaultre-mcp --arg --stdio \
    --env VAULTRE_TOKEN=${VAULTRE_TOKEN}
  ```
- **Which agent this node serves.** A node serves **one** agent per process. It
  takes `chat.defaults.agent` when there is one, the only agent when there is
  exactly one, and otherwise refuses until you pass `-agent <name>`:
  ```bash
  murtaugh-runtime cfg chat set --enabled true --default-agent default
  ```

**Both**

Store-wide helpers work the same on either binary, against that binary's own
configuration: `cfg show` (whole config), `cfg validate` (as this binary's role —
there is no `--role`), `cfg export [--file]` / `cfg import --file` (back up /
restore), and `cfg db migrate --to <sqlite|postgres|firestore>` to move the
store between backends. A node's backend does not have to match the gateway's.
