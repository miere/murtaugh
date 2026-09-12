# CLI & MCP server

Every Murtaugh capability is registered once as a **tool** and surfaced three
ways: as a Slack interaction, a CLI command, and an MCP tool. This page covers
the latter two — calling tools from your terminal, and exposing them to other AI
clients.

---

## Two binaries, each with its own surface

There is no combined `murtaugh` binary. Every registered tool is callable from
your terminal, but **which binary carries it depends on whose job it is**:

```sh
murtaugh-gateway ping                     # → pong (both carry this one)

murtaugh-gateway slack send_msg --to '#general' --body 'hello'
murtaugh-gateway journal query --stream gateway --since 1h --level error
murtaugh-gateway cfg access set --admin-user @you

murtaugh-runtime jobs run --name nightly-deploy
murtaugh-runtime cfg agent list           # administer this node's config store
```

Asking a binary for the other's command is an **unknown command**, not a
permission error — the role is implied by which file you ran, so no command
takes a `--role`. The command reference is shared: sections are written
`murtaugh <command>` because most read the same on either, and a section says so
when a command lives on only one.

### The `cfg` namespace

`cfg …` is the admin surface for the **config store** — the database that holds
agents, MCP servers, jobs, chat routing, access control, runtime defaults,
journal settings, and workflow/unfurl rules (everything except the `oauth:` +
`database:` blocks in `config.yaml`). **Each binary carries only its own half**,
acting on its own configuration root:

```sh
# gateway: access, jobs, election, workflow/unfurl rules, node split
murtaugh-gateway cfg access set --admin-user your-handle
murtaugh-gateway cfg job set --name nightly-backup --command /usr/local/bin/backup.sh --schedule "0 2 * * *"

# node: agent profiles, MCP servers, defaults, where it dials
murtaugh-runtime cfg agent create --name default --type native --provider gemini ...
murtaugh-runtime cfg node set --gateway wss://gateway.example.com:8443

# both, on that binary's own configuration
murtaugh-gateway cfg chat set --enabled true --default-agent default
murtaugh-gateway cfg show          # dump the whole config as JSON
murtaugh-gateway cfg validate      # validate as this binary's role — there is no --role
murtaugh-gateway cfg migrate       # bring the config DIRECTORY to this version's schema
murtaugh-gateway cfg launchd       # write this binary's LaunchAgent (macOS)
murtaugh-gateway cfg db migrate --to postgres --dsn-env MURTAUGH_DB_DSN
```

`cfg launchd`, `cfg validate` and `cfg migrate` are the three installer-shaped
commands that survive on both binaries; there is no install script. Every
mutation re-validates the whole store and rolls back an invalid change. Like the
rest, a daemon reads the store **once at startup** — restart to apply. See
[Configuration → the `cfg` surface](configuration.md#the-cfg-surface)
for the full command list.

### The `node` namespace

`murtaugh-gateway node token …` administers the bearer credentials **runtime
nodes** present to the gateway. A node never asserts its own identity: it sends
the token, and the gateway resolves which node and user that token belongs to.

```sh
murtaugh-gateway node token mint --node mac-mini --user U012ABCDEF --label "office mac"
murtaugh-gateway node token list --node mac-mini
murtaugh-gateway node token revoke --selector 1a2b3c4d5e6f7a8b
```

The token is printed **once** — only its SHA-256 is stored — and carries the
prefix `mrtg_node_` so a leaked one is greppable and scrubbed from troubleshoot
bundles. Two credentials may be live for one node at a time, which is what makes
a rotation need no downtime. Put the file on the node as
`~/.config/murtaugh/node/node-token`, mode `0600`.

### Discovering commands

The binary documents itself — this is the fastest reference:

```sh
murtaugh-gateway help              # list every command this binary carries
murtaugh-gateway help <command>    # full help for one command
murtaugh-runtime <command> --help  # same, e.g. `murtaugh-runtime jobs run --help`
murtaugh-gateway slack             # a namespace on its own lists its subcommands
```

Every flag, default, and example is documented there.

### Flag conventions

- **Every flag takes a value — booleans included.** Write `--load true`, not a
  bare `--load`.
- **snake_case arg names map to kebab flags** — `binary_path` → `--binary-path`,
  `app_token` → `--app-token`.
- **Schema-typed args are coerced automatically** — `--count 5` → integer,
  `--verbose true` → boolean, repeated `--args` flags → an array.

```sh
murtaugh-gateway jobs define \
  --name nightly-deploy \
  --command /usr/local/bin/deploy \
  --args --env --args production \   # repeated --args build the array
  --workdir /srv/deploy \
  --timeout 15m
```

### The daemons are the binaries themselves

Neither daemon is a subcommand. Run a binary with **no command** and it is the
daemon:

```sh
murtaugh-gateway                                          # the Slack daemon
murtaugh-gateway --config /path/to/config.yaml -node-listen 127.0.0.1:8787
murtaugh-runtime                                          # a node, dialling its gateway
```

`--config PATH` is the one global flag and applies to both modes. The daemon
mode additionally takes plain `flag`-package options (`-node-listen`,
`-node-advertise` on the gateway; `-gateway`, `-agent`, `-token-file` on a node)
— those are **not** tool flags, and only exist when no command is given.

See [Operations](operations.md) for running them as daemons.

---

## The MCP server

```sh
murtaugh-runtime mcp        # speaks MCP JSON-RPC over stdin/stdout
```

`mcp` is a **runtime-binary subcommand only** — the gateway has none. It exposes
every tool that node registers to MCP-capable AI clients (Claude Desktop, IDE
extensions, opencode, goose, …) over JSON-RPC on stdio. Stdout is reserved for
the protocol; diagnostics go to stderr.

What a client gets is the **node admin's** surface: `cfg_agent_*`, `cfg_mcp_*`,
`cfg_node_*`, `cfg_chat_*`, `cfg_show`, `cfg_validate`, `cfg_export`,
`cfg_import`, `cfg_db_migrate`, `cfg_launchd`, `cfg_migrate`,
`cfg_defaults_show`, `jobs_run`, `ping`, `version`, `help`, `setup_update`, and
`ask` / `present_plan` / `auth_request`. There are **no `slack_*` tools**: only
the gateway talks to Slack.

**Tool names use underscores, not dots.** The MCP name is the registry name with
every dot replaced by an underscore — `cfg.agent.create` → `cfg_agent_create`,
`jobs.run` → `jobs_run` — because some providers reject a `.` in a function
name. The dotted form stays the registry key, and the CLI spells the same tool
with a space (`murtaugh-runtime jobs run`).

### Registering with a client

**This is manual now.** `setup mcp_register` used to write the entry into
opencode, auggie and goose for you; it was removed with the rest of the
installer, and those files are the node admin's to edit. Point the client at the
runtime binary, the node root you want it to act on, and the `mcp` subcommand
— `--config` is a global flag, so it comes before `mcp`:

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

See [Agent chat → Registering Murtaugh inside opencode / auggie /
goose](agents.md#registering-murtaugh-inside-opencode--auggie--goose--manual) for
each client's own file and shape, and for why you should not point it at a node
root whose agent Murtaugh is already driving.

---

## Bundled agent skills

Murtaugh ships a set of **agent skills** in-binary — focused, progressive-
disclosure docs that teach a connected agent how to drive each surface
(`murtaugh-setup`, `murtaugh-operations`, `murtaugh-agents`, `murtaugh-slack`,
`murtaugh-jobs`, `murtaugh-journal`, `murtaugh-blueprint`). They are served from
the binary (not written to disk), so a native agent with the `skills` tool, or an
MCP client that reads skills, gets the same operational knowledge these docs
describe — straight from the running version.
