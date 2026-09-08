// Package nodeserve is the runtime node's half of the session channel: it
// answers the six requests a gateway makes and streams one agent's events back.
//
// It is the mirror of internal/agent/remote, and like that package it neither
// dials nor authenticates — it is handed a live nodelink.Conn and an
// agent.Client. What it adds over the fake node in remote's tests is the two
// things a real node has to own: the node-side approval gate, and the
// attachment transfer.
//
// # One node, one agent, on purpose
//
// The protocol carries no agent name. A link therefore serves exactly one
// agent.Client, and a gateway that has one node attached has one agent
// available. That is a deliberate simplification for #193: there is no node
// registry (#195) and no delegation resolver (#196) yet, so a single connected
// node is the whole world and hard-coding it is cheaper than inventing an
// addressing scheme that item 9 will replace. When the registry lands, the
// agent identity belongs in the handshake, not in every frame.
//
// # What runs where, and why the read loop is guarded so carefully
//
// nodelink acknowledges a frame only once the handler RETURNS, which is the
// backpressure mechanism for the event direction and must not be spent on the
// request direction. So:
//
//   - Requests are dispatched to their own goroutine. A prompt that took the
//     read loop with it would hold up the cancel that stops it, and Initialize
//     can spawn a process. The frames concerned are one per turn and tiny; the
//     volume is all in the other direction, where the handler stays inline and
//     the pacing signal survives.
//   - Permission answers are applied on the read loop. They are a buffered
//     channel send and cannot block.
//   - Events go out through Link.Send, which parks in awaitRoom when the
//     gateway stops consuming. That is the whole point: a Slack renderer stuck
//     on a file upload eventually stops this node's backend from producing,
//     rather than growing a queue nobody bounds.
//
// # The approval gate is here rather than on the gateway
//
// The native backend — the default one — does not raise permission events at
// all: it calls an Approver synchronously inside the tool call. Move it to a
// node and that call has nothing to reach. ToolGate is what it reaches: it
// raises the request as a GateTool frame on the current turn's stream, waits
// for the gateway's answer, and returns the (allowed, note) pair the loop
// expects — the note included, because for a native tool call the note is not
// diagnostics, it IS the tool's result handed back to the model.
//
// # Murtaugh's tools are proxied per call, not tunnelled
//
// ToolProxy is the node's stand-in for the tools that live on the gateway. It
// fills a real tools.Registry with one proxy tool per tool the gateway offers,
// and each proxy's Invoke is a single tool.call round trip.
//
// The alternative — tunnelling the MCP byte stream an acp agent already speaks —
// is what #170 Concern 5 calls the hard version, and it is hard for a specific
// reason: the gateway end would be an MCP server session whose entire state IS
// the byte stream, and the SDK refuses every method but initialize and ping on a
// session that has not completed initialisation, so a reconnected pipe is dead
// rather than degraded. Proxying per call does not solve that; it removes it.
// The MCP session stays on this machine, between the agent's subprocess and this
// node's own aggregator socket, and never reconnects. There is nothing to
// replay and no MCP request id to rewrite. The link below already sequences,
// acknowledges and orders frames, so layering a second, weaker guarantee on top
// of it would have been the wrong trade twice over.
//
// It also fixes the backend a tunnel would have missed. The NATIVE backend — the
// default one — never touches the aggregator: it resolves a []tools.Tool and
// calls Invoke. A tunnel would have restored Murtaugh's tools for acp and
// claude_code and left native silently tool-less.
//
// # The proxy is filled at the handshake, before either backend latches
//
// A native agent resolves its toolset on its first Initialize and keeps that
// answer for the life of the process. An acp or claude_code agent resolves its
// aggregator's toolset when its first session is registered, one step later. So
// refresh runs inside serveInitialize, immediately BEFORE the agent is brought
// up — ahead of both — and a gateway that cannot answer fails the handshake
// rather than publishing an agent with an empty toolset. The node's redial loop
// retries a second later. This is deliberate: construct-once plus a latch plus a
// warning-only failure is exactly the shape that made #185 invisible for a whole
// process lifetime, and it is not repeated here.
//
// The aggregator's half of that ordering had to be BUILT, not merely relied on:
// it used to resolve its built-ins when the agent was constructed, which on a
// node is while this registry is still empty. See agentbuild's resolvedToolset.
//
// The consequence, stated rather than discovered: the SET of tools is frozen at
// the first handshake. A later reconnect refreshes descriptions and schemas in
// place but does not add or remove tools, because the agent would not look
// again. A gateway whose partition changes needs the node restarted.
//
// # Failing an in-flight call, and what actually stops a repeat
//
// When the connection ends, every outstanding tool call fails — none is retried,
// none is left hanging. That policy is not caution, it is forced: there is no
// idempotency information anywhere in Murtaugh's tool surface, so nobody on
// either side can say whether the gateway ran the call, ran half of it, or never
// received it.
//
// The thing that enforces it is CANCELLATION, not prose. shutdown ends every
// turn before it fails the calls, and a turn's context is a child of the
// server's, so a node's agent is already cancelled when its dropped call
// returns; it aborts rather than reading the result. The note that comes back
// says the action may or may not have taken effect and says do not retry in the
// imperative, and it is worth having — but it is the backstop, for the caller
// whose context is not a turn's and so survives the drop. See droppedNote in
// tools.go, which spells out each clause, and says which of the two is the
// guarantee.
//
// # Background events leave by their own sink, not by a turn's stream
//
// A claude_code session emits after its turn's `result`: a subagent finishing,
// an auto-continue completing minutes later. In process those go to
// agentbuild.Deps.BackgroundSink and the gateway renders the "went quiet"
// notice from them. Across a link they have no stream to ride — the turn they
// belong to is over — so they are addressed by SESSION instead, through
// agentwire.BackgroundEvent, which is the frame shape the envelope carries for
// exactly this.
//
// BackgroundSink is the node's end of that. It is bound to the serving
// connection the way ToolGate is, because the backend captures its sink when
// its process starts and the connection outlives neither. A node built without
// one drops these events at the backend, one hop before the protocol could
// carry them, and the gateway's notice never appears with no error and no log
// line on the gateway side — which is what it did before this was wired.
package nodeserve
