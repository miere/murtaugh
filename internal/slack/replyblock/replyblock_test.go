package replyblock

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestRewriteResolvesUserMentions(t *testing.T) {
	names := map[string]string{"U1": "Miere", "U2": "Ana"}
	resolve := func(id string) string { return names[id] }

	text, mentions := Rewrite("ping <@U1> and <@U2> please", resolve)

	if want := "ping @Miere and @Ana please"; text != want {
		t.Fatalf("text = %q, want %q", text, want)
	}
	want := []Mention{{Kind: KindUser, ID: "U1"}, {Kind: KindUser, ID: "U2"}}
	if !reflect.DeepEqual(mentions, want) {
		t.Fatalf("mentions = %+v, want %+v", mentions, want)
	}
}

// An unresolvable id must still notify: the name is cosmetic, the mention is not.
func TestRewriteFailsSoftWhenNameUnresolved(t *testing.T) {
	for _, tc := range []struct {
		name    string
		resolve func(string) string
	}{
		{"no resolver", nil},
		{"resolver returns empty", func(string) string { return "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text, mentions := Rewrite("hi <@U9>", tc.resolve)
			if want := "hi @U9"; text != want {
				t.Fatalf("text = %q, want %q", text, want)
			}
			if len(mentions) != 1 || mentions[0].ID != "U9" {
				t.Fatalf("mentions = %+v, want the raw id collected", mentions)
			}
		})
	}
}

// The resolver reports the current display name; the "|label" form is legacy.
func TestRewritePrefersResolverOverInlineLabel(t *testing.T) {
	text, _ := Rewrite("<@U1|stale>", func(string) string { return "Miere" })
	if want := "@Miere"; text != want {
		t.Fatalf("text = %q, want %q", text, want)
	}

	text, _ = Rewrite("<@U1|stale>", nil)
	if want := "@stale"; text != want {
		t.Fatalf("unresolved text = %q, want %q", text, want)
	}
}

func TestRewriteDeduplicatesPreservingOrder(t *testing.T) {
	_, mentions := Rewrite("<@U2> <@U1> <@U2> <@U1>", nil)
	want := []Mention{{Kind: KindUser, ID: "U2"}, {Kind: KindUser, ID: "U1"}}
	if !reflect.DeepEqual(mentions, want) {
		t.Fatalf("mentions = %+v, want %+v", mentions, want)
	}
}

func TestRewriteBroadcastsAndUserGroups(t *testing.T) {
	text, mentions := Rewrite("<!here> <!channel> <!subteam^S7|@platform>", nil)

	if want := "@here @channel @platform"; text != want {
		t.Fatalf("text = %q, want %q", text, want)
	}
	want := []Mention{
		{Kind: KindBroadcast, ID: "here"},
		{Kind: KindBroadcast, ID: "channel"},
		{Kind: KindUserGroup, ID: "S7"},
	}
	if !reflect.DeepEqual(mentions, want) {
		t.Fatalf("mentions = %+v, want %+v", mentions, want)
	}
}

// Channel refs flatten: there is nothing to notify, so nothing to relocate.
func TestRewriteChannelRefsCollectNoMention(t *testing.T) {
	text, mentions := Rewrite("see <#C1|general> and <#C2>", nil)
	if want := "see #general and #C2"; text != want {
		t.Fatalf("text = %q, want %q", text, want)
	}
	if len(mentions) != 0 {
		t.Fatalf("mentions = %+v, want none", mentions)
	}
}

// A reference Slack may add later passes through rather than being guessed at,
// and a plain autolink is left for the Markdown renderer.
func TestRewriteLeavesUnknownRefsAndLinksAlone(t *testing.T) {
	in := "<!futurething> and <https://example.com|docs>"
	text, mentions := Rewrite(in, nil)
	if text != in {
		t.Fatalf("text = %q, want it unchanged", text)
	}
	if len(mentions) != 0 {
		t.Fatalf("mentions = %+v, want none", mentions)
	}
}

func TestRenderProducesMarkdownBlockOnly(t *testing.T) {
	doc := renderOrFail(t, "**bold** text", nil)

	if len(doc.Blocks) != 1 {
		t.Fatalf("blocks = %d, want 1 (no mentions means no footer)", len(doc.Blocks))
	}
	if doc.Blocks[0].Type != "markdown" {
		t.Fatalf("block type = %q, want markdown", doc.Blocks[0].Type)
	}
	if doc.Blocks[0].Text != "**bold** text" {
		t.Fatalf("text = %q", doc.Blocks[0].Text)
	}
}

func TestRenderAppendsMentionFooter(t *testing.T) {
	doc := renderOrFail(t, "hi @Miere", []Mention{
		{Kind: KindUser, ID: "U1"},
		{Kind: KindBroadcast, ID: "here"},
		{Kind: KindUserGroup, ID: "S7"},
	})

	if len(doc.Blocks) != 2 {
		t.Fatalf("blocks = %d, want 2", len(doc.Blocks))
	}
	if doc.Blocks[1].Type != "rich_text" {
		t.Fatalf("footer type = %q, want rich_text", doc.Blocks[1].Type)
	}

	elements := doc.Blocks[1].Elements[0].Elements
	var got []string
	for _, e := range elements {
		switch e.Type {
		case "user":
			got = append(got, "user:"+e.UserID)
		case "broadcast":
			got = append(got, "broadcast:"+e.Range)
		case "usergroup":
			got = append(got, "usergroup:"+e.UsergroupID)
		case "emoji":
			got = append(got, "emoji:"+e.Name)
		}
	}
	want := []string{"user:U1", "broadcast:here", "usergroup:S7", "emoji:point_up"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("footer elements = %v, want %v", got, want)
	}
}

// Agent prose is untrusted: a value that closes its JSON string literal could
// otherwise append sibling blocks of its own choosing.
func TestRenderEscapesAgentText(t *testing.T) {
	hostile := "\" , \"evil\": 1, \"x\": \"\nline\ttab\\slash"
	doc := renderOrFail(t, hostile, nil)

	if len(doc.Blocks) != 1 {
		t.Fatalf("blocks = %d, want 1 — text escaped into structure", len(doc.Blocks))
	}
	if doc.Blocks[0].Text != hostile {
		t.Fatalf("text = %q, want it round-tripped verbatim", doc.Blocks[0].Text)
	}
}

type blockDoc struct {
	Blocks []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		Elements []struct {
			Elements []struct {
				Type        string `json:"type"`
				UserID      string `json:"user_id"`
				Range       string `json:"range"`
				UsergroupID string `json:"usergroup_id"`
				Name        string `json:"name"`
				Text        string `json:"text"`
			} `json:"elements"`
		} `json:"elements"`
	} `json:"blocks"`
}

func renderOrFail(t *testing.T, text string, mentions []Mention) blockDoc {
	t.Helper()
	raw, err := NewRenderer("", nil).Render(text, mentions)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	var doc blockDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("rendered invalid JSON: %v\n%s", err, strings.TrimSpace(string(raw)))
	}
	return doc
}
