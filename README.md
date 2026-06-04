# Murtaugh Dev Toolkit

Go service for connecting Murtaugh to Slack via Socket Mode.

## Goals

- Make it easy to build refined Slack experiences with BlockKit.
- Handle custom Slack slash commands.
- Provide integration points for ACP agents, locally or remotely.

## Configuration

Create `~/.config/murtaugh/slack.yaml`:

~~~yaml
slack:
  app_token: xapp-your-socket-mode-app-token
  bot_token: xoxb-your-bot-token
  debug: false

commands:
  - name: /murtaugh
    description: Entrypoint for Murtaugh commands
~~~

The Slack app must have Socket Mode enabled and must subscribe to slash command
payloads for every command listed in the configuration.

## Run

~~~sh
go run ./cmd/murtaugh-slack
~~~

Use `--config /path/to/slack.yaml` to load a non-default config file.
