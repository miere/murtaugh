package client

import (
	"errors"
	"testing"
)

func TestNewClient_EmptyTokenReturnsErrTokenMissing(t *testing.T) {
	for _, tok := range []string{"", "   ", "\t"} {
		if _, err := NewClient(tok); !errors.Is(err, ErrTokenMissing) {
			t.Fatalf("NewClient(%q) err = %v, want ErrTokenMissing", tok, err)
		}
	}
}

func TestNewClient_TokenConstructsClient(t *testing.T) {
	c, err := NewClient("xoxb-test")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if c == nil || c.api == nil {
		t.Fatalf("NewClient returned %+v, want a constructed client", c)
	}
}

func TestLazyClient_CachesTokenError(t *testing.T) {
	lc := NewLazyClient("")
	if _, err := lc.Get(); !errors.Is(err, ErrTokenMissing) {
		t.Fatalf("Get err = %v, want ErrTokenMissing", err)
	}
	// Second call must return the same cached error.
	if _, err := lc.Get(); !errors.Is(err, ErrTokenMissing) {
		t.Fatalf("Get (cached) err = %v, want ErrTokenMissing", err)
	}
}
