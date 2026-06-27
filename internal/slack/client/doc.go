// Package client wraps github.com/slack-go/slack behind a small surface that
// Murtaugh's Slack tools depend on. The wrapper exposes only the data and
// behaviours those tools need, so tools can be tested against a fake
// implementation of the SlackAPI interface without touching the network.
//
// Unlike a standalone CLI, Murtaugh sources the bot token from its loaded
// configuration (oauth.bot_token in gateway.yaml) rather than the environment;
// callers pass the token into NewClient / NewLazyClient at construction time.
package client
