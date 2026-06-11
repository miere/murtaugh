package client

import (
	"strings"
	"testing"
	"time"
)

func TestFormatMessage_RendersSydneyTimeUserAndText(t *testing.T) {
	loc, _ := time.LoadLocation(SydneyTZ)
	when := time.Date(2025, 1, 2, 14, 30, 0, 0, loc)
	ts := SlackTS(when)
	got := FormatMessage(Message{TS: ts, User: "U1", Text: "hello"})
	want := "[14:30] @U1: hello"
	if got != want {
		t.Fatalf("FormatMessage = %q, want %q", got, want)
	}
}

func TestFormatMessage_UnknownUserWhenEmpty(t *testing.T) {
	loc, _ := time.LoadLocation(SydneyTZ)
	ts := SlackTS(time.Date(2025, 1, 2, 0, 0, 0, 0, loc))
	got := FormatMessage(Message{TS: ts, User: "", Text: "hi"})
	if !strings.Contains(got, "@unknown: hi") {
		t.Fatalf("FormatMessage = %q, want @unknown fallback", got)
	}
}

func TestFormatMessage_HandlesUnparseableTS(t *testing.T) {
	got := FormatMessage(Message{TS: "not-a-ts", User: "U1", Text: "x"})
	if !strings.Contains(got, "[??:??]") {
		t.Fatalf("FormatMessage = %q, want ??:?? placeholder", got)
	}
}

func TestFormatMessages_JoinsLines(t *testing.T) {
	loc, _ := time.LoadLocation(SydneyTZ)
	t1 := SlackTS(time.Date(2025, 1, 2, 10, 0, 0, 0, loc))
	t2 := SlackTS(time.Date(2025, 1, 2, 11, 0, 0, 0, loc))
	got := FormatMessages([]Message{
		{TS: t1, User: "U1", Text: "a"},
		{TS: t2, User: "U2", Text: "b"},
	})
	want := "[10:00] @U1: a\n[11:00] @U2: b"
	if got != want {
		t.Fatalf("FormatMessages = %q, want %q", got, want)
	}
}

func TestFormatMessages_EmptyIsEmptyString(t *testing.T) {
	if got := FormatMessages(nil); got != "" {
		t.Fatalf("FormatMessages(nil) = %q, want empty", got)
	}
}

func TestReverseMessages(t *testing.T) {
	in := []Message{{TS: "1"}, {TS: "2"}, {TS: "3"}}
	out := ReverseMessages(in)
	if len(out) != 3 || out[0].TS != "3" || out[1].TS != "2" || out[2].TS != "1" {
		t.Fatalf("ReverseMessages = %+v, want [3 2 1]", out)
	}
	if in[0].TS != "1" {
		t.Fatalf("ReverseMessages mutated input: %+v", in)
	}
}
