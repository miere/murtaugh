---
name: murtaugh-operations
description: Run and diagnose the long-lived murtaugh-gateway daemon — lifecycle, launchd logs, config changes, graceful restart, and fail-closed auth troubleshooting.
requires: [restart]
files:
  reference/lifecycle.md:                { requires: [restart], summary: "what happens at startup — warmup, ping, scheduler" }
  reference/config-and-restart.md:       { requires: [restart], summary: "apply config changes / do a graceful restart" }
  reference/auth-and-troubleshooting.md: { requires: [restart], summary: "diagnose auth / 'bot ignores me' / anything not working" }
---

# Skill: Murtaugh Gateway Operations

Running and debugging the gateway daemon — the long-lived Socket Mode process
that handles every Slack event (slash commands, button clicks, mentions, DMs,
link previews) and runs scheduled jobs. This is **operator-facing**: for
*installing* the daemon see `murtaugh-setup`; this skill is about what it does
once it's running and how to diagnose it.

## What the daemon is

**`murtaugh-gateway` with no command IS the daemon.** There is no `slack
gateway` subcommand and no combined `murtaugh` binary: run the gateway binary
with a command and it acts on its own configuration, run it with none and it
connects to Slack over Socket Mode and stays up.

Under launchd (macOS) it auto-restarts on crash and logs to files named after
its LaunchAgent label, `murtaugh.gateway.<alias>` (alias `default` unless the
operator gave one):

- **`~/Library/Logs/murtaugh/murtaugh.gateway.default.out.log`** (stdout)
- **`~/Library/Logs/murtaugh/murtaugh.gateway.default.err.log`** (stderr)

A node's logs sit beside them under `murtaugh.node.<alias>.{out,err}.log`.

**Start your debugging in those logs** — startup, agent warmup, event handling,
job runs, and errors are all there.

## Read the right file (don't load everything)

| When you're… | Read |
|---|---|
| Tracing what happens at startup (warmup, ping, scheduler) | `reference/lifecycle.md` |
| Applying config changes or doing a graceful restart | `reference/config-and-restart.md` |
| Diagnosing auth/"bot ignores me", or anything not working | `reference/auth-and-troubleshooting.md` |

## Key operational facts

- **Config changes need a restart.** The gateway loads config once at startup —
  from `config.yaml` and the **config database**. You change the database with
  `murtaugh-gateway cfg …`, then restart to apply; it never hot-reloads. (Upgrading a new
  binary against an old YAML tree auto-migrates it into the store first.) →
  `reference/config-and-restart.md`
- **Authorization is fail-closed.** Only the `cfg access set` admin + allowed
  users may interact; with both empty the bot is locked down. →
  `reference/auth-and-troubleshooting.md`
- **Restart is admin-only** (`/murtaugh restart` or the suggestion button) and
  preserves a "restarting… / back online" notice across the restart.
- **Some chat turns now wait on a human** — a `terminal` command an agent runs in
  live chat is approval-gated, and `ask`/`present_plan`/a held job's first-run can
  block on an Approve/Deny or answer in Slack. A quiet turn may be waiting, not
  hung. → `reference/auth-and-troubleshooting.md`
- The daemon also runs the **job scheduler** (see the `murtaugh-jobs` skill) and
  the **chat** and **unfurl** handlers (see `murtaugh-agents` and the
  `murtaugh-slack` skill's `unfurl.md`).
- **The daemon takes no tool flags** — only the global `--config PATH` and, in
  daemon mode, `-node-listen` / `-node-advertise`
  (`murtaugh-gateway --config /path/config.yaml -node-listen 127.0.0.1:8787`).
  Run `murtaugh-gateway help` to list every command it carries.
