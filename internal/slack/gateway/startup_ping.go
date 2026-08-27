package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/miere/murtaugh/internal/slack/alertcard"
	"github.com/slack-go/slack"
)

type StartupNotifier interface {
	NotifyStartup(context.Context) error
}

type SlackAPI interface {
	GetUsersContext(context.Context, ...slack.GetUsersOption) ([]slack.User, error)
	OpenConversationContext(context.Context, *slack.OpenConversationParameters) (*slack.Channel, bool, bool, error)
	PostMessageContext(context.Context, string, ...slack.MsgOption) (string, string, error)
}

type SlackStartupNotifier struct {
	api       SlackAPI
	adminUser string
	spec      alertcard.Spec
	// cards and cardAPI render and post the greeting as an info card. Both nil
	// (no raw-blocks client could be built) degrades it to the card's plain-text
	// form over api, which is the same fallback every other alert takes.
	cards   *alertcard.Renderer
	cardAPI alertMessagePoster
	logger  *slog.Logger
}

// NewSlackStartupNotifier builds the connect-time greeting for the admin DM.
// cards and cardAPI are the alert-card renderer and its raw-blocks client; a nil
// either leaves the greeting as text.
func NewSlackStartupNotifier(api SlackAPI, adminUser string, cards *alertcard.Renderer, cardAPI alertMessagePoster, logger *slog.Logger) (StartupNotifier, error) {
	if logger == nil {
		logger = slog.Default()
	}
	adminUser = strings.TrimSpace(adminUser)
	if adminUser == "" {
		logger.Warn("startup Slack ping disabled: configuration.admin_user is not set")
		return nil, nil
	}
	return &SlackStartupNotifier{
		api:       api,
		adminUser: adminUser,
		spec:      startupAlert(),
		cards:     cards,
		cardAPI:   cardAPI,
		logger:    logger,
	}, nil
}

func (n *SlackStartupNotifier) NotifyStartup(ctx context.Context) error {
	userID, err := n.resolveAdminUserID(ctx)
	if err != nil {
		return err
	}
	channel, _, _, err := n.api.OpenConversationContext(ctx, &slack.OpenConversationParameters{Users: []string{userID}, ReturnIM: true})
	if err != nil {
		return fmt.Errorf("open admin DM: %w", err)
	}
	if channel == nil || channel.ID == "" {
		return fmt.Errorf("open admin DM: Slack returned no channel")
	}
	if n.cards != nil && n.cardAPI != nil {
		res, err := postAlertCard(ctx, n.cardAPI, n.cards, channel.ID, "", n.spec)
		if err == nil {
			n.logger.Info("sent Slack startup ping", "admin_user", n.adminUser, "channel", res.Channel, "ts", res.TS)
			return nil
		}
		n.logger.Warn("failed to post startup card; falling back to text", "error", err)
	}
	_, ts, err := n.api.PostMessageContext(ctx, channel.ID, slack.MsgOptionText(alertcard.PlainText(n.spec), false))
	if err != nil {
		return fmt.Errorf("post startup ping: %w", err)
	}
	n.logger.Info("sent Slack startup ping", "admin_user", n.adminUser, "channel", channel.ID, "ts", ts)
	return nil
}

func (n *SlackStartupNotifier) resolveAdminUserID(ctx context.Context) (string, error) {
	ids, err := resolveUserIDs(ctx, n.api, []string{n.adminUser})
	if err != nil {
		return "", fmt.Errorf("resolve configuration.admin_user: %w", err)
	}
	if len(ids) != 1 {
		return "", fmt.Errorf("resolve configuration.admin_user %q: unexpected resolution result", n.adminUser)
	}
	return ids[0], nil
}

func looksLikeUserID(value string) bool {
	return len(value) > 3 && (strings.HasPrefix(value, "U") || strings.HasPrefix(value, "W"))
}

func slackUserMatchesHandle(user slack.User, handle string) bool {
	return strings.EqualFold(user.Name, handle) ||
		strings.EqualFold(user.Username, handle) ||
		strings.EqualFold(user.Profile.DisplayName, handle) ||
		strings.EqualFold(user.Profile.DisplayNameNormalized, handle)
}
