package gateway

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/slack-go/slack"

	"github.com/miere/murtaugh/internal/config"
	slackclient "github.com/miere/murtaugh/internal/slack/client"
)

// countingCanvasInfo is a canvasInfoAPI that returns a fixed channel/error and
// counts calls, for resolve-on-miss tests.
type countingCanvasInfo struct {
	channel *slack.Channel
	err     error
	calls   atomic.Int32
}

func (c *countingCanvasInfo) GetConversationInfoContext(context.Context, *slack.GetConversationInfoInput) (*slack.Channel, error) {
	c.calls.Add(1)
	return c.channel, c.err
}

func channelNamed(name string) *slack.Channel {
	ch := &slack.Channel{}
	ch.Name = name
	return ch
}

// TestResolveChannelName_ResolvesAndMemoizesOnMiss: a channel canvas whose parent
// channel isn't in the warm list resolves via conversations.info and is cached.
func TestResolveChannelName_ResolvesAndMemoizesOnMiss(t *testing.T) {
	info := &countingCanvasInfo{channel: channelNamed("feature-xyz")}
	cache := newChannelNameCache(&fakeChannelDirectory{}, info, time.Second, cacheTestLogger())

	name, ok := cache.resolveChannelName(context.Background(), "C1")
	if !ok || name != "feature-xyz" {
		t.Fatalf("resolveChannelName = (%q, %v), want (feature-xyz, true)", name, ok)
	}
	if n2, ok2 := cache.nameFor("C1"); !ok2 || n2 != "feature-xyz" {
		t.Fatalf("nameFor after resolve = (%q, %v), want it memoized", n2, ok2)
	}
	_, _ = cache.resolveChannelName(context.Background(), "C1")
	if info.calls.Load() != 1 {
		t.Fatalf("conversations.info called %d times, want 1 (second lookup memoized)", info.calls.Load())
	}
}

// TestResolveChannelName_StandaloneCanvasHasNoName: an "FC:…" file conversation has
// no parent channel, so it returns no name and is NOT memoized (it must not poison
// exact-ID/default routing).
func TestResolveChannelName_StandaloneCanvasHasNoName(t *testing.T) {
	ch := &slack.Channel{}
	ch.NameNormalized = "FC:F0CANVAS:My Canvas"
	info := &countingCanvasInfo{channel: ch}
	cache := newChannelNameCache(&fakeChannelDirectory{}, info, time.Second, cacheTestLogger())

	if name, ok := cache.resolveChannelName(context.Background(), "C0CANVAS"); ok || name != "" {
		t.Fatalf("standalone resolve = (%q, %v), want (\"\", false)", name, ok)
	}
	if _, ok := cache.nameFor("C0CANVAS"); ok {
		t.Fatal("standalone canvas id was memoized; it must not be")
	}
}

func TestResolveChannelName_ErrorFallsThrough(t *testing.T) {
	info := &countingCanvasInfo{err: errors.New("slack down")}
	cache := newChannelNameCache(&fakeChannelDirectory{}, info, time.Second, cacheTestLogger())
	if name, ok := cache.resolveChannelName(context.Background(), "C1"); ok || name != "" {
		t.Fatalf("errored resolve = (%q, %v), want (\"\", false)", name, ok)
	}
	if _, ok := cache.nameFor("C1"); ok {
		t.Fatal("errored resolve must not memoize")
	}
}

func TestResolveChannelName_NilInfoDegradesToNameFor(t *testing.T) {
	cache := newChannelNameCache(&fakeChannelDirectory{}, nil, time.Second, cacheTestLogger())
	if _, ok := cache.resolveChannelName(context.Background(), "C1"); ok {
		t.Fatal("nil info must miss cleanly, no panic")
	}
}

// TestResolveChannelName_UnblocksNameGlobRouting is the routing-relevant seam: a
// canvas turn's channel, unknown to the warm cache, resolves via conversations.info
// so matchChannel's name-glob picks the configured agent (feature-* → coder). This
// is exactly what was silently defaulting before the fix.
func TestResolveChannelName_UnblocksNameGlobRouting(t *testing.T) {
	info := &countingCanvasInfo{channel: channelNamed("feature-xyz")}
	cache := newChannelNameCache(&fakeChannelDirectory{}, info, time.Second, cacheTestLogger())
	channels := map[string]config.ChannelConfig{"feature-*": {Agent: "coder"}}

	if _, ok := matchChannel("C1", "", channels); ok {
		t.Fatal("expected no name-glob match while the name is unresolved")
	}
	name, _ := cache.resolveChannelName(context.Background(), "C1")
	cc, ok := matchChannel("C1", name, channels)
	if !ok || cc.Agent != "coder" {
		t.Fatalf("after resolve, matchChannel = (%+v, %v), want coder", cc, ok)
	}
}

// fakeChannelDirectory is an in-memory channelDirectoryAPI. The channels it
// returns and any error are swappable under a lock so a test can change them
// between refreshes; calls counts the number of ListChannels invocations.
type fakeChannelDirectory struct {
	mu       sync.Mutex
	channels []slackclient.Channel
	err      error
	calls    atomic.Int32
}

func (f *fakeChannelDirectory) ListChannels(context.Context) ([]slackclient.Channel, error) {
	f.calls.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.channels, f.err
}

func (f *fakeChannelDirectory) set(channels []slackclient.Channel, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.channels = channels
	f.err = err
}

func cacheTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestChannelNameCacheWarmAndLookup(t *testing.T) {
	dir := &fakeChannelDirectory{channels: []slackclient.Channel{
		{ID: "C1", Name: "general"},
		{ID: "C2", Name: "feature-login"},
	}}
	cache := newChannelNameCache(dir, nil, time.Second, cacheTestLogger())

	if name, ok := cache.nameFor("C1"); ok || name != "" {
		t.Fatalf("before warm: got (%q, %v), want miss", name, ok)
	}
	if err := cache.refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if name, ok := cache.nameFor("C1"); !ok || name != "general" {
		t.Fatalf("after warm: got (%q, %v), want general", name, ok)
	}
	if name, ok := cache.nameFor("C2"); !ok || name != "feature-login" {
		t.Fatalf("after warm: got (%q, %v), want feature-login", name, ok)
	}
	if _, ok := cache.nameFor("C-unknown"); ok {
		t.Fatalf("unknown id should miss")
	}
}

func TestChannelNameCacheRefreshErrorKeepsPrevious(t *testing.T) {
	dir := &fakeChannelDirectory{channels: []slackclient.Channel{{ID: "C1", Name: "general"}}}
	cache := newChannelNameCache(dir, nil, time.Second, cacheTestLogger())
	if err := cache.refresh(context.Background()); err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	dir.set(nil, errors.New("slack is down"))
	if err := cache.refresh(context.Background()); err == nil {
		t.Fatalf("expected error from failing refresh")
	}
	// The prior contents must survive a transient failure.
	if name, ok := cache.nameFor("C1"); !ok || name != "general" {
		t.Fatalf("after failed refresh: got (%q, %v), want general kept", name, ok)
	}
}

func TestChannelNameCacheRefreshAsyncOnMiss(t *testing.T) {
	dir := &fakeChannelDirectory{channels: []slackclient.Channel{{ID: "C1", Name: "general"}}}
	cache := newChannelNameCache(dir, nil, time.Second, cacheTestLogger())

	// Simulate a brand-new channel: the resolver looks it up, misses, and kicks
	// off an async refresh that will learn it.
	if _, ok := cache.nameFor("C1"); ok {
		t.Fatalf("expected miss before any refresh")
	}
	cache.refreshAsync(context.Background())

	if !waitFor(func() bool {
		_, ok := cache.nameFor("C1")
		return ok
	}, time.Second) {
		t.Fatalf("async refresh did not populate the cache in time")
	}
	if name, ok := cache.nameFor("C1"); !ok || name != "general" {
		t.Fatalf("after async refresh: got (%q, %v), want general", name, ok)
	}
}

// TestChannelNameCacheRefreshAsyncDeduplicates checks that a burst of misses
// does not spawn many overlapping list calls: while one refresh is in flight,
// further refreshAsync calls are dropped.
func TestChannelNameCacheRefreshAsyncDeduplicates(t *testing.T) {
	release := make(chan struct{})
	dir := &blockingDirectory{release: release}
	cache := newChannelNameCache(dir, nil, time.Second, cacheTestLogger())

	for i := 0; i < 10; i++ {
		cache.refreshAsync(context.Background())
	}
	// Let the single in-flight refresh proceed, then wait for it to finish.
	close(release)
	if !waitFor(func() bool { return !cache.refreshing.Load() }, time.Second) {
		t.Fatalf("refresh did not settle")
	}
	if got := dir.calls.Load(); got != 1 {
		t.Fatalf("ListChannels calls = %d, want 1 (deduplicated)", got)
	}
}

// blockingDirectory blocks its first ListChannels until release is closed, so a
// test can observe the in-flight dedup window.
type blockingDirectory struct {
	release chan struct{}
	calls   atomic.Int32
}

func (b *blockingDirectory) ListChannels(context.Context) ([]slackclient.Channel, error) {
	b.calls.Add(1)
	<-b.release
	return []slackclient.Channel{{ID: "C1", Name: "general"}}, nil
}

func waitFor(cond func() bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return cond()
}

// TestChannelNameCacheNilSafe verifies the cache's methods are safe to call on a
// nil receiver, matching how the resolver treats it (channelCache may be nil).
func TestChannelNameCacheNilSafe(t *testing.T) {
	var cache *channelNameCache
	if name, ok := cache.nameFor("C1"); ok || name != "" {
		t.Fatalf("nil cache nameFor: got (%q, %v), want miss", name, ok)
	}
	cache.refreshAsync(context.Background()) // must not panic
	if err := cache.refresh(context.Background()); err != nil {
		t.Fatalf("nil cache refresh: %v", err)
	}
	cache.run(context.Background(), time.Minute) // must not panic / block
}
