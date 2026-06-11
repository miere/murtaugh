# Running and defining jobs

Jobs are invoked the same way whether or not they're scheduled. The two tools
are exposed identically on the CLI and over MCP (when Murtaugh runs as
`murtaugh mcp`).

## `jobs.run` — execute a job

```bash
murtaugh jobs run --name cleanup-logs
```

- Resolves the job by name from `jobs.yaml`.
- Applies the job's `timeout` (default 10m) as a hard deadline.
- Runs `command` with `args` in `workdir`.
- Streams the child's stdout/stderr to the caller (your terminal on the CLI;
  captured into the JSON result over MCP) and reports the **exit code**.

A non-zero exit is returned as the result's `exit_code` (it is not, by itself, a
tool error). A failure to *start* the process (missing binary, etc.) is an error.

## `jobs.define` — register / update a job

```bash
murtaugh jobs define --name hourly-sync \
  --command /usr/local/bin/sync.sh \
  --every 1h
```

- Reads `jobs.yaml`, adds or replaces the named entry, writes it back. Other
  jobs are preserved verbatim.
- Accepts `--schedule` / `--every` (mutually exclusive), plus `--workdir` and
  `--timeout`. Same validation as `configuring.md`.
- Does **not** run the job — only defines it.

## Who runs a job

| Caller | How |
|---|---|
| You, by hand | `murtaugh jobs run --name <n>` |
| An MCP client / agent | the `jobs.run` tool |
| A Slack workflow | a `run` trigger in `workflow-rules` (see the `murtaugh-slack` skill) |
| The scheduler | automatically, per `schedule` / `every` (see `scheduling.md`) |

All paths share one execution path, so a job behaves the same no matter who
fires it.

## Output and logs

For scheduled runs there is no terminal attached: stdout/stderr flow to the
gateway process, which launchd writes to the Murtaugh log files (e.g. under
`~/.local/share/murtaugh/`). The scheduler also logs a line when a job starts,
completes, or fails — grep those logs by job name to see history.
