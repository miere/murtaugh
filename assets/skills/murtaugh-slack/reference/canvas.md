# Canvas: read & edit a Slack canvas

Read or edit a **Slack canvas** document through Murtaugh — one tool,
`slack_canvas`, on the CLI (`murtaugh slack canvas …`) and over MCP
(`slack_canvas`), backed by the gateway's bot token. **Reads and writes both use
standard Markdown**, so you work in one syntax in both directions.

> **Standard Markdown, not Slack `mrkdwn`.** Canvas content is CommonMark/GFM —
> `# heading`, `**bold**`, `[text](url)`, `- item`, `- [ ] task`, `| a | b |`
> tables. Do **not** use Slack's message `mrkdwn` (`*bold*`, `<url|text>`); it
> won't render in a canvas.

## Actions

| `action` | Does | Needs |
|---|---|---|
| `read` | return the whole canvas as Markdown | `canvas_id` |
| `edit_page` | append (default) or prepend Markdown to the page | `canvas_id`, `markdown` |
| `edit_section` | replace / insert around / delete the section matching some text | `canvas_id`, `section_contains` (+ `markdown` unless deleting) |

## Arguments

| Arg | Required | Meaning |
|---|---|---|
| `action` | yes | `read` \| `edit_page` \| `edit_section`. |
| `canvas_id` | yes | The canvas file id (`F…`). See "Getting the canvas id" below. |
| `markdown` | for edits | Standard Markdown content. Required for `edit_page`, and for `edit_section` unless `operation: delete`. |
| `operation` | no | `edit_page`: `append` (default) or `prepend`. `edit_section`: `replace` (default), `insert_after`, `insert_before`, or `delete`. |
| `section_contains` | for `edit_section` | Text identifying the section to edit; the first matching section is used. |

## Getting the canvas id

When someone **@-mentions the bot from inside a canvas**, the turn arrives with a
`<canvas-context>` note in your context that names the `canvas_id` and includes
the section text you were tagged on — use that id directly. To act on any other
canvas, you need its `F…` id (e.g. from a shared canvas URL/reference).

## Editing patterns

- **Append a section** — `edit_page` with `operation: append` (the default) and
  your Markdown. `prepend` puts it at the top.
- **Rewrite a specific section** — `edit_section` with `section_contains` naming a
  distinctive phrase from that section and `operation: replace`. Use
  `insert_after` / `insert_before` to add around it, `delete` to remove it (no
  `markdown` needed).
- Full-page *replace* is not a single operation (canvas edits are section-scoped):
  append/prepend for whole-page additions; target sections for surgical edits.

## Caveats when reading

- **Multi-column sections flatten.** Markdown has no column primitive, so a
  side-by-side column layout reads back as consecutive stacked paragraphs
  (content preserved, layout lost) — and there is no Markdown way to *create* a
  column layout on write.
- `read` returns the entire page; there is no section-scoped read (locate the text
  yourself in the returned Markdown).

> Canvas access is an operator concern: the Slack app needs the `canvases:read` /
> `canvases:write` scopes (and `files:read` for reads). If a call fails with a
> missing-scope error, that's the fix — say so and stop.
