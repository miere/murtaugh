# Claude Code Authentication Management
## Overall Acceptance Criteria
- Logouts will happen - the system must accommodate for that and preemptively detect that
- Once a logout is detected and refreshing the Auth session is not possible, the user must be notified immediately.
- The user must not, by any means, get awareness of this by sending a prompt and receiving a plain text message saying login is required.
## Concerns
### 1 - Having Write Access to Its Own Files
Should the agent (ACP or ClaudeCode) have access to its own creds and configuration? From the security point of view, we need to make sure that the agent won’t go rogue and preventing a model from accessing it might be a must-have in some circunstances. 

Keeping the authentication handshake out of sight for a moment, picture this real-world example: 
- Someone is authorised to talk to Murtaugh on Slack
- This person asks Murtaugh to write new Claude credentials
- Should ~/.claude be writable, Murtaugh naively will override the credentials.

A second example would be someone asking the agent to configure new MCPs, being able to maliciously run code using the already set credentials on the system. Having a readable file is dangerous enough, let alone being writable.

### 2 - Claude And Murtaugh Should Not Compete For Credentials
Claude Code decides to refresh on its own schedule, and the rotation happens **server-side**. The moment a boxed session hits `Expiry time - 5m` and calls the token endpoint, Anthropic retires the old refresh token whether or not the client can persist the new one. Given that in sandbox mode Claude Code can't write into its own credentials file, a pure write-deny doesn't turn "agent refreshes" into "agent fails harmlessly" — it turns it into "agent destroys the credential." That is precisely today's bug, and it's already what the box does.

## Goals
- Ensure that, in agent isolated mode, we assume that workdir is the "home" for Claude resources.
- Make sure that, in sandbox mode
	- we don't grant write access to any Claude resource (`~/.claude`, `~/.claude.json`, etc)
	- Murtaugh will keep Claude Code authenticated

As result, when isolated, all Claude-related configuration will never share the same resources as the main user in the host machine. We will also enforce that in sandboxed mode authentication or configuration can't be modified from inside Claude without Murtaugh knowing it. Better than that, in the future, we could have Approval-Gated configuration changes on Claude Code, so changes on its own configurations would require direct approval from the Admin User before proceeding.
## Proposed Solution

### The idea in one paragraph

Today the sandboxed Claude Code process owns its own credential: it reads it, it decides when to refresh it, and it tries to write the new one back. The sandbox blocks that write, and because the refresh has already happened on Anthropic's side, the credential is destroyed rather than merely left alone. The proposal is to take the credential away from the agent entirely. Murtaugh holds it, Murtaugh refreshes it, and the agent is handed a placeholder string that is worth nothing to anybody. Every API call the agent makes is routed through Murtaugh, which swaps the placeholder for the real token on the way out. The agent never has a credential to lose, to leak, or to overwrite.

This is not a workaround. Claude Code has built-in support for being driven this way — it is how corporate proxies and hosted environments are expected to run it. The environment variables involved are listed below and all of them were tested against the installed version.

### Change A — Murtaugh becomes the one that holds the credential

This is the core of the proposal. Everything else is supporting work.

**How it works.** When Murtaugh starts an agent, it also starts a small local server listening on a Unix socket — a socket is just a file on disk that two programs on the same machine use to talk to each other, instead of going over the network. Murtaugh then sets two environment variables on the agent process:

- `ANTHROPIC_UNIX_SOCKET` — tells Claude Code to send its API traffic to that socket instead of out to the internet.
- `NODE_EXTRA_CA_CERTS` — tells Claude Code to trust a certificate that Murtaugh generated, so the encrypted connection to the socket succeeds.

When the agent makes a request, it arrives at Murtaugh's socket instead of at Anthropic. Murtaugh then:

1. Decrypts the request.
2. Removes the `Authorization` and `x-api-key` headers — whatever the agent put there is thrown away, so it cannot smuggle a credential of its own.
3. Adds the real token, which Murtaugh has been keeping fresh.
4. Sends the request on to the real `api.anthropic.com`.
5. Streams the reply straight back to the agent, unchanged.

Everything else in the request is passed through untouched. Murtaugh only owns one header.

**Why the agent still needs a placeholder.** Claude Code will not send a request at all if it thinks it has no credential — it stops early and prints "Not logged in". So the agent's environment still contains something that looks like a key. It is a fixed, meaningless string. Reading it, printing it, or pasting it into Slack gains an attacker nothing, because Murtaugh replaces it before the request leaves the machine. This is what answers Concern 1: there is no real credential in the agent's reach, so a Murtaugh that has been talked into leaking or overwriting its credentials has nothing to work with.

**Where the certificate keys live.** Murtaugh generates its certificate in memory each time it starts and only ever writes the public half to disk. The private half never touches the filesystem, so it cannot be read by the agent or anyone else who gets at the machine's files.

**Why this also fixes Concern 2.** Because Murtaugh is in the middle of every call, it is the one that sees a `401 Unauthorized` come back from Anthropic. It can refresh the credential — safely, because it runs outside the sandbox and can write — retry the request once, and carry on. Claude Code never discovers there was a problem, never runs a refresh of its own, and therefore never destroys the credential. The competition described in Concern 2 stops existing rather than being carefully timed around.

### Change B — Claude Code's configuration moves out of the user's home directory

Right now every agent shares the real `~/.claude` folder and the host user's macOS keychain. We want an isolated agent to touch neither.

Two environment variables do this:

- `CLAUDE_CONFIG_DIR` moves the configuration folder (settings, sessions, history, todos) somewhere else. It must be an absolute path.
- `CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST` stops Claude Code reading the macOS keychain at all. This was tested: with a perfectly valid credential sitting in the keychain, Claude Code reported "Not logged in" once this was set. That is the behaviour we want — it proves the agent is genuinely blind to the host user's credential rather than merely discouraged from using it.

**One correction to the Goals section above.** The goal says the workdir should become the "home" for Claude resources. Putting the configuration folder literally inside the workdir would work, but it would make that configuration *writable by the agent*, because the workdir is the one place the sandbox deliberately allows writes. That would make the MCP problem in Concern 1 worse, not better — an agent could add a new MCP server to its own configuration and have it loaded on the next run.

The recommendation is therefore a per-agent directory **beside** the workdir, owned by Murtaugh, which the sandbox does not grant write access to. The agent can read its configuration and cannot change it. Anything that must never be overridable — the credential rules in particular — additionally goes into a managed settings file via `CLAUDE_CODE_MANAGED_SETTINGS_PATH`. Claude Code treats managed settings as pinned: a settings file the agent can write cannot switch them off.

### Change C — the sandbox stops allowing writes to credential files

Two gaps exist today:

- `~/.claude/.credentials.json` **is currently writable**. The sandbox grants write access to all of `~/.claude` because Claude Code needs that folder for session state and history, and the credential file happens to live inside it.
- `~/Library/Keychains` is not writable, but only because nobody added it to the allow-list. Nothing states that it must stay that way, so a future change could re-open it silently and no test would fail.

The fix is to add an explicit deny-list to the sandbox configuration, mirroring the read deny-list that already exists, and to render it *after* the allow rules — the sandbox language applies the last matching rule, so ordering matters. Both paths go on it. A test then asserts that the generated sandbox profile denies writes to both, **even when the allow-list includes their parent folders**. That test is what stops this quietly regressing later.

**Important: this change must come last.** Denying the write on its own does not make the agent fail harmlessly — it makes the agent destroy the credential, which is exactly the bug described in Concern 2. It is only safe once Change A means the agent has no credential to refresh in the first place.

### Change D — the warden learns to notice, and to tell someone

The component that keeps the credential fresh has three defects that need fixing regardless of the above, and they are what the Acceptance Criteria are really about.

**It reads a clock instead of checking whether the credential works.** It looks at the expiry timestamp and nothing else. A credential that has been revoked still carries a perfectly valid future timestamp, so a revoked credential is invisible to it. It should treat the *shape* of the stored credential as the signal: if the blob cannot be parsed, or the expiry has vanished or gone backwards, the credential is broken now — not at some point in the future when the clock runs out.

**It knows and does not tell.** There is already a code path meaning "this credential is not coming back by being asked again; a re-authentication is what fixes it now." It writes one warning line to a log file and goes quiet. It should instead record a journal event and send the admin the existing re-authentication card, unprompted. There is already a card and a guard that stops duplicates being sent; the warden needs to be given access to it rather than a second one being built.

**Failures reach the user as plain text.** In the incident that prompted this document, Claude Code reported the turn as *successful* and put "Failed to authenticate. API Error: 401 OAuth access token has been revoked." in the reply text. Murtaugh only checks for credential problems on the failure path, so nothing noticed, and the message went straight to the user in Slack. Three fixes:

- Check the *reply text* of a completed turn, not just errors. A function for exactly this already exists in the codebase and is currently never called.
- Widen the list of phrases we recognise. None of the seven phrases we currently look for match the message above.
- Before a turn starts, if the credential is known to be dead, do not start it. Tell the user their turn is blocked and that the admin has been asked — wording that already exists in the code.

Once Change A is in place most of this becomes much simpler, because Murtaugh sees a plain HTTP `401` rather than having to guess at English sentences in the CLI's output. The text-matching becomes a safety net rather than the primary mechanism.

### How this meets the Acceptance Criteria

| Criterion | How it is met |
| --- | --- |
| Logouts will happen and must be detected preemptively | Murtaugh sees the `401` on the API call itself (Change A), and the warden judges the stored credential by shape rather than by clock (Change D) |
| Notify immediately when refresh is not possible | Refresh failure records a journal event and sends the admin the re-authentication card straight away, instead of writing a log line (Change D) |
| The user must never learn about it from a plain-text reply | Two gates: refuse to start a turn on a known-dead credential, and intercept the reply text of a completed turn before it reaches Slack (Change D) |

### What was tested, and what is still unknown

Tested against Claude Code 2.1.251 on the host machine, using deliberately invalid tokens throughout. The live credential was checked before and after and was unchanged.

- `CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST=1` stops keychain reads. **Confirmed.**
- `ANTHROPIC_UNIX_SOCKET` redirects API traffic to a Unix socket. **Confirmed** — the connection arrived on the socket.
- The connection is encrypted, and Claude Code accepts a certificate we supply via `NODE_EXTRA_CA_CERTS`. **Confirmed** — the handshake succeeded and the full request was readable, including the authentication header we intend to replace. There is no certificate pinning blocking this.
- `CLAUDE_CODE_HOST_CREDS_FILE` is **not** a way to supply a credential, as first assumed. It marks a file the agent is forbidden to overwrite. Useful as an extra safeguard, but not the mechanism.
- Whether an OAuth-mode request differs from the API-key-mode request we captured. **Unknown.** Our proxy strips both header styles so the swap should be safe either way, but this should be observed rather than assumed.
- Whether proxying interferes with streaming replies. **Unknown and important** — streamed output is the entire chat experience, so this needs proving early in the build rather than discovered late.

All of these environment variables are undocumented internals of a specific Claude Code version. The implementation must check for them at startup and fall back to today's behaviour if a future version removes them, rather than failing outright.

### Suggested build order

The ordering matters; doing it in a different order produces more outages, not fewer.

1. **Change D** — the detection and notification fixes. Independent of everything else, small, and it immediately stops the user finding out about outages by being told to run `/login`.
2. **Change A** — the socket proxy. Prove streaming works before building anything else on top of it.
3. **Change B** — move the configuration folder and turn off keychain access.
4. **Change C** — deny the writes. Safe only now, and from this point it is a backstop rather than the mechanism.

### Risks

- Murtaugh ends up in the path of every model call. A bug there affects every agent at once, so the proxy must stay deliberately simple and fail loudly rather than cleverly.
- Streamed responses must be passed through without being buffered, or replies will arrive in one lump at the end instead of appearing progressively.
- The certificate Murtaugh generates is trusted by the agent's whole process, not just for Anthropic's address. Keeping the private key in memory only is what keeps that from becoming a real exposure.
- This relies on undocumented behaviour that a Claude Code upgrade could change. Feature-detection and a safe fallback are required, not optional.

### Related, but out of scope here

The agent can currently **read** the host user's credentials — both the Claude credential in the keychain and Murtaugh's own `.env`, which holds the Slack and GitHub tokens. Changes A and B remove the Claude credential from its reach. The rest of the read exposure is a separate piece of work and should be tracked on its own.
