package config

// This file is the configuration SPLIT: which half of #170's gateway/runtime
// divide a given configuration belongs to, and therefore which of its rules
// apply to it.
//
// # Why a role rather than two types
//
// #170's Change I gives the two halves separate configuration files, not
// separate schemas. A node holds agent profile bodies, MCP servers, its own
// assignment rules and its own store; a gateway holds Slack credentials, access,
// election timings and the pins. But the sections OVERLAP — `chat.channels` is
// simultaneously the gateway's routing table and the node's assignment rules
// (internal/nodeclaim derives the advertisement from it), and `defaults` is read
// by both — so two structs would have to keep a shared middle in sync by hand.
//
// One struct with a role is the smaller change and it puts the difference where
// it actually bites: in Validate.
//
// # The behavioural change this introduces, stated once
//
// Today the default agent NAME is validated against a profile BODY at write
// time — `murtaugh cfg …` refuses a chat.defaults.agent that names no agent, and
// the daemon refuses to start on one. Once bodies live on nodes the gateway
// cannot do that: it does not hold the body, and the node that does may be
// asleep. So for RoleGateway those name→body checks are DEFERRED to connect
// time, where internal/nodehost resolves them against the profiles the attaching
// user's fleet advertises.
//
// The consequence is worth stating plainly because an operator will meet it:
// "is my configuration valid" now depends partly on who is online. A gateway
// whose chat.defaults.agent names a profile only Alice's laptop serves is valid
// while Alice's laptop is attached and unserviceable while it is not — and the
// second state is reported when a node attaches, not at startup, because a
// gateway that refused to boot with an empty registry could never boot at all.
//
// RoleCombined keeps every check exactly where it is today, which is what makes
// the in-process path — still the shipping default — unaffected by any of this.

// Role names which half of the split a configuration belongs to.
//
// The zero value is RoleCombined deliberately: every existing caller, every
// test, and `murtaugh slack gateway` itself get today's behaviour without
// naming a role, and only the two new binaries opt into a half.
type Role string

const (
	// RoleCombined is one process holding both halves: Slack credentials AND
	// agent profile bodies. It is what `murtaugh slack gateway` is, and it
	// remains the shipping default.
	RoleCombined Role = ""
	// RoleGateway is the broker half: Slack, access, election, pins. It holds no
	// agent profile bodies, so it cannot check an agent NAME against one.
	RoleGateway Role = "gateway"
	// RoleNode is the runtime half: agent profile bodies, MCP servers, its own
	// assignment rules and the gateway seed address. It has no Slack connection
	// at all, so requiring Slack credentials of it would make a node
	// unstartable without credentials it must never hold.
	RoleNode Role = "node"
)

// HoldsSlackCredentials reports whether this role talks to Slack, and therefore
// whether oauth.app_token / oauth.bot_token are required.
//
// A node has no Slack connection — it reaches Slack only through the gateway it
// dials, and #170's table puts the tokens firmly in the gateway's column. A node
// that had to carry them would be a node whose owner had been handed the
// workspace's bot token to run an agent on their laptop.
func (r Role) HoldsSlackCredentials() bool { return r != RoleNode }

// HoldsAgentProfiles reports whether this role holds agent profile BODIES, and
// therefore whether a name that references one can be resolved locally.
//
// False for RoleGateway is the whole behavioural change: see the file comment.
func (r Role) HoldsAgentProfiles() bool { return r != RoleGateway }

// String makes a role printable in an error or a log line. RoleCombined prints
// as "combined" rather than as the empty string it is stored as.
//
// The empty string is the point: RoleCombined is the zero value so that every
// existing caller gets today's behaviour without naming a role, and that same
// choice would put a hole in the middle of any message that printed it raw.
// `cfg db migrate`'s refusal is the caller — the same store is valid for a
// broker gateway and invalid for a combined install, so the half that judged it
// is the part an operator cannot work out for themselves.
func (r Role) String() string {
	if r == RoleCombined {
		return "combined"
	}
	return string(r)
}
