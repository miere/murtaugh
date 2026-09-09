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

// This file is the node's half of #170's zero-profile onboarding trigger.
//
// A node that has never been configured attaches with an empty advertisement.
// The gateway sees that, offers the node's OWNER the existing Slack setup form
// — the node has no Slack, so it could not — and sends the answers back down the
// connection the node already holds open. This applies them, into this node's
// own store.
//
// # The node decides, and that is the whole design
//
// #170 says node admins own their node. The method that crosses does not change
// that because THIS side refuses it unless the node holds no agent profile at
// all. So a gateway can bootstrap an empty node exactly once, and can never
// reconfigure, overwrite or reach into one that is running. A gateway admin who
// wants to change somebody's node still has to ask them.
//
// # And then it restarts
//
// Both agent backend families latch: a native agent resolves its toolset at its
// first Initialize and an acp/claude_code agent's aggregator resolves it when
// its first session is registered. A process built with no agent therefore
// cannot grow one — the run below constructs the runtime once, from a snapshot
// taken at startup, deliberately. So the node writes the configuration, says it
// is restarting, and stops; its supervisor brings it back up serving what it was
// just given, and the gateway sees a redial about a second later.

// configurer applies a gateway-supplied configuration to this node's store.
type configurer struct {
	store config.Store
	// baseDir is where this node's configuration lives. It is the fallback
	// workdir for a profile the gateway could not root, and the directory the
	// .env sits in.
	baseDir string
	logger  *slog.Logger
	// restarts records whether this node CAN restart itself, which is what makes
	// the answer's Restarting field honest. The restart itself is not fired here:
	// it is fired by nodeserve after the answer is on the wire, because the thing
	// it cancels is the context that connection is being served on — a node that
	// stopped a moment early would leave the operator watching a form that
	// appeared to fail.
	restarts bool
}

// apply writes the profiles, the chat block and the credentials, then asks for a
// restart.
//
// Order matters and is the same order the gateway's own onboarding writer uses:
// the credential goes to .env FIRST, because a profile references its key by
// variable name and a profile whose key is not yet on disk builds into an agent
// that cannot reach its model. Agents next. The chat block LAST, because it
// names the agents and a store holding chat without them is a store this node
// would refuse to load on its next start — which, since the next thing that
// happens is a restart, would be an install that bricked itself at the last step.
func (c *configurer) apply(ctx context.Context, incoming agentwire.NodeConfiguration) (agentwire.NodeConfigured, error) {
	if c.store == nil {
		return agentwire.NodeConfigured{}, errors.New("this node has no configuration store open")
	}
	// The guarantee, enforced here rather than trusted from the far side. It is
	// read fresh rather than taken from the snapshot this process started with,
	// because the node's owner may have configured it from a terminal in the
	// meantime — and the last thing an onboarding form should do is overwrite
	// the profile somebody just wrote by hand.
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
		// A profile the form left without a work_dir is rooted here, in this
		// node's own configuration directory. The gateway cannot supply that
		// path — it is a directory on somebody else's machine — so the node is
		// the only side that can answer, and there is nothing on the wire to
		// defer to.
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

// rootProfile fills in a work_dir the gateway could not.
//
// There is exactly one such profile — the owner's `tweaker`, rooted wherever the
// configuration lives so it can edit it — and only this side knows that path: it
// is a directory on this machine. Every other profile carries the directory the
// owner typed into the form, which is likewise a path on this machine and is
// taken as given.
func rootProfile(body json.RawMessage, fallback string) (config.AgentProfile, error) {
	var profile config.AgentProfile
	if err := json.Unmarshal(body, &profile); err != nil {
		return config.AgentProfile{}, err
	}
	// Through config.RootedAt rather than the field: the workdir guard forbids a
	// downstream READ of the raw work_dir, and it is right to — a downstream read
	// is somebody consuming an unresolved workdir. This is authoring, and it
	// belongs beside the field it defaults.
	return profile.RootedAt(fallback), nil
}

// writeEnvVar stores a credential in the .env beside this node's config.
//
// It goes through the setup.env tool for the reason the gateway's own writer
// does: that tool already owns the merge semantics — preserve unrelated keys,
// back up the previous file — and re-implementing them here would be a second
// place for a node's provider credentials to be lost.
func (c *configurer) writeEnvVar(ctx context.Context, key, value string) error {
	if strings.TrimSpace(c.baseDir) == "" {
		return errors.New("the config directory is unknown, so there is no .env to write")
	}
	envPath := filepath.Join(c.baseDir, config.EnvFileName)
	tool := setupenv.New(func() string { return envPath })
	_, err := tool.Invoke(ctx, map[string]any{"set": []any{key + "=" + value}})
	return err
}
