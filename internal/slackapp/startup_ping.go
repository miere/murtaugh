package slackapp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/miere/murtaugh-dev-toolkit/assets"
	"github.com/slack-go/slack"
)

const startupPingText = ":zap: The server has started."

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
	blocks    []slack.Block
	logger    *slog.Logger
}

func NewSlackStartupNotifier(api SlackAPI, adminUser string, logger *slog.Logger) (StartupNotifier, error) {
	adminUser = strings.TrimSpace(adminUser)
	if adminUser == "" {
		return nil, nil
	}
	blocks, err := loadStartupPingBlocks()
	if err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &SlackStartupNotifier{api: api, adminUser: adminUser, blocks: blocks, logger: logger}, nil
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
	_, ts, err := n.api.PostMessageContext(ctx, channel.ID, slack.MsgOptionText(startupPingText, false), slack.MsgOptionBlocks(n.blocks...))
	if err != nil {
		return fmt.Errorf("post startup ping: %w", err)
	}
	n.logger.Info("sent Slack startup ping", "admin_user", n.adminUser, "channel", channel.ID, "ts", ts)
	return nil
}

func (n *SlackStartupNotifier) resolveAdminUserID(ctx context.Context) (string, error) {
	handle := strings.TrimPrefix(strings.TrimSpace(n.adminUser), "@")
	if looksLikeUserID(handle) {
		return handle, nil
	}
	users, err := n.api.GetUsersContext(ctx)
	if err != nil {
		return "", fmt.Errorf("list Slack users: %w", err)
	}
	for _, user := range users {
		if user.Deleted || user.IsBot {
			continue
		}
		if slackUserMatchesHandle(user, handle) {
			return user.ID, nil
		}
	}
	return "", fmt.Errorf("configuration.admin_user %q was not found", n.adminUser)
}

func loadStartupPingBlocks() ([]slack.Block, error) {
	data, err := assets.FS.ReadFile("ping/01-ping.json")
	if err != nil {
		return nil, fmt.Errorf("read startup ping asset: %w", err)
	}
	var blocks slack.Blocks
	if err := json.Unmarshal(data, &blocks); err != nil {
		return nil, fmt.Errorf("parse startup ping blocks: %w", err)
	}
	if len(blocks.BlockSet) == 0 {
		return nil, fmt.Errorf("startup ping asset contains no blocks")
	}
	return blocks.BlockSet, nil
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
