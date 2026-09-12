# Configuring a job

Jobs live in the **config database**, keyed by name, and are managed with
`cfg job set` (create/overwrite), `cfg job list`, `cfg job show --name <n>`, and
`cfg job delete --name <n>`. A job runs **either** a command **or** an agent — the
two are mutually exclusive. Every `cfg job set` re-validates the whole config and
rolls back an invalid change; **restart the gateway** for a schedule change to
take effect.

```bash
# command job
murtaugh-gateway cfg job set --name cleanup-logs \
  --command /usr/bin/find \
  --arg /var/log --arg -mtime --arg +7 --arg -delete \
  --workdir /tmp --timeout 5m
  # add --schedule "0 3 * * *"  (cron) or --every 1h  (interval) for auto-runs

# agent-delegated job
murtaugh-gateway cfg job set --name code-review-job \
  --agent default \
  --prompt 'Review the changes in PR {{ 1 }} at {{ 2 }} and post your feedback.'
```

## Flags

| Flag | Required | Meaning |
|---|---|---|
| `--name` | yes | The job's key. `cfg job set` on an existing name overwrites that job. |
| `--command` | one of | The executable. Absolute path, or a name resolved on `PATH`. A relative path resolves against `--workdir`. Mutually exclusive with `--agent`/`--prompt`. |
| `--arg` | no | Positional process argument for a command job (verbatim, no shell splitting) — **repeat the flag**, once per argument. For an agent job, the default values for the prompt's `{{ N }}` placeholders when no run-time args are passed. |
| `--agent` | one of | Name of an agent (`cfg agent list`). Runs it in an isolated one-shot session instead of a command. Requires `--prompt`; mutually exclusive with `--command`. |
| `--prompt` | with `--agent` | The agent prompt. Supports positional placeholders `{{ 1 }}`, `{{ 2 }}`, … (1-based) that expand to the run-time args (falling back to the job's `--arg` values). |
| `--report-to` | no | Agent jobs only. Where the gateway posts the agent's final reply after a scheduled run: `#channel`, a channel ID, `@handle` or a user ID (a person gets it in their DM with the bot). → "Reporting the reply" below. |
| `--workdir` | no | Working directory for the process. Defaults to the **workspace** (the config dir, e.g. `~/.config/murtaugh`). |
| `--timeout` | no | A Go duration (`30s`, `5m`, `2h`). The run is killed if it exceeds this. Defaults to **10m**. |
| `--schedule` | no | Cron expression for automatic runs. Mutually exclusive with `--every`. → `scheduling.md` |
| `--every` | no | Interval duration for automatic runs. Mutually exclusive with `--schedule`. → `scheduling.md` |

The first-run gate — a job's `confirmed` flag — is **not** a `cfg` flag; you
cannot set it directly. Every write stamps it `false`, so a job created **or
edited** with `cfg job set` is **held** until the admin approves its next
scheduled run, and so is one written by the `jobs_define` agent tool (which is
additionally approval-gated at write time, prompting a human with the rendered
command + schedule). The only way to clear the hold is to approve the job in the
admin DM. An approval covers that exact entry: edit the job and it is held again,
however small the change. See `scheduling.md` and `running.md`.

## Agent jobs

Murtaugh starts the agent, sends the rendered prompt, and hands the agent's
final reply back to whoever ran the job (`jobs run` returns it; `jobs run`
itself journals none of it). The agent does its work through its own tools/MCP (it might open a
PR, say). An agent on a runtime node has **no Slack tools** — a node never talks
to Slack — so "post the result to #ops" in the prompt cannot work there; use
`--report-to`. Pass positional args at run time to fill the prompt placeholders:

```sh
murtaugh-runtime jobs run --name code-review-job --args 1234 --args /path/to/repo
```

Here `{{ 1 }}` becomes `1234` and `{{ 2 }}` becomes `/path/to/repo`. For a
**scheduled** agent job (no run-time args), bake the values into the job's
`--arg` values so the placeholders still resolve.

## Reporting the reply (`--report-to`)

After a **scheduled** run the gateway posts the agent's final reply, as the bot,
to the job's `--report-to` destination:

```sh
murtaugh-gateway cfg job set --name nightly-digest \
  --agent default --prompt "Summarise last night's alerts." \
  --schedule "0 7 * * *" --report-to "#ops"
```

- The gateway posts **and keeps** a reply only when the node that ran the job
  belongs to the gateway admin (the user its node token was minted for is
  `access.admin_user`), or when the agent ran inside the gateway process itself.
  Kept means a `job.reply` event on the `job` journal stream, with a long reply
  in a journal blob file; that happens with or without `--report-to`.
- For anyone else's node the reply is **neither posted nor kept**, whether or
  not the job has `--report-to`: no journal row or blob file carries any of it.
  A `job.reply` event records only that it was withheld — job, node, owner, and
  the `--report-to` destination if any — and when the job has `--report-to` the
  admin gets a DM saying so, once until a report gets through again.
- The destination comes only from the job definition. Nothing the agent writes
  in its reply can redirect it.
- A failed run is not reported (the admin gets the failed-job alert), an empty
  reply is not posted, and a delivery that fails (unknown channel, bot not
  invited) is journalled at ERROR.
- Changing it re-arms first-run approval like any other edit, and the approval
  card names the destination. `--report-to ""` clears it.

## No shell interpretation

Args are passed straight to the process, not through a shell. Pipes, redirects,
globbing, and `$VAR` expansion do **not** happen. If you need them, make the
command a shell explicitly:

```sh
murtaugh-gateway cfg job set --name piped-report \
  --command /bin/sh \
  --arg -c --arg 'generate | tee $HOME/report.txt'
```

## Validation

A `cfg job set` is rejected (and rolled back) when:

- neither `--command` nor `--agent`+`--prompt` is set (a job needs one or the other).
- both `--command` and `--agent`/`--prompt` are set (they are mutually exclusive).
- `--agent` is set without `--prompt` (or vice versa).
- `--agent` names an agent that is not defined (`cfg agent list`).
- `--timeout` is set but not a valid Go duration.
- `--every` is set but not a valid, positive Go duration.
- both `--schedule` and `--every` are set.
- `--report-to` is set on a command job (only an agent job has a reply to report).

A bad `--schedule` (malformed cron) is not caught at set time; instead the gateway
logs it and skips that one job at startup — see `scheduling.md`.

## Defining jobs programmatically

`cfg job set` is the operator path. The `jobs_define` tool (CLI / MCP) writes an
entry for an agent and preserves the others, stamping it `confirmed: false`. See
`running.md`.
