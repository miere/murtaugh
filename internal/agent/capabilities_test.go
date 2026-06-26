package agent

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestParseAgentCapabilities(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want AgentCapabilities
	}{
		{
			name: "full",
			in:   `{"protocolVersion":1,"agentCapabilities":{"loadSession":true,"mcpCapabilities":{"http":true,"sse":true}}}`,
			want: AgentCapabilities{ProtocolVersion: 1, MCP: MCPCapabilities{HTTP: true, SSE: true}, LoadSession: true},
		},
		{
			name: "stdio only (no mcpCapabilities)",
			in:   `{"protocolVersion":1,"agentCapabilities":{}}`,
			want: AgentCapabilities{ProtocolVersion: 1},
		},
		{
			name: "empty result",
			in:   ``,
			want: AgentCapabilities{},
		},
		{
			name: "garbage decodes to zero",
			in:   `not json`,
			want: AgentCapabilities{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseAgentCapabilities([]byte(tc.in))
			if got != tc.want {
				t.Fatalf("parseAgentCapabilities(%q) = %+v, want %+v", tc.in, got, tc.want)
			}
		})
	}
}

func TestInitializeCapturesCapabilities(t *testing.T) {
	client := NewProcessClient(ProcessOptions{Command: os.Args[0], Args: []string{"-test.run", "TestACPHelperProcess", "--", "acp-helper"}})
	t.Cleanup(func() { _ = client.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Initialize(ctx); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	got := client.Capabilities()
	want := AgentCapabilities{ProtocolVersion: 1, MCP: MCPCapabilities{HTTP: true, SSE: false}, LoadSession: true}
	if got != want {
		t.Fatalf("Capabilities() = %+v, want %+v", got, want)
	}
}
