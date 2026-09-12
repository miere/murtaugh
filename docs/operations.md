# Operations

Running and debugging the gateway daemon — the long-lived Socket Mode process
that handles every Slack event (slash commands, button clicks, mentions, DMs,
link previews) and runs scheduled jobs. For *installing* the daemon see
[Getting started](getting-started.md); this page is about what it does once it's
running and how to keep it healthy.

---

## Running the gateway

```sh
murtaugh-gateway                                  # the daemon
murtaugh-gateway -node-listen 127.0.0.1:8787      # …and accept node connections
```

**`murtaugh-gateway` with no command IS the daemon.** There is no `slack
gateway` subcommand: give the binary a command and it acts on its own
configuration, give it none and it connects to Slack over Socket Mode and stays
up. At startup it warms up the configured agents, sends the **"Murtaugh has
started"** info card to the admin DM, and starts the job scheduler.

It runs no agents itself — those live on `murtaugh-runtime` nodes, which dial
in. `-node-listen` is what lets them attach; without it the gateway accepts
none.

### As a daemon (macOS)

`murtaugh-gateway cfg launchd` writes
`~/Library/LaunchAgents/murtaugh.gateway.<alias>.plist` so the gateway starts on
login and restarts on crash. `--alias` defaults to `default`, and the label is
decided by which binary you ran — a node's is `murtaugh.node.<alias>`.

**It writes the plist and stops.** Loading it is yours, once you are ready for
the daemon to start:

```sh
murtaugh-gateway cfg launchd --node-listen 127.0.0.1:8787
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/murtaugh.gateway.default.plist
```

An existing plist is **refused** unless you pass `--update-existing true`: it is
very likely running a live daemon, and overwriting one on the way past is how
you take Murtaugh off Slack without noticing.

Under launchd it logs to files named after the label:

- **`~/Library/Logs/murtaugh/murtaugh.gateway.default.out.log`** — stdout
- **`~/Library/Logs/murtaugh/murtaugh.gateway.default.err.log`** — stderr

**Start any debugging in those logs** — startup, agent warmup, event handling,
job runs, and errors all land there.

On other platforms, run `murtaugh-gateway` under your own supervisor (systemd, a
process manager, etc.).

---

## Applying config changes

The gateway **hot-reloads under admin approval**. The leader polls the config
store, and any change it did not make itself is rendered as a YAML diff and sent
to the admin's DM with **Apply Modifications** / **Rollback**.

- **Apply** performs a soft reload: the gateway stops serving, is rebuilt from
  the new configuration, and starts again — all while holding the leader lease,
  so the cluster never sees a gap. Agents own backend process trees decided at
  construction, so a reload restarts them: any conversation or job in flight is
  stopped, and the approval card says so before you click.
- **Rollback** — and a timeout, and an unreachable admin — writes the running
  configuration back over the edit. None of those is approval.

(Every `cfg` change is still validated against the whole store immediately, so a
rejected change never reaches the store in the first place.)

**Restart** remains available — `/murtaugh restart`, or the suggestion button —
and is admin-only. It preserves a "restarting… / back online" notice across the
restart so users aren't left wondering.

### Auto-migration on upgrade

The first run after upgrading from a YAML-tree install **auto-migrates** the old
sibling YAMLs (`agents.yaml`, `jobs.yaml`, `journal.yaml`, `workflow-rules.yaml`,
`unfurl-rules.yaml`, `troubleshoot.yaml`, plus the `access`/`chat` blocks) into a
SQLite config store, rewrites `config.yaml` down to `oauth:` + `database:`, and
**moves** the old files into `~/.config/murtaugh/migrated-<timestamp>/` (never
deletes them). This is automatic and idempotent — a second run is a no-op. From
then on, edit configuration with `cfg …`. See
[Configuration](configuration.md#upgrading-from-the-yaml-tree).

---

## A quiet turn may be waiting, not hung

Some chat turns now wait on a human before continuing:

- a native agent's `terminal` command can be **approval-gated** (see
  [Agent chat → The approval gate](agents.md#the-approval-gate));
- `ask` and `present_plan` **block** on your Approve/Deny or your answer in Slack;
- `auth.request` **blocks** until the owner of the machine the agent runs on
  finishes the sign-in sent to them by DM;
- a held job's first scheduled run blocks on admin confirmation (see
  [Jobs → Trusted vs held jobs](jobs.md#trusted-vs-held-jobs)).

If a turn has gone quiet, check whether there's a card waiting for your click
before assuming it's stuck.

---

## Troubleshooting

### Access is fail-closed

Only the admin plus everyone in the access allowed-users list may interact. With
the list empty, the bot is **admin-only** — so *"the bot ignores me"* is most
often an access-list problem, not a bug. Inspect it with `murtaugh-gateway cfg
access show`. Handles in the access lists are resolved to IDs at startup, and **the
gateway refuses to start if any entry can't be resolved** — check the startup log
for a resolution error.

### "The bot ignores me" checklist

1. Are you the admin, or in the allowed-users list? (`murtaugh-gateway cfg access show`)
2. In a channel, did you `@mention` the bot?
3. Is chat enabled and pointed at a real agent? (`murtaugh-runtime cfg chat show`,
   `murtaugh-runtime cfg agent list`)
4. Did the gateway actually start? Check `murtaugh.gateway.default.err.log` for an auth or
   config-validation failure.

### Query the journal

For *"why did this workflow / unfurl / job misbehave?"*, don't grep logs — query
the structured [event journal](journal.md). It records each interaction with a
correlation id so you can replay one click end to end. The `connection` events on
the `gateway` stream are where to look for *"why did the daemon go silent?"*.

### Ship a diagnostics bundle

```sh
murtaugh-gateway slack send_msg ...   # if Slack itself works
/murtaugh troubleshoot             # from Slack: bundles config.yaml + a config-store dump
```

`/murtaugh troubleshoot` collects `config.yaml` and a dump of the config store
(the same content as `murtaugh-gateway cfg show`) into an uploadable bundle. It
deliberately **never** includes `.env`, so secrets don't leak — and because every
value in the store is a `${VAR}` reference, the dump carries no credentials
either (see [Configuration](configuration.md)).

It can also fold in a downstream provider's own sessions and logs.
`troubleshoot.providers` in the gateway's store decides which by default, and it
is a **manual** knob now — the tool that used to append to it,
`setup mcp_register`, is gone. **An empty list — the default — means every
provider Murtaugh knows how to collect diagnostics for**, today `goose` and
`claude-code`; missing files are skipped at collection time, so the all-known
fallback is safe on a machine running only some of them. Read it with
`murtaugh-gateway cfg troubleshoot show`, and narrow a single bundle with
`troubleshoot bundle --include <provider>` (repeatable). There is no
`cfg troubleshoot set`: to pin a narrower default, edit a `cfg export` snapshot
and `cfg import` it back.

---

## Claude Code credentials

A `claude_code` agent runs on the Claude Code CLI's own OAuth credential — the
one `claude auth login` writes. Murtaugh keeps it alive and can repair it from
Slack, so a lapsed login does not mean SSH-ing to the host.

The credential belongs to the machine the agent runs on, so that machine does
the work. A runtime node (`murtaugh-runtime`) looks after the credentials of its
own `claude_code` agents and sends anything that needs a person to the **node's
owner** — the Slack user its token was minted for. `murtaugh-gateway` runs no
agents, so it holds no Claude Code credential: it watches nothing and never
starts a sign-in on its own machine, only relays the node's.

### Why it needs keeping alive

A sandboxed agent (`sandbox: seatbelt`) can **read** its credential but cannot
**write** it back: on macOS the store is the login keychain, a single file under
`~/Library/Keychains`, which the seatbelt profile's blanket `(deny file-write*)`
covers. A failed refresh leaves a stranded `login.keychain-db.sb-*` temp behind —
a zero-byte file next to the keychain is the tell.

That is worse than a write that merely fails. Anthropic's refresh tokens
**rotate**: the server retires the old one the moment it issues a new one. So a
refresh that cannot be persisted *destroys* the stored credential. The next spawn
presents an already-retired token and the agent is locked out — which looks
exactly like Anthropic revoking the token out of the blue.

### The warden

The machine the agents run on watches each Claude Code credential's expiry and,
shortly before it lapses, runs one minimal **unsandboxed** `claude` turn. Claude Code refreshes and
persists normally, because nothing is denying the write, and sandboxed sessions
only ever read a credential that is already valid.

It **aims** rather than polls. Claude Code refreshes proactively only once the
token is inside its own threshold, measured at **five minutes**: a forcing turn
at 5m13s remaining did nothing, and the next at 3m13s refreshed. So the warden
sleeps until three minutes before expiry and acts then, retrying every 30s until
the stored expiry actually moves. Exit status 0 is not treated as success — the
expiry is read back, because a turn can complete perfectly and change nothing.

Every wait is capped at five minutes and every pass re-reads the real expiry
rather than trusting elapsed time. Go timers run on the monotonic clock, which
stops while the host sleeps, so a wait computed before a suspend fires late by
however long the machine was away; re-reading means a credential that lapsed
overnight is noticed on the next pass instead of after a timer that was asleep
too.

On a gateway it runs for the **daemon's** lifetime, not the leader's. A Claude Code credential
is scoped to the machine — one keychain item shared by every `claude_code` agent
on the host — while leadership is scoped to the cluster. A standby that let its
own credential lapse would turn the next failover into a promotion of a node that
cannot authenticate.

It is **derived, not configured**. There is no enable flag: the warden exists
because a `claude_code` agent exists and disappears when the last one is removed.
It is one watcher per distinct credential — the `(claude binary, HOME)` pair —
because two concurrent refreshes would race the server's rotation and cause the
very lockout it prevents.

It is also deliberately **not** a job. A job would be runnable by name,
redefinable, and silently disable-able by any agent holding the `jobs` tool
group; the warden is internal, so there is nothing to enumerate or turn off.

When a credential starts failing, the warden says so once rather than on every
retry: a card in the admin's DM for the gateway's own agents, or in the node
owner's DM for a node's, edited in place once it recovers. A node tells its
gateway where each credential stands every time it connects, so an outage that
began while it was disconnected is still reported.

Two consequences worth knowing:

- It spends a **small amount of quota**: one throwaway prompt per refresh.
  `claude auth status` was measured and does *not* refresh — it reads local state
  and returns — so only a real turn will do.
- **ACP agents are not covered.** An ACP command may be Claude Code behind an
  adapter, but the adapter's name is arbitrary, so guessing would fail quietly.
  Credentials for ACP agents are the admin's own responsibility.

### Repairing a login

```sh
/murtaugh auth status   # admin-only: what each credential's warden last saw
/murtaugh auth login          # start a Claude Code sign-in now
/murtaugh auth login <node>   # on a node you name, where no node is pinned
```

`auth status` reports timings only, never token material. For a gateway's own
agents that is the observed expiry, when the warden next intends to look, how
many turns it has spent against the current expiry without moving it, and the
last error. For agents on nodes it is what each connected node last reported:
whether the credential works, its expiry, the error, and how long ago.

`auth login` posts the [Auth Request](agents.md#what-an-agent-can-do-tools)
card: open the link, sign in, paste the code back. On a gateway running its own
agents it is admin-only and the card comes to you. On a gateway whose agents run
on nodes it signs in the node **the conversation you type it in is pinned to**
— typed in a thread, that thread's conversation — and the card goes to that
node's owner; the admin or that owner may run it. Where no node is pinned, name
one: `auth login <node>` accepts a connected node you own (the admin may name
any), and without a name the reply lists the nodes you may name. Murtaugh never
picks a node for you. A node whose owner may no longer use the gateway is
refused, and asking again replaces a sign-in the node already has open.

You rarely need to ask. When a turn fails because the credential was rejected,
the machine the agent runs on starts the sign-in **unprompted** — a node DMs its
owner, a gateway running its own agents DMs the admin — and the user is told
in-thread that their turn is blocked until it is done. One sign-in runs at a
time per credential, no matter how many conversations fail at once. If the owner
cannot be shown one, or turns it down or leaves it, the user is told so and the
node waits ten minutes before asking again on its own.

> **A caveat worth stating.** The sandbox is not a boundary for this credential.
> Reads are allow-by-default and the keychain is reachable over IPC, so any agent
> holding the `terminal` tool group can read the Claude Code credential. Treat
> `terminal` as credential-equivalent when deciding which agents get it.

---

## What the gateway owns, and what a node owns

The gateway process owns:

- the **Slack event loop** (slash commands, mentions, DMs, buttons, links);
- the **rendering** of every chat turn, including its streaming reply
  ([Agent chat](agents.md));
- the **workflow** and **unfurl** handlers ([Slack](slack.md));
- the **job scheduler** ([Jobs](jobs.md));
- the **event journal** writer ([Gateway Debug Mode](journal.md)).

A node (`murtaugh-runtime`) owns the other half: the **agents** themselves and
their tools, their MCP servers, and the **Claude Code credential warden** for
the agents it runs (above).

If the gateway is down, scheduled jobs don't fire and Slack events go unanswered
— every Slack event flows through it. If the node serving a conversation is
down, that conversation has nowhere to run even though the gateway is up.
