package acp

import "context"

// Agent is the boundary between Slack command handling and an ACP-compatible
// local or remote agent implementation.
type Agent interface {
	Invoke(context.Context, Request) (Response, error)
}

// Request is the normalized input Murtaugh will pass to ACP agents.
type Request struct {
	Command string
	Text    string
	UserID  string
	TeamID  string
}

// Response is the normalized output ACP agents can return to Slack handlers.
type Response struct {
	Text string
}
