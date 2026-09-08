package nodeclaim

import (
	"slices"
	"strings"

	"github.com/miere/murtaugh/internal/agentwire"
	"github.com/miere/murtaugh/internal/config"
)

// Advertise derives what a node claims from its configuration and the profile
// names the process actually serves.
//
// serving is passed in rather than read off the config because only the node's
// own wiring knows it: cmd/murtaugh-runtime resolves one agent at startup, and
// the configuration it resolved that agent OUT OF may name several. See the
// package doc for why advertising the others would be a lie the gateway acts
// on.
//
// The result is a full snapshot and it replaces whatever the gateway held. It
// is deterministic — profiles sorted, claims in the node's own configured order
// — because an advertisement that reordered itself between two identical
// configurations would look like a change and be journalled as one.
func Advertise(cfg config.Config, serving []string) agentwire.Advertisement {
	profiles := make([]string, 0, len(serving))
	served := make(map[string]bool, len(serving))
	for _, name := range serving {
		name = strings.TrimSpace(name)
		if name == "" || served[name] {
			continue
		}
		served[name] = true
		profiles = append(profiles, name)
	}
	slices.Sort(profiles)

	fallback := strings.TrimSpace(cfg.Chat.Defaults.Agent)
	var claims []agentwire.AssignmentClaim
	for _, rule := range cfg.Chat.Channels {
		match := strings.TrimSpace(rule.Match)
		if match == "" {
			continue
		}
		// The rule's own agent, then the node's default — the same fallback
		// chain the matcher applies, resolved HERE so the gateway is never
		// handed a claim whose profile it would have to guess at.
		profile := strings.TrimSpace(rule.Agent)
		if profile == "" {
			profile = fallback
		}
		if !served[profile] {
			// A rule routing to a profile this process does not serve is a
			// claim the node could not honour. Dropping it here rather than
			// sending it is the difference between a conversation that
			// round-robins to a node that can take it and one that is delegated
			// to a machine which answers with an error.
			continue
		}
		// Order is preserved: it is the node's own first-match-wins order, and
		// the gateway never merges rule lists across nodes.
		claims = append(claims, agentwire.AssignmentClaim{Match: match, Profile: profile})
	}
	// Deliberately absent: AllowAnyone and ReplyOnThread. Both are gateway
	// decisions written by a node admin. See the package doc.
	return agentwire.Advertisement{Profiles: profiles, Claims: claims}
}
