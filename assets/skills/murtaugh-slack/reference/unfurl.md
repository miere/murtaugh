# Unfurl-rules: rich link previews

Murtaugh can replace a bare URL posted in Slack with a **rich Block Kit preview**.
When a message containing a matched link is posted, Slack sends a `link_shared`
event; Murtaugh matches it against an `unfurl-rules` entry and renders a preview
from a **template**, a **command** you run, or an **agent** you delegate to. This
is operator config, stored in the config database and applied with
`cfg unfurl-rule set --name <n> --from-file <rule.yaml>` (view/manage with
`cfg unfurl-rule list|show|delete`). It's a sibling of `workflow-rules.md` — same
shape: inbound Slack event → rule match → action. Each `cfg` change re-validates
the whole config and rolls back an invalid rule; **restart the gateway** to apply.

> **Prerequisite — register the domain.** Slack only delivers `link_shared` for
> domains listed in your Slack app's **App Unfurl Domains** (max 5). If a domain
> isn't registered there, no event arrives and nothing here will fire. This is
> enforced by Slack, not Murtaugh.

## The flow

1. A user posts a message containing a URL on a registered domain.
2. Slack delivers a `link_shared` event to Murtaugh over Socket Mode.
3. Murtaugh evaluates each `unfurl-rules` entry in **sorted-key order** and takes
   the **first** rule that matches the URL.
4. The rule's action renders a Block Kit attachment — a `template`, a `run`
   command, or a `delegate-to-agent`.
5. Murtaugh posts all previews for the message in one `chat.unfurl` call.

## Configuring the rule

Each rule's `--from-file` YAML holds the rule **body** (the `match` + `unfurl`
fields below); `--name` is the rule's key:

```yaml
# github-pr.yaml  →  cfg unfurl-rule set --name github-pr --from-file github-pr.yaml
match:
  domain: github.com
  url_pattern: '^https://github\.com/(?P<owner>[^/]+)/(?P<repo>[^/]+)/pull/(?P<number>\d+)'
unfurl:
  template: templates/unfurl/github-pr.json
```

### `match` — which links a rule claims

| Field | Meaning |
|---|---|
| `domain` | Matches the link's domain **exactly or as a suffix** (so `github.com` also matches `gist.github.com`), case-insensitive. |
| `url_prefix` | The full URL must start with this exact string. |
| `url_pattern` | A Go regexp the full URL must match. **Named capture groups** (`(?P<name>…)`) are extracted into `Captures` for the action. |
| `channels` | Optional list of Slack channel IDs; the rule only applies in those channels. Omit to match every channel. |

**At least one** of `domain`, `url_prefix`, or `url_pattern` is required
(validation rejects a rule with none). You can combine them — all present
conditions must hold.

### Matching order

- Rules are evaluated in **sorted key order** (alphabetical by rule name); the
  **first** match wins. Name a specific rule to sort ahead of a catch-all.
- Per rule, the checks run: channel → domain → url_prefix → url_pattern. Any
  failing check moves on to the next rule.

### Behavior & limits

- **Up to 10 unique links** per `link_shared` event are processed; duplicates
  within the event are de-duplicated.
- **Composition previews are skipped** — Slack sends a non-numeric (UUID)
  timestamp while a user is still typing; Murtaugh ignores those and only acts on
  posted messages.
- Each `run` command is bounded by its `timeout` (default **30s**); the whole
  unfurl handling for a message is bounded at 2 minutes.
- A per-link failure is logged and that link is skipped — it never blocks the
  other links in the same message.

### Validation rules

- `match` requires at least one of `domain` / `url_prefix` / `url_pattern`.
- `url_pattern`, if set, must compile as a Go regexp.
- each `channels` entry must be non-blank.
- `unfurl` requires exactly one of `template`, `run`, or `delegate-to-agent`; a
  `run` requires `cmd`; a `delegate-to-agent` requires both `agent` (defined —
  `cfg agent list`) and `prompt`.

## The three actions

All three produce the same thing — a **Slack Block Kit attachment** as JSON,
posted via `chat.unfurl`. They differ only in how that JSON is produced. Exactly
one is set per rule.

| Mode | Field | Use when |
|---|---|---|
| **template** | `template:` | the preview is a pure function of the URL — fill a JSON template with URL parts |
| **run** | `run:` | the preview needs a lookup (call an API, read a DB) before rendering |
| **delegate-to-agent** | `delegate-to-agent:` | producing the preview needs an agent's judgement or tool use (output must be attachment JSON) |

### Shared data

All three actions receive the same fields about the shared link:

| Field | Meaning |
|---|---|
| `URL` | the full shared link |
| `Domain` | the link's domain |
| `Channel` | Slack channel ID where it was posted |
| `User` | Slack user ID who posted it |
| `MessageTS` | the message timestamp |
| `ThreadTS` | thread timestamp (empty if not in a thread) |
| `TeamID` | Slack workspace/team ID |
| `Captures` | map of **named** regex captures from `match.url_pattern` (empty if none) |

### `template` — Go text/template → JSON

The template path resolves against the **workspace** first
(`<workspace>/templates/...`), then falls back to the embedded defaults at the
same path. Variables are the fields above, dot-prefixed; captures are
`.Captures.<name>`:

```json
{
  "blocks": [
    {
      "type": "section",
      "text": {
        "type": "mrkdwn",
        "text": "*<{{ .URL }}|Pull Request #{{ .Captures.number }}>*\n`{{ .Captures.owner }}/{{ .Captures.repo }}`"
      }
    }
  ]
}
```

- Rendering uses `missingkey=error`: referencing a capture that didn't match is
  an error (the preview is skipped), so guard optional captures in your regex.
- The rendered output must be valid JSON that decodes to a Slack attachment.

### `run` — command: JSON in → attachment JSON out

Murtaugh runs `cmd` (with `args`, in `workdir`, bounded by `timeout`) and:

- **Writes to stdin** — the shared-data object above as JSON.
- **Reads from stdout** — one valid Block Kit attachment JSON object.
- **Print nothing, or exit non-zero → the link is silently skipped** (no preview,
  no error to the channel). This is the intended way to say "no preview for this
  one". stdout is trimmed and must be valid JSON decoding to a `slack.Attachment`;
  malformed output is treated as a skip-with-log.

```yaml
    unfurl:
      run:
        cmd: /path/to/agent
        args: [unfurl-jira]
        timeout: 8s        # Go duration; default 30s
        workdir: /some/dir # optional
```

### `delegate-to-agent` — agent renders the attachment JSON

Hands the preview to an agent (keyed in the config — `cfg agent list`) running in
an isolated one-shot session. The prompt is rendered with the same shared-data fields as a
template (`{{ .URL }}`, `{{ .Captures.<name> }}`, …), using `missingkey=error`.

```yaml
    unfurl:
      delegate-to-agent:
        agent: default
        prompt: |
          Summarise GitHub issue {{ .URL }} and return me solely a valid Slack
          attachment JSON object containing the summary in one paragraph.
```

- The agent's **final output must be a single valid JSON** Slack attachment.
- **Non-JSON output → the link is skipped** and a warning (with the raw output)
  is logged. Same skip-with-log behaviour as `run`.
- The agent may use its own tools/MCP to gather context (call an API, read a repo)
  before producing the JSON — richer than a `run` command, but slower and
  non-deterministic.
