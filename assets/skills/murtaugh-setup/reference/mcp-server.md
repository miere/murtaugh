# Running a node as an MCP server

`murtaugh-runtime mcp` runs a node as a **stdio MCP server** (JSON-RPC over
stdin/stdout). It is a **runtime-binary subcommand only**: the gateway has no
`mcp` command, because the tools worth handing a local AI client — agent
profiles, MCP servers, jobs — are the node admin's.

It serves exactly the tools that node registers, each with its own input schema:
`ping`, `version`, `help`, `jobs_run`, `setup_update`, the whole `cfg_*` node
surface (`cfg_agent_*`, `cfg_mcp_*`, `cfg_node_*`, `cfg_chat_*`, `cfg_defaults_show`,
`cfg_show`, `cfg_validate`, `cfg_export`, `cfg_import`, `cfg_db_migrate`,
`cfg_launchd`, `cfg_migrate`), and the three that reach a person — `ask`,
`present_plan`, `auth_request`. There are **no `slack_*` tools**: only the
gateway talks to Slack.

Tool names over MCP are the registry name with every dot replaced by an
underscore (`cfg.agent.create` → `cfg_agent_create`), because some providers
reject a `.` in a function name.

## How it's used

You rarely run it by hand; an MCP client launches it. Wiring the launch command
into the client's config is now a **manual** step — `setup_mcp_register` is gone.
See `reference/daemon-and-clients.md` for concrete opencode / auggie / goose
entries. Once registered, the client can:

- **list** the tools (names + schemas), and
- **call** a tool by name with arguments matching its schema; the result comes
  back as JSON text (errors are returned as an error result, not a crash).

## Notes

- Pass the node root explicitly. `--config` is a **global** flag and must come
  before the subcommand: `murtaugh-runtime --config <root>/config.yaml mcp`.
  Without it the node uses `~/.config/murtaugh/node/config.yaml`.
- It's the **same tools** the node's CLI exposes, so anything documented in the
  other skills works identically over MCP; pass the schema properties as the
  tool's arguments.
- **No config required to start.** The MCP server starts before a full config
  exists; individual tools surface a clear error if they need configuration that
  isn't there yet.
- `ask`, `present_plan` and `auth_request` are on the surface but only act where
  there is a person to reach. Off a node's own connection to a gateway they
  answer on the terminal.
- Run it directly only to inspect the surface. stdout is reserved for protocol
  traffic:

```bash
murtaugh-runtime mcp        # speaks JSON-RPC on stdio; Ctrl-C to exit
```
