# Daemon, MCP clients & updates: `cfg launchd`, manual registration, `setup update`

> **CLI flags carry values — booleans included.** `update_existing` below is a
> boolean, but the CLI parser has no bare switches: write
> `--update-existing true` (a bare `--update-existing` fails with `flag
> --update-existing requires a value`). snake_case `Arg` names map to kebab
> flags (`binary_path` → `--binary-path`, `node_listen` → `--node-listen`). Over
> MCP, pass a real JSON boolean. Run `murtaugh-gateway help cfg launchd` (or
> `--help` on any command) for the full reference.

## `cfg launchd` — write this binary's LaunchAgent (macOS)

*Write this binary's macOS LaunchAgent so launchd runs it. Writes the plist
only; loading it is `launchctl bootstrap`.*

| Arg | Required | Meaning |
|---|---|---|
| `binary_path` | no | Absolute path to the binary launchd should run. Defaults to the running binary — put the file where you want it first. |
| `alias` | no | Names this LaunchAgent. Defaults to `default`. |
| `update_existing` | no | Boolean. Replace a plist that already exists. Without it an existing plist is **refused**. |
| `gateway` | no | **Node binary only.** Gateway seed address baked into the plist's arguments, overriding `node.gateway`. |
| `node_listen` | no | **Gateway binary only.** Address to accept node connections on, e.g. `127.0.0.1:8787`. Omit to accept none. |
| `node_advertise` | no | **Gateway binary only.** Address(es) nodes should use to reach this gateway. Omit to derive them from the listener. |

**macOS only** (errors on other platforms, naming the OS it found). Both binaries
reach the same shared implementation, so a gateway and a node are installed the
same way. It writes `~/Library/LaunchAgents/<label>.plist` with:

- **Label** `murtaugh.gateway.<alias>` or `murtaugh.node.<alias>`, decided by
  **which binary you ran** — the role is never a flag. `--alias` defaults to
  `default`, and is what finally lets two nodes share one machine.
- **ProgramArguments** `[<binary>, --config <the config you invoked it with>,
  …role flags]`. There is no second `--config`: the plist runs against the
  configuration this command was given, so a second node's LaunchAgent is
  written by invoking the binary against that node's configuration.
- **RunAtLoad** + **KeepAlive** `true` (starts at login, restarts on crash)
- logs to **`~/Library/Logs/murtaugh/<label>.out.log`** and **`…/<label>.err.log`**
  — e.g. `murtaugh.gateway.default.err.log`.

**It writes the plist and stops.** Loading it is yours:

```
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/murtaugh.gateway.default.plist
```

**An existing plist is refused** unless you pass `--update-existing true`. It is
very likely running a live daemon, and overwriting one on the way past is how
you take Murtaugh off Slack without noticing. Passing the other role's flag
(`--gateway` on the gateway, `--node-listen` on a node) is an error rather than a
plist that fails at launch.

```bash
murtaugh-gateway cfg launchd --node-listen 127.0.0.1:8787
murtaugh-runtime --config ~/.config/murtaugh/node/config.yaml \
  cfg launchd --alias laptop --gateway wss://gw.example:8443
```

## Registering Murtaugh in another AI client — now manual

`setup_mcp_register` is gone. Registering Murtaugh into opencode, auggie or
goose is not Murtaugh's business, and it writes a file in the operator's home
that Murtaugh has no reason to own. Do it by hand, on the **node** — a node is
what carries the MCP server (`murtaugh-runtime mcp`); the gateway has no `mcp`
subcommand at all.

The entry always runs the same command: the runtime binary, the `--config` of
the node root you want it to act on, and the `mcp` subcommand. Use absolute
paths — these clients do not run under your login shell.

**opencode** — `~/.config/opencode/opencode.json`:

```json
{
  "$schema": "https://opencode.ai/config.json",
  "mcp": {
    "murtaugh": {
      "type": "local",
      "command": ["/usr/local/bin/murtaugh-runtime", "--config", "/Users/you/.config/murtaugh/node/config.yaml", "mcp"],
      "enabled": true
    }
  }
}
```

**auggie** — `~/.augment/settings.json`:

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

**goose** — `~/.config/goose/config.yaml`:

```yaml
extensions:
  murtaugh:
    name: murtaugh
    type: stdio
    enabled: true
    cmd: /usr/local/bin/murtaugh-runtime
    args: ["--config", "/Users/you/.config/murtaugh/node/config.yaml", "mcp"]
    timeout: 300
```

Merge the entry into whatever is already in the file; none of these clients
wants its other keys replaced.

Two things this does **not** do:

- It does not give an ACP agent that Murtaugh itself drives access to Murtaugh's
  tools. That already happens, per session, over an internal bridge Murtaugh
  hands the agent in `session/new` — see the `murtaugh-agents` skill. Registering
  the entry above as well gives such an agent a **second**, differently scoped
  copy of the surface, with no approval gate and no turn context. Register it for
  standalone use of the client, not to wire up a Murtaugh agent.
- It no longer records the client in `troubleshoot.providers`, which used to be
  its one side effect. That list is a **manual knob on the gateway** now: an
  empty one — the default — means **every** provider Murtaugh knows how to
  collect diagnostics for, today `goose` and `claude-code`. Missing files are
  skipped at collection time, so the all-known fallback is safe on a machine
  running only some of them, and nothing is lost by leaving it empty. Read it
  with `murtaugh-gateway cfg troubleshoot show`; narrow a single bundle with
  `troubleshoot bundle --include <provider>` (repeatable). There is no
  `cfg troubleshoot set` — to pin the default list, edit an exported snapshot
  and `cfg import` it.

## `setup update` — is there a newer release?

*Report whether a newer Murtaugh release exists and where to read its notes.*

| Arg | Required | Meaning |
|---|---|---|
| `version` | no | Release tag to compare against instead of the latest one (e.g. `v0.0.2`). |
| `release_json_url` | no | Override the release JSON URL; primarily for local fixtures and tests. |

**It downloads and installs nothing.** Murtaugh does not replace its own binary:
where the file lives is the operator's decision, and a package manager makes it
theirs to automate. Fetch the release yourself, put it where the current one is,
and restart the daemon.

```bash
murtaugh-gateway setup update
murtaugh-runtime setup update --version v0.5.0
```
