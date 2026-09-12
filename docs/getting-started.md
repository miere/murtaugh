# Getting started

This guide takes you from nothing to a running Murtaugh that answers in Slack:
**place the binaries**, **create the Slack app**, **configure the gateway**,
**configure a node**.

---

## 0. Two binaries, and no install script

Murtaugh ships **two** binaries and no installer:

- **`murtaugh-gateway`** — the Slack side. Run it with no command and it is the
  Socket Mode daemon; run it with a command and it acts on its own
  configuration. It never runs an agent.
- **`murtaugh-runtime`** — a **node**. It dials the gateway and runs the agents.
  Agent profiles, provider keys and MCP servers live here. (Jobs are still
  defined and scheduled on the gateway; the node is what executes an agent one.)

There is no combined `murtaugh` binary. Putting a file where you want it is not
Murtaugh's job, so there is nothing to `curl | bash`: you place the file, and
each binary then configures itself with `cfg launchd`, `cfg validate` and
`cfg migrate`. The role is implied by which file you ran — no command takes a
`--role`.

The two keep **separate configuration roots**, so nothing they own can collide:
the gateway's is `~/.config/murtaugh`, a node's is `~/.config/murtaugh/node`
(point either elsewhere with the global `--config /path/to/config.yaml`).

---

## 1. Place the binaries

Download `murtaugh-gateway` and `murtaugh-runtime` from the
[latest release](https://github.com/miere/murtaugh/releases/latest) and put them
anywhere on your `$PATH`, or build them (requires **Go 1.26+**):

```sh
git clone https://github.com/miere/murtaugh.git
cd murtaugh
go build -o murtaugh-gateway ./cmd/murtaugh-gateway
go build -o murtaugh-runtime ./cmd/murtaugh-runtime
```

Both may sit on one machine — that is the ordinary single-host install, just
with two processes instead of one.

---

## 2. Create the Slack app

Murtaugh connects over **Socket Mode**. In your Slack app settings, enable:

1. **Socket Mode** — generates the `xapp-…` app-level token.
2. **Slash commands** — register `/murtaugh` (and optionally a standalone
   `/stop`). Murtaugh recognises the verbs `chat`, `stop`, `troubleshoot`,
   `restart`, and `help`.
3. **OAuth scopes** (Bot Token):
   - `commands` — slash commands
   - `app_mentions:read`, `im:history` — chat
   - `chat:write`, `chat:write.public` — sending messages
   - `files:write` — uploading the `/murtaugh troubleshoot` bundle and agent attachments
   - `files:read` — reading canvas documents, **and routing canvas turns to the
     right agent** (a canvas conversation cannot name its own channel; the canvas
     file is what knows which channel it lives in). Without this, every canvas
     mention is answered by the default agent regardless of channel rules.
   - `canvases:read`, `canvases:write` — the `slack.canvas` tool (reading and
     editing canvas documents)
   - `links:read` — link unfurling (only if you use it)
4. **Event subscriptions**:
   - `app_mention`, `message.im` — AI chat
   - `link_shared` — URL unfurling (only if you use it)
   - `app_home_opened` — the App Home control panel
5. **App Unfurl Domains** — register each domain you want to unfurl (max 5).
6. **App Home** — enable the **Home Tab**. This surfaces a control panel: the
   Murtaugh banner and the running version. For the `admin_user` it also offers
   a **Restart** button (a graceful, confirmed restart on demand), a **Test
   communication** button (a round-trip self-test answered by the binary
   itself), and an **Upgrade to version …** button when a newer release is
   available — which announces the release and links its notes; Murtaugh never
   replaces its own binary. No extra scope required.

Copy the **app-level token** (`xapp-…`) and the **bot token** (`xoxb-…`); you
need both next.

---

## 3. Configure the gateway

Two files live in `~/.config/murtaugh/`: the secret `.env` and a slimmed
`config.yaml`. Everything else — chat routing, access, jobs, rules — lives in a
config store and is edited with `murtaugh-gateway cfg …`. Secrets stay in
`.env`; `config.yaml` and the store reference them as `${VAR}`.

**Seed the root by running any command against it.** The first run writes
`config.yaml`, a template `.env`, the templates and an empty store — and then
tells you the Slack tokens are missing, which is the expected first-run output:

```sh
murtaugh-gateway cfg validate
# oauth.app_token is required: a gateway talks to Slack, so it needs the
# app-level token. Set SLACK_APP_TOKEN in the .env beside config.yaml
```

### `.env` — secrets

```sh
# ~/.config/murtaugh/.env   (mode 0600 — keep it secret)
SLACK_APP_TOKEN=xapp-your-socket-mode-app-token
SLACK_BOT_TOKEN=xoxb-your-bot-token
```

Provider API keys do **not** go here — they belong to the node that runs the
agents (step 4).

### `config.yaml` — oauth + database

```yaml
# ~/.config/murtaugh/config.yaml
oauth:
  app_token: ${SLACK_APP_TOKEN}
  bot_token: ${SLACK_BOT_TOKEN}

database:
  backend: sqlite                 # default; the config store is a single file
  # sqlite:
  #   path: /custom/config.db     # default: config.db beside this file
```

`config.yaml` no longer holds `access:` or `chat:` — those are records in the
config store now. Run `murtaugh-gateway cfg validate` again once the tokens are
in place; a gateway refuses to start only on a required value that has no
default, and names the field when it does.

### Access

There is no admin to set up front: an unclaimed gateway adopts the first person
who direct-messages it, once, and announces the fact. Set it explicitly if you
would rather not race for it:

```sh
murtaugh-gateway cfg access set --admin-user your-slack-handle --debug false
#   allowed_users defaults to empty = admin-only (fail-closed)
```

`--admin-user` may be a handle (with or without `@`) or a user ID. On startup
Murtaugh opens a DM with that user and sends a **"Murtaugh has started"** card.
To check the link at any other time, open Murtaugh's **App Home** tab and press
**Test communication**.

### Start the gateway, and mint a node credential

```sh
murtaugh-gateway -node-listen 127.0.0.1:8787
```

`murtaugh-gateway` with **no command** is the daemon. `-node-listen` is what
lets nodes attach; without it the gateway accepts none. On macOS write a
LaunchAgent instead so it starts on login and restarts on crash — see
**[Operations](operations.md)**.

Each node needs its own bearer credential, minted here and displayed **once**:

```sh
murtaugh-gateway node token mint --node my-laptop --user U012ABCDEF \
  --label "my laptop" --token-file /tmp/node-token
```

`--user` is the Slack user ID of the node's owner: a node never asserts who it
is, it presents the token and the gateway resolves the rest.

---

## 4. Configure a node

The node is where agents, provider keys and MCP servers live. Its root is
`~/.config/murtaugh/node` — a directory of its own, so the two roles never share
a `.env`, a store or a schema migration.

```sh
murtaugh-runtime cfg validate          # seeds ~/.config/murtaugh/node
install -m 0600 /tmp/node-token ~/.config/murtaugh/node/node-token
murtaugh-runtime cfg node set --gateway ws://127.0.0.1:8787
```

A node **dials in**; the gateway never dials out. Those two — the credential
file and a seed address — are the only values a node has no default for, and it
names whichever is missing when you start it.

### `.env` — provider keys

```sh
# ~/.config/murtaugh/node/.env   (mode 0600 — keep it secret)
# Only the provider keys your native agents use:
GEMINI_API_KEY=your-key-here
```

There are deliberately no `SLACK_*` variables here. A node reaches Slack only
through the gateway it dials.

### The chat agent

Create a minimal native agent (Murtaugh runs the LLM loop itself, no external
process):

```sh
murtaugh-runtime cfg agent create --name default --type native \
  --tools files --tools terminal --tools skills \
  --tools ask --tools present_plan \
  --provider gemini --model gemini-2.5-pro --api-key-env GEMINI_API_KEY

murtaugh-runtime cfg chat set --enabled true --default-agent default
```

`--api-key-env` names the variable in the node's `.env` (never the key itself).
A node serves **one** agent per process: `chat.defaults.agent` when there is one,
the only agent when there is exactly one, and otherwise it refuses until you pass
`-agent <name>`. See **[Agent chat](agents.md)** for ACP agents, tools, and
tuning.

Because every `cfg` change re-validates the whole store, `chat set
--default-agent default` is rejected until that agent exists — create the agent
first, then enable chat.

### Start the node

```sh
murtaugh-runtime
```

Like the gateway, `murtaugh-runtime` with no command is the daemon, and
`cfg launchd --alias <name>` writes its LaunchAgent.

---

## 5. Verify it works

**Test the connection.** Open your DM with the bot and press the **Ping** button
on the startup card. A pong reply within a second or two confirms Socket Mode,
OAuth, and the workflow engine are all wired up.

**Ask the bot to help you.** DM it (or `@mention` it in a channel) and describe
what you want:

> "Hey Murtaugh, create a workflow rule that replies with a thank-you whenever
> someone clicks Approve on a code-review card in `#eng-reviews`."

The bot can draft workflow-rule YAML, suggest Block Kit templates, and guide you
through any extra Slack configuration the new rule needs.

---

## Next steps

- [Configuration](configuration.md) — `config.yaml`, `.env`, and the full `cfg` surface.
- [Agent chat](agents.md) — tune which agent answers, its tools, and approvals.
- [Slack](slack.md) — workflow rules and link unfurling in depth.
- [Jobs](jobs.md) — schedule recurring work.
