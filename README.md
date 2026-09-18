# Murtaugh

Murtaugh is a Slack gateway for AI agents. People talk to it in Slack; the agents run on nodes
such as [Riggs](https://github.com/miere/riggs), which connect to it over
[RAX](https://github.com/miere/rax-protocol).

## Running the gateway

The gateway reads one small TOML file per profile, by default
`~/.config/murtaugh/default/murtaugh.toml` (`--config PATH` points elsewhere). Relative paths in it
are resolved against its folder, so each profile keeps its own database, `.env` and logs apart. It
holds only the Slack credentials, where the configuration store lives, and where nodes dial:

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

[nodes]
listen = "127.0.0.1:7443"          # put a TLS terminator in front; nodes dial wss://…/rax/v1/link

[log]
level = "info"                     # trace, debug, info, warn or error
format = "text"                    # or "json"
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

## Who may use it

Everything below changes the store directly and reaches a running gateway within seconds.

```sh
murtaugh-gateway admin set U0123ADMIN                 # the only onboarding step
murtaugh-gateway grant approve U0456ALICE             # Alice may run her own nodes
murtaugh-gateway node mint --owner U0456ALICE --name laptop --token-file alice-laptop.token
murtaugh-gateway user allow U0789BOB                  # Bob may use the admin's nodes
murtaugh-gateway grant revoke U0456ALICE              # her nodes are disconnected
murtaugh-gateway node revoke <selector>               # one node is disconnected
```

A grant is per person, and every node that person runs shares it. Hand node tokens over in
person: the gateway never shows one twice. Someone with no node of their own and no
pre-authorisation gets a 🤐 reaction and nothing else.

A conversation is a Slack thread. It goes to the person's own node with the fewest live
sessions, or, if none of theirs is connected, to one of the admin's when they are pre-authorised.
If that node goes away, the next message says so and continues on another node, catching it up
from the thread.

| Crate | What it is |
|---|---|
| `murtaugh-gateway` | The `murtaugh-gateway` binary. |
| `murtaugh-store` | Where the gateway keeps its configuration: SQLite on one machine, Firestore for a cluster. |
| `murtaugh-slack` | A Slack client: Socket Mode and the Web API methods the gateway calls. |
| `slack-sim` | A fake Slack that validates and records every call, for testing without a workspace. |

## Testing

```sh
cargo test --workspace
FIRESTORE_EMULATOR_HOST=localhost:8080 cargo test -p murtaugh-store   # the Firestore half
```
