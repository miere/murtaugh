package agentwire

import (
	"path"
	"strings"
)

// Advertisement is written by a node admin, so it carries no node id and grants
// nothing: a node cannot be trusted to decide either for the gateway.
type Advertisement struct {
	// Profiles lists only what the process serves, not everything configured: the gateway acts
	// on a claim, and the node cannot honour a profile it does not run.
	Profiles []string `json:"profiles,omitempty"`
	// Claims keeps the node's first-match-wins order; the gateway never merges lists across
	// nodes, so this order is the only precedence there is.
	Claims []AssignmentClaim `json:"claims,omitempty"`
}

type AssignmentClaim struct {
	Match string `json:"match"`
	// Profile is informational for now: nothing on the wire can address a profile yet.
	Profile string `json:"profile,omitempty"`
}

func (a Advertisement) ClaimFor(channelID, channelName string) (AssignmentClaim, bool) {
	for _, claim := range a.Claims {
		if claim.Matches(channelID, channelName) {
			return claim, true
		}
	}
	return AssignmentClaim{}, false
}

// Matches follows chat.channels' rules exactly: a claim that matched differently on the node
// and the gateway would look just like a channel nobody claimed.
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

func (a Advertisement) Empty() bool { return len(a.Profiles) == 0 && len(a.Claims) == 0 }

// Clone exists because a value copy shares both slices, and the gateway and the node each
// hand out or rebuild theirs on other goroutines.
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
