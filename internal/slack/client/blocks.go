package client

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	slackgo "github.com/slack-go/slack"
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

// rawBlock is a Block Kit block carried as the exact JSON the caller supplied.
//
// slack-go decodes recognised block types into typed structs, and
// encoding/json silently discards any field those structs do not declare. A
// payload using a Block Kit feature newer than the pinned slack-go release
// therefore loses those fields on the way out, with no error at all — the
// message simply arrives missing pieces. (Wholly unrecognised block types are
// safe: slack-go keeps those verbatim in its own UnknownBlock. It is the
// partially-known types that lose data.)
//
// Caller-supplied JSON is never Murtaugh's to normalise, so it rides through
// untouched instead.
type rawBlock struct {
	blockType slackgo.MessageBlockType
	blockID   string
	raw       json.RawMessage
}

// BlockType satisfies slackgo.Block. Only the discriminator is decoded; it is
// not used to reinterpret the payload.
func (b rawBlock) BlockType() slackgo.MessageBlockType { return b.blockType }

// ID satisfies slackgo.Block.
func (b rawBlock) ID() string { return b.blockID }

// MarshalJSON returns the caller's bytes unchanged. slackgo.MsgOptionBlocks
// only stores the block; serialisation runs through Blocks.MarshalJSON, which
// marshals each element individually — so this is what reaches Slack.
func (b rawBlock) MarshalJSON() ([]byte, error) { return b.raw, nil }

// DecodeBlocks turns raw Block Kit JSON into slack-go blocks that marshal back
// to byte-identical JSON. Two shapes are accepted:
//
//   - a bare array of blocks — `[{"type":"section",…}]`
//   - a Block Kit Builder document — `{"blocks":[{"type":"section",…}]}`
//
// The second shape is what Block Kit Builder exports and what template files
// hold, so accepting it saves every caller from unwrapping by hand.
//
// Only "type" and "block_id" are read, to satisfy the slackgo.Block interface;
// every other field is opaque and passes through verbatim.
func DecodeBlocks(raw []byte) ([]slackgo.Block, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, nil
	}

	var elems []json.RawMessage
	switch trimmed[0] {
	case '[':
		if err := json.Unmarshal(trimmed, &elems); err != nil {
			return nil, fmt.Errorf("Error parsing blocks JSON: %s", err.Error())
		}
	case '{':
		var doc struct {
			Blocks []json.RawMessage `json:"blocks"`
		}
		if err := json.Unmarshal(trimmed, &doc); err != nil {
			return nil, fmt.Errorf("Error parsing blocks JSON: %s", err.Error())
		}
		elems = doc.Blocks
	default:
		return nil, fmt.Errorf(`Error parsing blocks JSON: expected an array of blocks or an object with a "blocks" key`)
	}

	blocks := make([]slackgo.Block, 0, len(elems))
	for i, elem := range elems {
		var header struct {
			Type    slackgo.MessageBlockType `json:"type"`
			BlockID string                   `json:"block_id"`
		}
		if err := json.Unmarshal(elem, &header); err != nil {
			return nil, fmt.Errorf("Error parsing blocks JSON: block %d: %s", i, err.Error())
		}
		if header.Type == "" {
			return nil, fmt.Errorf(`Error parsing blocks JSON: block %d has no "type"`, i)
		}
		blocks = append(blocks, rawBlock{blockType: header.Type, blockID: header.BlockID, raw: elem})
	}
	return blocks, nil
}
