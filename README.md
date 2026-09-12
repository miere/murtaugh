<div align="center">

# 🛠️ Murtaugh

_Your Slack-native AI agent and developer toolkit — chat, automations, jobs, and link previews, in a single Go binary._

[![Go](https://img.shields.io/badge/Go-1.26%2B-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![Platform](https://img.shields.io/badge/platform-macOS%20%7C%20Linux-555)](#quick-start)
[![Slack](https://img.shields.io/badge/Slack-Socket%20Mode-4A154B?logo=slack&logoColor=white)](https://api.slack.com/apis/socket-mode)
[![MCP](https://img.shields.io/badge/MCP-server-blue)](docs/cli-and-mcp.md)

</div>

Murtaugh turns Slack into a first-class developer surface. It connects to your
workspace over Socket Mode and adds:

- 💬 **AI chat** — DM the bot or `@mention` it; a built-in native LLM agent (or
  any ACP-compatible agent) streams its reply back into the thread.
- 🔘 **Workflow rules** — react to Block Kit button clicks and form submissions
  with templated replies, shell commands, or agent delegation.
- 🔗 **Link unfurling** — replace bare URLs with rich Block Kit previews.
- ⏰ **Jobs** — run shell commands or agents on demand or on a cron/interval.
- 🔍 **Gateway Debug Mode** — every interaction is recorded as a structured,
  queryable event so you (or an agent) can ask *"why did that misbehave?"*.
- 🧰 **CLI + MCP server** — every capability is a terminal command and an MCP
  tool exposed to other AI clients.
- 🔑 **Node credentials** — `murtaugh-gateway node token mint|list|revoke`
  issues, lists and withdraws the bearer tokens runtime nodes authenticate
  with.

---

## Quick start

Murtaugh ships **two binaries** and no install script: putting a file where you
want it is not Murtaugh's job. Download `murtaugh-gateway` and `murtaugh-runtime`
from the [latest release](https://github.com/miere/murtaugh/releases/latest), or
build them (requires [Go 1.26+](https://go.dev/dl/)):

```sh
git clone https://github.com/miere/murtaugh.git
cd murtaugh
go build -o murtaugh-gateway ./cmd/murtaugh-gateway
go build -o murtaugh-runtime ./cmd/murtaugh-runtime
```

Each binary then configures itself. Put your Slack tokens in
`~/.config/murtaugh/.env`, pick a storage backend in `config.yaml` (SQLite by
default; Firestore and Postgres are commented out there), and start the gateway:

```sh
murtaugh-gateway                                 # the Slack daemon
murtaugh-gateway cfg launchd --update-existing true   # …or run it under launchd
```

`cfg launchd` writes the LaunchAgent and stops; `launchctl bootstrap
gui/$(id -u) <path>` starts it when you are ready. Everything after that — the
agent, its model, its credentials — is configured by direct-messaging Murtaugh
in Slack. The first person to DM it becomes its administrator.

👉 Full walkthrough: **[Getting started](docs/getting-started.md)**.

---

## Documentation

| Guide | What it covers |
|---|---|
| 🚀 [Getting started](docs/getting-started.md) | Install, create the Slack app, write the config, first run. |
| ⚙️ [Configuration](docs/configuration.md) | Each binary's configuration root (`config.yaml` + `.env`) and the `cfg` admin CLI for everything else. |
| 🤖 [Agent chat](docs/agents.md) | Native, claude_code and ACP agents on a node, tools, routing, streaming, interrupts, approval. |
| 💬 [Slack](docs/slack.md) | Messaging, asking the user, Block Kit, workflow rules, and link unfurling. |
| ⏰ [Jobs](docs/jobs.md) | Define, run, and schedule shell-command and agent jobs. |
| 🔍 [Gateway Debug Mode](docs/journal.md) | Query the structured event journal to debug and audit Murtaugh. |
| 🧰 [CLI & MCP server](docs/cli-and-mcp.md) | Call any tool from the terminal or expose them over MCP. |
| 🛟 [Operations](docs/operations.md) | Run the daemon, restart it, read its logs, and troubleshoot. |
| 🏗️ [Architecture](ARCHITECTURE.md) | Internal design, package layout, and data flow. |

---

## How it fits together

```
                       ┌──────────────────────────────┐
   Slack workspace ◄──►│   murtaugh-gateway            │
   (Socket Mode)       │   the long-lived daemon       │
                       │                               │
   • slash commands    │   • chat   → a node's agent   │
   • @mentions / DMs   │   • workflow rules            │
   • button clicks     │   • link unfurls              │──► journal (SQLite)
   • shared links      │   • job scheduler             │
                       └──────────────┬───────────────┘
                                      │ nodes dial in; the
                                      │ gateway never dials out
                       ┌──────────────▼───────────────┐
                       │   murtaugh-runtime           │──► LLM provider
                       │   agents, tools, jobs        │    or ACP process
                       └──────────────┬───────────────┘
                                      │
                       murtaugh-runtime <tool> | mcp
                       (CLI one-shot, or MCP over stdio)
```

Every capability is registered as a **tool** with one definition, surfaced three
ways: as a Slack interaction, a CLI command, and an MCP tool. See
[ARCHITECTURE.md](ARCHITECTURE.md) for the details.

---

## Contributing

1. Fork the repository and create a feature branch.
2. Run `go build ./...`, `go vet ./...`, and `go test ./...` before opening a PR.
3. Follow the conventions in [ARCHITECTURE.md](ARCHITECTURE.md).

History is kept linear — rebase your branch, don't merge `main` into it.
</content>
</invoke>
