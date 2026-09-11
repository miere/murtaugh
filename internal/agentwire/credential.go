package agentwire

import "time"

// CredentialHealth names no owner and no node: the gateway reads both off the
// connection, so a node can only ever report on its own credentials.
type CredentialHealth struct {
	Credential string    `json:"credential"`
	Degraded   bool      `json:"degraded,omitempty"`
	Reason     string    `json:"reason,omitempty"`
	Since      time.Time `json:"since,omitzero"`
	ExpiresAt  time.Time `json:"expires_at,omitzero"`
}

// CredentialRenewal says what the node did, so nobody is told a sign-in is on
// its way when the node already had one open or had nothing to sign in.
type CredentialRenewal struct {
	Status string `json:"status"`
}
