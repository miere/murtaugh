package troubleshoot

import "testing"

func TestRedactText(t *testing.T) {
	cases := []struct {
		name        string
		in          string
		wantChanged bool
		mustContain []string
		mustNotHave []string
	}{
		{
			name:        "slack bot and app tokens scrubbed by value",
			in:          "oauth:\n  app_token: xapp-1-A0B-123-deadbeefcafe\n  bot_token: xoxb-111-222-secretvalue\n",
			wantChanged: true,
			mustNotHave: []string{"xapp-1-A0B-123-deadbeefcafe", "xoxb-111-222-secretvalue"},
			mustContain: []string{"app_token:", "bot_token:", redactedToken},
		},
		{
			name:        "secret-looking env value scrubbed by key",
			in:          "env:\n  ANTHROPIC_API_KEY: sk-ant-abc123xyz\n  HOME: /Users/x\n",
			wantChanged: true,
			mustNotHave: []string{"sk-ant-abc123xyz"},
			mustContain: []string{"ANTHROPIC_API_KEY:", "/Users/x", redactedToken},
		},
		{
			name:        "non-secret content untouched",
			in:          "name: github-review-queue\nevery: 1m\nworkdir: /tmp\n",
			wantChanged: false,
			mustContain: []string{"github-review-queue", "every: 1m"},
		},
		{
			name:        "bare token in a non-secret position still scrubbed",
			in:          `args: ["--token", "xoxb-9-9-loosetoken"]`,
			wantChanged: true,
			mustNotHave: []string{"xoxb-9-9-loosetoken"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, changed := redactText([]byte(c.in))
			if changed != c.wantChanged {
				t.Fatalf("changed = %v, want %v (out=%q)", changed, c.wantChanged, out)
			}
			for _, s := range c.mustContain {
				if !contains(out, s) {
					t.Errorf("output missing %q\n%s", s, out)
				}
			}
			for _, s := range c.mustNotHave {
				if contains(out, s) {
					t.Errorf("output still contains secret %q\n%s", s, out)
				}
			}
		})
	}
}

func contains(haystack []byte, needle string) bool {
	return len(needle) == 0 || indexOf(string(haystack), needle) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
