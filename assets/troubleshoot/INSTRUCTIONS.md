# Murtaugh troubleshooting bundle — how to investigate

You are analysing a **Murtaugh** diagnostics bundle. Murtaugh is a Go daemon that
bridges Slack to AI coding agents over **ACP** (Agent Client Protocol). It talks
to an agent process (commonly **Goose**), which in turn talks to an LLM provider
(Google/Gemini, Anthropic, OpenAI, …). Murtaugh sits **above** the ACP boundary:
it never makes the provider's HTTP call itself.

Start with `manifest.json` (versions, OS, the user's symptom, the file list,
redaction status, and any non-fatal collection errors). Read the user's
`symptom` first — it scopes everything.

## What's in the bundle

| Path | What it is |
|---|---|
| `manifest.json` | provenance + table of contents + the reported symptom |
| `journal/journal.db` | Murtaugh's SQLite event journal (consistent snapshot) — **the primary evidence** |
| `journal/transcripts/*.ndjson` | per-session ACP turn transcripts (prompt/response/outcome) |
| `config/*.yaml` | slack/agents/jobs/journal config (secrets **redacted**) |
| `logs/slack.{out,err}.log` | daemon stdout/stderr (tail-truncated if large) |
| `providers/<name>/…` | optional downstream agent artifacts (e.g. Goose sessions DB + logs) |

## Reading the journal (the workhorse)

`journal.db` has one table, `events`:
`ts` (epoch ms), `stream` (`gateway`/`job`/`acp_session`), `kind`, `level`,
`corr_id`, `channel_id`, `thread_ts`, `user_id`, `session_id`, `job_name`,
`rule_id`, `summary`, `payload` (JSON), `blob_ref`.

High-signal queries:
- Per-turn outcome: `kind='session.turn'` rows. The `payload` JSON carries
  `bytes`, `chunks`, `duration_ms`, `outcome`, `stop_reason`. **A turn with
  `bytes:0, chunks:0, stop_reason:"end_turn"` is the canonical "silent reply"
  symptom** — the agent ended the turn producing no text.
- Errors: `SELECT * FROM events WHERE level IN('error','warn') ORDER BY ts`.
- Group a conversation by `session_id`; correlate across streams with `corr_id`.

## Known failure shapes (check these first)

1. **Silent / empty replies** — `session.turn` rows with `0 bytes / 0 chunks /
   end_turn`. Murtaugh forwards faithfully; the empty text originates upstream
   (the agent ended the turn with no message, or the provider returned an empty
   completion). To confirm origin you need provider-side logs (see below) — do
   **not** assume Murtaugh dropped text. Cross-check the daemon log for
   `"ACP turn completed with no agent text"`.
2. **Tool-call rejected / "could not be parsed"** — look in the **provider/agent
   logs** for a function-name or schema rejection (e.g. an invalid character in
   a tool name). This is an agent/provider-format issue surfaced through ACP.
3. **Workflow-rule failures** — `stream='gateway'`, `kind` around interactive
   workflows; `payload.error` names the cause (template function not defined,
   `run` command exit status, delegate timeout). `run` trigger args are **not**
   templated — `{{…}}` placeholders in a `run` bash script are a config bug.
4. **Scheduled-job failures** — `stream='job'`; `exited with code -1` means the
   child was killed by a signal (often an external script crash), not a Murtaugh
   timeout — check `duration_ms` against the job's configured `timeout`.

## Correlating Murtaugh with a provider (e.g. Goose)

When `providers/goose/` is present:
- `providers/goose/sessions/sessions.db` (SQLite): `sessions` and `messages`
  tables. **Murtaugh's `session_id` matches Goose's session `id`** — join on it.
  A turn's last `assistant` message that is only a `toolRequest` (no `text`
  content) confirms the agent ended without a final message.
- `providers/goose/logs/llm_request.*.jsonl`: each file is one provider call —
  `input` (system + messages sent) and `data` (streamed response). A response
  with **zero content lines and `output_tokens: null`** is a genuine **empty
  completion** from the provider (token arithmetic: `total_tokens ==
  input_tokens` means zero output generated). That attributes a silent turn to
  the provider, not Murtaugh.
- The bundle does **not** include the provider's raw HTTP `finishReason` —
  Murtaugh can't see below ACP. If you need it, it's an upstream agent concern.

## Important caveats

- **Redaction is partial.** It removes Slack tokens and obvious secret YAML
  values. It does **not** scrub secrets inside transcripts, binary `.db` files,
  or arbitrary strings. Treat the bundle as sensitive; see
  `manifest.redaction_limitations`.
- Logs may be **tail-truncated** (`manifest.files[].truncated`); you are seeing
  the most recent window only.
- `manifest.errors` lists artifacts that couldn't be collected (e.g. a provider
  whose files weren't found, or logs on a non-macOS host) — absence of a file
  is not evidence of absence of a problem.

## Output

Produce: (1) the most likely root cause with the specific evidence (file +
row/line) behind it; (2) whether it is Murtaugh's fault, the agent/provider's,
or user config; (3) the concrete fix. Distinguish what the evidence *proves*
from what it merely *suggests*.
