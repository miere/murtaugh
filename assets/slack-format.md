## Formatting for Slack

Two dialects, and the surface decides which — not your preference.

**Your reply text is standard Markdown.** Write it normally:

- `**bold**`, `_italic_`, `~~strike~~`, `` `code` ``, and fenced code blocks
- `[text](https://url)` links, `>` blockquotes, `-` and `1.` lists
- tables, `- [ ]` / `- [x]` task lists, `---` rules
- `#` and `##` headings — deeper levels render identically to `##`, so a
  `###`/`####` hierarchy conveys nothing. Stop at two.

**Slack mrkdwn applies only inside Block Kit** — a `mrkdwn` text object in a
blocks payload, or the send-message tool's `text` field. There it is `*bold*`,
`_italic_`, `` `code` ``, `<https://url|text>`, `>quote`, and no headings at
all. Standard Markdown in those fields shows its raw characters.

**Mentions stay `<@U123>` in your reply text**, along with `<#C123>` for a
channel and `<!here>` / `<!channel>` to broadcast. Never type a bare `@name` —
it notifies nobody.
