---
name: murtaugh-jobs
description: Define, run, and schedule Murtaugh jobs (stored in the config database) via `murtaugh cfg job` and the jobs_run/jobs_define tools — a shell command or an agent, run manually or on a cron/interval.
requires: [jobs]
files:
  reference/configuring.md: { requires: [jobs], summary: "define a job (cfg job set) — command / agent+prompt / arg / workdir / timeout" }
  reference/scheduling.md:  { requires: [jobs], summary: "choose a schedule (cron) / every (interval) value; held-job approval" }
  reference/running.md:     { requires: [jobs], summary: "run a job by hand or wire jobs_run / jobs_define" }
---

# Skill: Murtaugh Jobs

A **job** is a named unit of work stored in the **config database** and managed
with `murtaugh cfg job …` (`cfg job set|list|show|delete`, also `cfg.*` over MCP).
It runs **either** a shell command (with args, working directory, and timeout)
**or** an agent (`--agent` + `--prompt`, fire-and-forget) — the two are mutually
exclusive. Jobs run **on demand** (CLI, MCP, or a workflow trigger) and can
additionally run **automatically** on a schedule. Use this whenever a task
involves defining, running, or scheduling work that Murtaugh executes — backups,
syncs, reconcile scripts, clock-tick automations, or agent-delegated chores.

## The three trigger modes (at a glance)

Every job has exactly one trigger mode, decided by two optional, **mutually
exclusive** fields:

| Mode | Fields set | Runs when |
|---|---|---|
| **manual** | neither | only when you invoke it (`jobs_run`, MCP, or a workflow) |
| **cron** | `schedule:` | automatically, on a 5-field cron expression (e.g. `"0 2 * * *"`) |
| **interval** | `every:` | automatically, on a fixed interval (e.g. `"1h"`, `"30m"`) |

Scheduled modes (`schedule`/`every`) only fire while the **`slack gateway`
daemon** is running — it owns the in-process scheduler.

**Every new or modified job is held.** Whichever surface writes it — `cfg job
set` or the `jobs_define` tool, CLI or MCP — the entry is stamped
`confirmed: false` and **held**: it is still scheduled, but on its **next
trigger** the scheduler asks the admin (in their DM) to approve that run before
it executes. Approving stamps `confirmed: true`, which persists, so a gateway
restart does not re-ask; editing the job stamps it back to `false`, so the
approval never carries over to a changed command — see
`reference/scheduling.md`. This exists because a defined job's command runs
**headless and ungated**, so nothing gets to define-then-auto-run a command
without a human OK.

## Read the right file (don't load everything)

| When you're… | Read |
|---|---|
| Defining a job's command / agent+prompt / arg / workdir / timeout | `reference/configuring.md` |
| Choosing or writing a `schedule` / `every` value | `reference/scheduling.md` |
| Running a job by hand or wiring `jobs_run` / `jobs_define` | `reference/running.md` |
| Wanting worked `cfg job set` examples | `examples/` |

## Global guidelines (defaults — follow unless the user says otherwise)

- **`cfg job list` first.** The store is the source of truth for existing job
  names; reuse / overwrite (`cfg job set --name <same>`) a job that serves the
  same purpose rather than adding a parallel one.
- **One trigger mode per job.** Never set both `--schedule` and `--every` —
  Murtaugh rejects that at validation time (and rolls the change back). Leave both
  unset for a manual-only job.
- **Schedule edits apply on the next gateway restart**, not live. After a
  `cfg job set`, restart the gateway (e.g. the **Restart** button on the
  App Home tab).
- **`jobs_define` requires approval.** Defining a job via the agent tool is never a
  silent write — it always prompts a human, showing the rendered command +
  schedule, and stamps the new/updated entry `confirmed: false` so its next
  scheduled run is held until the admin confirms it (see `reference/running.md` and
  `reference/scheduling.md`). `cfg job set` skips the write-time prompt, but stamps
  the same mark, so its jobs are held for that first-run confirmation too.
- **`--command` should be an absolute path** (or a binary on `PATH`); a relative
  command resolves against the job's `--workdir`, which defaults to the workspace
  (`~/.config/murtaugh`).
- **Scheduled runs are best-effort.** A run that would fire while the gateway is
  down is **skipped, not caught up** (see `reference/scheduling.md`). Don't rely
  on a scheduled job for must-not-miss accounting without external safeguards.
- **Ask the binary for exact flags.** `murtaugh help cfg job` (defining) and
  `murtaugh help jobs run` (running by hand) — or `--help` on either — print the
  full flag reference: which are required, the repeatable `--arg` form, and the
  `--timeout`/`--schedule`/`--every` value formats.
