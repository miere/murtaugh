package agentwire

// This file is the ADVERTISEMENT: what a node tells the gateway it can serve.
//
// It rides two ways, and both are needed. The connect-time snapshot travels on
// InitializeResult, because that is the node's one structured answer at
// handshake and the gateway reads it at a point where it has no registry entry
// yet — a node-initiated frame sent at the same moment would arrive before
// there was anywhere to put it. Every LATER change travels as a node-initiated
// MethodAdvertise request, which is why the gateway does not have to poll.

// Advertisement is what one node claims: the agent profile NAMES it serves and
// the channels it asserts an assignment rule for.
//
// It is a FULL SNAPSHOT and it replaces whatever the gateway held for that
// connection, never merges with it. There is no resume across a reconnect and
// the link drops a duplicate silently, so a delta protocol would need ordering
// guarantees against the connect-time state that nothing here provides.
// Replace-the-whole-claim-set makes a duplicated or re-sent advertisement
// harmless, which is the property that lets the node re-advertise whenever it
// is unsure.
//
// Two things are deliberately NOT on it.
//
// There is no node id. internal/nodetoken's rule is that a node never asserts
// its own identity — inside a fleet, a node that announces who it is can
// announce somebody else. The gateway keys the registry from the credential it
// verified at the handshake, which is the only identity it has any reason to
// trust.
//
// There is nothing that GRANTS. A node admin owns their node's configuration,
// so anything on this type is written by somebody the gateway has not
// authorised to make gateway-level decisions. `allow_anyone` is the case in
// point: it waives the gateway's own access list for a channel's chat surface,
// and accepting it here would let any node owner open the gateway to the whole
// workspace by editing a file on their laptop. The claims below carry a match
// pattern and a profile name and nothing else, which is the same rule
// internal/toolset/partition.go states for tools: enforcement is gateway-side
// because a node cannot be trusted to filter itself.
type Advertisement struct {
	// Profiles is the agent profile names this node actually serves, sorted.
	//
	// "Actually serves" is narrower than "is configured with", and the
	// difference matters: a node's config may define six profiles while the
	// process serves one, and advertising the other five would be a claim the
	// gateway could act on and the node could not honour.
	Profiles []string `json:"profiles,omitempty"`
	// Claims is the node's ordered channel assignment rules.
	//
	// Order is carried because it is the node's own first-match-wins order, and
	// the gateway does not merge rule lists across nodes — #170 is explicit
	// that each node evaluates its OWN list and answers yes or no, so there is
	// no cross-node specificity ordering to invent and no tie-break to get
	// wrong.
	Claims []AssignmentClaim `json:"claims,omitempty"`
}

// AssignmentClaim is one channel a node says it would take.
//
// Match has the same three shapes chat.channels' `match` has — an exact Slack
// channel ID, an exact channel name, or a channel-name glob — because it IS
// that field, carried across. The gateway matches it with the same matcher it
// already applies to its own rules, so a claim cannot mean something on one
// side of the link that it does not mean on the other.
type AssignmentClaim struct {
	Match string `json:"match"`
	// Profile is which of Profiles this node would run the channel on. It is
	// informational for now: nothing on the wire can address a profile yet (see
	// InitializeResult.Advertisement), so the gateway records it and cannot act
	// on it. It is carried anyway because the node is the only side that knows
	// it, and a claim whose profile the node does not serve is dropped before it
	// is ever sent rather than filtered here.
	Profile string `json:"profile,omitempty"`
}

// Empty reports whether a node claimed nothing at all.
//
// A node with no profiles has never been configured, which #170's Change I
// makes the trigger for onboarding its owner. That is not this item's work, but
// the question has to be answerable without every caller re-deriving what
// "nothing" means.
func (a Advertisement) Empty() bool { return len(a.Profiles) == 0 && len(a.Claims) == 0 }

// Clone returns a copy that shares no slice with the original.
//
// The gateway holds an advertisement under a mutex and hands it out to
// delegation; the node holds one and rebuilds it on every configuration change.
// Neither can afford the other's slice header, and a value copy of a struct with
// two slices in it is exactly the sharing that looks safe and is not.
func (a Advertisement) Clone() Advertisement {
	out := Advertisement{}
	if a.Profiles != nil {
		out.Profiles = append([]string(nil), a.Profiles...)
	}
	if a.Claims != nil {
		out.Claims = append([]AssignmentClaim(nil), a.Claims...)
	}
	return out
}
