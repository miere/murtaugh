package config

import (
	"fmt"
	"sort"
	"strings"
)

// This file enumerates every place a configuration names an agent PROFILE.
//
// It exists because #198 moves those checks from write time to connect time.
// Validate can no longer answer them on a gateway — it holds no bodies — so the
// question has to be askable somewhere else, against a different source of
// truth: the profiles the attaching user's fleet advertises. That check lives in
// internal/nodehost, and it needs the list of names to check.
//
// The one rule to keep: this list and the name→body checks in Validate are the
// same set, seen from two sides. A reference added to Validate and not here is a
// name whose typo the gateway silently never reports; one added here and not
// there is a name a node's own configuration stops rejecting.
//
// The two directions are guarded by two different tests, because they are two
// different questions. agentrefs_test.go pins this list against a hard-coded
// seven, which catches a site removed from it. agentrefs_guard_test.go runs
// Validate under both roles over a configuration naming a different agent at
// each of the seven, and counts the role-gated checks in this package's own
// SOURCE against a pinned number — because a site that does not exist yet cannot
// be run, and adding an eighth to Validate without adding it here is the
// direction a contributor actually takes.

// AgentReference is one place a configuration names an agent profile.
//
// Field is the configuration path in the form an operator would edit — it goes
// into the message they read, so "chat.defaults.agent" rather than "Agent".
type AgentReference struct {
	Field string
	Name  string
}

// AgentReferences returns every agent name this configuration references, in a
// stable order.
//
// Deterministic because it is reported: a list that reordered itself between two
// identical configurations would journal a different event for the same fact,
// and the operator reading it twice would think something changed.
//
// A blank name is skipped. Blankness is a separate error that Validate raises
// for every role (it needs no body to detect), and reporting it here as well
// would name the same problem twice in two different places. That separation is
// what makes three of the sites below raise their blank EXPLICITLY rather than
// letting the body lookup fail on it: a blank caught only by the lookup is
// deferred on a gateway along with the lookup, and skipped here, so it is
// reported nowhere.
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
		// Map iteration is random, so this one sub-list is sorted by its key
		// before it is walked. Everything else below has an order of its own.
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

// triggerDelegates returns the delegate-to-agent blocks one trigger carries,
// both the top-level action and the one nested under reply-to-slack.
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

// sortedKeys returns a map's keys in a stable order.
func sortedKeys[T any](m map[string]T) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// UnresolvedAgents returns the references whose name is not in served.
//
// It is the connect-time half of the check Validate used to make. served is the
// union of the profile names one FLEET advertises — not every connected node's,
// because delegation is fleet-scoped: a name only Bob's laptop serves is no use
// to Alice, and reporting it as resolved would say a configuration works for a
// user it cannot work for.
//
// An empty served set yields every reference, which is correct and is exactly
// why this must never be fatal: a gateway that has just restarted has an empty
// registry, and one that refused its own working configuration until a node
// arrived could never start at all.
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
