---
name: murtaugh-setup
description: Install and configure Murtaugh from scratch with the `cfg` admin CLI on each of the two binaries — placing murtaugh-gateway / murtaugh-runtime, writing config.yaml (oauth + database) and the .env secrets by hand, seeding the config database, writing the macOS LaunchAgent, and checking for a newer release.
requires: [setup]
files:
  reference/config-tools.md:       { requires: [setup], summary: "write config.yaml (oauth+database) / .env secrets / seed the config DB via cfg" }
  reference/daemon-and-clients.md: { requires: [setup], summary: "write the LaunchAgent, register Murtaugh in another AI client by hand, check for a release" }
  reference/mcp-server.md:         { requires: [setup], summary: "run a node as an MCP server for another tool" }
---

# Skill: Murtaugh Setup & Install

How to install and configure Murtaugh from scratch using each binary's own
`cfg …` admin CLI. This is **operator-facing**: getting the binaries in place, writing
the two on-disk files, seeding the config **database**, and (on macOS) writing
the daemon's LaunchAgent. For *running and debugging* the daemon afterward, see
the `murtaugh-operations` skill.

**Two binaries, and no install script.** Murtaugh ships `murtaugh-gateway` (the
Slack daemon) and `murtaugh-runtime` (a node that runs agents). There is no
combined `murtaugh` binary and no `curl … | bash` installer: putting a file
where you want it is the operator's job, and each binary then configures itself
with `cfg launchd`, `cfg validate` and `cfg migrate`. The role is implied by
which file you ran, so no command takes a `--role`.

**The `setup_*` tools are gone.** `setup_bootstrap`, `setup_slack`, `setup_env`,
`setup_agents` and `setup_mcp_register` were decommissioned: the first three are
a text editor and two `cfg` commands, agents are `cfg agent create`, and
registering Murtaugh into another AI client is that client's business. Only
`setup_update` survives, and only to compare versions and link the release
notes. If you are following older notes that call one of these, use the `cfg`
equivalent in `reference/config-tools.md` instead.

**Where config lives now.** Each binary has its own configuration **root** — the
gateway's is `~/.config/murtaugh`, a node's is `~/.config/murtaugh/node` — and
each root holds only **two files**:

- **`config.yaml`** — the gateway's has two blocks, `oauth:` (Slack tokens via
  `${VAR}`) and `database:`; a node's has `database:` only, because a node has no
  Slack connection.
- **`.env`** — **all secrets**: Slack tokens (gateway), provider API keys
  (node), a Postgres DSN.

**Everything else lives in the config database** — agents, mcp_servers, jobs,
chat routing, access, runtime defaults, journal, troubleshoot, workflow/unfurl
rules — and is managed with `cfg …` (also exposed over MCP as `cfg_*`
tools). The default store is SQLite beside `config.yaml` (`config.yaml` →
`config.db`), so two roots never share a store.

**Each binary carries only its own half of `cfg`.** The gateway has `access`,
`job`, `election`, `workflow_rule`, `unfurl_rule` and `node split`; the node has
`agent`, `mcp`, `defaults` and `node set|show`. `chat`, `show`, `export`,
`import`, `db migrate`, `validate`, `migrate` and `launchd` are on both, acting
on that binary's own configuration. Asking a binary for the other half's command
is an unknown command, not a permission error.

**Upgrading migrates the directory.** `cfg migrate` brings a configuration root
up to the schema this version expects; both daemons run the same pass at
startup. (`cfg db migrate` is a different command — it moves the store's
*content* between SQLite, Postgres and Firestore.)

Every `cfg` mutation **re-validates the whole config** and rolls back an invalid
change. The bundled agent skills are served in-binary (not written to disk), so
there's no on-disk skill copy to keep in sync — see `reference/config-tools.md`.

## Install order (the workflow)

**On the gateway host:**

1. **Place `murtaugh-gateway`** wherever you want it (a release download, or
   `go build -o murtaugh-gateway ./cmd/murtaugh-gateway`).
2. **Run any command once** (`murtaugh-gateway cfg validate` will do) — the root
   seeds itself: a commented `config.yaml`, a template `.env`, the templates,
   and an empty config store. It then reports the missing Slack tokens, which is
   the expected first-run output.
3. **Put the Slack tokens in `~/.config/murtaugh/.env`.** `config.yaml`'s
   `oauth:` block already references them as `${VAR}`.
   → `reference/config-tools.md`
4. **`murtaugh-gateway cfg validate`** — it names the missing field if one is.
5. **`murtaugh-gateway cfg launchd`** *(macOS)* — writes the LaunchAgent; you
   load it with the `launchctl bootstrap` line it prints.
   → `reference/daemon-and-clients.md`
6. **`murtaugh-gateway node token mint --node <id> --user <U…> --token-file …`**
   — one credential per node. Printed once.

There is no admin to set: an unclaimed gateway adopts the first person who
direct-messages it, and says so. Set it explicitly instead with
`murtaugh-gateway cfg access set --admin-user @you` if you would rather not race
for it.

**On each node host:**

7. **Place `murtaugh-runtime`**, then put the minted token at
   `~/.config/murtaugh/node/node-token` (mode `0600`).
8. **`murtaugh-runtime cfg node set --gateway wss://host:port`** — where it
   dials. A node dials in; the gateway never dials out.
9. **`murtaugh-runtime cfg agent create …`** — the agent this node serves
   (native, ACP, or claude_code), plus `cfg mcp set` for any external MCP
   servers it attaches. → `reference/config-tools.md`
10. **`murtaugh-runtime cfg launchd --alias <name>`** *(macOS)*, then load it.

Later: **`setup update`** reports whether a newer release exists and links its
notes. It downloads nothing.

## Read the right file (don't load everything)

| When you're… | Read |
|---|---|
| Writing config.yaml (oauth+database) / .env secrets / seeding the DB with `cfg` | `reference/config-tools.md` |
| Writing the LaunchAgent, registering Murtaugh in another AI client, or checking for a release | `reference/daemon-and-clients.md` |
| Running a node as an MCP server for another tool | `reference/mcp-server.md` |
| Wanting a copy-paste install sequence | `examples/install-sequence.sh` |

## Global guidelines (defaults — follow unless the user says otherwise)

- **A binary starts on sensible defaults and refuses only what has none.** A
  gateway needs `oauth.app_token` and `oauth.bot_token`; a node needs a
  credential file and a gateway seed address. Each failure names the field, so
  read the error rather than guessing at the configuration around it.
- **`config.yaml` and `.env` hold secrets** — write them `0600`. Slack tokens
  live in the gateway's `.env`; provider API keys live in the **node's** `.env`,
  because that is where agents run. `config.yaml` and the store reference them by
  `${VAR}`. Agents reference their key by variable name via `api_key_env`. Don't
  commit them or echo tokens into logs.
- **Restart to apply.** The runtime still loads config **once** at startup. After
  any `cfg` change (or a file edit), restart the daemon for it to take effect.
- **`cfg launchd` is macOS-only** and only WRITES the plist; on other platforms
  run the binary under your own supervisor.
- Tools run as `murtaugh-gateway cfg <group> <verb> …` /
  `murtaugh-runtime cfg <group> <verb> …` on the CLI, and as `cfg_*` over MCP.
  `cfg` works **before** a valid config exists (it creates the store).
- **CLI flags always carry a value — booleans included.** Write `--enabled true`,
  `--update-existing true`; a bare `--enabled` is rejected. Arrays repeat the
  flag (`--tools files --tools terminal`). snake_case arg names map to kebab
  flags (`binary_path` → `--binary-path`, `update_existing` →
  `--update-existing`).
- **When in doubt, ask the binary.** `murtaugh-gateway help` /
  `murtaugh-runtime help` lists every command that binary carries;
  `… help cfg <group>` (or `--help` on any) prints that command's full flag
  reference — required/optional, types, defaults, examples.
