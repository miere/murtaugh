# Murtaugh

Murtaugh is a Slack gateway for AI agents. People talk to it in Slack; the agents run on nodes
such as [Riggs](https://github.com/miere/riggs), which connect to it over
[RAX](https://github.com/miere/rax-protocol).

## Installing

Releases carry a `murtaugh-gateway` binary for Apple silicon (`aarch64-apple-darwin`), Intel Macs
(`x86_64-apple-darwin`) and Linux (`x86_64-unknown-linux-gnu`). The repository is private, so
download with `gh`:

```sh
target=aarch64-apple-darwin
tmp=$(mktemp -d)
gh release download --repo miere/murtaugh --pattern "*-$target.tar.gz*" --dir "$tmp"
(cd "$tmp" \
  && shasum -a 256 -c murtaugh-gateway-*-$target.tar.gz.sha256 \
  && tar -xzf murtaugh-gateway-*-$target.tar.gz)
install -m 0755 "$tmp"/murtaugh-gateway-*-$target/murtaugh-gateway ~/.local/bin/
rm -rf "$tmp"
```

The empty directory matters: the globs above match every version they find, so a tarball left over
from an earlier install would make `tar` and `install` pick the wrong one.

`~/.local/bin` is on the PATH of the LaunchAgent that `murtaugh-gateway launchd` writes. To build
from source instead: `cargo install --locked --git ssh://git@github.com/miere/murtaugh murtaugh-gateway`.

The macOS release binaries are signed with a self-signed certificate, so the folder and
Accessibility approvals macOS asks for survive upgrades. A source build is ad-hoc signed, and
macOS treats every one as a new program that has to be approved again.

## Creating the Slack app

```sh
murtaugh-gateway slack-manifest --name Murtaugh > manifest.json
```

At <https://api.slack.com/apps>, choose **Create New App → From a manifest** and paste it. It turns
on Socket Mode and asks for exactly the scopes and events the gateway uses. Two things a manifest
cannot do: install the app to your workspace (**Install App**, which gives you the `xoxb-` bot
token) and create the app-level token (**Basic Information → App-Level Tokens**, with the
`connections:write` scope, which gives you the `xapp-` token).

## Running the gateway

The gateway reads one small TOML file per alias, by default
`~/.config/murtaugh/default/murtaugh.toml` (`--config PATH` points elsewhere). Relative paths in it
are resolved against its folder, so each alias keeps its own database, `.env` and logs apart. It
holds only the Slack credentials, where the configuration store lives, and where nodes and clients
dial:

```toml
[slack]
app_token = "${SLACK_APP_TOKEN}"   # xapp-…, for Socket Mode
bot_token = "${SLACK_BOT_TOKEN}"   # xoxb-…

[database]
backend = "sqlite"                 # or "firestore"
# [database.sqlite]
# path = "murtaugh.db"             # the default, beside this file
# [database.firestore]             # every field optional; Google's default credentials are used
# project_id = "my-project"
# database_id = "(default)"
# collection = "murtaugh"
# credentials_file = "~/sa.json"

[rax]                              # formerly [nodes], which still works
listen = "127.0.0.1:7443"          # put a TLS terminator in front; nodes and clients dial wss://…/rax/v1/link

[log]
level = "info"                     # trace, debug, info, warn or error
format = "text"                    # or "json"
turn_timings = false               # true logs, per turn, how long the node took to accept and answer
```

`${VAR}` is read from the environment first, then from `env_file` (by default a `.env` beside the
file). Unknown keys are an error, and `validate` names every problem at once.

Only one gateway serves a Slack app at a time. With `sqlite` that holds across one machine; with
`firestore` it holds across every gateway sharing the collection, and a standby takes over within
30 seconds of the leader dying. A standby refuses nodes with 503, so give each node every
gateway's address and it finds the one serving.

```sh
murtaugh-gateway validate
murtaugh-gateway run
```

On macOS, `murtaugh-gateway launchd` manages a LaunchAgent that starts the gateway at login and
restarts it if it stops:

```sh
murtaugh-gateway launchd install     # write the LaunchAgent
murtaugh-gateway launchd start       # hand it to launchd
murtaugh-gateway launchd status      # is it loaded, is it running, what did it exit with
murtaugh-gateway launchd restart     # shut the gateway down cleanly, then start it again
murtaugh-gateway launchd stop        # take it off launchd
murtaugh-gateway launchd uninstall   # stop it and delete the LaunchAgent
```

`status` is the one to reach for when launchd is being unhelpful — it digs the pid, the last exit
code and the log paths out of `launchctl print`, and tells "never installed" apart from "installed
but not loaded":

```
murtaugh.default is running (pid 4812).
  LaunchAgent: /Users/you/Library/LaunchAgents/murtaugh.default.plist
  last exit:   0
  out log:     /Users/you/Library/Logs/murtaugh/murtaugh.default.out.log
  err log:     /Users/you/Library/Logs/murtaugh/murtaugh.default.err.log
```

`--alias NAME` picks which gateway you mean: it names both the job (`murtaugh.<alias>`) and the
configuration at `~/.config/murtaugh/<alias>/murtaugh.toml`. It is global, so it goes with `run`
and `validate` too, and it defaults to `default`.

`install` never replaces an existing plist unless you pass `--update-existing`, and it is the only
one of the six that reads a configuration — the others act on the job `--alias` names, and refuse
`--config` rather than quietly ignoring it.

`stop` and a plain `restart` unload the job, which asks the gateway to shut down cleanly: launchd
sends SIGTERM and waits out `ExitTimeOut` (20 seconds unless you set it in the plist) before
killing it. `murtaugh-gateway launchd restart --force` skips the wait and kills the process.

Logs go to `~/Library/Logs/murtaugh/`. `murtaugh-gateway version --check` tells you whether a newer
release exists; the repository is private, so set `GH_TOKEN` (for example
`GH_TOKEN=$(gh auth token)`).

## Who may use it

The gateway decides three things: who may use it (allowed users), who may connect nodes (node
admins, who hold a grant), and which of their machines may connect (node tokens). Each rests on
the one before it. Minting a token grants its owner, and granting someone allows them. Revoking a
grant revokes every token that person holds, for good, and disallowing someone takes their grant
too. Revoking one node leaves the grant alone, and revoking a grant leaves the person allowed.

The admin does all of this from the app's Home tab. A node admin's Home tab shows their tool
approval mode and their own nodes, which they can mint, disable and revoke. A new token goes to
its owner by DM. Anyone allowed also sees their client tokens (see below). Everyone else sees only
the footer. The same changes work from the CLI, which writes to the store directly and reaches a
running gateway within seconds:

```sh
murtaugh-gateway admin set U0123ADMIN                 # the only onboarding step
murtaugh-gateway node mint --owner U0456ALICE --name laptop --token-file alice-laptop.token
                                                      # also grants Alice, and allows her
murtaugh-gateway grant approve U0456ALICE             # Alice may run nodes, and use the gateway
murtaugh-gateway user allow U0789BOB                  # Bob may use the gateway
murtaugh-gateway grant revoke U0456ALICE              # her tokens are revoked for good
murtaugh-gateway node revoke <selector>               # one node is disconnected
murtaugh-gateway user token mint --owner U0789BOB --name editor --scope rax --out bob-editor.token
                                                      # a client token; also allows Bob
murtaugh-gateway user token revoke <selector>         # one client is disconnected
```

A client token, `mrtg_user_…`, lets a person's own client use the gateway's nodes without running
one, such as an editor bridged by `murtaugh-client`. Anyone allowed may mint one for themselves
from the Home tab, and the admin may mint one for anyone. It needs no grant, and it stops working
as soon as its owner is no longer allowed. Each token carries one or more scopes, the gateway's
entry points it may open; each entry point checks only for its own. Today there is one, `rax`, the
RAX API below (`--scope`, repeatable, on the CLI; checkboxes on the Home tab).

A node can dial as soon as its token is minted: the gateway checks the store for a token it has
not seen yet. The gateway never shows a token twice. Someone who isn't allowed gets a 🤐 reaction
and nothing else. When the admin hands the gateway over, the old admin keeps their machines as a
node admin.

Which nodes an allowed person may use is up to each node's owner, through the `murtaugh_access`
metadata their node sends: `{"policy": "always_allow"}` shares it with everyone allowed on the
gateway, and `{"policy": "allow_list", "people": [...]}` with the people listed. A node that says
nothing serves its owner alone.

A conversation is a Slack thread. It goes to the person's own node with the fewest live sessions,
or, if none of theirs is connected, to the least busy node whose owner lets them in. If that node
goes away, the next message says so and continues on another node, catching it up from the thread.

### The RAX API

A client token also opens the RAX API, on the same listener as nodes: the client dials as a
gateway (`rax.v1.gateway`) and Murtaugh plays the node toward it, relaying each session to a node of
the fleet. That is how `murtaugh-client acp` puts an editor's agent sessions on your machines.

- A session goes to the same node a Slack thread would, among those that take tool groups.
- The tools a client lends a session reach the node suffixed with the client's link id, such as
  `openknowledge_x7k2`, and only that session may call them. Files the client links reach the node
  as `bridge://<link id>/<uri>`, and only a node holding one of the client's sessions may read them.
- Tool calls are ruled by the node owner's tool mode and whitelist first. When those leave a call
  to a person, that person is the node's owner, since the tool runs on their machine: a client of
  their own asks them in the editor, and anyone else's client waits while the owner gets an
  approval card in their Slack DM. Sign-ins still go to the node's owner in Slack.
- Client sessions live in memory and are never pinned. A client that stays away past the link's
  retention takes its sessions with it, and each person's Home tab lists their live ones.

| Crate | What it is |
|---|---|
| `murtaugh-gateway` | The `murtaugh-gateway` binary. |
| `murtaugh-client` | The `murtaugh-client` binary: a person's own client for the RAX API, such as an editor's ACP bridge. |
| `murtaugh-common` | What both binaries share: credentials, logging that redacts them, the version check, the launchd job. |
| `murtaugh-store` | Where the gateway keeps its configuration: SQLite on one machine, Firestore for a cluster. |
| `murtaugh-slack` | A Slack client: Socket Mode and the Web API methods the gateway calls. |
| `slack-sim` | A fake Slack that validates and records every call, for testing without a workspace. |

## murtaugh-client

`murtaugh-client acp` lets an editor that speaks [ACP](https://agentclientprotocol.com) run its
agent sessions on your Murtaugh fleet. The editor starts it as an agent over stdio; it dials
Murtaugh's RAX API with a client token and relays each session to a node.

Mint a client token from the Home tab (*New client token*) and save it with Murtaugh's address:

```sh
murtaugh-client login --gateway wss://murtaugh.example.com < editor.token
```

That writes `~/.config/murtaugh/client/default.toml` and a `default.token` beside it, readable by
you alone; `--profile <alias>` keeps several apart. Then point the editor's agent entry at:

```sh
murtaugh-client acp --name openknowledge
```

- `--name` is the namespace the editor's tools are lent under: `[a-z0-9_]`, at most 27 characters,
  `acp` by default. `--gateway` and `--token-file` override the profile.
- The MCP servers the editor names for a session are connected here, on your machine, and lent to
  the session as one group; so are the editor's file reads and writes when it offers them. Files
  the editor links are read through the editor, unsaved changes included.
- A tool call your node's tool rules leave to a person is asked in the editor.
- Logs go to `~/Library/Logs/murtaugh/client/<profile>.log`, never stdout, which carries ACP.

## Releasing

Push a tag such as `v0.1.0`. The release workflow builds both binaries for every target, stamps the version from the
tag, and publishes the archives with their SHA-256 sums. Both workflows need a `RAX_READ_TOKEN`
secret that can read `miere/rax-rs`, since Cargo fetches the RAX crates from that private
repository.

## Testing

```sh
cargo test --workspace
FIRESTORE_EMULATOR_HOST=localhost:8080 cargo test -p murtaugh-store   # the Firestore half
```
