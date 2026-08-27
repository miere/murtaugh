package config

import (
	"fmt"
	"math/rand/v2"
	"net/url"
	"strings"
)

// AgentIcons is the palette an agent's face is drawn from. Every agent profile
// carries one so the surfaces Murtaugh renders can tell two agents apart at a
// glance instead of showing the same generic bot for all of them.
//
// The list is the curated set from the design canvas (icons8, 128px, flat
// robot/AI motifs) with its one duplicate removed. It is deliberately a fixed
// palette rather than a free-for-all: a random pick has to land on something
// that looks like it belongs to the same family as its neighbours. An operator
// who wants their own artwork still can — Icon accepts any http(s) URL — but
// nothing Murtaugh chooses on its own leaves this list.
var AgentIcons = []string{
	"https://img.icons8.com/external-flaticons-lineal-color-flat-icons/128/external-robot-robotics-icons-flaticons-lineal-color-flat-icons.png",
	"https://img.icons8.com/external-flaticons-flat-flat-icons/128/external-robotics-industry-flaticons-flat-flat-icons.png",
	"https://img.icons8.com/external-flaticons-lineal-color-flat-icons/128/external-artificial-intelligence-industry-flaticons-lineal-color-flat-icons-3.png",
	"https://img.icons8.com/external-flaticons-lineal-color-flat-icons/128/external-machine-data-analytics-flaticons-lineal-color-flat-icons.png",
	"https://img.icons8.com/external-flaticons-flat-flat-icons/128/external-turret-robotics-flaticons-flat-flat-icons.png",
	"https://img.icons8.com/external-flaticons-lineal-color-flat-icons/128/external-artificial-intelligence-big-data-flaticons-lineal-color-flat-icons.png",
	"https://img.icons8.com/external-flaticons-flat-flat-icons/128/external-cyborg-robotics-flaticons-flat-flat-icons.png",
	"https://img.icons8.com/external-flaticons-flat-flat-icons/128/external-robot-bioengineering-flaticons-flat-flat-icons.png",
}

// PickAgentIcon returns a random icon from AgentIcons. It is the only place an
// icon is invented: assignment happens once, at the seam that can persist the
// result, so an agent keeps the same face for its lifetime rather than
// reshuffling on every render.
func PickAgentIcon() string {
	return AgentIcons[rand.IntN(len(AgentIcons))]
}

// validateAgentIcon rejects an icon that is not an absolute http(s) URL. Empty
// is allowed and means "not assigned yet" — the startup backfill fills it in.
// The palette itself is not enforced: a custom icon is a legitimate choice, and
// rejecting one would turn a cosmetic preference into a startup failure.
func validateAgentIcon(field, value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	u, err := url.Parse(value)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("%s must be an http(s) URL (got %q)", field, value)
	}
	return nil
}
