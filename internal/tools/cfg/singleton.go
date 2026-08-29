package cfg

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/tools"
)

// Singletons are the single-valued config blocks (access/chat/defaults/journal/
// troubleshoot). They are stored via Put/GetSingleton rather than as collection
// items, so they get bespoke set tools (only chat and access carry typed flags)
// and a shared, read-only show tool.

// singletonShowTool prints one singleton's stored JSON body. Absent singletons
// (never configured) render a helpful "not set" line rather than an error.
type singletonShowTool struct {
	p     Provider
	key   string
	label string
}

func (t *singletonShowTool) Name() string { return "cfg." + t.key + ".show" }
func (t *singletonShowTool) Description() string {
	return fmt.Sprintf("Show the %s configuration.", t.label)
}
func (t *singletonShowTool) InputSchema() *jsonschema.Schema { return nil }
func (t *singletonShowTool) Invoke(ctx context.Context, _ map[string]any) (any, error) {
	s, err := t.p()
	if err != nil {
		return nil, err
	}
	body, ok, err := s.GetSingleton(ctx, t.key)
	if err != nil {
		return nil, err
	}
	if !ok || len(body) == 0 {
		return okResult{Message: fmt.Sprintf("%s is not set", t.label)}, nil
	}
	return showResult{Name: t.key, Body: body}, nil
}

// chatSetTool updates the chat singleton, applying only the flags given.
type chatSetTool struct{ p Provider }

func (t *chatSetTool) Name() string { return "cfg.chat.set" }
func (t *chatSetTool) Description() string {
	return "Update the chat surface config (enabled, default/DM agent, reply strategy)."
}
func (t *chatSetTool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"enabled":         {Type: "boolean", Description: "gate the Slack chat surface (DM and @mention replies)"},
			"default_agent":   {Type: "string", Description: "fallback agent for channels without an override"},
			"dm_agent":        {Type: "string", Description: "agent that answers direct messages"},
			"reply_on_thread": {Type: "boolean", Description: "default reply strategy: true roots a thread, false replies in-channel"},
		},
	}
}
func (t *chatSetTool) Invoke(ctx context.Context, args map[string]any) (any, error) {
	s, err := t.p()
	if err != nil {
		return nil, err
	}
	var cfg config.ChatConfig
	if body, ok, err := s.GetSingleton(ctx, config.SingletonChat); err != nil {
		return nil, err
	} else if ok && len(body) > 0 {
		if err := json.Unmarshal(body, &cfg); err != nil {
			return nil, err
		}
	}
	if v, ok := boolArg(args, "enabled"); ok {
		cfg.Enabled = v
	}
	if v, ok := stringArg(args, "default_agent"); ok {
		cfg.Defaults.Agent = v
	}
	if v, ok := stringArg(args, "dm_agent"); ok {
		cfg.Defaults.DMAgent = v
	}
	if v, ok := boolArg(args, "reply_on_thread"); ok {
		b := v
		cfg.Defaults.ReplyOnThread = &b
	}
	if err := putSingletonValidated(ctx, s, config.SingletonChat, cfg); err != nil {
		return nil, err
	}
	return okResult{Message: "saved chat config"}, nil
}

// accessSetTool updates the access singleton, applying only the flags given.
type accessSetTool struct{ p Provider }

func (t *accessSetTool) Name() string { return "cfg.access.set" }
func (t *accessSetTool) Description() string {
	return "Update the access config (admin user, allowed users, debug)."
}
func (t *accessSetTool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"admin_user":    {Type: "string", Description: "admin Slack user ID or handle"},
			"allowed_users": {Type: "array", Items: &jsonschema.Schema{Type: "string"}, Description: "allowed Slack user (repeatable; replaces the list)"},
			"debug":         {Type: "boolean", Description: "enable access debug logging"},
		},
	}
}
func (t *accessSetTool) Invoke(ctx context.Context, args map[string]any) (any, error) {
	s, err := t.p()
	if err != nil {
		return nil, err
	}
	var cfg config.AccessConfig
	if body, ok, err := s.GetSingleton(ctx, config.SingletonAccess); err != nil {
		return nil, err
	} else if ok && len(body) > 0 {
		if err := json.Unmarshal(body, &cfg); err != nil {
			return nil, err
		}
	}
	if v, ok := stringArg(args, "admin_user"); ok {
		cfg.AdminUser = v
	}
	if v, ok := arrayArg(args, "allowed_users"); ok {
		cfg.AllowedUsers = v
	}
	if v, ok := boolArg(args, "debug"); ok {
		cfg.Debug = v
	}
	if err := putSingletonValidated(ctx, s, config.SingletonAccess, cfg); err != nil {
		return nil, err
	}
	return okResult{Message: "saved access config"}, nil
}

// fallbackSetTool (cfg.fallback.set) edits the leader-election block.
//
// It writes to the config store rather than to a local file on purpose: every
// contending node must agree on these timings, and a per-node setting would let
// two nodes disagree about when the incumbent's lease lapsed.
type fallbackSetTool struct{ p Provider }

func (t *fallbackSetTool) Name() string { return "cfg.fallback.set" }
func (t *fallbackSetTool) Description() string {
	return "Update the leader-election config (enable failover, lease and renewal timings)."
}
func (t *fallbackSetTool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"enabled":       {Type: "boolean", Description: "contend for leadership instead of always running"},
			"lease_seconds": {Type: "integer", Description: "how long a leadership claim lasts without renewal (default 30)"},
			"renew_seconds": {Type: "integer", Description: "how often the leader refreshes its claim (default 10; at most half the lease)"},
		},
	}
}
func (t *fallbackSetTool) Invoke(ctx context.Context, args map[string]any) (any, error) {
	s, err := t.p()
	if err != nil {
		return nil, err
	}
	var cfg config.FallbackConfig
	if body, ok, err := s.GetSingleton(ctx, config.SingletonFallback); err != nil {
		return nil, err
	} else if ok && len(body) > 0 {
		if err := json.Unmarshal(body, &cfg); err != nil {
			return nil, err
		}
	}
	if v, ok := boolArg(args, "enabled"); ok {
		cfg.Enabled = v
	}
	if v, ok := intArg(args, "lease_seconds"); ok {
		cfg.LeaseSeconds = v
	}
	if v, ok := intArg(args, "renew_seconds"); ok {
		cfg.RenewSeconds = v
	}
	// putSingletonValidated re-validates the whole assembled config and rolls
	// back on failure, so an unworkable lease/renew pair is refused here rather
	// than discovered during a failover.
	if err := putSingletonValidated(ctx, s, config.SingletonFallback, cfg); err != nil {
		return nil, err
	}
	return okResult{Message: "saved fallback config; restart Murtaugh to apply"}, nil
}

// SingletonTools returns the typed set tools for chat/access plus read-only
// show tools for every singleton block.
func SingletonTools(p Provider) []tools.Tool {
	return []tools.Tool{
		&chatSetTool{p: p},
		&singletonShowTool{p: p, key: config.SingletonChat, label: "chat"},
		&accessSetTool{p: p},
		&singletonShowTool{p: p, key: config.SingletonAccess, label: "access"},
		&singletonShowTool{p: p, key: config.SingletonDefaults, label: "defaults"},
		&singletonShowTool{p: p, key: config.SingletonJournal, label: "journal"},
		&singletonShowTool{p: p, key: config.SingletonTroubleshoot, label: "troubleshoot"},
		&fallbackSetTool{p: p},
		&singletonShowTool{p: p, key: config.SingletonFallback, label: "fallback"},
	}
}
