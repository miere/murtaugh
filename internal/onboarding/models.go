package onboarding

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// This file asks each backend which models it will actually serve.
//
// Asking beats listing. A hard-coded catalogue is wrong the moment a provider
// ships something new or the operator's key lacks access to something old, and
// the form would then offer a choice that fails at the first message. The
// provider already knows the answer for this specific credential; every backend
// here is asked with the key the operator just typed, so what comes back is
// what they can actually use.
//
// Claude Code is the exception and does not appear here: it publishes no
// machine-readable list, so its choices are the aliases in ClaudeCodeModels.

// discoveryTimeout bounds one probe. The operator is sitting in a modal waiting
// for it, so this is a "did it answer" budget, not a "wait for a slow endpoint"
// one.
const discoveryTimeout = 15 * time.Second

// maxDiscoveredModels caps the list. Slack's static_select allows 100 options,
// and some OpenAI-compatible endpoints return several hundred.
const maxDiscoveredModels = 100

// Doer is the HTTP seam, so tests can answer without a network.
type Doer interface {
	Do(req *http.Request) (*http.Response, error)
}

// DiscoverModels returns the models the draft's backend will serve for the
// credential it carries.
func DiscoverModels(ctx context.Context, client Doer, d Draft) ([]string, error) {
	if d.Kind == KindClaudeCode {
		return ClaudeCodeModels, nil
	}
	provider, ok := ProviderFor(d.Kind)
	if !ok {
		return nil, fmt.Errorf("unknown agent type %q", d.Kind)
	}
	if provider.NeedsKey && strings.TrimSpace(d.Key) == "" {
		return nil, fmt.Errorf("an API key is required to list %s models", provider.Label)
	}
	if client == nil {
		client = &http.Client{}
	}

	probeCtx, cancel := context.WithTimeout(ctx, discoveryTimeout)
	defer cancel()

	req, err := modelsRequest(probeCtx, d)
	if err != nil {
		return nil, err
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not reach %s: %w", provider.Label, err)
	}
	defer res.Body.Close()

	// Bounded read: a misconfigured base URL can point at anything, and this
	// runs on a credential the operator has just handed us.
	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("could not read the %s model list: %w", provider.Label, err)
	}
	if res.StatusCode != http.StatusOK {
		// The status is what distinguishes "wrong key" from "wrong URL", and
		// the operator is about to have to tell them apart.
		return nil, fmt.Errorf("%s returned %s while listing models: %s",
			provider.Label, res.Status, summarise(body))
	}

	models, err := parseModels(d.Kind, body)
	if err != nil {
		return nil, err
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("%s returned no models for this key", provider.Label)
	}
	return models, nil
}

// modelsRequest builds the provider's list call.
func modelsRequest(ctx context.Context, d Draft) (*http.Request, error) {
	base := strings.TrimRight(strings.TrimSpace(d.BaseURL), "/")

	switch d.Kind {
	case KindGemini:
		// Gemini authenticates the list call with a query parameter rather than
		// a header, and has one endpoint, so BaseURL is not offered for it.
		url := "https://generativelanguage.googleapis.com/v1beta/models?key=" + strings.TrimSpace(d.Key)
		return http.NewRequestWithContext(ctx, http.MethodGet, url, nil)

	case KindAnthropic:
		if base == "" {
			base = "https://api.anthropic.com"
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/models", nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("x-api-key", strings.TrimSpace(d.Key))
		// Required by the Anthropic API; omitting it is a 400, not a default.
		req.Header.Set("anthropic-version", "2023-06-01")
		return req, nil

	case KindOpenAI:
		if base == "" {
			base = "https://api.openai.com/v1"
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/models", nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(d.Key))
		return req, nil

	default:
		return nil, fmt.Errorf("cannot list models for agent type %q", d.Kind)
	}
}

// parseModels pulls the ids out of a provider's response.
func parseModels(kind Kind, body []byte) ([]string, error) {
	var names []string

	switch kind {
	case KindGemini:
		var payload struct {
			Models []struct {
				Name string `json:"name"`
				// Gemini lists embedding and tuning endpoints alongside chat
				// models; only the ones that can generate content are usable
				// here, and offering the rest guarantees a broken agent.
				SupportedGenerationMethods []string `json:"supportedGenerationMethods"`
			} `json:"models"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			return nil, fmt.Errorf("could not parse the Gemini model list: %w", err)
		}
		for _, m := range payload.Models {
			if !supportsGeneration(m.SupportedGenerationMethods) {
				continue
			}
			names = append(names, strings.TrimPrefix(m.Name, "models/"))
		}

	case KindAnthropic, KindOpenAI:
		// Both use the OpenAI-shaped {"data":[{"id":...}]} envelope.
		var payload struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			return nil, fmt.Errorf("could not parse the model list: %w", err)
		}
		for _, m := range payload.Data {
			names = append(names, m.ID)
		}

	default:
		return nil, fmt.Errorf("cannot parse models for agent type %q", kind)
	}

	names = dedupe(names)
	sort.Strings(names)
	if len(names) > maxDiscoveredModels {
		names = names[:maxDiscoveredModels]
	}
	return names, nil
}

// supportsGeneration reports whether a Gemini model can answer a prompt. An
// empty list is treated as usable: a provider that stops sending the field
// should degrade to offering too much rather than nothing.
func supportsGeneration(methods []string) bool {
	if len(methods) == 0 {
		return true
	}
	for _, m := range methods {
		if m == "generateContent" || m == "streamGenerateContent" {
			return true
		}
	}
	return false
}

func dedupe(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := in[:0]
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// summarise trims a provider's error body to something that fits in a modal.
func summarise(body []byte) string {
	s := strings.TrimSpace(string(body))
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 200 {
		return s[:200] + "…"
	}
	if s == "" {
		return "(no response body)"
	}
	return s
}
