// Package remote is agent.Client over a link to a runtime node.
//
// # Where it sits, and why that is the whole design
//
// It implements agent.Client, so it is substituted UNDER the existing
// *agent.SessionManager — exactly where agentbuild.Client's return value goes
// today — rather than in place of the manager.
//
// That placement is not cosmetic. Six optional capability surfaces are
// type-asserted for on this path, and only two of them are asserted on the
// CLIENT: sessionCloser (CloseSession) and cancelCapabilityProber
// (SupportsCancel), both in internal/agent/session_manager.go. The other four
// are asserted on the MANAGER, from the gateway package — ChatSessionWarmer and
// an anonymous Discard(agent.ConversationKey) in chat_handler.go, an anonymous
// Interruptible() bool and io.Closer in gateway.go. Put the remote client at
// agent.Client and *SessionManager keeps answering all four, unchanged. Put it
// at ChatSessionManager instead and all six would have to be re-satisfied,
// three of them by surfaces whose absence is a SILENT no-op: a manager that
// does not implement Discard disarms the idle-timeout drop, the tool-ceiling
// drop and the credential-rejection drop at once, and logs nothing.
//
// Both client-side surfaces are answered rather than degraded, and one of them
// could not have been degraded:
//
//   - CloseSession tears down a real per-conversation OS process on two of the
//     three backends (acp and claude_code). A remote client that let it fall
//     through to the manager's no-op would leak one node-side process per
//     evicted conversation, without bound. It is also called while
//     SessionManager.mu is HELD (Discard, and evictLocked inside session()) and
//     has neither a context nor an error return, so a synchronous round trip
//     there would stall every conversation on the agent behind one network
//     call. It is therefore enqueued and sent by another goroutine.
//   - SupportsCancel is answered from the initialize response rather than by a
//     probe of its own. A node that does not report it degrades to
//     interruptible — the same verdict the session manager reaches from an
//     unresolved probe — and says so in a warning, because the alternative
//     (a plain bool defaulting to false) would quietly disable interrupting an
//     in-flight turn.
//
// # The liveness contract this must not break
//
// Two independent consumers do `cancel(); for range events {}` and block until
// the channel closes — gateway/chat_handler.go on the idle path and
// agentdelegate/delegate.go on its own. Abandoning that channel stalls event
// delivery for every other conversation, which is why they drain rather than
// walk away. So a prompt's channel is closed promptly when the caller's context
// is cancelled, when the turn ends, and when the link dies. That is the single
// most load-bearing behaviour in this package, and there is a test for each of
// the three.
//
// Cancelling the caller's context also SENDS a cancel to the node. In process
// the context reaches the backend directly; across a link it reaches nothing,
// so a turn whose consumer walked away would keep running on the node forever.
//
// # What this package does not do
//
// It does not dial, listen or authenticate: it is handed a nodelink.Conn. It
// registers no backend kind, and nothing selects it — a remote client is built
// by the stage that owns the session channel. And it does not drive an
// attachment's side transfer: a deliverer that pulls chunks from this same link
// must not be called from inside the read loop, which is the transfer's design
// constraint and belongs with the transfer.
package remote
