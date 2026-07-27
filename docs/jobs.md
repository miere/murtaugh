# Jobs

A **job** is a named unit of work in the config store, managed with
`murtaugh cfg job …`. It runs **either** a shell command (with args, working
directory, and timeout) **or** an agent (`--agent` + `--prompt`,
fire-and-forget) — the two are mutually exclusive. Jobs run **on demand** (CLI,
MCP, or a workflow trigger) and can additionally run **automatically** on a
schedule.

Use jobs for backups, syncs, reconcile scripts, clock-tick automations, or any
chore you want to delegate to an agent.

---

## Trigger modes

Every job has exactly one trigger mode, decided by two optional, mutually
exclusive flags:

| Mode | Flag set | Runs when |
|---|---|---|
| **manual** | neither | only when you invoke it (`jobs run`, MCP, or a workflow) |
| **cron** | `--schedule` | automatically, on a 5-field cron expression (`"0 2 * * *"`) |
| **interval** | `--every` | automatically, on a fixed interval (`"1h"`, `"30m"`) |

Scheduled modes only fire while the **`slack gateway`** daemon is running — it
owns the in-process scheduler. Setting both `--schedule` and `--every` is
rejected when the `cfg job set` change is validated.

---

## Defining a job

Create or update jobs with `cfg job set` (repeat the command to edit an existing
one — the change re-validates the whole store):

```sh
# Command job, manual.
murtaugh cfg job set --name example-job \
  --command /bin/echo --arg "hello from murtaugh"
  # --workdir /path/to/working/directory --timeout 5m

# Command job, cron-scheduled (daily at 02:00).
murtaugh cfg job set --name nightly-backup \
  --command /usr/local/bin/backup.sh --schedule "0 2 * * *"

# Command job, interval-scheduled (hourly).
murtaugh cfg job set --name hourly-sync \
  --command /usr/local/bin/sync.sh --every 1h

# Agent-delegated job.
murtaugh cfg job set --name code-review-job \
  --agent default \
  --prompt 'Review the code changes in this PR and provide feedback.
- pr: {{ 1 }}
- local repository: {{ 2 }}'

murtaugh cfg job list                       # existing jobs
murtaugh cfg job show --name code-review-job
murtaugh cfg job delete --name code-review-job
```

- **`--command`** should be an absolute path (or a binary on `PATH`); a relative
  command resolves against `--workdir`, which defaults to the workspace
  (`~/.config/murtaugh`). `--arg` is repeatable.
- An **agent job** (`--agent` + `--prompt`) starts the named agent in an isolated
  one-shot session and sends the rendered prompt; it is fire-and-forget — the
  agent acts through its own tools.
- Prompts (and command args) support **positional placeholders** `{{ 1 }}`,
  `{{ 2 }}`, … that expand to the args passed at run time.

---

## Running a job

```sh
murtaugh jobs run --name nightly-backup

# Pass positional args (fill {{ 1 }}, {{ 2 }}, …):
murtaugh jobs run --name code-review-job --args 1234 --args /path/to/repo
```

Define a job from the CLI or an MCP client:

```sh
murtaugh jobs define \
  --name nightly-deploy \
  --command /usr/local/bin/deploy \
  --args --env --args production \
  --workdir /srv/deploy \
  --timeout 15m
```

Both `jobs run` and `jobs define` are also MCP tools (`jobs.run`, `jobs.define`).
Run `murtaugh help jobs run` / `murtaugh help jobs define` for the full flag
reference, including the repeatable `--args` form and the
`--timeout`/`--schedule`/`--every` value formats.

---

## Trusted vs held jobs

How a scheduled job's first run is treated depends on **who wrote it**:

- A job **you author** with `cfg job set` is **trusted**: a scheduled one
  auto-runs as soon as the gateway is up.
- A job created by the **`jobs.define` tool** is stamped `confirmed: false` and
  **held**: it is still scheduled, but on its **first trigger** the scheduler
  DMs the admin to approve that run before it executes.

This exists because a defined job's command runs headless and ungated — so an
agent can never define-then-auto-run a command without a human OK. `jobs.define`
also always prompts a human at definition time, showing the rendered command and
schedule.

---

## Things to know

- **Run `cfg job list` first.** It is the source of truth for existing job names;
  reuse or overwrite a job that serves the same purpose (`cfg job set` with the
  same `--name`) rather than adding a parallel one.
- **Schedule edits apply on the next gateway restart**, not live. After a
  `cfg job set` change, restart the gateway (e.g. the **Restart Murtaugh** button
  on the App Home tab).
- **Scheduled runs are best-effort.** A run that would fire while the gateway is
  down is **skipped, not caught up**. Don't rely on a scheduled job for
  must-not-miss accounting without external safeguards.
- **Job runs are journaled.** Every execution lands on the `job`
  [journal](journal.md) stream with its exit code and duration.
