#!/usr/bin/env bash
# Example first-run install sequence for Murtaugh.
# There is no install script: you place the two binaries, and each one then
# configures itself. Assumes murtaugh-gateway and murtaugh-runtime are already
# somewhere on PATH.
set -euo pipefail

NODE_CONFIG="$HOME/.config/murtaugh/node/config.yaml"

# ---------------------------------------------------------------- gateway host

# 1. Seed the gateway root. Any command does it; this one also tells you which
#    required field is still missing. Expect it to report the Slack tokens on a
#    fresh root — that is the fail-closed message, not a broken install.
murtaugh-gateway cfg validate || true

# 2. Slack credentials go in the .env beside config.yaml, which already
#    references them as ${VAR}. Edit it by hand — `setup env` is gone.
#      $HOME/.config/murtaugh/.env
#        SLACK_APP_TOKEN=xapp-REPLACE
#        SLACK_BOT_TOKEN=xoxb-REPLACE
${EDITOR:-vi} "$HOME/.config/murtaugh/.env"
murtaugh-gateway cfg validate

# 3. There is no admin to set: the first person to DM Murtaugh becomes its
#    administrator. Set it explicitly only if you would rather not race for it.
# murtaugh-gateway cfg access set --admin-user "@you"

# 4. (macOS) Write the gateway's LaunchAgent, then load it yourself. `cfg
#    launchd` writes the plist and stops; -node-listen is what lets nodes attach.
#    The launchctl line is left commented ON PURPOSE: bootstrapping a plist over
#    a gateway that is already running takes Murtaugh off Slack. Check first
#    (`pgrep -fl murtaugh-gateway`), then run it yourself.
murtaugh-gateway cfg launchd --node-listen 127.0.0.1:8787
# launchctl bootstrap "gui/$(id -u)" "$HOME/Library/LaunchAgents/murtaugh.gateway.default.plist"

# 5. Mint one credential per node. Printed once — only its hash is stored.
murtaugh-gateway node token mint --node mac-mini --user U012ABCDEF \
  --label "office mac" --token-file /tmp/mac-mini-node-token

# ------------------------------------------------------------------- node host

# 6. Seed the node root, then install the credential the gateway minted.
murtaugh-runtime --config "$NODE_CONFIG" cfg validate
install -m 0600 /tmp/mac-mini-node-token "$HOME/.config/murtaugh/node/node-token"

# 7. Tell the node where to dial. A node dials in; the gateway never dials out.
murtaugh-runtime --config "$NODE_CONFIG" cfg node set --gateway ws://127.0.0.1:8787

# 8. Provider API keys go in the NODE's .env — that is where agents run.
#      $HOME/.config/murtaugh/node/.env
#        GEMINI_API_KEY=AIza-REPLACE
${EDITOR:-vi} "$HOME/.config/murtaugh/node/.env"

# 9. The agent this node serves. Native is the default kind; --tools repeats.
murtaugh-runtime --config "$NODE_CONFIG" cfg agent create \
  --name default --type native \
  --provider gemini --model gemini-2.5-pro --api-key-env GEMINI_API_KEY \
  --tools files --tools terminal --tools skills --tools ask --tools present_plan

#    Alternative: an ACP agent. --type acp needs --command; --arg repeats.
#    Its credentials are the node admin's own responsibility.
# murtaugh-runtime --config "$NODE_CONFIG" cfg agent create \
#   --name coder --type acp --command /usr/local/bin/claude-code-acp --arg --stdio

# 10. (macOS) Write and load the node's LaunchAgent. --alias is what lets a
#     second node share this machine.
murtaugh-runtime --config "$NODE_CONFIG" cfg launchd --alias default
# launchctl bootstrap "gui/$(id -u)" "$HOME/Library/LaunchAgents/murtaugh.node.default.plist"

# Later: check for a newer release. It downloads nothing — fetch the file
# yourself, put it where the current one is, and restart the daemon.
# murtaugh-gateway setup update
