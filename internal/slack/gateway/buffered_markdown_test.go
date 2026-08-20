package gateway

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// bufferedBlocks posts text through a bufferedSink and returns the Block Kit
// document rendered for each resulting message, alongside the poster so the
// notification fallback and threading can be asserted too.
//
// The documents are re-rendered through replyDocument rather than read back off
// the captured MsgOptions: slack-go folds blocks into the request only at build
// time, so they are not visible in the applied form values.
func bufferedBlocks(t *testing.T, text string, opts StreamWriterOptions) (*fakePoster, []blockDoc) {
	t.Helper()
	opts.Logger = discardLogger()
	poster := &fakePoster{}
	sink := newBufferedSink(poster, "C1", opts)
	ctx := context.Background()

	if err := sink.Append(ctx, text); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := sink.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	docs := make([]blockDoc, 0, len(poster.posts))
	for i, post := range poster.posts {
		raw := sink.replyDocument(ctx, post.text)
		if raw == nil {
			t.Fatalf("post %d: reply document did not render", i)
		}
		var doc blockDoc
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("post %d: blocks are not valid JSON: %v\n%s", i, err, raw)
		}
		docs = append(docs, doc)
	}
	return poster, docs
}

type blockDoc struct {
	Blocks []block `json:"blocks"`
}

type block struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	Elements []struct {
		Elements []struct {
			Type   string `json:"type"`
			UserID string `json:"user_id"`
			Range  string `json:"range"`
		} `json:"elements"`
	} `json:"elements"`
}

// The buffered transport carries standard Markdown in a markdown block, so the
// same prose renders identically on both transports.
func TestBufferedSink_PostsMarkdownBlock(t *testing.T) {
	poster, docs := bufferedBlocks(t, "Text can be **bold** and `code`.", StreamWriterOptions{})

	if len(docs) != 1 || len(docs[0].Blocks) != 1 {
		t.Fatalf("blocks = %+v, want a single markdown block", docs)
	}
	if docs[0].Blocks[0].Type != "markdown" {
		t.Fatalf("block type = %q, want markdown", docs[0].Blocks[0].Type)
	}
	if docs[0].Blocks[0].Text != "Text can be **bold** and `code`." {
		t.Fatalf("block text = %q", docs[0].Blocks[0].Text)
	}
	// Slack reads `text` for the push notification even when blocks render.
	if poster.posts[0].text == "" {
		t.Fatal("fallback text is empty; mobile notifications would degrade")
	}
}

// A markdown block will not resolve <@U…>, so the reference becomes readable
// text inline and is re-emitted as a rich_text footer that actually notifies.
func TestBufferedSink_RelocatesMentions(t *testing.T) {
	opts := StreamWriterOptions{
		ResolveUserName: func(_ context.Context, id string) string {
			return map[string]string{"U1": "Miere"}[id]
		},
	}
	_, docs := bufferedBlocks(t, "asked <@U1> to look, cc <!here>", opts)

	if len(docs[0].Blocks) != 2 {
		t.Fatalf("blocks = %d, want markdown + rich_text footer", len(docs[0].Blocks))
	}
	if want := "asked @Miere to look, cc @here"; docs[0].Blocks[0].Text != want {
		t.Fatalf("inline text = %q, want %q", docs[0].Blocks[0].Text, want)
	}

	elements := docs[0].Blocks[1].Elements[0].Elements
	var kinds []string
	for _, e := range elements {
		if e.Type == "user" || e.Type == "broadcast" {
			kinds = append(kinds, e.Type+":"+e.UserID+e.Range)
		}
	}
	if len(kinds) != 2 || kinds[0] != "user:U1" || kinds[1] != "broadcast:here" {
		t.Fatalf("footer = %v, want the user and the broadcast", kinds)
	}
}

// No tags means no footer — the tagged-user list appears only when it has work
// to do.
func TestBufferedSink_NoFooterWithoutMentions(t *testing.T) {
	_, docs := bufferedBlocks(t, "nothing to ping here", StreamWriterOptions{})
	if len(docs[0].Blocks) != 1 {
		t.Fatalf("blocks = %d, want the markdown block alone", len(docs[0].Blocks))
	}
}

// The rendered document must actually ride on the post, not merely render.
func TestBufferedSink_AttachesBlocksToPost(t *testing.T) {
	sink := newBufferedSink(&fakePoster{}, "C1", StreamWriterOptions{Logger: discardLogger()})

	options, err := sink.postOptions(context.Background(), "hello")
	if err != nil {
		t.Fatalf("postOptions: %v", err)
	}
	if len(options) != 2 {
		t.Fatalf("options = %d, want fallback text + blocks", len(options))
	}
}

// A template an operator has broken must not cost the user their reply: the
// turn is already finished, so posting it unformatted beats failing.
func TestBufferedSink_FallsBackToPlainTextWhenTemplateBroken(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "templates", "reply"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	broken := filepath.Join(dir, "templates", "reply", "markdown.json")
	if err := os.WriteFile(broken, []byte(`{{ this is not a template`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	poster := &fakePoster{}
	sink := newBufferedSink(poster, "C1", StreamWriterOptions{TemplateDir: dir, Logger: discardLogger()})
	ctx := context.Background()

	options, err := sink.postOptions(ctx, "hello")
	if err != nil {
		t.Fatalf("postOptions: %v", err)
	}
	if len(options) != 1 {
		t.Fatalf("options = %d, want the fallback text alone", len(options))
	}

	if err := sink.Append(ctx, "hello"); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := sink.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if len(poster.posts) != 1 || poster.posts[0].text != "hello" {
		t.Fatalf("posts = %+v, want the reply delivered as plain text", poster.posts)
	}
}

// Mentions are scoped to the message they appear in: a reply long enough to
// split must not ping someone named only in a later part.
func TestBufferedSink_ScopesMentionsPerChunk(t *testing.T) {
	first := strings.Repeat("a", maxBufferedPostChars-20) + " <@U1>"
	text := first + "\n" + strings.Repeat("b", 50) + " <@U2>"

	_, docs := bufferedBlocks(t, text, StreamWriterOptions{})
	if len(docs) != 2 {
		t.Fatalf("posts = %d, want the reply split in two", len(docs))
	}

	for i, want := range []string{"U1", "U2"} {
		if len(docs[i].Blocks) != 2 {
			t.Fatalf("post %d: blocks = %d, want a footer", i, len(docs[i].Blocks))
		}
		elements := docs[i].Blocks[1].Elements[0].Elements
		var users []string
		for _, e := range elements {
			if e.Type == "user" {
				users = append(users, e.UserID)
			}
		}
		if len(users) != 1 || users[0] != want {
			t.Fatalf("post %d: footer users = %v, want only %s", i, users, want)
		}
	}
}
