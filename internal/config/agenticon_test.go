package config

import (
	"strings"
	"testing"
)

// iconConfig builds a minimal valid config whose only variable is one agent's
// icon.
func iconConfig(icon string) Config {
	return Config{
		OAuth:  OAuthConfig{AppToken: "xapp-test", BotToken: "xoxb-test"},
		Agents: map[string]AgentProfile{"coding": {Icon: icon, ACP: &ACPProfile{Command: "/bin/agent"}}},
	}
}

func TestValidateAgentIcon(t *testing.T) {
	valid := []string{
		"",
		"  ",
		AgentIcons[0],
		"http://example.com/robot.png",
		"https://example.com/robot.png?size=64",
	}
	for _, icon := range valid {
		if err := iconConfig(icon).Validate(); err != nil {
			t.Errorf("icon %q should be valid: %v", icon, err)
		}
	}

	invalid := []string{
		"robot.png",                   // a bare filename, the likely typo
		"/icons/robot.png",            // a local path Slack cannot fetch
		"ftp://example.com/robot.png", // a scheme Slack will not load
		"https://",                    // no host
	}
	for _, icon := range invalid {
		err := iconConfig(icon).Validate()
		if err == nil || !strings.Contains(err.Error(), "agents[coding].icon") {
			t.Errorf("icon %q should be rejected, got: %v", icon, err)
		}
	}
}

// TestPickAgentIconStaysInThePalette: the random pick never invents a URL, and
// over enough draws it does actually spread across the palette rather than
// pinning one entry.
func TestPickAgentIconStaysInThePalette(t *testing.T) {
	if len(AgentIcons) < 2 {
		t.Fatalf("palette is too small to be a palette: %d", len(AgentIcons))
	}
	seen := map[string]bool{}
	for range 200 {
		icon := PickAgentIcon()
		if !contains(AgentIcons, icon) {
			t.Fatalf("picked an icon outside the palette: %q", icon)
		}
		seen[icon] = true
	}
	if len(seen) < 2 {
		t.Errorf("200 picks landed on %d distinct icons; the pick is not random", len(seen))
	}
}

// TestAgentIconsHasNoDuplicates: a duplicated entry silently doubles that
// icon's odds — the canvas list shipped with one, and it was dropped on the way
// in.
func TestAgentIconsHasNoDuplicates(t *testing.T) {
	seen := map[string]bool{}
	for _, icon := range AgentIcons {
		if seen[icon] {
			t.Errorf("duplicate icon in the palette: %q", icon)
		}
		seen[icon] = true
	}
}
