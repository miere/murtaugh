# Journal event kinds

Every event has an envelope — `time`, `stream`, `kind`, `level`
(debug/info/warn/error), `corr_id`, correlation `keys`
(team/channel/thread/user/session/job/rule), a one-line `summary`, and a
`kind`-specific `payload`. Filter on the envelope; read the `payload` for detail.

## `gateway` stream

Recorded while handling a Slack interaction. All events from one interaction
share a `corr_id` (minted at ingress as `gw_…`).

| kind | level | meaning / payload |
|------|-------|-------------------|
| `slash.command` | info | A slash command was accepted. payload: `command`, `text`. |
| `interactive.received` | info | A block-action / view-submission arrived. payload: `interaction_type`, `callback_id`. |
| `workflow.matched` | info | A workflow rule matched the interaction. keys: `rule_id`. payload: `interaction_type`. |
| `workflow.no_match` | debug | No rule matched. payload: `interaction_type`, `callback_id`, `action_ids`. The usual cause of "nothing happened". |
| `workflow.trigger` | info / warn / error | One trigger ran. payload: `trigger` (`reply-to-slack`\|`run`\|`delegate-to-agent`). **error** carries `error`; **warn** (`json_valid:false`) means a delegate-to-agent reply produced non-JSON and was skipped. |
| `unfurl.no_match` | debug | A shared link matched no unfurl rule. payload: `url`, `domain`. |
| `unfurl.render` | info / error | Built (or failed to build) a link preview. keys: `rule_id`. payload: `url`, `error` on failure. |
| `unfurl.post` | info / error | The `chat.unfurl` call. payload: `count`, `error` on failure. |
| `connection` | info / warn | Socket Mode connection-health transition from the connection watchdog. payload always carries `state`: `connecting` (info — a fresh connect attempt is starting), `reconnecting` (warn — the previous attempt ended and the supervisor is backing off; adds `lasted_ms`, `backoff_ms`, and `error` when the attempt failed), `stalled` (warn — no inbound socket event within the silence window, so the link is recycled; adds `silent_ms`), `heartbeat_failed` (warn — the `auth.test` heartbeat failed repeatedly, so the link is recycled; adds `consecutive_failures`). Unlike the interaction events above, these come from the socket supervisor (not a Slack interaction) and don't share an interaction `corr_id`. |

### Why the daemon went silent

If the bot stopped responding and the logs are quiet, scan the `connection`
events: `murtaugh journal query --stream gateway --kind connection --since 24h`.
A healthy daemon shows the occasional `connecting` → `reconnecting` pair; a
`stalled` or repeated `heartbeat_failed` marks where a half-open or unreachable
socket was force-recycled (the watchdog that ended the days-long "zombie" hang).

### Reading a failed interaction

A typical failing approve-button story, pulled with `--corr-id`:

1. `interactive.received` — the click arrived.
2. `workflow.matched` (`rule_id: code-review-approval`) — a rule claimed it.
3. `workflow.trigger` **error** — payload `error` says why (e.g. a template
   referenced a field the payload didn't have, since templates use
   `missingkey=error`).

If you see `interactive.received` but **no** `workflow.matched`, look for
`workflow.no_match` — the rule's `match` didn't fit the payload (wrong
`action_id`, `block_id`, or channel).

## `job` stream

| kind | level | meaning / payload |
|------|-------|-------------------|
| `job.run` | info / error | A `jobs_run` invocation. keys: `job_name`. payload: `command` or `agent`, `duration_ms`, and `exit_code` (command jobs). A non-zero exit is recorded at **error** level; a process that failed to start carries `error`. |

## `acp_session` stream

Persistent ACP chat session logs.

| kind | level | meaning / payload |
|------|-------|-------------------|
| `session.turn` | info / warn / error | One completed chat turn. keys: `session_id` + channel/thread/user. payload: `agent`, `source`, `outcome` (`completed`/`interrupted`/`timed_out`/`errored`), `stop_reason` (the agent's reported reason, e.g. `end_turn`/`max_tokens`/`refusal`), `duration_ms`, `chunks`, `bytes`. Level follows the outcome (timeout → warn, error → error). **The full prompt/response text is not in the row** — it lives in the transcript file at `blob_ref` (NDJSON under the journal `blob_dir`). |

When a turn shows `bytes: 0` (an **empty reply**), check `stop_reason`: a value other than `end_turn` (e.g. `max_tokens`, `refusal`) means the agent ended without producing text — Murtaugh surfaces this to the user as a note rather than silence. A `stop_reason` of `end_turn` with `bytes: 0` means the agent ran only tools and sent no message. Enable `configuration.debug: true` to also log every raw `session/update` kind, which reveals if the agent streamed text under an envelope Murtaugh didn't recognise.

The in-turn interaction flows — a `terminal` approval gate, the `ask`/`present_plan` prompts, a held job's first-run confirmation — emit **no journal events** of their own, and a denied/timed-out approval does **not** add a turn outcome: it's a skip-with-note, so the turn still ends `completed`. There is no `outcome` value or event kind to look for here; to see whether the agent was waiting on a human, look in the Slack thread, not the journal.

Reviewing transcripts (as opposed to debugging gateway interactions) has its own
skill: `murtaugh-acp-sessions`.
