package agentbuild

import (
	"context"
	"slices"
	"testing"

	"github.com/miere/murtaugh/internal/agent"
)

// TestTurnDecoratorCarriesBothFacts. Every bridged tool call runs inside the
// daemon, so the only way it can act AS the agent is if the agent's facts ride
// the context: the thread to answer in, and the environment to spawn with.
func TestTurnDecoratorCarriesBothFacts(t *testing.T) {
	env := []string{"CLOUDSDK_CONFIG=/srv/work/.gcloud"}
	decorate := turnDecorator(agent.SessionMetadata{ChannelID: "C1", ThreadTS: "1.1"}, env)
	if decorate == nil {
		t.Fatal("no decorator for a session that has both a location and an environment")
	}

	ctx := decorate(context.Background())
	loc, ok := agent.TurnLocationFromContext(ctx)
	if !ok || loc.ChannelID != "C1" || loc.ThreadTS != "1.1" {
		t.Errorf("location = %+v (ok=%v), want C1/1.1", loc, ok)
	}
	if got := agent.TurnEnvFromContext(ctx); !slices.Equal(got, env) {
		t.Errorf("env = %v, want %v", got, env)
	}
}

// TestTurnDecoratorRunsForHeadlessSessions is the case the original
// location-only decorator dropped. A delegated job authenticating gcloud has
// exactly the same split-brain problem a chat turn does, and no thread to
// report it in — so "no channel" must not mean "no environment".
func TestTurnDecoratorRunsForHeadlessSessions(t *testing.T) {
	env := []string{"AWS_CONFIG_FILE=/srv/work/.aws/config"}
	decorate := turnDecorator(agent.SessionMetadata{}, env)
	if decorate == nil {
		t.Fatal("a headless session for an agent with an environment got no decorator")
	}

	ctx := decorate(context.Background())
	if got := agent.TurnEnvFromContext(ctx); !slices.Equal(got, env) {
		t.Errorf("env = %v, want %v", got, env)
	}
	// Still no location: an interactive tool must refuse rather than post
	// somewhere arbitrary.
	if _, ok := agent.TurnLocationFromContext(ctx); ok {
		t.Error("a headless session was given a Slack location it does not have")
	}
}

// TestTurnDecoratorIsNilWhenThereIsNothingToSay keeps the existing contract:
// a native-shaped, non-chat session stays undecorated.
func TestTurnDecoratorIsNilWhenThereIsNothingToSay(t *testing.T) {
	if decorate := turnDecorator(agent.SessionMetadata{}, nil); decorate != nil {
		t.Error("a session with neither a location nor an environment was decorated anyway")
	}
}
