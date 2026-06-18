package llm

import "testing"

// TestBuildRequest_CacheRetentionGatedByFamily is the regression for the live
// finding that litellm's Gemini provider REJECTS the cache_retention extra
// (Anthropic/OpenAI accept it; Gemini caches a static prefix implicitly).
func TestBuildRequest_CacheRetentionGatedByFamily(t *testing.T) {
	cases := []struct {
		family    Family
		wantExtra bool
	}{
		{FamilyAnthropic, true},
		{FamilyOpenAI, true},
		{FamilyGemini, false},
	}
	for _, tc := range cases {
		p, err := newLiteLLMProvider(tc.family, "m", "", "x", nil)
		if err != nil {
			t.Fatalf("%s: newLiteLLMProvider: %v", tc.family, err)
		}
		lreq, err := p.buildRequest(Request{
			CacheRetention: "5m",
			Messages:       []Message{{Role: RoleUser, Text: "hi"}},
		})
		if err != nil {
			t.Fatalf("%s: buildRequest: %v", tc.family, err)
		}
		if _, has := lreq.Extra["cache_retention"]; has != tc.wantExtra {
			t.Errorf("%s: cache_retention extra present=%v, want %v", tc.family, has, tc.wantExtra)
		}
	}
}

func TestBuildRequest_NoCacheRetentionWhenEmpty(t *testing.T) {
	p, err := newLiteLLMProvider(FamilyAnthropic, "m", "", "x", nil)
	if err != nil {
		t.Fatalf("newLiteLLMProvider: %v", err)
	}
	lreq, err := p.buildRequest(Request{Messages: []Message{{Role: RoleUser, Text: "hi"}}})
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	if _, has := lreq.Extra["cache_retention"]; has {
		t.Error("cache_retention must be unset when CacheRetention is empty")
	}
}
