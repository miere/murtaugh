package gateway

import (
	"context"
	"log/slog"
	"strings"
	"sync"

	"github.com/slack-go/slack"
)

// userInfoAPI is the minimal Slack surface userNameCache needs. *slack.Client
// satisfies it. It is deliberately separate from userDirectoryAPI: that one
// maps handles to ids with a users.list sweep, this one goes the other way for
// a single id.
type userInfoAPI interface {
	GetUserInfoContext(ctx context.Context, user string) (*slack.User, error)
}

// userNameCache resolves a Slack user id to a display name, remembering what it
// learns for the life of the gateway.
//
// The cache is what makes id → name affordable on the buffered reply path: a
// reply that tags the same three people repeatedly costs three users.info calls
// once, and nothing thereafter.
//
// Lookups fail soft, returning "". A name is cosmetic here — the caller still
// emits the mention, so the notification lands either way — and a reply is not
// worth failing over a directory hiccup. Only successes are cached, so a
// transient error does not pin an empty name in place forever.
type userNameCache struct {
	api    userInfoAPI
	logger *slog.Logger

	mu    sync.Mutex
	names map[string]string
}

func newUserNameCache(api userInfoAPI, logger *slog.Logger) *userNameCache {
	if logger == nil {
		logger = slog.Default()
	}
	return &userNameCache{api: api, logger: logger, names: make(map[string]string)}
}

// Name returns the display name for id, or "" if it cannot be resolved.
func (c *userNameCache) Name(ctx context.Context, id string) string {
	if c == nil || c.api == nil || id == "" {
		return ""
	}
	c.mu.Lock()
	cached, ok := c.names[id]
	c.mu.Unlock()
	if ok {
		return cached
	}

	user, err := c.api.GetUserInfoContext(ctx, id)
	if err != nil {
		c.logger.Debug("could not resolve Slack user name", "user", id, "error", err)
		return ""
	}
	name := displayNameOf(user)
	if name == "" {
		return ""
	}
	c.mu.Lock()
	c.names[id] = name
	c.mu.Unlock()
	return name
}

// displayNameOf picks the friendliest name Slack offers, in the order a human
// would expect to see it.
func displayNameOf(user *slack.User) string {
	if user == nil {
		return ""
	}
	for _, candidate := range []string{user.Profile.DisplayName, user.RealName, user.Name} {
		if trimmed := strings.TrimSpace(candidate); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
