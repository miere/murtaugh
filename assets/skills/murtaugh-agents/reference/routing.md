# Chat routing: which agent answers

Routing is **backend-agnostic** — the same rules pick a native or an ACP agent.
The `chat` block in `config.yaml` decides which agent handles each conversation
and whether the reply is threaded:

```yaml
chat:
  defaults:
    agent: default              # required when chat.enabled
    dm_agent: support           # optional: override for DMs
    reply_on_thread: true       # optional: global reply strategy (default true)
  channels:                     # optional: ORDERED per-channel overrides
    - match: C0ENG1             #   by exact channel ID
      agent: coding
    - match: support            #   by exact channel name
      agent: support
    - match: "feature-*"        #   by channel-name glob
      agent: coding
      reply_on_thread: false    #   reply directly in the channel, not a thread
```

`channels` is a **list**, and the **first matching rule wins**. Order is the
precedence — put narrow rules above broad ones.

## Resolution order

For an incoming prompt, Murtaugh picks the agent like this:

- **DM** → `chat.defaults.dm_agent` if set, otherwise `chat.defaults.agent`.
- **Channel @-mention** → the **first** `chat.channels` rule matching that channel
  (by ID, name, or name glob — see below); its `agent`, or `chat.defaults.agent`
  when the rule omits one.
- **`/murtaugh chat …`** → same rules, based on where the command was run.

## Reply strategy: thread vs channel

`reply_on_thread` controls where the bot posts a reply to a **top-level** channel
message:

- **`true`** (the default) — the bot roots a thread on the triggering message,
  as it always has. Each thread is its own conversation/session.
- **`false`** — the bot replies **directly in the channel**. The channel is then
  treated as one long rolling conversation (a single shared session), not a fresh
  session per message. Reset it with `/clear`.

Effective value = the matched channel's `reply_on_thread` → `chat.defaults.reply_on_thread`
→ `true`. A message that is **already in a thread** always gets a threaded reply,
regardless of the flag. DMs are always threaded.

## Match forms: ID, name, or glob

A `chat.channels` rule's `match` — and a `chat.no_mention.by_channel` key, which
uses the same syntax — can be any of three forms:

1. **Exact channel ID** (`C…`/`G…`) — always works. Grab it from the channel's
   "About" panel or a message link.
2. **Exact channel name** — e.g. `support` (no leading `#`).
3. **Channel-name glob** — e.g. `feature-*` (`path.Match` syntax).

There is **no specificity scoring**: `matchChannel` walks the list top to bottom
and takes the first rule that matches. `feature-api-*` beats `feature-*` only
when it is listed first. (`chat.no_mention.by_channel` is still a map and still
unions across every matching key — it is a waiver list, not a routing rule.)

> Name and glob matches require Murtaugh to resolve the channel's **name**. It
> reads an in-memory cache built via `conversations.list`, and on a miss resolves
> the name read-through via `conversations.info` and memoizes it — so a
> brand-new channel matches on its **first** message, not its second. Only a
> genuine API failure (missing scope, an erroring endpoint) leaves the name
> unresolved, and routing then falls back to **exact-ID-only**. Channel **IDs**
> always work regardless — use them if name resolution is unavailable.

## Validation (fail-closed)

When `chat.enabled: true`, the gateway refuses to start unless:

- `chat.defaults.agent` is **set** and names an agent defined in `agents.yaml`.
- `chat.defaults.dm_agent`, if set, names a known agent.
- every `chat.channels[].agent` that is set names a known agent.
- every rule has a non-blank `match`, and no two rules share one (past the first,
  a duplicate could never be reached).
- at least one agent is defined in `agents.yaml`.

So a typo in an agent name is caught at startup, not at first message.

## Worked example

```yaml
# agents.yaml has: default, coding, support
chat:
  defaults:
    agent: default
    dm_agent: support            # DMs go to the support agent
  channels:
    - match: C0ENG1
      agent: coding              # by ID: this channel's mentions go to coding
    - match: "support-*"
      agent: support             # by glob: any support-* channel goes to support
      reply_on_thread: false     # …replying in-channel as one rolling thread
# → a mention in any other channel falls back to `default` (threaded)
```

## Opening a channel to everyone

A rule may set `allow_anyone: true` to waive `access.allowed_users` for that
channel, so any workspace user who can post there may talk to the routed agent:

```yaml
  channels:
    - match: "nc-secrets"
      agent: admin               # listed first, so it stays allowlist-only…
    - match: "nc-*"
      agent: coder               # …while its siblings are open to everyone
      allow_anyone: true
    - match: "mt-*"
      agent: admin
```

Authority follows one ladder everywhere: `admin_user` → `allowed_users` → the
matched rule's `allow_anyone`. A guest admitted by the third rung may talk to the
routed agent in that channel **and answer the prompts it raises** (`ask`
questions and tool-approval buttons) — a user who can ask the agent to act can
answer what it asks back.

It stops there. The waiver does not lift the `@mention` requirement, and never
reaches slash commands, DMs, restart, the App Home controls, or workflow rules
(whose configured actions are not bounded by the channel agent's toolset).

So what an opened channel's agent can be talked into doing is bounded by its own
`tools`, `mcp_servers`, and `approval` policy — set those deliberately.
