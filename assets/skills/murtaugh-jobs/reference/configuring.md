# Configuring a job

Jobs live under the `jobs:` map in `jobs.yaml`, keyed by name. Each entry is a
`JobProfile`:

```yaml
jobs:
  cleanup-logs:
    command: /usr/bin/find          # required — absolute path or PATH binary
    args: ["/var/log", "-mtime", "+7", "-delete"]
    workdir: /tmp                    # optional — defaults to the workspace
    timeout: 5m                      # optional — Go duration, defaults to 10m
    # schedule: "0 3 * * *"          # optional — cron (see scheduling.md)
    # every: 1h                      # optional — interval (mutually exclusive)
```

## Fields

| Field | Required | Meaning |
|---|---|---|
| `command` | yes | The executable. Absolute path, or a name resolved on `PATH`. A relative path resolves against `workdir`. |
| `args` | no | Positional arguments, as a YAML list. Each element is passed verbatim — no shell splitting. |
| `workdir` | no | Working directory for the process. Defaults to the **workspace** (the config dir, e.g. `~/.config/murtaugh`). |
| `timeout` | no | A Go duration (`30s`, `5m`, `2h`). The run is killed if it exceeds this. Defaults to **10m**. |
| `schedule` | no | Cron expression for automatic runs. Mutually exclusive with `every`. → `scheduling.md` |
| `every` | no | Interval duration for automatic runs. Mutually exclusive with `schedule`. → `scheduling.md` |

## No shell interpretation

`args` are passed straight to the process, not through a shell. Pipes,
redirects, globbing, and `$VAR` expansion do **not** happen. If you need them,
make `command` a shell explicitly:

```yaml
  piped-report:
    command: /bin/sh
    args: ["-c", "generate | tee $HOME/report.txt"]
```

## Validation

`Validate()` rejects a job when:

- `command` is empty.
- `timeout` is set but not a valid Go duration.
- `every` is set but not a valid, positive Go duration.
- both `schedule` and `every` are set.

A bad `schedule` (malformed cron) is not caught at config load; instead the
gateway logs it and skips that one job at startup — see `scheduling.md`.

## Defining jobs programmatically

You usually edit `jobs.yaml` by hand, but `jobs.define` (CLI / MCP) writes an
entry for you and preserves the others. See `running.md`.
