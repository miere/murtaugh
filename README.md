# Murtaugh Dev Toolkit

Go toolkit that exposes the same capabilities through three frontends:

- **Slack** — Socket Mode daemon (`murtaugh slack`, the default).
- **CLI** — direct human-facing commands (`murtaugh <tool> [...]`).
- **MCP** — JSON-RPC stdio server for AI clients (`murtaugh mcp`).

Every Murtaugh tool is registered exactly once and is automatically available
through both the CLI and MCP frontends.

## Goals

- Make it easy to build refined Slack experiences with BlockKit.
- Handle custom Slack slash commands.
- Provide integration points for ACP agents, locally or remotely.
- Expose every capability as a tool that AI clients (over MCP) and humans (over
  the CLI) can call.

## Configuration

Create `~/.config/murtaugh/slack.yaml`:

~~~yaml
oauth:
  app_token: xapp-your-socket-mode-app-token
  bot_token: xoxb-your-bot-token

configuration:
  admin_user: your-slack-handle
  debug: false

chat:
  default_agent: default
  channel_agents:
    C12345: coding
  dm_agent: default

commands:
  - name: /murtaugh
    description: Entrypoint for Murtaugh commands

workflow-rules:
  code-review-approval:
    request_event: interactive
    match:
      channel: { name: nc-code-reviews }
      actions:
        - block_id: github_pull_request
          action_id: approve_only
    trigger:
      - reply-to-slack:
          template: code-review/approved.json
      - run:
          cmd: /path/to/background-command
          args: [param1, param2]

Create `~/.config/murtaugh/agents.yaml`:

~~~yaml
acp:
  enabled: true
  request_timeout: 10m
  session_idle_timeout: 30m
  max_sessions: 100
  stream_append_interval: 750ms
  stream_min_chunk_chars: 96

agents:
  default:
    command: /path/to/default-agent
    args: [--stdio]
  coding:
    command: /path/to/coding-agent
    args: [--stdio]
~~~

The Slack app must have Socket Mode enabled and must subscribe to slash command
payloads for every command listed in the configuration. For ACP chat, subscribe
to the Events API event types `app_mention` and `message.im`, and grant scopes
for slash commands, app mentions, IM history, and chat writes. For custom link
unfurling, subscribe to the `link_shared` bot event and grant the `links:read`
and `chat:write` scopes.

`oauth.app_token` and `oauth.bot_token` hold the Slack Socket Mode and bot
tokens. `configuration.admin_user` may be a Slack handle with or without `@` or
a Slack user ID. When Socket Mode reports that it is connected, Murtaugh opens a
DM with that user and sends the startup ping message from `assets/ping/01-ping.json`.

## ACP chat

When `acp.enabled` is true in `agents.yaml`, Murtaugh can chat through a local
ACP-compatible agent process. It supports three entrypoints:

- DM the bot.
- Mention the bot in a channel.
- Run `/murtaugh chat <prompt>`.

Murtaugh keeps one ACP session per Slack conversation:

- DMs use one session per DM channel.
- Channel mentions use one session per Slack thread. If the mention is not in a
  thread, the mention message timestamp becomes the root thread key.

Responses use Slack's native streaming-message APIs, not simulated `chat.update`
loops:

- `chat.startStream` starts the streamed response.
- `chat.appendStream` appends ACP text deltas.
- `chat.stopStream` finalizes the response.

Murtaugh flushes the first ACP text chunk immediately, then uses
`acp.stream_append_interval` and `acp.stream_min_chunk_chars` from `agents.yaml`
to coalesce later small chunks so it does not call `chat.appendStream` for every
tiny token.

## Workflow rules

Workflow rules let Murtaugh respond to Slack interactive form/button submissions.
Interactive events are acknowledged immediately through Socket Mode, then the
matching workflow runs asynchronously. `reply-to-slack` triggers render JSON and
POST it to the Slack `response_url` from the interaction payload.

- `match` is a partial match against Slack's interaction JSON payload.
- Array match entries match when any payload array item contains the configured
  partial object.
- Triggers run in the order they are configured.
- Template paths are resolved relative to the configuration file directory.
- Commands are executed directly with explicit args, not through a shell. The
  Slack interaction JSON is passed to commands on stdin.
- `response_url` is treated as sensitive webhook data and is never logged.

If `workflow-rules` is omitted or empty, Murtaugh installs a built-in ping/pong
rule. If you already have workflow rules configured, add an explicit
`startup-ping-pong` rule that points at `ping/02-pong.json`. Murtaugh first
looks for that template relative to your config directory and then falls back to
the embedded reference asset. Pressing the startup message's `ping` button posts
the rendered pong response through Slack's `response_url`, using the original
startup message timestamp as `thread_ts` so the response appears in the message
thread.

## Custom link unfurling

Unfurl rules teach Murtaugh to replace bare URLs with Block Kit previews. When a
message containing a matching URL is posted in a channel the bot can see, the
`link_shared` event arrives and Murtaugh posts a preview via `chat.unfurl`.

~~~yaml
unfurl-rules:
  github-pr:
    match:
      domain: github.com
      url_pattern: '^https://github\.com/(?P<owner>[^/]+)/(?P<repo>[^/]+)/pull/(?P<number>\d+)'
    unfurl:
      template: unfurl/github-pr.json
  github-pr-eng-only:
    match:
      channels: [C0ENG1, C0ENG2]
      domain: github.com
      url_pattern: '/pull/(?P<number>\d+)'
    unfurl:
      run:
        cmd: /path/to/unfurl-script
        args: ["--number", "{{ .Captures.number }}"]
        timeout: 8s
~~~

- `match` requires at least one of `domain`, `url_prefix`, or `url_pattern`.
  `domain` matches the URL host exactly or as a subdomain suffix; `url_prefix`
  is a plain prefix; `url_pattern` is an RE2 regex whose named groups become
  `.Captures`.
- `match.channels` is an optional allowlist of Slack channel IDs (`C…`/`D…`/
  `G…`); when set the rule only fires in those channels.
- `unfurl` requires exactly one of `template` or `run`. `template` renders a
  Block Kit attachment JSON (resolved relative to the config directory, then the
  embedded assets); `run` executes a command that prints attachment JSON on
  stdout, receiving the link context as JSON on stdin.
- Rules are evaluated in sorted-key order; the first match per URL wins.
- Each `match.domain` must be registered in the Slack app's **App Unfurl
  Domains** list (max 5) or no `link_shared` event is delivered. Composer-mode
  link previews (before a message is sent) are not supported.

## Reference assets

The `assets/` directory contains a fake `slack.yaml` plus the default ping and
pong JSON payloads and an example `unfurl/github-pr.json` template. They are safe
reference files; copy or adapt them to your runtime config/template location if
you want to override the built-in defaults.

## Run

Build the single binary:

~~~sh
go build -o murtaugh ./cmd/murtaugh
~~~

## macOS installer

This repository includes a macOS installer at `install/macos/install.sh`.

It will:

- download the latest GitHub release for the current macOS architecture (or a
  specific version with `--version`),
- install `murtaugh` into a user-writable bin directory,
- write `~/.config/murtaugh/slack.yaml` and `agents.yaml` on first install,
- **preserve existing config files by default** when re-run,
- optionally create `~/Library/LaunchAgents/dev.murtaugh.plist`,
- optionally configure Murtaugh as an MCP server in supported clients,
- restart the LaunchAgent automatically when a running binary is updated.

Supported Slack Chat agent choices are:

- skip chat-agent setup,
- `opencode acp`,
- `goose acp`,
- `auggie --acp --allow-indexing`,
- custom ACP-compatible command.

The installer is configure-only for third-party agents: it checks whether the
selected agent binary already exists and does not install OpenCode, Goose, or
Auggie for you.

For MCP setup, the installer can update supported client config files and will
create a backup before modifying any existing file, printing the backup path as
part of the install output. Goose MCP setup remains manual-only in v1.

### Update behavior

Re-running the installer is safe:

- If the installed version matches the latest release, the installer exits
  cleanly with no changes.
- If an older version is installed, it updates the binary and restarts the
  LaunchAgent if present.
- Config files are **preserved** by default. Use `--reconfigure` to force a
  full rewrite.
- Use `--skip-config` to update the binary only.

Run it with:

~~~sh
bash install/macos/install.sh
~~~

Common flags:

~~~sh
bash install/macos/install.sh --yes                  # non-interactive
bash install/macos/install.sh --version v1.2.3       # install a specific version
bash install/macos/install.sh --force                # reinstall even if current
bash install/macos/install.sh --skip-config          # update binary only
bash install/macos/install.sh --reconfigure          # rewrite all config
bash install/macos/install.sh --dry-run              # preview changes
~~~

### Slack Socket Mode daemon (default)

~~~sh
murtaugh slack         # explicit
murtaugh               # implicit — no command runs the daemon
~~~

### MCP stdio server

~~~sh
murtaugh mcp           # speaks MCP JSON-RPC on stdin/stdout
~~~

Stdout is reserved for the protocol — never parse it as plain text. Diagnostics
go to stderr.

### CLI tools

Every registered tool is callable directly. Flat tools take one token; nested
tools (e.g. `jobs.run`) take a namespace + subcommand:

~~~sh
murtaugh ping                                          # → pong

murtaugh jobs run --name nightly-deploy                # run a job

murtaugh jobs define \
  --name nightly-deploy \
  --command /usr/local/bin/deploy \
  --args --env --args production \
  --workdir /srv/deploy \
  --timeout 15m                                        # write the job into jobs.yaml
~~~

Schema-typed arguments are coerced for you: `--count 5` becomes an integer,
`--verbose true` becomes a boolean, and array-typed flags (such as `--args`)
accumulate when repeated.

Use `--config /path/to/slack.yaml` to load a non-default config file. The flag
applies to every mode.
