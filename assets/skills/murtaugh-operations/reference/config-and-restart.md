# Config changes & graceful restart

## Config is loaded once

The gateway reads `gateway.yaml` (only `oauth:` + `database:`) and the **config
database** it points at **at startup only**. Changing config — via `murtaugh cfg
…` for everything in the database (access, chat routing, agents, jobs,
workflow/unfurl rules, journal), or editing `gateway.yaml`'s two blocks — changes
nothing until the daemon restarts. Each `cfg` mutation re-validates the whole
config and rolls back an invalid change, but the **live** gateway keeps running
the config it loaded at boot until you restart it.

(Upgrading a new binary against an old YAML tree — `agents.yaml`, `jobs.yaml`,
`journal.yaml`, `workflow-rules.yaml`, `unfurl-rules.yaml`, `troubleshoot.yaml` —
**auto-migrates** the whole tree into SQLite on first run, slims `gateway.yaml` to
`oauth:`+`database:`, and archives the old siblings to
`~/.config/murtaugh/migrated-<timestamp>/`. Validated, rolled back on failure.
Move the store to Postgres later with
`murtaugh cfg db migrate --to postgres --dsn-env MURTAUGH_DB_DSN`.)

## Picking up a config change

Config changes are not detected automatically — after any `cfg …` change (or a
`gateway.yaml` edit), restart the daemon yourself to load them (see below).

## Triggering a restart

Three ways, all **admin-only**:

- **`/murtaugh restart`** (slash command),
- the **Restart Murtaugh** button on the **App Home** tab, or
- the **Restart now** button on a restart-approval card posted by the `restart`
  tool (`murtaugh_restart_suggestion_confirm`; **Dismiss** is
  `murtaugh_restart_suggestion_dismiss`). The card is only posted when the
  `restart` tool is invoked by an agent/MCP/CLI — it asks; it never restarts on
  its own, and confirm goes through the same admin-gated path as the others.

Guards:
- Requires `IsAdminUser` — a non-admin gets an ephemeral/edited "only the admin
  can restart" message.
- A **cool-down** prevents back-to-back restarts; a request during cool-down (or
  while one is already in flight) is declined with a "busy, try again" message.
- If no restart coordinator is wired, it reports the feature unavailable.

The restart itself is a clean process exit; the supervisor (launchd `KeepAlive`,
or your own) brings it back.

## The "restarting… / back online" notice

Across a restart the gateway preserves a **single** notice so the requester sees
it complete:

1. Before exiting it posts **":hourglass_flowing_sand: Restarting Murtaugh
   now…"** and writes a **resume marker** to disk —
   `$XDG_STATE_HOME/murtaugh/restart.json` (else `~/.local/state/murtaugh/restart.json`).
   When the restart was approved via the `restart` tool's approval card, this
   notice is posted **in a thread under that card**, so the whole exchange nests
   where it was approved.
2. On reconnect it consumes the marker **once** and edits that same message into
   the **":white_check_mark: Murtaugh is back online."** ping card — the
   back-online confirmation *is* the Test communication card, so there is one
   restart message, not three. The standalone startup ping is suppressed while a
   marker is being consumed.

A marker older than **1 hour** is treated as stale and ignored (so a crash long
after the request doesn't post a misleading "back online"). The marker is
best-effort: if posting or persisting fails, the restart still happens — just
without the confirmation edit.
