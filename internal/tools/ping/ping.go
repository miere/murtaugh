// Package ping implements Murtaugh's health-check tool. It returns the
// fixed string "pong" with no error, and serves as the canonical example of
// the tool contract under internal/tools.
package ping

import (
	"context"

	"github.com/google/jsonschema-go/jsonschema"
)

// Response is the logical result the ping tool returns.
const Response = "pong"

// Tool is the ping capability.
type Tool struct{}

// New constructs a new ping Tool.
func New() *Tool { return &Tool{} }

// Name returns the tool's identifier.
func (t *Tool) Name() string { return "ping" }

// Description returns a short, human-readable description of the tool.
func (t *Tool) Description() string { return "Health check — returns pong." }

// InputSchema returns nil — ping takes no parameters.
func (t *Tool) InputSchema() *jsonschema.Schema { return nil }

// Invoke returns Response with no error.
func (t *Tool) Invoke(_ context.Context, _ map[string]any) (any, error) {
	return Response, nil
}
