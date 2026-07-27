# Configuring a job

Jobs live in the **config database**, keyed by name, and are managed with
`cfg job set` (create/overwrite), `cfg job list`, `cfg job show --name <n>`, and
`cfg job delete --name <n>`. A job runs **either** a command **or** an agent — the
two are mutually exclusive. Every `cfg job set` re-validates the whole config and
rolls back an invalid change; **restart the gateway** for a schedule change to
take effect.

```bash
# command job
murtaugh cfg job set --name cleanup-logs \
  --command /usr/bin/find \
  --arg /var/log --arg -mtime --arg +7 --arg -delete \
  --workdir /tmp --timeout 5m
  # add --schedule "0 3 * * *"  (cron) or --every 1h  (interval) for auto-runs

# agent-delegated job
murtaugh cfg job set --name code-review-job \
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
| `--workdir` | no | Working directory for the process. Defaults to the **workspace** (the config dir, e.g. `~/.config/murtaugh`). |
| `--timeout` | no | A Go duration (`30s`, `5m`, `2h`). The run is killed if it exceeds this. Defaults to **10m**. |
| `--schedule` | no | Cron expression for automatic runs. Mutually exclusive with `--every`. → `scheduling.md` |
| `--every` | no | Interval duration for automatic runs. Mutually exclusive with `--schedule`. → `scheduling.md` |

The first-run gate — a job's `confirmed` flag — is **not** a `cfg` flag. A job you
create with `cfg job set` is operator-trusted and auto-runs. The `jobs_define`
agent tool instead writes every entry `confirmed: false` and is itself
approval-gated (it prompts a human, showing the rendered command + schedule,
before writing), so an agent-defined scheduled job is **held** until the admin
confirms its first run — see `scheduling.md` and `running.md`.

## Agent jobs

An agent job is **fire-and-forget**: Murtaugh starts the agent, sends the
rendered prompt, and discards the agent's text output — the agent does its work
through its own tools/MCP (it might open a PR, post to Slack, etc.). Pass
positional args at run time to fill the prompt placeholders:

```sh
murtaugh jobs run --name code-review-job --args 1234 --args /path/to/repo
```

Here `{{ 1 }}` becomes `1234` and `{{ 2 }}` becomes `/path/to/repo`. For a
**scheduled** agent job (no run-time args), bake the values into the job's
`--arg` values so the placeholders still resolve.

## No shell interpretation

Args are passed straight to the process, not through a shell. Pipes, redirects,
globbing, and `$VAR` expansion do **not** happen. If you need them, make the
command a shell explicitly:

```sh
murtaugh cfg job set --name piped-report \
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

A bad `--schedule` (malformed cron) is not caught at set time; instead the gateway
logs it and skips that one job at startup — see `scheduling.md`.

## Defining jobs programmatically

`cfg job set` is the operator path. The `jobs_define` tool (CLI / MCP) writes an
entry for an agent and preserves the others, stamping it `confirmed: false`. See
`running.md`.
