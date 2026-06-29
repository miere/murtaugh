# Config changes & graceful restart

## Config is loaded once

The gateway reads `gateway.yaml`, `agents.yaml`, `jobs.yaml`, and the rule-file
siblings `workflow-rules.yaml` / `unfurl-rules.yaml` **at startup only**. Editing
them on disk changes nothing until the daemon restarts — this applies to
allowlists, chat routing, agents, workflow/unfurl rules, and job schedules alike.

(A legacy `slack.yaml` config dir is auto-migrated to this layout on the first
run of a new binary — backed up and validated, rolled back on failure — or
convert it ahead of time with `murtaugh config migrate`; a `.schema_version`
sidecar tracks the version.)

## Picking up a config change

Config changes are not detected automatically — after editing any of the files
above, restart the daemon yourself to load them (see below).

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
