package config

import (
	"encoding/json"
	"errors"
	"fmt"
)

// TriggerConfig is a polymorphic workflow-rule action stored, in YAML, as a
// single-key mapping ({action: config}) and decoded by its custom UnmarshalYAML.
// These JSON methods mirror that shape so workflow rules round-trip through the
// database's JSON bodies exactly as they do through YAML: MarshalJSON emits
// {"<action>": <config>} and UnmarshalJSON restores the matching sub-block.

// MarshalJSON renders the trigger as a single-key object keyed by its action.
func (t TriggerConfig) MarshalJSON() ([]byte, error) {
	out := make(map[string]any, 1)
	switch t.Type {
	case "reply-to-slack":
		out[t.Type] = t.ReplyToSlack
	case "run":
		out[t.Type] = t.Run
	case "delegate-to-agent":
		out[t.Type] = t.DelegateToAgent
	default:
		return nil, fmt.Errorf("unsupported trigger action %q", t.Type)
	}
	return json.Marshal(out)
}

// UnmarshalJSON restores a trigger from a single-key {action: config} object,
// mirroring UnmarshalYAML.
func (t *TriggerConfig) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if len(raw) != 1 {
		return errors.New("trigger must be an object with exactly one action")
	}
	for action, body := range raw {
		t.Type = action
		switch action {
		case "reply-to-slack":
			var cfg ReplyToSlackTriggerConfig
			if err := json.Unmarshal(body, &cfg); err != nil {
				return err
			}
			t.ReplyToSlack = &cfg
		case "run":
			var cfg RunTriggerConfig
			if err := json.Unmarshal(body, &cfg); err != nil {
				return err
			}
			t.Run = &cfg
		case "delegate-to-agent":
			var cfg DelegateToAgentConfig
			if err := json.Unmarshal(body, &cfg); err != nil {
				return err
			}
			t.DelegateToAgent = &cfg
		default:
			return fmt.Errorf("unsupported trigger action %q", action)
		}
	}
	return nil
}
