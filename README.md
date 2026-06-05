# Murtaugh Dev Toolkit

Go service for connecting Murtaugh to Slack via Socket Mode.

## Goals

- Make it easy to build refined Slack experiences with BlockKit.
- Handle custom Slack slash commands.
- Provide integration points for ACP agents, locally or remotely.

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

~~~sh
go run ./cmd/murtaugh-slack
~~~

Use `--config /path/to/slack.yaml` to load a non-default config file.
