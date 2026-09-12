# Murtaugh documentation

Murtaugh turns Slack into a developer surface: AI chat, reactive workflow rules,
link previews, scheduled jobs, an event journal, a CLI, and an MCP server. It
ships as **two** Go binaries — `murtaugh-gateway` (the Slack daemon) and
`murtaugh-runtime` (a node that runs the agents) — with no install script: you
place each file, and it configures itself. These pages explain how to do that,
configure it, and operate it.

> New here? Start with **[Getting started](getting-started.md)**.

## Guides

| Guide | What it covers |
|---|---|
| [Getting started](getting-started.md) | Install Murtaugh, create the Slack app, write the config, and run the gateway. |
| [Configuration](configuration.md) | Each binary's configuration root (`config.yaml`, `.env`), the database config store, and the `cfg` admin CLI. |
| [Agent chat](agents.md) | Native and ACP agents, the tools they can call, routing, streaming, interrupts, and approval gates. |
| [Slack](slack.md) | Posting and reading messages, asking the user, Block Kit, workflow rules, and link unfurling. |
| [Jobs](jobs.md) | Defining, running, and scheduling shell-command and agent jobs. |
| [Gateway Debug Mode](journal.md) | Querying the structured event journal to debug interactions and audit chat sessions. |
| [CLI & MCP server](cli-and-mcp.md) | Running any tool from the terminal, and exposing the toolset to other AI clients over MCP. |
| [Operations](operations.md) | Running the daemon, restarting it, reading its logs, and troubleshooting. |

## Design specs

Proposals under discussion. Unlike the guides above, these describe work that is
planned or in progress rather than how Murtaugh behaves today.

| Spec | What it proposes |
|---|---|
| [Claude Code authentication management](specs/01-claude-code-authentication-management.md) | Move the Claude Code credential out of the sandboxed agent's reach, and make credential failures visible to the admin instead of to the user. |

## Concepts in one minute

- **The gateway** is `murtaugh-gateway` with no command — the long-lived daemon.
  It owns every Slack event, the workflow/unfurl handlers and the job scheduler,
  and it renders every chat turn. It runs no agents.
- **A node** is `murtaugh-runtime`. It dials the gateway (never the other way
  round) and runs the agents, their tools and their MCP servers.
- **Tools** are the unit of capability. Each tool is defined once and surfaced
  three ways — as a Slack interaction, a CLI command, and an MCP tool
  (`murtaugh-runtime mcp`). Over MCP the name is the registry name with dots
  turned into underscores (`cfg_agent_create`).
- **Config lives in a database**, managed with `cfg …`. Each binary has its own
  configuration root holding only two files — a slimmed `config.yaml` and a
  secret `.env` — and carries only its own half of the `cfg` surface. Secrets
  are *only* in `.env`; everything else references them as `${VAR}` so config
  can be shared safely.
- **Agents** answer chat and can be delegated work by jobs, workflow rules, and
  unfurls. A *native* agent runs the LLM loop in the node's process; an *ACP* or
  *claude_code* agent is an external process the node drives.
- **The journal** records what happened as structured events, so you debug by
  querying rather than grepping logs.

## A note on the config schema

Murtaugh moved its configuration **out of hand-edited YAML files and into a
database** managed with `cfg …`. A legacy YAML-tree config directory is
**auto-migrated** on the first run of a new binary — imported into a SQLite
config store, `config.yaml` rewritten to `oauth` + `database`, and the old
sibling YAMLs moved (never deleted) into `~/.config/murtaugh/migrated-<timestamp>/`.
The migration is idempotent. These docs describe the current model.
