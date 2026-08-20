# Formatting: the two dialects

Slack parses **two different markup languages**, and which one applies is decided
by the field the text lands in — never by preference. Putting one dialect in the
other's field is silent: nothing errors, the characters just show up raw or the
emphasis quietly lands on the wrong words.

| Where your text goes | Dialect |
|---|---|
| Your reply to the user (streamed or posted) | **standard Markdown** |
| A `markdown` block's `text` | **standard Markdown** |
| A `mrkdwn` text object inside a blocks payload | **Slack mrkdwn** |
| `send-msg`'s `text` field | **Slack mrkdwn** |
| A canvas document | **standard Markdown** |

## Standard Markdown — your replies

Confirmed working end to end:

| Syntax | Notes |
|---|---|
| `**bold**` `_italic_` `~~strike~~` | `*single asterisks*` is *italic* here, not bold |
| `` `code` `` and fenced blocks | |
| `[text](https://url)` | the angle-bracket `<url\|text>` form does NOT work |
| `- item`, `1. item`, `> quote` | |
| `- [ ]` / `- [x]` task lists | render as real checkboxes |
| tables | render as real tables |
| `---` | renders as a horizontal rule |
| `#` heading | large and bold |
| `##` … `######` | **all identical** — bold, body size |

Because every heading below `#` renders the same, a `##` → `###` → `####`
hierarchy communicates nothing to the reader. Use `#` for a title and `##` for
sections, and stop there.

## Slack mrkdwn — Block Kit and `send-msg`

| Syntax | Notes |
|---|---|
| `*bold*` `_italic_` `~strike~` | one delimiter, not two |
| `` `code` `` and ```` ``` ```` blocks | |
| `<https://url\|text>` | `[text](url)` shows its brackets |
| `> quote`, `- item` | |
| headings | none — `#` shows as a literal `#` |

## Mentions

Write `<@U123>` in your reply text. `<#C123>` links a channel; `<!here>` and
`<!channel>` broadcast. A bare `@name` is plain text and notifies nobody.

**A `markdown` block cannot resolve mentions** — it renders `<@U123>` literally.
Murtaugh works around this on the buffered reply path: the reference is rewritten
to readable text (`@Miere`) and re-emitted as a trailing `rich_text` block that
does notify. See `internal/slack/replyblock`.

The trade-off is that relocated mentions lose their place in the sentence: "I
asked `<@U1>` to review and `<@U2>` to deploy" produces a footer of "@ann @bob"
with no indication of who does what. When *who* matters, name people in the prose
as well as tagging them.

If you are hand-building blocks and need an inline, notifying mention, use a
`rich_text` block with a `user` element rather than a `markdown` block:

```json
{ "type": "rich_text", "elements": [
  { "type": "rich_text_section", "elements": [
    { "type": "text", "text": "over to " },
    { "type": "user", "user_id": "U0B20G0ET9T" }
  ]}
]}
```

Note `user` — `canvas_user_mention` is the canvas element and does not render in
a message.

## Why both exist

Murtaugh's two reply transports use different Slack APIs. Streaming sends a
`markdown_text` chunk to `chat.appendStream`; the buffered fallback (used where a
surface cannot host a stream, notably a canvas) posts via `chat.postMessage`,
whose `text` field is mrkdwn. The buffered path now wraps replies in a `markdown`
block so both speak standard Markdown — but `send-msg` and Block Kit text objects
still take mrkdwn, which is why the split survives.
