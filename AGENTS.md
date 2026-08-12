# Rules
- always work on a worktree
- worktrees can be placed in the ./ignore/worktrees
- never use "merge" branch commits - always do a rebase-merge so we keep the history linear and clean.
- never commit against the main - upstream won't accept.
- NEVER start, restart, or replace the Slack gateway while a healthy one is
  already connected — see "Never clobber a live gateway" below.

# Never clobber a live gateway

The machine you are running on is very likely the machine hosting the live
Murtaugh gateway — quite possibly the very process relaying your own replies to
Slack. Taking it down cuts your only channel to the user, so you cannot report
what you broke, and the user's first symptom is silence.

**Before** running `murtaugh slack gateway`, `install/macos/install.sh`,
`murtaugh setup launchd`, any `launchctl` verb against `dev.murtaugh`, or
`go test ./...` (the installer tests drive the real installer), check for a live
gateway:

```sh
launchctl print "gui/$(id -u)/dev.murtaugh" >/dev/null 2>&1 && echo "LIVE AGENT"
pgrep -fl 'murtaugh slack gateway'
```

If either reports something, treat the gateway as live and **stop**. Do not
start a second one: two gateways on the same bot token both receive every Slack
event and answer twice. Ask the user to stop it, or work around it — never
assume you may take it over.

To confirm the live gateway is actually healthy rather than looping, read the
journal instead of guessing:

```sh
sqlite3 ~/.config/murtaugh/config-journal.db \
  "SELECT datetime(ts/1000,'unixepoch'), level, summary FROM events
   WHERE stream='gateway' AND kind='connection' ORDER BY ts DESC LIMIT 20;"
```

A healthy gateway shows a recent `Slack socket connected`. A wall of
`connection_error` with no `connected` means it is stuck in the reconnect loop —
report that to the user rather than silently restarting it.

Scoped commands (`go build ./...`, `go test ./internal/...`, a single `-run`)
are safe and preferred. When you genuinely need the full suite, `go test
-short ./...` skips the installer tests. The installer additionally refuses to
touch `gui/$(id -u)` when `$HOME` is not the login home
(`launchd_domain_is_ours` in `install/macos/install.sh`), but that is the last
line of defence — not a licence to skip the check above.

This rule exists because it already happened: an agent ran `go test ./...`, the
macOS installer test bootstrapped its sandbox plist over the live
`dev.murtaugh`, and the daemon spent ~19 hours running from a deleted temp
binary, unable to reach Slack, with nobody able to tell the user.

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

