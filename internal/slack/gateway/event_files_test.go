package gateway

import (
	"encoding/json"
	"testing"

	"github.com/slack-go/slack/socketmode"
)

func TestEventFilesParsesRawPayload(t *testing.T) {
	// slack-go's MessageEvent does not surface `files`, so eventFiles re-parses
	// the raw Events API envelope. This mirrors a DM file upload payload.
	payload := `{
		"team_id": "T1",
		"event": {
			"type": "message",
			"subtype": "file_share",
			"channel_type": "im",
			"text": "have a look",
			"files": [
				{"id": "F1", "name": "notes.txt", "mimetype": "text/plain", "filetype": "text", "size": 12, "url_private": "https://files/notes"}
			]
		}
	}`
	event := socketmode.Event{Request: &socketmode.Request{Payload: json.RawMessage(payload)}}

	files := eventFiles(event)
	if len(files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(files))
	}
	if files[0].Name != "notes.txt" || files[0].Mimetype != "text/plain" || files[0].URLPrivate != "https://files/notes" {
		t.Fatalf("file fields not parsed: %+v", files[0])
	}
}

func TestEventFilesEmptyWhenNoRequestOrNoFiles(t *testing.T) {
	if got := eventFiles(socketmode.Event{}); got != nil {
		t.Fatalf("nil request must yield nil, got %v", got)
	}
	event := socketmode.Event{Request: &socketmode.Request{Payload: json.RawMessage(`{"event":{"type":"message","text":"hi"}}`)}}
	if got := eventFiles(event); len(got) != 0 {
		t.Fatalf("message without files must yield none, got %v", got)
	}
}
