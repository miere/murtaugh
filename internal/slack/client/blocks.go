package client

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// ResolveBlocks turns a user-supplied blocks argument into the raw JSON bytes
// the SlackAPI layer expects. An empty (or whitespace-only) input returns
// (nil, nil), meaning "no blocks". A non-empty input is classified by its
// first non-whitespace byte: '[' or '{' means the caller passed JSON inline;
// anything else means the caller passed a path to a file containing JSON,
// which is read off disk. In both cases the result is validated with
// json.Valid before being returned, so callers never see malformed payloads.
//
// Errors are prefixed with "Error parsing blocks JSON:" so error rendering
// stays consistent across the inline and file branches.
func ResolveBlocks(input string) ([]byte, error) {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return nil, nil
	}

	if first := trimmed[0]; first == '[' || first == '{' {
		if !json.Valid([]byte(trimmed)) {
			return nil, fmt.Errorf("Error parsing blocks JSON: invalid JSON")
		}
		return []byte(trimmed), nil
	}

	data, err := os.ReadFile(trimmed)
	if err != nil {
		return nil, fmt.Errorf("Error parsing blocks JSON: cannot read file %s: %s", trimmed, err.Error())
	}
	if !json.Valid(data) {
		return nil, fmt.Errorf("Error parsing blocks JSON: file %s does not contain valid JSON", trimmed)
	}
	return data, nil
}
