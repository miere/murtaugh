// Package agentwire is the serialisable form of the agent event abstraction —
// the protocol a runtime node speaks to the gateway.
//
// # Derive, do not equal
//
// Every type here is declared afresh rather than aliased from internal/agent,
// and Encoder/Decoder are the only places the two vocabularies meet. If the wire
// type WERE agent.Event with tags added, every internal refactor would become a
// wire-compatibility event and the internal type would stop being free to
// change. The cost is one translation; the benefit is that renaming a field on
// agent.Event is a rename, not a protocol break.
//
// The protocol is ours. Both ends are this binary, from this repository, so
// conformance to a third-party agent protocol buys interoperability that will
// never be exercised and charges for it by constraining a boundary we own.
//
// # Where it sits
//
// A top-level sibling of internal/agent, alongside internal/agentbuild and
// internal/agentdelegate: everything UNDER internal/agent is a backend
// implementing agent.Client (native, acp, claudecode) or backend support, and a
// codec is neither. The placement is also load-bearing for the gateway/runtime
// split: the gateway must become incapable of running an agent, which is a rule
// about importing agent BACKENDS, and the gateway must be able to import this
// package. Keeping it out of internal/agent/ keeps that rule a clean prefix
// instead of a rule with an exception on its first day.
//
// It imports internal/agent and internal/llm and nothing else of ours. It
// imports no backend: an error's backend-specific structure reaches it through
// a small interface declared here (RPCFaulter) that the backend satisfies
// structurally, the same way internal/agent declares Aggregator rather than
// importing the implementation above it.
//
// # Three fields needed decisions, and they are recorded in the code
//
// Seven event kinds carry over directly. Three fields do not:
//
//   - Event.Error is an interface compared by IDENTITY at the gateway.
//     Serialisation preserves an error's text and destroys its identity, and
//     nothing fails loudly when it does. See Error and ErrorKind.
//   - PermissionPrompt.Decision is a live channel. A channel is a request
//     awaiting a response modelled as a one-way event; on a wire it needs a
//     correlation identifier and a response frame. See PermissionRequest and
//     PermissionResponse.
//   - AttachmentEvent.Path is a path on the producing host, which means nothing
//     on the consuming one. See Attachment and Transfer.
//
// The rule these imply, and the one to apply to anything added later: anything
// on this seam that is a pointer, a channel, a file handle or an identity
// comparison is a protocol design question, not a plumbing detail.
//
// # What this package is not
//
// It is not a transport. There is no connection, no framing, no sequencing and
// no acknowledgement here: those belong in the ENVELOPE that wraps these
// payloads, so delivery and meaning stay separable. roundtrip_test.go is the
// proof that the meaning survives, and it exists before any transport does.
package agentwire
