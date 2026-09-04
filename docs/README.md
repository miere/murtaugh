# Murtaugh documentation

Murtaugh is a single Go binary that turns Slack into a developer surface: AI
chat, reactive workflow rules, link previews, scheduled jobs, an event journal,
a CLI, and an MCP server. These pages explain how to install it, configure it,
and operate it.

> New here? Start with **[Getting started](getting-started.md)**.

## Guides

| Guide | What it covers |
|---|---|
| [Getting started](getting-started.md) | Install Murtaugh, create the Slack app, write the config, and run the gateway. |
| [Configuration](configuration.md) | The two on-disk files (`config.yaml`, `.env`), the database config store, and the `murtaugh cfg` admin CLI. |
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

- **The gateway** (`murtaugh slack gateway`) is the long-lived daemon. It owns
  every Slack event, the chat agents, the workflow/unfurl handlers, and the job
  scheduler. Almost everything runs inside it.
- **Tools** are the unit of capability. Each tool is defined once and surfaced
  three ways — as a Slack interaction, a CLI command (`murtaugh <tool>`), and an
  MCP tool (`murtaugh mcp`).
- **Config lives in a database**, managed with `murtaugh cfg …`. Only two files
  stay on disk in `~/.config/murtaugh/`: a slimmed `config.yaml` (`oauth` +
  `database`) and a secret `.env`. Secrets are *only* in `.env`; everything else
  references them as `${VAR}` so config can be shared safely.
- **Agents** answer chat and can be delegated work by jobs, workflow rules, and
  unfurls. A *native* agent runs the LLM loop in-process; an *ACP* agent is an
  external process Murtaugh drives.
- **The journal** records what happened as structured events, so you debug by
  querying rather than grepping logs.

## A note on the config schema

Murtaugh moved its configuration **out of hand-edited YAML files and into a
database** managed with `murtaugh cfg …`. A legacy YAML-tree config directory is
**auto-migrated** on the first run of a new binary — imported into a SQLite
config store, `config.yaml` rewritten to `oauth` + `database`, and the old
sibling YAMLs moved (never deleted) into `~/.config/murtaugh/migrated-<timestamp>/`.
The migration is idempotent. These docs describe the current model.
