package slackapp

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/miere/murtaugh-dev-toolkit/internal/config"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"
)

type App struct {
	api     *slack.Client
	socket  *socketmode.Client
	handler SlashCommandHandler
	logger  *slog.Logger
}

func New(cfg config.Config, logger *slog.Logger) *App {
	if logger == nil {
		logger = slog.Default()
	}
	api := slack.New(cfg.Slack.BotToken, slack.OptionAppLevelToken(cfg.Slack.AppToken))
	socket := socketmode.New(api, socketmode.OptionDebug(cfg.Slack.Debug))
	return &App{
		api:     api,
		socket:  socket,
		handler: NewDefaultSlashCommandHandler(cfg.Commands),
		logger:  logger,
	}
}

func (a *App) Run(ctx context.Context) error {
	errCh := make(chan error, 1)
	go func() {
		errCh <- a.socket.RunContext(ctx)
	}()

	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-errCh:
			if ctx.Err() != nil {
				return nil
			}
			return err
		case event := <-a.socket.Events:
			a.handleEvent(ctx, event)
		}
	}
}

func (a *App) handleEvent(ctx context.Context, event socketmode.Event) {
	switch event.Type {
	case socketmode.EventTypeConnecting, socketmode.EventTypeConnected, socketmode.EventTypeHello:
		a.logger.Debug("socket mode lifecycle event", "type", event.Type)
	case socketmode.EventTypeSlashCommand:
		a.handleSlashCommand(ctx, event)
	default:
		a.logger.Debug("ignored socket mode event", "type", event.Type)
	}
}

func (a *App) handleSlashCommand(ctx context.Context, event socketmode.Event) {
	command, ok := event.Data.(slack.SlashCommand)
	if !ok {
		a.ack(event, ephemeralText("Unsupported slash command payload."))
		a.logger.Warn("unexpected slash command payload", "type", fmt.Sprintf("%T", event.Data))
		return
	}

	response, err := a.handler.HandleSlashCommand(ctx, command)
	if err != nil {
		a.logger.Error("slash command failed", "command", command.Command, "error", err)
		response = ephemeralText("Murtaugh hit an error while handling that command.")
	}
	a.ack(event, response)
}

func (a *App) ack(event socketmode.Event, response AckResponse) {
	if event.Request == nil {
		a.logger.Warn("cannot acknowledge event without request", "type", event.Type)
		return
	}
	if err := a.socket.Ack(*event.Request, response); err != nil {
		a.logger.Error("failed to acknowledge Slack request", "error", err)
	}
}
