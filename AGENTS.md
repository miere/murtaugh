# Rules
- always work on a worktree
- worktrees can be placed in the ./ignore/worktrees
- never use "merge" branch commits - always do a rebase-merge so we keep the history linear and clean.
- never commit against the main - upstream won't accept.

# Validated core
- A hard-precondition value (one a downstream tool cannot function without) is
  resolved AND validated exactly once, at the build seam where its inputs first
  co-exist — not re-derived with ad-hoc empty-checks at each use site.
- The agent workspace is the canonical example: it is resolved once in
  `agentbuild.Resolve` (profile workdir → workspace dir) and flows downstream as a
  constructed `*files.Root` (a `ResolvedAgent`), never as raw `profile.WorkDir` + a
  base-dir fallback. Downstream packages must not read `profile.WorkDir` or
  re-apply the fallback; the `internal/archtest` `go/analysis` pass (run in CI via
  `cmd/archcheck`) enforces this.
- When you add a workdir-rooted or native-only tool group, classify it once in
  `toolset.NativeGroups`; the exhaustiveness test keeps both consumers (the
  resolver switch and the ACP strip) in sync.
- A precondition that fails for ONE tool degrades that tool, not the whole agent:
  drop the tool, keep the agent and the rest of its toolset alive, and record a
  structured problem (agent + tool + reason) on the `startup.routing` summary so it
  is visible in logs and the troubleshoot bundle. Reserve a fatal error for states
  where no client can be built at all.

# Backend parity (ACP == native UX)
- The two agent backends (`internal/agent/native` and the ACP `ProcessClient`)
  implement ONE `agent.Client` interface and feed ONE `agent.Event` stream into the
  shared Slack relay (`internal/slack/gateway` ChatHandler + chatRenderer). The
  user-visible experience MUST be the same regardless of backend — treat this as a
  hard constraint, not a nice-to-have.
- Everything the user sees (reply text, the task list, tool activity, approval
  prompts, attachments, interrupts) flows as an `agent.Event` so the renderer
  orders and renders it once, for both backends. Do NOT add a backend-specific side
  channel that posts to Slack directly: it bypasses the renderer, races the stream,
  and diverges the two UXs. Permission prompts were the cautionary case — an ACP
  side channel posted approval cards while the reply was still streaming (truncated
  look); they now ride `EventPermission` on the same stream, mirroring how the
  native loop gates a tool call inline.
- When a backend exposes a structured concept the other already renders (e.g. ACP's
  `plan` update vs native's per-tool task events), translate it into the existing
  `agent.Event` shape rather than dropping it or leaking it into the reply prose.
- New surfaces are added to the `chatRenderer` interface (implemented by BOTH the
  woven and section renderers) and emitted by BOTH backends — never wired for one
  backend only.

