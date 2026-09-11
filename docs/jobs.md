# Jobs

A **job** is a named unit of work in the config store, managed with
`murtaugh cfg job …`. It runs **either** a shell command (with args, working
directory, and timeout) **or** an agent (`--agent` + `--prompt`, optionally
reporting its reply with `--report-to`) — the two are mutually exclusive. Jobs run **on demand** (CLI,
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

# Agent-delegated job, cron-scheduled, reporting its reply to #ops.
murtaugh cfg job set --name nightly-digest \
  --agent default --prompt 'Summarise last night'"'"'s alerts.' \
  --schedule "0 7 * * *" --report-to "#ops"

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
  one-shot session and sends the rendered prompt. What happens to the agent's
  final reply depends on whose machine ran it — see "What happens to an agent
  job's reply" below. When the run is fired by the daemon
  (a schedule, or `jobs run` inside the gateway) the agent gets the same tools
  and MCP servers it has in chat. On a runtime node that set has no Slack tools
  — a node never talks to Slack — so a prompt ending "post the result to #ops"
  cannot be carried out there; name the destination with `--report-to` instead
  (below). Two things the agent does not get anywhere: an **approval gate** —
  nobody is in a thread to answer a card, so the agent's own `approval` policy
  is the only gate — and the `ask`/`present_plan` tools, which need a live
  conversation and fail with a clear error. Run the same job straight from the
  CLI and it drops to the backend's own built-ins: the aggregator only runs
  inside the daemon.
- Prompts (and command args) support **positional placeholders** `{{ 1 }}`,
  `{{ 2 }}`, … that expand to the args passed at run time.

---

## What happens to an agent job's reply (`--report-to`)

`--report-to` names where the **gateway** posts the agent's final reply after a
scheduled run: `#channel-name`, a channel ID, `@handle` or a user ID (a person
gets it in their DM with the bot). It is accepted on agent jobs only. The reply
is posted as the bot, as-is, in the same Markdown the agent wrote.

Who ran the job decides everything else. The gateway looks at the node that ran
it — its owner is the user its node token was minted for — after the run and
before it keeps or shows anything:

- **The gateway admin's own node** (the owner is `access.admin_user`), **or the
  gateway's own process** (`murtaugh slack gateway` runs agents itself). The
  reply is kept in the journal as a `job.reply` event on the `job` stream (a
  long one is kept whole in a journal blob file), and posted to `--report-to`
  when the job has one.
- **Anyone else's node.** The reply is neither posted nor kept, whether or not
  the job has `--report-to`: no journal row and no blob file carries a word of
  it. The journal gets a `job.reply` event saying it was withheld, naming the
  job, the node, its owner and the `--report-to` destination if there is one.
  When the job has `--report-to`, the admin is also sent a DM saying the report
  was withheld and whose node ran it — once, until a report for that job gets
  through again.

`jobs run` itself keeps no reply: its own `job.run` event records only the
agent and how long the run took. The destination only ever comes from the job's
own definition in the gateway's configuration; nothing a node or its agent says
can change it.

A run that fails is not reported: the admin gets the failed-job alert instead.
An empty reply is not posted, and a reply that cannot be delivered (unknown
channel, bot not invited) is journalled at ERROR. Clear the setting with
`--report-to ""`.

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

## Held jobs and first-run confirmation

Every write to a job — `cfg job set` or the `jobs.define` tool, over the CLI or
MCP — stamps the entry `confirmed: false`, which **holds** it: the job is still
scheduled, but on its next trigger the scheduler DMs the admin to approve that
run before it executes.

- **Approve** → the job runs, and `confirmed: true` is written back to the config
  store. Later triggers run straight through, and a gateway restart does not
  re-ask.
- **Edit the job afterwards** → the write stamps `confirmed: false` again, so the
  changed command is held for a fresh approval. An approval covers the exact
  definition it was shown, nothing else.

The DM is the same approval card a gated tool call renders as: the command it
will run (or, for a delegated job, the prompt the agent will be sent), the
schedule you are agreeing to, and Approve / Deny. Unlike a tool approval, the
settled card is **not** swept from the DM afterwards — the decision is a standing
one, so the record of who allowed the job to run unattended stays put.

This exists because a job's command runs headless and ungated — so nothing can
define-then-auto-run a command without a human OK. `jobs.define` additionally
prompts a human at definition time, showing the rendered command and schedule.

Jobs migrated from a hand-written `jobs.yaml` carry no `confirmed` field at all
and stay operator-trusted until something writes to them.

---

## Things to know

- **Run `cfg job list` first.** It is the source of truth for existing job names;
  reuse or overwrite a job that serves the same purpose (`cfg job set` with the
  same `--name`) rather than adding a parallel one.
- **Schedule edits apply on the next gateway restart**, not live. After a
  `cfg job set` change, restart the gateway (e.g. the **Restart** button
  on the App Home tab).
- **Scheduled runs are best-effort.** A run that would fire while the gateway is
  down is **skipped, not caught up**. Don't rely on a scheduled job for
  must-not-miss accounting without external safeguards.
- **Job runs are journaled.** Every execution lands on the `job`
  [journal](journal.md) stream with its exit code and duration.
