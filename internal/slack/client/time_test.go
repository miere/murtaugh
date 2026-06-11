package client

import (
	"strings"
	"testing"
	"time"
)

func TestParseSince_EmptyDefaultsTo24hSydney(t *testing.T) {
	loc, err := time.LoadLocation(SydneyTZ)
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	got, err := ParseSince("")
	if err != nil {
		t.Fatalf("ParseSince: %v", err)
	}
	if got.Location().String() != loc.String() {
		t.Fatalf("ParseSince location = %s, want %s", got.Location(), loc)
	}
	delta := time.Now().In(loc).Sub(got)
	if delta < 23*time.Hour+59*time.Minute || delta > 24*time.Hour+1*time.Minute {
		t.Fatalf("ParseSince(empty) is %s ago, want ~24h", delta)
	}
}

func TestParseSince_ValidLiteralIsSydney(t *testing.T) {
	got, err := ParseSince("2025-01-02 03:04:05")
	if err != nil {
		t.Fatalf("ParseSince: %v", err)
	}
	loc, _ := time.LoadLocation(SydneyTZ)
	want := time.Date(2025, 1, 2, 3, 4, 5, 0, loc)
	if !got.Equal(want) {
		t.Fatalf("ParseSince = %s, want %s", got, want)
	}
}

func TestParseSince_InvalidReturnsCLIMessage(t *testing.T) {
	_, err := ParseSince("yesterday")
	if err == nil {
		t.Fatalf("ParseSince should fail")
	}
	if !strings.Contains(err.Error(), "must be in format 'YYYY-MM-DD HH:mm:ss'") {
		t.Fatalf("ParseSince err = %q, want CLI-style message", err.Error())
	}
}

func TestSlackTS_HasMicrosecondPrecision(t *testing.T) {
	loc, _ := time.LoadLocation(SydneyTZ)
	in := time.Date(2025, 1, 2, 3, 4, 5, 123456000, loc)
	got := SlackTS(in)
	if !strings.Contains(got, ".") {
		t.Fatalf("SlackTS = %q, want fractional seconds", got)
	}
	dot := strings.Index(got, ".")
	if frac := got[dot+1:]; len(frac) != 6 {
		t.Fatalf("SlackTS fractional part = %q, want 6 digits", frac)
	}
}
