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
package nodeserve
