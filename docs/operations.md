# Operations

Running and debugging the `slack gateway` daemon — the long-lived Socket Mode
process that handles every Slack event (slash commands, button clicks, mentions,
DMs, link previews) and runs scheduled jobs. For *installing* the daemon see
[Getting started](getting-started.md); this page is about what it does once it's
running and how to keep it healthy.

---

## Running the gateway

```sh
murtaugh slack gateway
```

The gateway connects to Slack over Socket Mode and stays up. At startup it warms
up the configured agents, sends the **"Murtaugh has started"** info card to the
admin DM, and starts the job scheduler.

### As a daemon (macOS)

The macOS installer can create `~/Library/LaunchAgents/dev.murtaugh.plist` (via
`murtaugh setup launchd`) so the gateway starts automatically on login and
restarts on crash. Under launchd it logs to:

- **`~/Library/Logs/murtaugh/slack.out.log`** — stdout
- **`~/Library/Logs/murtaugh/slack.err.log`** — stderr

**Start any debugging in those logs** — startup, agent warmup, event handling,
job runs, and errors all land there.

On other platforms, run `murtaugh slack gateway` under your own supervisor
(systemd, a process manager, etc.).

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
then on, edit configuration with `murtaugh cfg …`. See
[Configuration](configuration.md#upgrading-from-the-yaml-tree).

---

## A quiet turn may be waiting, not hung

Some chat turns now wait on a human before continuing:

- a native agent's `terminal` command can be **approval-gated** (see
  [Agent chat → The approval gate](agents.md#the-approval-gate));
- `ask` and `present_plan` **block** on your Approve/Deny or your answer in Slack;
- a held job's first scheduled run blocks on admin confirmation (see
  [Jobs → Trusted vs held jobs](jobs.md#trusted-vs-held-jobs)).

If a turn has gone quiet, check whether there's a card waiting for your click
before assuming it's stuck.

---

## Troubleshooting

### Access is fail-closed

Only the admin plus everyone in the access allowed-users list may interact. With
the list empty, the bot is **admin-only** — so *"the bot ignores me"* is most
often an access-list problem, not a bug. Inspect it with `murtaugh cfg access
show`. Handles in the access lists are resolved to IDs at startup, and **the
gateway refuses to start if any entry can't be resolved** — check the startup log
for a resolution error.

### "The bot ignores me" checklist

1. Are you the admin, or in the allowed-users list? (`murtaugh cfg access show`)
2. In a channel, did you `@mention` the bot?
3. Is chat enabled and pointed at a real agent? (`murtaugh cfg chat show`,
   `murtaugh cfg agent list`)
4. Did the gateway actually start? Check `slack.err.log` for an auth or
   config-validation failure.

### Query the journal

For *"why did this workflow / unfurl / job misbehave?"*, don't grep logs — query
the structured [event journal](journal.md). It records each interaction with a
correlation id so you can replay one click end to end. The `connection` events on
the `gateway` stream are where to look for *"why did the daemon go silent?"*.

### Ship a diagnostics bundle

```sh
murtaugh slack send-msg ...        # if Slack itself works
/murtaugh troubleshoot             # from Slack: bundles config.yaml + a config-store dump
```

`/murtaugh troubleshoot` collects `config.yaml` and a dump of the config store
(the same content as `murtaugh cfg show`) into an uploadable bundle. It
deliberately **never** includes `.env`, so secrets don't leak — and because every
value in the store is a `${VAR}` reference, the dump carries no credentials
either (see [Configuration](configuration.md)).

---

## Claude Code credentials

A `claude_code` agent runs on the Claude Code CLI's own OAuth credential — the
one `claude auth login` writes. Murtaugh keeps it alive and can repair it from
Slack, so a lapsed login does not mean SSH-ing to the host.

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

The gateway watches each Claude Code credential's expiry and, shortly before it
lapses, runs one minimal **unsandboxed** `claude` turn. Claude Code refreshes and
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

It runs for the **daemon's** lifetime, not the leader's. A Claude Code credential
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

Two consequences worth knowing:

- It spends a **small amount of quota**: one throwaway prompt per refresh.
  `claude auth status` was measured and does *not* refresh — it reads local state
  and returns — so only a real turn will do.
- **ACP agents are not covered.** An ACP command may be Claude Code behind an
  adapter, but the adapter's name is arbitrary, so guessing would fail quietly.
  Credentials for ACP agents are the admin's own responsibility.

### Repairing a login

```sh
/murtaugh auth status   # admin-only: expiry, last refresh, last error per credential
/murtaugh auth          # admin-only: start a Claude Code sign-in now
```

`auth status` reports timings only, never token material: observed expiry, when
the warden next intends to look, how many turns it has spent against the current
expiry without moving it, and the last error. `auth` posts the
[Auth Request](agents.md#what-an-agent-can-do-tools) card to your DMs: open the
link, sign in, paste the code back.

You rarely need to ask. When a turn fails because the credential was rejected,
the gateway posts that card **unprompted** and tells the user in-thread that
their turn is blocked pending the admin. One card is posted no matter how many
conversations fail at once.

> **A caveat worth stating.** The sandbox is not a boundary for this credential.
> Reads are allow-by-default and the keychain is reachable over IPC, so any agent
> holding the `terminal` tool group can read the Claude Code credential. Treat
> `terminal` as credential-equivalent when deciding which agents get it.

---

## What the daemon owns

One process runs it all:

- the **Slack event loop** (slash commands, mentions, DMs, buttons, links);
- the **chat agents** and their streaming replies ([Agent chat](agents.md));
- the **workflow** and **unfurl** handlers ([Slack](slack.md));
- the **job scheduler** ([Jobs](jobs.md));
- the **Claude Code credential warden** (above);
- the **event journal** writer ([Gateway Debug Mode](journal.md)).

If the gateway is down, scheduled jobs don't fire and Slack events go unanswered
— everything flows through it.
