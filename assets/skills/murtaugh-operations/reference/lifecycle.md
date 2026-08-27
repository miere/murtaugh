# Gateway lifecycle

What `murtaugh slack gateway` does, in order, when it starts:

1. **Resolve the allowlist.** `admin_user` and `allowed_users` (handles or IDs)
   are resolved to Slack user IDs up front, with one `users.list` call if any
   entry is a handle. Unresolvable entries are **fatal** (fail-closed). If both
   lists are empty, it logs a warning and runs locked down. →
   `reference/auth-and-troubleshooting.md`
2. **Connect** to Slack over Socket Mode (in the background).
3. **Warm the agents.** Each **ACP** agent is probed for session/cancel support
   (bounded by `startup_timeout`); the verdict is logged. A failed warmup is
   logged, not fatal. Native (in-process) agents have no such probe — there's no
   subprocess to handshake with — so they aren't warmed here. (See the
   `murtaugh-agents` skill.)
4. **Start the job scheduler** — registers cron/`every` jobs from `jobs.yaml`
   (manual jobs are ignored here). (See the `murtaugh-jobs` skill.)
5. **Run the event loop** — dispatch slash commands, interactions, mentions,
   DMs, and `link_shared` until shutdown.

Config files are read **once at startup** — there is no on-disk watcher. After
editing config, restart the daemon (e.g. the **Restart** button on the
App Home tab) to load the changes. → `reference/config-and-restart.md`

## Startup greeting

Once connected, the gateway greets the admin **once per process** — and exactly
one of two things happens:

- **Fresh boot:** it DMs the admin a **"Murtaugh has started"** info card — the
  same collapsed alert card used everywhere else, at `info` severity.
- **Returning from a restart:** the greeting is suppressed; instead the pending
  "Restarting Murtaugh…" notice is edited in place into **"Murtaugh is back
  online"** (see `config-and-restart.md`).

## Test communication

The self-test button lives in the **App Home control row**, to the right of
**Restart** — so it is reachable at any time, not just from whichever lifecycle
message happens to be the newest in the DM. Its ids are Go constants
(`internal/slack/pingcard`) and the click is answered by the binary itself with
a **"Communication check — The server communication is functional."** info card;
no workflow rule or template is involved, so the self-test can't be broken by
config edits. A click from the App Home carries no channel, so the reply lands
in the clicker's DM.

A reconnect won't repeat the greeting. Seeing it is the quickest confirmation the
daemon is up and the admin user resolved correctly. If it never arrives, check
that `admin_user` is set and resolvable. If it arrives as plain text rather than
a card, the bot token could not build a raw-blocks client — every alert degrades
the same way.

## Event deduplication

Slack's Events API delivers **at least once**, so a mention or DM can arrive
twice (e.g. after a reconnect). The gateway suppresses duplicates by
`teamID|channelID|messageTS` for ~15 minutes, so a redelivery doesn't spawn a
second chat that interrupts the first. If you see a single message handled twice,
suspect something *other* than redelivery (e.g. two running daemons).
