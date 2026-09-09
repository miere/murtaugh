package agentwire

import "encoding/json"

// This file is the ONE method that carries configuration, and the constraints on
// it are the interesting part.
//
// # Why it exists at all
//
// #170 Change I makes a node connecting with zero profiles the trigger for the
// existing Slack onboarding, run against the node's owner. The form is
// necessarily gateway-side — the node has no Slack — but the profiles it
// produces belong in the NODE's store, because that is where profile bodies live
// after this item. Without a method that crosses, the operator would fill in a
// form in Slack and then be told to go and type the answers into a terminal on
// the machine they were configuring, which is not onboarding.
//
// # Why it is not "the gateway may write a node's configuration"
//
// #170 is explicit that node admins own their node. This method does not change
// that, and the guarantee is enforced on the NODE, not asserted here: the node
// applies a configuration only while it has no agent profile of its own. It can
// therefore bootstrap an empty node exactly once and can never overwrite,
// reconfigure or reach into a node that is already running. A gateway admin who
// wanted to change somebody's node still has to ask them.
//
// # Why the bodies are opaque JSON
//
// This package is the wire and must not depend on internal/config: a protocol
// that imported the configuration schema would make every configuration refactor
// a wire-compatibility event, which is exactly what Change A's two-types rule
// exists to prevent. The bodies are the same JSON the config store already holds
// per row, so the node decodes them with its own version of the schema — the one
// that will actually have to load them.

// NodeConfiguration is the configuration a gateway hands a node that has never
// been configured.
//
// Every field is optional. A configuration that carries nothing is not an error:
// it is a form that produced nothing to write, and the node answers that it
// applied nothing.
type NodeConfiguration struct {
	// Agents is agent profile bodies keyed by profile name, each the JSON of one
	// config store row.
	Agents map[string]json.RawMessage `json:"agents,omitempty"`
	// Chat is the node's chat block: its default agent and its assignment rules.
	//
	// It crosses because the node needs both — it picks which profile it serves
	// from the default, and internal/nodeclaim derives the advertisement the
	// gateway matches from the channel rules. #170's table calls the same field
	// the gateway's routing and the node's assignment; this is the node's half.
	Chat json.RawMessage `json:"chat,omitempty"`
	// Env is provider credentials to write into the node's own .env, keyed by
	// variable name. A profile references its key by variable name, so a profile
	// whose key is not on the node's disk builds into an agent that cannot
	// reach its model.
	//
	// These are the node OWNER's credentials, typed by them into their own
	// onboarding form. The gateway relays and does not retain them.
	Env map[string]string `json:"env,omitempty"`
	// There is deliberately no work-dir field here. Exactly one profile arrives
	// without a work_dir — the owner's `tweaker`, rooted wherever the
	// configuration lives so it can edit it — and the gateway cannot fill it in:
	// it is a directory on somebody else's machine. A field carrying the empty
	// answer would state a contract it cannot hold, because `omitempty` drops an
	// explicit "" from the wire and leaves it indistinguishable from a producer
	// that never set it. The node substitutes its own configuration directory,
	// which is the only answer anybody could give.
}

// Empty reports a configuration with nothing to apply.
func (c NodeConfiguration) Empty() bool {
	return len(c.Agents) == 0 && len(c.Chat) == 0 && len(c.Env) == 0
}

// NodeConfigured is the node's answer: what it did with the configuration.
//
// It reports rather than merely acknowledging because the gateway tells a human
// the outcome, and "saved 2 profiles" and "refused: this node is already
// configured" are two different things to say.
type NodeConfigured struct {
	// Applied is how many agent profiles were written.
	Applied int `json:"applied"`
	// Restarting is whether the node is stopping so its supervisor can bring it
	// back up serving what it was just given.
	//
	// It restarts because both agent backend families latch: a native agent
	// resolves its toolset at its first Initialize and an acp/claude_code
	// agent's aggregator resolves it when its first session is registered, so a
	// process that was built with no agent cannot grow one. Saying so lets the
	// gateway tell the operator to expect a reconnect instead of leaving them
	// watching a node that went quiet.
	Restarting bool `json:"restarting,omitempty"`
}
