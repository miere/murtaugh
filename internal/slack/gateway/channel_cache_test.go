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

// canvasConversation builds the shape a file-backed canvas conversation has:
// no name, and a name_normalized naming the FILE. Both a channel-tab canvas and
// a standalone one look like this — which is exactly why the parent must come
// from files.info rather than from the conversation.
func canvasConversation(fileID string) *slack.Channel {
	ch := &slack.Channel{}
	ch.NameNormalized = "FC:" + fileID + ":My Canvas"
	return ch
}

// stubCanvasParent is a canvasParentResolver returning a fixed answer, counting
// calls so memoization is observable.
type stubCanvasParent struct {
	id    string
	name  string
	err   error
	calls atomic.Int32
}

func (s *stubCanvasParent) CanvasParentChannel(context.Context, string) (string, string, error) {
	s.calls.Add(1)
	return s.id, s.name, s.err
}

// sequencedCanvasInfo answers conversations.info per channel id, so a test can
// model the canvas conversation and its parent channel distinctly.
type sequencedCanvasInfo struct {
	byID  map[string]*slack.Channel
	calls atomic.Int32
}

func (s *sequencedCanvasInfo) GetConversationInfoContext(_ context.Context, in *slack.GetConversationInfoInput) (*slack.Channel, error) {
	s.calls.Add(1)
	return s.byID[in.ChannelID], nil
}

// TestResolveChannelName_CanvasRoutesByParentChannel is the regression this fix
// exists for: a Canvas TAB reports name_normalized "FC:<fileId>:…" exactly like a
// standalone doc, so the conversation cannot name its channel — but files.info
// can, and shares carries channel_name inline (no second lookup). Mirrors the
// live case F0BLEABKSCD → C0BLBB1LTUK "nc-proj-spdd" → the nc-* rule → coder,
// which had been silently answering with the default agent instead.
func TestResolveChannelName_CanvasRoutesByParentChannel(t *testing.T) {
	info := &countingCanvasInfo{channel: canvasConversation("F0BLEABKSCD")}
	parent := &stubCanvasParent{id: "C0BLBB1LTUK", name: "nc-proj-spdd"}
	cache := newChannelNameCache(&fakeChannelDirectory{}, info, parent, time.Second, cacheTestLogger())

	name, ok := cache.resolveChannelName(context.Background(), "C0BLEABKSCD")
	if !ok || name != "nc-proj-spdd" {
		t.Fatalf("canvas resolve = (%q, %v), want (nc-proj-spdd, true)", name, ok)
	}
	// Memoized under the CANVAS id — that is what routing looks up.
	if n2, ok2 := cache.nameFor("C0BLEABKSCD"); !ok2 || n2 != "nc-proj-spdd" {
		t.Fatalf("nameFor(canvas) = (%q, %v), want it memoized as the parent name", n2, ok2)
	}
	_, _ = cache.resolveChannelName(context.Background(), "C0BLEABKSCD")
	if parent.calls.Load() != 1 {
		t.Fatalf("files.info called %d times, want 1 (second lookup memoized)", parent.calls.Load())
	}

	// The routing seam: the resolved name must now match the channel's glob.
	cc, matched := matchChannel("C0BLEABKSCD", name, map[string]config.ChannelConfig{"nc-*": {Agent: "coder"}})
	if !matched || cc.Agent != "coder" {
		t.Fatalf("matchChannel = (%+v, %v), want coder", cc, matched)
	}
}

// TestResolveChannelName_CanvasParentNameFromConversationsInfo: when files.info
// yields only an id (the groups/channels lists carry no channel_name), the parent
// name is finished off with one ordinary conversations.info on that parent.
func TestResolveChannelName_CanvasParentNameFromConversationsInfo(t *testing.T) {
	info := &sequencedCanvasInfo{byID: map[string]*slack.Channel{
		"C0CANVAS": canvasConversation("F0CANVAS"),
		"C0PARENT": channelNamed("nc-proj-spdd"),
	}}
	parent := &stubCanvasParent{id: "C0PARENT"} // id only, no name
	cache := newChannelNameCache(&fakeChannelDirectory{}, info, parent, time.Second, cacheTestLogger())

	name, ok := cache.resolveChannelName(context.Background(), "C0CANVAS")
	if !ok || name != "nc-proj-spdd" {
		t.Fatalf("canvas resolve = (%q, %v), want (nc-proj-spdd, true)", name, ok)
	}
	if info.calls.Load() != 2 {
		t.Fatalf("conversations.info called %d times, want 2 (canvas, then parent)", info.calls.Load())
	}
}

// TestResolveChannelName_CanvasWithNoParentFallsThrough: a canvas shared into no
// channel has no parent to route by. It must report a miss and NOT be memoized,
// so exact-ID/default routing stays intact rather than caching a wrong answer.
func TestResolveChannelName_CanvasWithNoParentFallsThrough(t *testing.T) {
	info := &countingCanvasInfo{channel: canvasConversation("F0CANVAS")}
	for _, tc := range []struct {
		name   string
		parent canvasParentResolver
	}{
		{"unshared", &stubCanvasParent{}},
		{"lookup failed", &stubCanvasParent{err: errors.New("missing_scope")}},
		{"resolver disabled", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cache := newChannelNameCache(&fakeChannelDirectory{}, info, tc.parent, time.Second, cacheTestLogger())
			if name, ok := cache.resolveChannelName(context.Background(), "C0CANVAS"); ok || name != "" {
				t.Fatalf("resolve = (%q, %v), want (\"\", false)", name, ok)
			}
			if _, ok := cache.nameFor("C0CANVAS"); ok {
				t.Fatal("a canvas with no resolvable parent must not be memoized")
			}
		})
	}
}

// TestCanvasParentChannel_PrefersSharesAndIsDeterministic pins the selection
// rules on the files.info payload: private before public (a tab on a private
// channel leaves `channels` empty), shares before the bare id lists (shares
// carry the name), and sorted ids so a multi-channel canvas routes identically
// on every turn instead of following Go's randomised map order.
func TestCanvasParentChannel_PrefersSharesAndIsDeterministic(t *testing.T) {
	share := func(name string) []slack.ShareFileInfo {
		return []slack.ShareFileInfo{{ChannelName: name}}
	}
	for _, tc := range []struct {
		name     string
		file     slack.File
		wantID   string
		wantName string
	}{
		{
			name: "private share wins over public",
			file: slack.File{Shares: slack.Share{
				Private: map[string][]slack.ShareFileInfo{"C0PRIV": share("nc-proj-spdd")},
				Public:  map[string][]slack.ShareFileInfo{"C0PUB": share("general")},
			}},
			wantID: "C0PRIV", wantName: "nc-proj-spdd",
		},
		{
			name: "shares win over the bare id lists",
			file: slack.File{
				Channels: []string{"C0LIST"},
				Shares:   slack.Share{Public: map[string][]slack.ShareFileInfo{"C0PUB": share("general")}},
			},
			wantID: "C0PUB", wantName: "general",
		},
		{
			name: "multi-channel share picks the lowest id, every time",
			file: slack.File{Shares: slack.Share{Public: map[string][]slack.ShareFileInfo{
				"C0ZZZ": share("zeta"), "C0AAA": share("alpha"), "C0MMM": share("mu"),
			}}},
			wantID: "C0AAA", wantName: "alpha",
		},
		{
			name:   "groups fall back to an id with no name",
			file:   slack.File{Groups: []string{"C0GRP"}},
			wantID: "C0GRP", wantName: "",
		},
		{
			name: "unshared canvas resolves to nothing",
			file: slack.File{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resolver := slackCanvasParent{api: &stubFileAPI{file: tc.file}}
			// Repeat: map iteration order varies per run, so a single pass would
			// not catch non-determinism.
			for i := 0; i < 20; i++ {
				id, name, err := resolver.CanvasParentChannel(context.Background(), "F0CANVAS")
				if err != nil {
					t.Fatalf("CanvasParentChannel: %v", err)
				}
				if id != tc.wantID || name != tc.wantName {
					t.Fatalf("= (%q, %q), want (%q, %q)", id, name, tc.wantID, tc.wantName)
				}
			}
		})
	}
}

// stubFileAPI is a canvasFileAPI returning a fixed files.info payload.
type stubFileAPI struct {
	file slack.File
	err  error
}

func (s *stubFileAPI) GetFileInfoContext(context.Context, string, int, int) (*slack.File, []slack.Comment, *slack.Paging, error) {
	if s.err != nil {
		return nil, nil, nil, s.err
	}
	f := s.file
	return &f, nil, nil, nil
}

// TestResolveChannelName_ResolvesAndMemoizesOnMiss: a channel canvas whose parent
// channel isn't in the warm list resolves via conversations.info and is cached.
func TestResolveChannelName_ResolvesAndMemoizesOnMiss(t *testing.T) {
	info := &countingCanvasInfo{channel: channelNamed("feature-xyz")}
	cache := newChannelNameCache(&fakeChannelDirectory{}, info, nil, time.Second, cacheTestLogger())

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

func TestResolveChannelName_ErrorFallsThrough(t *testing.T) {
	info := &countingCanvasInfo{err: errors.New("slack down")}
	cache := newChannelNameCache(&fakeChannelDirectory{}, info, nil, time.Second, cacheTestLogger())
	if name, ok := cache.resolveChannelName(context.Background(), "C1"); ok || name != "" {
		t.Fatalf("errored resolve = (%q, %v), want (\"\", false)", name, ok)
	}
	if _, ok := cache.nameFor("C1"); ok {
		t.Fatal("errored resolve must not memoize")
	}
}

func TestResolveChannelName_NilInfoDegradesToNameFor(t *testing.T) {
	cache := newChannelNameCache(&fakeChannelDirectory{}, nil, nil, time.Second, cacheTestLogger())
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
	cache := newChannelNameCache(&fakeChannelDirectory{}, info, nil, time.Second, cacheTestLogger())
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
	cache := newChannelNameCache(dir, nil, nil, time.Second, cacheTestLogger())

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
	cache := newChannelNameCache(dir, nil, nil, time.Second, cacheTestLogger())
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
	cache := newChannelNameCache(dir, nil, nil, time.Second, cacheTestLogger())

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
	cache := newChannelNameCache(dir, nil, nil, time.Second, cacheTestLogger())

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
