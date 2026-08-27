// Package pingcard is the single source of truth for Murtaugh's built-in
// communication self-test: the "Test communication" button and the reply it
// produces.
//
// It lives in its own package, owned entirely by the binary. The button's
// identity is a Go constant and the click is handled directly by the gateway —
// never routed through the configurable workflow engine or on-disk templates.
// That keeps the ping → pong round-trip stable no matter what agents or
// operators do to config and template files, which is the whole point of moving
// it here.
//
// The button used to ride on the startup and back-online messages, which is why
// this package still owns a BlockID. It now lives in the App Home control row
// next to Restart (internal/slack/gateway/app_home.go), so the self-test is
// reachable at any time rather than only from whichever lifecycle message
// happened to be the most recent one in the DM. The lifecycle messages
// themselves are ordinary info alert cards.
package pingcard

const (
	// BlockID tagged the section that used to carry the Test-communication
	// button on the startup and back-online cards. The gateway router still
	// keys on it so a card posted by an older process stays clickable; new
	// renderings are recognised by ActionPing alone.
	BlockID = "murtaugh_ping"
	// ActionPing fires the pong self-test reply (handled in the gateway).
	// It is namespaced so a user-defined workflow rule can never collide with
	// or shadow the built-in self-test.
	ActionPing = "murtaugh_ping_test"

	// ButtonLabel is the button's copy, wherever it is rendered.
	ButtonLabel = "Test communication"

	// PongTitle and PongSubtitle are the reply posted when the button is
	// clicked, split into the headline and the one-line summary of the info
	// card that carries it.
	PongTitle    = "Communication check"
	PongSubtitle = "The server communication is functional."
)
