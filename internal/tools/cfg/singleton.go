package cfg

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

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
	return "Update the access config (admin user, allowed users, debug, main node)."
}
func (t *accessSetTool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"admin_user":    {Type: "string", Description: "admin Slack user ID or handle"},
			"allowed_users": {Type: "array", Items: &jsonschema.Schema{Type: "string"}, Description: "allowed Slack user (repeatable; replaces the list)"},
			"debug":         {Type: "boolean", Description: "enable access debug logging"},
			"main_node":     {Type: "string", Description: "node id that serves headless work (jobs, workflow triggers, unfurls); empty string clears it"},
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
	// Designating the main node is the gateway admin writing down which machine
	// serves work that has no user to fleet on. It is here, on the gateway's own
	// access block, and not on the node's configuration, because a node that
	// could declare itself main would be granting itself the right to serve
	// every user's unfurls and every scheduled job — the thing #170's item 4
	// settled a node may never do. An empty value clears the designation, which
	// is the only way to un-designate and has to be reachable.
	if v, ok := stringArg(args, "main_node"); ok {
		cfg.MainNode = strings.TrimSpace(v)
	}
	if err := putSingletonValidated(ctx, s, config.SingletonAccess, cfg); err != nil {
		return nil, err
	}
	return okResult{Message: "saved access config"}, nil
}

// electionSetTool (cfg.election.set) edits the leader-election timings.
//
// It writes to the config store rather than to a local file on purpose: every
// contending node must agree on these timings, and a per-node setting would let
// two nodes disagree about when the incumbent's lease lapsed.
//
// There is no enable flag to set. Election follows the configuration backend
// and is always on; these are only the numbers.
type electionSetTool struct{ p Provider }

func (t *electionSetTool) Name() string { return "cfg.election.set" }
func (t *electionSetTool) Description() string {
	return "Update the leader-election timings (lease and renewal seconds)."
}
func (t *electionSetTool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"lease_seconds": {Type: "integer", Description: "how long a leadership claim lasts without renewal (default 30)"},
			"renew_seconds": {Type: "integer", Description: "how often the leader refreshes its claim (default 10; at most half the lease)"},
		},
	}
}
func (t *electionSetTool) Invoke(ctx context.Context, args map[string]any) (any, error) {
	s, err := t.p()
	if err != nil {
		return nil, err
	}
	var cfg config.ElectionConfig
	if body, ok, err := s.GetSingleton(ctx, config.SingletonElection); err != nil {
		return nil, err
	} else if ok && len(body) > 0 {
		if err := json.Unmarshal(body, &cfg); err != nil {
			return nil, err
		}
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
	if err := putSingletonValidated(ctx, s, config.SingletonElection, cfg); err != nil {
		return nil, err
	}
	return okResult{Message: "saved election config; restart Murtaugh to apply"}, nil
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
		&electionSetTool{p: p},
		&singletonShowTool{p: p, key: config.SingletonElection, label: "election"},
		&nodeSetTool{p: p},
		&singletonShowTool{p: p, key: config.SingletonNode, label: "node"},
	}
}

// nodeSetTool updates the node singleton: where a runtime node dials.
//
// It is a set tool rather than a bootstrap-file field because the address is
// ordinary configuration that an operator edits when a gateway moves, and the
// bootstrap file is deliberately credentials-and-store only. It is also the one
// block whose CONTENTS are about the gateway and whose OWNER is the node — see
// internal/config/node.go.
type nodeSetTool struct{ p Provider }

func (t *nodeSetTool) Name() string { return "cfg.node.set" }
func (t *nodeSetTool) Description() string {
	return "Update this runtime node's config (the gateway seed addresses it dials)."
}
func (t *nodeSetTool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"gateway": {
				Type:  "array",
				Items: &jsonschema.Schema{Type: "string"},
				Description: "gateway seed address, ws:// or wss:// (repeatable; replaces the list). " +
					"Addresses a gateway names in a redirect are added to these and never replace them.",
			},
		},
	}
}
func (t *nodeSetTool) Invoke(ctx context.Context, args map[string]any) (any, error) {
	s, err := t.p()
	if err != nil {
		return nil, err
	}
	var cfg config.NodeConfig
	if body, ok, err := s.GetSingleton(ctx, config.SingletonNode); err != nil {
		return nil, err
	} else if ok && len(body) > 0 {
		if err := json.Unmarshal(body, &cfg); err != nil {
			return nil, err
		}
	}
	if v, ok := arrayArg(args, "gateway"); ok {
		cfg.Gateway = v
	}
	// Checked here as well as by the store's own validation, because the store's
	// runs under whichever role opened it and this tool is reachable from a
	// combined install where nothing else would look at the block.
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if err := putSingletonValidated(ctx, s, config.SingletonNode, cfg); err != nil {
		return nil, err
	}
	return okResult{Message: "saved node config; restart the node to apply"}, nil
}
