package client

import (
	"encoding/json"
	"strings"
	"testing"

	slackgo "github.com/slack-go/slack"
)

// marshalBlocks renders decoded blocks the way slack-go does on the wire:
// Blocks.MarshalJSON marshals the set, delegating to each element.
func marshalBlocks(t *testing.T, blocks []slackgo.Block) string {
	t.Helper()
	out, err := json.Marshal(slackgo.Blocks{BlockSet: blocks})
	if err != nil {
		t.Fatalf("marshal blocks: %v", err)
	}
	return string(out)
}

// The regression this whole change exists for: card carries subtext and
// slack_icon, which the pinned slack-go CardBlock does not declare. Round
// tripping through the typed structs dropped them silently.
func TestDecodeBlocks_PreservesFieldsUnknownToSlackGo(t *testing.T) {
	in := `[{"type":"card","title":{"type":"plain_text","text":"Authentication Required"},` +
		`"slack_icon":{"type":"icon","name":"user"},` +
		`"subtext":{"type":"mrkdwn","text":"Triggered by ` + "`Miere`" + `"}}]`

	blocks, err := DecodeBlocks([]byte(in))
	if err != nil {
		t.Fatalf("DecodeBlocks: %v", err)
	}
	got := marshalBlocks(t, blocks)
	if got != in {
		t.Fatalf("round trip altered payload:\n got %s\nwant %s", got, in)
	}
	for _, field := range []string{"slack_icon", "subtext"} {
		if !strings.Contains(got, field) {
			t.Errorf("%q was dropped in the round trip", field)
		}
	}
}

func TestDecodeBlocks_BareArrayRoundTripsByteIdentical(t *testing.T) {
	in := `[{"type":"section","text":{"type":"mrkdwn","text":"hi"}},{"type":"divider"}]`
	blocks, err := DecodeBlocks([]byte(in))
	if err != nil {
		t.Fatalf("DecodeBlocks: %v", err)
	}
	if len(blocks) != 2 {
		t.Fatalf("len(blocks) = %d, want 2", len(blocks))
	}
	if got := marshalBlocks(t, blocks); got != in {
		t.Fatalf("marshal = %s, want %s", got, in)
	}
}

// Block Kit Builder exports {"blocks":[…]}, and template files hold the same
// shape, so both are accepted.
func TestDecodeBlocks_AcceptsBuilderDocument(t *testing.T) {
	in := `{"blocks":[{"type":"divider"}]}`
	blocks, err := DecodeBlocks([]byte(in))
	if err != nil {
		t.Fatalf("DecodeBlocks: %v", err)
	}
	if got, want := marshalBlocks(t, blocks), `[{"type":"divider"}]`; got != want {
		t.Fatalf("marshal = %s, want %s", got, want)
	}
}

func TestDecodeBlocks_ExposesTypeAndBlockID(t *testing.T) {
	blocks, err := DecodeBlocks([]byte(`[{"type":"actions","block_id":"form-actions"}]`))
	if err != nil {
		t.Fatalf("DecodeBlocks: %v", err)
	}
	if got := blocks[0].BlockType(); got != slackgo.MessageBlockType("actions") {
		t.Errorf("BlockType() = %q, want actions", got)
	}
	if got := blocks[0].ID(); got != "form-actions" {
		t.Errorf("ID() = %q, want form-actions", got)
	}
}

func TestDecodeBlocks_EmptyInputIsNoBlocks(t *testing.T) {
	for _, in := range []string{"", "   ", "{}", "[]"} {
		blocks, err := DecodeBlocks([]byte(in))
		if err != nil {
			t.Fatalf("DecodeBlocks(%q): %v", in, err)
		}
		if len(blocks) != 0 {
			t.Fatalf("DecodeBlocks(%q) = %d blocks, want 0", in, len(blocks))
		}
	}
}

func TestDecodeBlocks_RejectsMalformedAndUntypedBlocks(t *testing.T) {
	cases := map[string]string{
		"not json":           `[{"type":`,
		"scalar":             `"just a string"`,
		"block without type": `[{"text":"no type here"}]`,
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeBlocks([]byte(in)); err == nil {
				t.Fatalf("DecodeBlocks(%q) = nil error, want failure", in)
			}
		})
	}
}
