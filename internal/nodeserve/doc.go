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
//
// # What this node claims, and the two paths it says it on
//
// Advertiser holds the node's claim: the agent profile names it serves and the
// channels it asserts an assignment rule for. It has ToolGate's and
// BackgroundSink's shape — built before the agent, bound to whichever server is
// serving, dropping rather than queueing while unbound — but it is READ at one
// specific moment rather than only called into.
//
// The connect-time claim rides the handshake answer, and the reason is
// ordering. The gateway builds its registry entry the instant that answer
// lands, so a claim carried on it is in hand exactly when there is somewhere to
// put it; a node.advertise frame sent at the same moment races the gateway's
// own bookkeeping, and losing that race drops the claim for a window nobody
// would think to look at. Every LATER change is pushed as node.advertise, which
// is what keeps the gateway from having to poll.
//
// Dropping while unbound is right rather than a compromise, and the reason is
// the same fact from the other side: a fresh connection re-advertises the whole
// claim on its handshake, so a queued push would replay a stale claim on top of
// a current one. The VALUE is kept; only the send is dropped.
//
// What decides the claim is internal/nodeclaim, not this package. It is the
// only thing that reads a node's configuration, and it is where the rule that a
// node may not advertise a gateway access decision is applied.
package nodeserve
