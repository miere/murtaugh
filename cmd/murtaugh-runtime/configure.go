package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/miere/murtaugh/internal/agentwire"
	"github.com/miere/murtaugh/internal/config"
	setupenv "github.com/miere/murtaugh/internal/tools/setup/env"
)

type configurer struct {
	store    config.Store
	baseDir  string
	logger   *slog.Logger
	restarts bool
}

func (c *configurer) apply(ctx context.Context, incoming agentwire.NodeConfiguration) (agentwire.NodeConfigured, error) {
	if c.store == nil {
		return agentwire.NodeConfigured{}, errors.New("this node has no configuration store open")
	}
	existing, err := c.store.ListItems(ctx, config.SectionAgent)
	if err != nil {
		return agentwire.NodeConfigured{}, fmt.Errorf("check whether this node is already configured: %w", err)
	}
	if len(existing) > 0 {
		return agentwire.NodeConfigured{}, fmt.Errorf(
			"this node already has %d agent profile(s) and will not be reconfigured from a gateway; edit it with `murtaugh cfg agent …` on this machine", len(existing))
	}
	if incoming.Empty() {
		return agentwire.NodeConfigured{}, errors.New("the configuration carried nothing to apply")
	}

	for key, value := range incoming.Env {
		if strings.TrimSpace(key) == "" {
			continue
		}
		if err := c.writeEnvVar(ctx, key, value); err != nil {
			return agentwire.NodeConfigured{}, fmt.Errorf("store the %s credential: %w", key, err)
		}
	}

	applied := 0
	for name, body := range incoming.Agents {
		profile, err := rootProfile(body, c.baseDir)
		if err != nil {
			return agentwire.NodeConfigured{}, fmt.Errorf("read the %q agent: %w", name, err)
		}
		if err := c.store.UpsertItem(ctx, config.SectionAgent, name, profile); err != nil {
			return agentwire.NodeConfigured{}, fmt.Errorf("save the %q agent: %w", name, err)
		}
		applied++
	}

	if len(incoming.Chat) > 0 {
		var chat config.ChatConfig
		if err := json.Unmarshal(incoming.Chat, &chat); err != nil {
			return agentwire.NodeConfigured{}, fmt.Errorf("read the chat block: %w", err)
		}
		if err := c.store.PutSingleton(ctx, config.SingletonChat, chat); err != nil {
			return agentwire.NodeConfigured{}, fmt.Errorf("save this node's assignment rules: %w", err)
		}
	}

	c.logger.Info("this node has been configured; restarting to serve what it was given",
		"profiles", applied, "config", c.baseDir)
	return agentwire.NodeConfigured{Applied: applied, Restarting: c.restarts}, nil
}

func rootProfile(body json.RawMessage, fallback string) (config.AgentProfile, error) {
	var profile config.AgentProfile
	if err := json.Unmarshal(body, &profile); err != nil {
		return config.AgentProfile{}, err
	}
	return profile.RootedAt(fallback), nil
}

func (c *configurer) writeEnvVar(ctx context.Context, key, value string) error {
	if strings.TrimSpace(c.baseDir) == "" {
		return errors.New("the config directory is unknown, so there is no .env to write")
	}
	envPath := filepath.Join(c.baseDir, config.EnvFileName)
	tool := setupenv.New(func() string { return envPath })
	_, err := tool.Invoke(ctx, map[string]any{"set": []any{key + "=" + value}})
	return err
}
