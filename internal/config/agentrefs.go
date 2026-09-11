package config

import (
	"fmt"
	"sort"
	"strings"
)

// AgentReference.Field is the path an operator edits ("chat.defaults.agent", not "Agent"),
// because it goes into the message they read.
type AgentReference struct {
	Field string
	Name  string
}

// Keep in step with Validate's name checks: a site missing here is a typo a gateway never reports.
// Blank names are skipped because Validate already reports them for every role.
func (c Config) AgentReferences() []AgentReference {
	var refs []AgentReference
	add := func(field, name string) {
		if name = strings.TrimSpace(name); name != "" {
			refs = append(refs, AgentReference{Field: field, Name: name})
		}
	}

	if c.Chat.Enabled {
		add("chat.defaults.agent", c.Chat.Defaults.Agent)
		add("chat.defaults.dm_agent", c.Chat.Defaults.DMAgent)
		users := make([]string, 0, len(c.Chat.Defaults.DMAgents))
		for user := range c.Chat.Defaults.DMAgents {
			users = append(users, user)
		}
		sort.Strings(users)
		for _, user := range users {
			add(fmt.Sprintf("chat.defaults.dm_agents[%s]", user), c.Chat.Defaults.DMAgents[user])
		}
		for _, rule := range c.Chat.Channels {
			add(fmt.Sprintf("chat.channels[%s].agent", strings.TrimSpace(rule.Match)), rule.Agent)
		}
	}

	for _, name := range sortedKeys(c.Jobs) {
		add(fmt.Sprintf("jobs[%s].agent", name), c.Jobs[name].Agent)
	}
	for _, name := range sortedKeys(c.WorkflowRules) {
		for i, trigger := range c.WorkflowRules[name].Triggers {
			for _, d := range triggerDelegates(trigger) {
				add(fmt.Sprintf("workflow-rules[%s].trigger[%d].agent", name, i), d.Agent)
			}
		}
	}
	for _, name := range sortedKeys(c.UnfurlRules) {
		if d := c.UnfurlRules[name].Unfurl.DelegateToAgent; d != nil {
			add(fmt.Sprintf("unfurl-rules[%s].unfurl.agent", name), d.Agent)
		}
	}
	return refs
}

func triggerDelegates(t TriggerConfig) []*DelegateToAgentConfig {
	var out []*DelegateToAgentConfig
	if t.DelegateToAgent != nil {
		out = append(out, t.DelegateToAgent)
	}
	if t.ReplyToSlack != nil && t.ReplyToSlack.DelegateToAgent != nil {
		out = append(out, t.ReplyToSlack.DelegateToAgent)
	}
	return out
}

func sortedKeys[T any](m map[string]T) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Never treat the result as fatal: a freshly restarted gateway serves no names yet, so it would
// refuse its own working configuration until a node attached.
func UnresolvedAgents(refs []AgentReference, served []string) []AgentReference {
	if len(refs) == 0 {
		return nil
	}
	have := make(map[string]bool, len(served))
	for _, name := range served {
		have[strings.TrimSpace(name)] = true
	}
	var out []AgentReference
	for _, ref := range refs {
		if !have[ref.Name] {
			out = append(out, ref)
		}
	}
	return out
}
