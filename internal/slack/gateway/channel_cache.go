package gateway

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	slackclient "github.com/miere/murtaugh/internal/slack/client"
	"github.com/slack-go/slack"
)

// channelDirectoryAPI is the minimal Slack surface the channel-name cache needs
// to warm and refresh its ID→name map. *slackclient.SlackClient satisfies it,
// and tests substitute an in-memory fake.
type channelDirectoryAPI interface {
	ListChannels(ctx context.Context) ([]slackclient.Channel, error)
}

// defaultChannelCacheRefresh is how often the cache re-lists channels in the
// background so renamed/new channels are eventually picked up without a
// process restart. Channel renames are rare, so the cadence is generous.
const defaultChannelCacheRefresh = 10 * time.Minute

// conversationInfoTimeout bounds the single conversations.info lookup a routing
// miss triggers (resolveChannelName), kept short so a slow Slack call can never
// stall a turn.
const conversationInfoTimeout = 5 * time.Second

// channelNameCache is an in-memory Slack channel ID→name map consulted by the
// chat resolver to route channel→agent by NAME glob without doing Slack API
// I/O on the socket goroutine. It is warmed once at startup, refreshed on a
// ticker, and refreshed asynchronously on a lookup miss (a brand-new channel
// the cache has not learned yet). Lookups are pure in-memory reads guarded by
// an RWMutex; the resolver never blocks on the network.
type channelNameCache struct {
	api channelDirectoryAPI
	// info resolves a single channel via conversations.info on a cache miss
	// (resolveChannelName). Unlike api's ListChannels it returns file-backed
	// canvas conversations too, which is how canvas turns learn their channel.
	// nil disables resolve-on-miss (routing then degrades to exact-ID matching).
	info canvasInfoAPI
	// parent maps a canvas file id to the channel the document lives in, for the
	// file-backed canvas conversations `info` alone cannot route. nil skips the
	// hop (canvas turns then fall through to exact-ID/default).
	parent canvasParentResolver
	logger *slog.Logger

	mu      sync.RWMutex
	byID    map[string]string
	primed  bool
	timeout time.Duration

	// refreshing guards against piling up overlapping background refreshes when
	// a burst of messages arrives in an unknown channel: only one async refresh
	// is in flight at a time.
	refreshing atomic.Bool
}

// newChannelNameCache builds an empty cache over the given directory. info
// (conversations.info) backs the synchronous resolve-on-miss and parent
// (files.info) the canvas→parent-channel hop; pass nil to disable either.
// timeout bounds each ListChannels call; a non-positive value uses a 30s
// default. A nil logger falls back to slog.Default.
func newChannelNameCache(api channelDirectoryAPI, info canvasInfoAPI, parent canvasParentResolver, timeout time.Duration, logger *slog.Logger) *channelNameCache {
	if logger == nil {
		logger = slog.Default()
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &channelNameCache{
		api:     api,
		info:    info,
		parent:  parent,
		logger:  logger,
		byID:    map[string]string{},
		timeout: timeout,
	}
}

// nameFor returns the cached channel name for id and whether it was known.
// An empty id, an unprimed cache, or an id the cache has not learned yet all
// report ("", false). It is a pure in-memory read safe to call from the Slack
// socket goroutine.
func (c *channelNameCache) nameFor(id string) (string, bool) {
	if c == nil || id == "" {
		return "", false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	name, ok := c.byID[id]
	return name, ok
}

// resolveChannelName returns the channel name for id, resolving it via a bounded
// conversations.info on a cache miss and memoizing the result so later turns and
// the interrupt path are warm. Unlike nameFor it may do Slack I/O, so callers
// MUST invoke it OFF the socket goroutine (the per-turn goroutine). It is how a
// canvas turn learns its channel: ListChannels never returns canvas/file-backed
// conversations, but conversations.info does. A file-backed canvas conversation
// ("FC:<fileId>:<title>") has no routable name of its own and defers to
// resolveCanvasParent. Any error leaves the cache untouched and returns
// ("", false) so routing falls back to exact-ID/default rather than failing.
func (c *channelNameCache) resolveChannelName(ctx context.Context, id string) (string, bool) {
	if c == nil || id == "" {
		return "", false
	}
	if name, ok := c.nameFor(id); ok {
		return name, true
	}
	if c.info == nil {
		return "", false
	}
	ch, err := c.lookupConversation(ctx, id)
	if err != nil || ch == nil {
		return "", false
	}
	// A file-backed canvas conversation. Its name_normalized identifies the canvas
	// FILE, never the channel — and it looks identical whether the canvas is a tab
	// on a real channel or a standalone doc. Ask the file where it lives.
	if fileID := canvasFileIDFromName(ch.NameNormalized); fileID != "" {
		return c.resolveCanvasParent(ctx, id, fileID)
	}
	name := ch.Name
	if name == "" {
		name = ch.NameNormalized
	}
	if name == "" {
		return "", false
	}
	c.memoize(id, name)
	return name, true
}

// resolveCanvasParent routes a canvas conversation by the channel its document
// lives in, resolved from the canvas file id via files.info. The parent's NAME is
// what channel→agent glob routing needs, so when files.info yields only an id
// (the `groups`/`channels` lists carry no name) one ordinary conversations.info
// on that parent finishes the job.
//
// The name is memoized under the CANVAS conversation id — that is the id routing
// looks up — so the hop costs at most two calls per canvas per process. A canvas
// with no discoverable parent is NOT memoized, leaving exact-ID/default routing
// intact rather than caching a wrong answer.
func (c *channelNameCache) resolveCanvasParent(ctx context.Context, canvasConvID, fileID string) (string, bool) {
	if c.parent == nil {
		return "", false
	}
	lookupCtx, cancel := context.WithTimeout(ctx, conversationInfoTimeout)
	parentID, parentName, err := c.parent.CanvasParentChannel(lookupCtx, fileID)
	cancel()
	if err != nil {
		// INFO, not Debug: a missing files:read scope surfaces here, and a canvas
		// silently answering with the default agent is exactly the failure this
		// resolution exists to end — it must not be invisible at the default level.
		c.logger.Info("canvas parent-channel lookup failed; routing falls back to default",
			"canvas", canvasConvID, "file", fileID, "error", err)
		return "", false
	}
	if parentID == "" {
		c.logger.Info("canvas is not shared into any channel; routing falls back to default",
			"canvas", canvasConvID, "file", fileID)
		return "", false
	}
	if parentName == "" {
		if ch, err := c.lookupConversation(ctx, parentID); err == nil && ch != nil {
			parentName = ch.Name
			if parentName == "" {
				parentName = ch.NameNormalized
			}
		}
	}
	if parentName == "" {
		return "", false
	}
	c.memoize(canvasConvID, parentName)
	c.logger.Info("canvas routed by its parent channel",
		"canvas", canvasConvID, "file", fileID, "parent", parentID, "parent_name", parentName)
	return parentName, true
}

// lookupConversation is one bounded conversations.info. A failure is logged at
// Debug and reported as an error so callers degrade rather than fail the turn.
func (c *channelNameCache) lookupConversation(ctx context.Context, id string) (*slack.Channel, error) {
	lookupCtx, cancel := context.WithTimeout(ctx, conversationInfoTimeout)
	defer cancel()
	ch, err := c.info.GetConversationInfoContext(lookupCtx, &slack.GetConversationInfoInput{ChannelID: id})
	if err != nil {
		c.logger.Debug("channel-name resolve-on-miss failed", "channel", id, "error", err)
		return nil, err
	}
	return ch, nil
}

func (c *channelNameCache) memoize(id, name string) {
	c.mu.Lock()
	c.byID[id] = name
	c.mu.Unlock()
}

// refresh re-lists the workspace channels and replaces the cache contents. It
// is called once at startup (synchronously, bounded by ctx) and from the
// background ticker. A failed list leaves the previous contents in place so a
// transient Slack error does not blank the cache.
func (c *channelNameCache) refresh(ctx context.Context) error {
	if c == nil {
		return nil
	}
	listCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	channels, err := c.api.ListChannels(listCtx)
	if err != nil {
		return err
	}
	byID := make(map[string]string, len(channels))
	for _, ch := range channels {
		if ch.ID == "" {
			continue
		}
		byID[ch.ID] = ch.Name
	}
	c.mu.Lock()
	c.byID = byID
	c.primed = true
	c.mu.Unlock()
	return nil
}

// refreshAsync triggers a one-off background refresh, deduplicated so a burst
// of misses spawns at most one in-flight list. It never blocks the caller —
// the resolver uses it to learn a brand-new channel's name for the NEXT
// message after falling back to the default agent for the current one.
func (c *channelNameCache) refreshAsync(ctx context.Context) {
	if c == nil {
		return
	}
	if !c.refreshing.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer c.refreshing.Store(false)
		if err := c.refresh(ctx); err != nil {
			c.logger.Warn("channel-name cache refresh failed", "error", err)
		}
	}()
}

// run warms the cache once (synchronously, so the first messages after startup
// can match by name) and then refreshes it on a ticker until ctx is done. It is
// modelled on startJournalSweeper: prime at startup, then poll, so a long-lived
// daemon picks up renamed/new channels without a restart. A nil cache or nil
// api makes this a no-op.
func (c *channelNameCache) run(ctx context.Context, every time.Duration) {
	if c == nil || c.api == nil {
		return
	}
	if every <= 0 {
		every = defaultChannelCacheRefresh
	}
	if err := c.refresh(ctx); err != nil {
		c.logger.Warn("channel-name cache warmup failed", "error", err)
	} else {
		c.mu.RLock()
		n := len(c.byID)
		c.mu.RUnlock()
		c.logger.Info("channel-name cache warmed", "channels", n)
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := c.refresh(ctx); err != nil {
				c.logger.Warn("channel-name cache refresh failed", "error", err)
			}
		}
	}
}
