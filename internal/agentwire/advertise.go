package agentwire

import (
	"path"
	"strings"
)

// This file is the ADVERTISEMENT: what a node tells the gateway it can serve.
//
// It rides two ways, and both are needed. The connect-time snapshot travels on
// InitializeResult, because that is the node's one structured answer at
// handshake and the gateway reads it at a point where it has no registry entry
// yet — a node-initiated frame sent at the same moment would arrive before
// there was anywhere to put it. Every LATER change travels as a node-initiated
// MethodAdvertise request, which is why the gateway does not have to poll.

// Advertisement is written by a node admin, so it carries no node id and grants
// nothing: a node cannot be trusted to decide either for the gateway.
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

// ClaimFor evaluates this node's OWN rule list against a channel and answers
// with the first claim that matches.
//
// First match wins, walked in the order the node's admin wrote it — the same
// positional precedence chat.channels has, because these ARE those rules
// carried across. #170 is explicit that each node evaluates its own list and
// answers yes or no, and that the gateway never merges lists across nodes, so
// there is no cross-node specificity ordering to invent here and no tie-break
// to get wrong. Which claim won is returned rather than a bare bool because it
// names the profile the node would run, which is worth logging even while
// nothing on the wire can address a profile.
//
// channelName may be empty when the gateway has not resolved it; only an exact
// channel-id claim can match in that case.
func (a Advertisement) ClaimFor(channelID, channelName string) (AssignmentClaim, bool) {
	for _, claim := range a.Claims {
		if claim.Matches(channelID, channelName) {
			return claim, true
		}
	}
	return AssignmentClaim{}, false
}

// Matches reports whether this claim covers a channel.
//
// The three shapes are chat.channels' three, deliberately: an exact Slack
// channel ID, an exact channel NAME, or a `*` glob over the name. A claim would
// otherwise mean something on the node that wrote it that it does not mean on
// the gateway that reads it, and the failure of that would be silent — a
// channel matching on one side and not the other looks exactly like a node
// nobody claimed.
func (c AssignmentClaim) Matches(channelID, channelName string) bool {
	match := strings.TrimSpace(c.Match)
	if match == "" {
		return false
	}
	if channelID != "" && match == channelID {
		return true
	}
	if channelName == "" {
		return false
	}
	if !strings.ContainsRune(match, '*') {
		return match == channelName
	}
	matched, err := path.Match(match, channelName)
	return err == nil && matched
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
