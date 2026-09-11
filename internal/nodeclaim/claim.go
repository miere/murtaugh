package nodeclaim

import (
	"slices"
	"strings"

	"github.com/miere/murtaugh/internal/agentwire"
	"github.com/miere/murtaugh/internal/config"
)

// Only profiles this process serves are advertised: the gateway would route to the others and get
// an error. The output is sorted so an unchanged configuration never looks like a change.
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
		profile := strings.TrimSpace(rule.Agent)
		if profile == "" {
			profile = fallback
		}
		if !served[profile] {
			continue
		}
		claims = append(claims, agentwire.AssignmentClaim{Match: match, Profile: profile})
	}
	return agentwire.Advertisement{Profiles: profiles, Claims: claims}
}
