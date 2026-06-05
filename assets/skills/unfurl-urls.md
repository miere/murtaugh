# Skill: Unfurl URLs

Murtaugh can replace bare URLs posted in Slack with rich Block Kit previews. When
a message containing a matched URL is posted, Murtaugh calls your agent as a child
process and expects a Block Kit attachment on stdout.

## How it works

1. A user posts a message containing a URL.
2. Slack delivers a `link_shared` event to Murtaugh (only for domains registered in
   the Slack app's **App Unfurl Domains** list).
3. Murtaugh evaluates each `unfurl-rules` entry in sorted-key order and picks the
   first match for that URL.
4. If the matched rule uses `run`, Murtaugh spawns the configured command, writes a
   JSON context object to its stdin, and reads a Block Kit attachment JSON from its
   stdout.
5. Murtaugh calls `chat.unfurl` with the resulting attachment.

## Stdin schema

```json
{
  "url":        "https://example.com/browse/PROJ-42",
  "domain":     "example.com",
  "channel":    "C0ENG1",
  "user":       "U0ABCDEF",
  "message_ts": "1700000000.000100",
  "thread_ts":  "",
  "team_id":    "T0ABCDEF",
  "captures":   { "key": "PROJ-42" }
}
```

`captures` contains the named groups from `match.url_pattern` (empty map when no
pattern is configured). `thread_ts` is empty when the URL was not posted in a
thread.

## Stdout schema

Return a valid Block Kit attachment as JSON. The simplest useful shape:

```json
{
  "blocks": [
    {
      "type": "section",
      "text": {
        "type": "mrkdwn",
        "text": "*<https://example.com/browse/PROJ-42|PROJ-42>* — My ticket title"
      }
    }
  ]
}
```

Print nothing (or exit non-zero) to suppress the unfurl for this URL silently.

## Configuration

```yaml
unfurl-rules:
  jira-ticket:
    match:
      domain: example.com
      url_pattern: '/browse/(?P<key>[A-Z]+-\d+)'
    unfurl:
      run:
        cmd: /path/to/agent
        args: [unfurl-jira]
        timeout: 8s
```

- `match.channels` (optional list of Slack channel IDs) scopes the rule to specific
  channels or DMs; omit to match everywhere.
- Rules are evaluated in ascending sorted-key order; the first match wins.
- Each `match.domain` must appear in the Slack app's App Unfurl Domains list (max 5).

## Template alternative

If the preview is static enough to be expressed with Go `text/template`, use
`template` instead of `run`:

```yaml
    unfurl:
      template: unfurl/my-preview.json
```

The same data fields are available as template dot-variables: `.URL`, `.Domain`,
`.Channel`, `.User`, `.MessageTS`, `.ThreadTS`, `.TeamID`, `.Captures`.
