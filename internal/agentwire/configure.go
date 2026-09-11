package agentwire

import "encoding/json"

type NodeConfiguration struct {
	Agents map[string]json.RawMessage `json:"agents,omitempty"`
	Chat   json.RawMessage            `json:"chat,omitempty"`
	// Env holds the node owner's own provider credentials; the gateway relays them
	// and never keeps them.
	Env map[string]string `json:"env,omitempty"`
}

func (c NodeConfiguration) Empty() bool {
	return len(c.Agents) == 0 && len(c.Chat) == 0 && len(c.Env) == 0
}

type NodeConfigured struct {
	Applied int `json:"applied"`
	// Restarting is needed because both agent backend families fix their toolset at
	// first use, so a process built with no agent cannot grow one.
	Restarting bool `json:"restarting,omitempty"`
}
