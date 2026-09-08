package toolset

import "strings"

// The tool partition: which of Murtaugh's tools a runtime node's agent may
// reach over the tool channel, and which are the gateway's alone.
//
// # Why this is a table and not a rule
//
// #170 Concern 6 states the trust decision plainly: the tools hold GATEWAY
// credentials, so a node on a developer's laptop reaching them is being handed
// production database access. The reason it cannot be derived is that nothing
// about a tools.Tool says what it holds — the interface is Name, Description,
// InputSchema, Invoke, and the credentials are closed over at construction in
// internal/app. slack.send-msg and ping are the same type. So the classification
// is authored, and the drift guard (internal/app) is what keeps it honest: a new
// registry family with no verdict here fails that test.
//
// # Why the zero value denies
//
// Reach's zero value is GatewayOnly. An unclassified family is therefore
// refused, and the failure mode of forgetting to classify something is a tool
// that does not cross rather than a credential that does.
//
// # Where it is enforced
//
// ONE place: internal/nodehost, on both tool.list and tool.call. The node is
// never asked to filter itself — it could not be trusted to, since a node admin
// owns their node's config and could list any family they liked in an agent's
// `tools:`. A node that asks for a tool it may not have is refused by the
// gateway.
//
// # What this is NOT
//
// It is not agentbuild's bridgeUnsafe, and it deliberately does not replace it.
// bridgeUnsafe answers "what may an external agent running IN THIS PROCESS see"
// and bans setup.* from it; this answers "what may leave this machine". They
// overlap and are not the same question, and folding them together would change
// the in-process surface — which item 8 must not do, because the in-process path
// is still the shipping default.
//
// It is also not a per-node capability grant. #170 puts those out of scope for
// this item and asks that adding policy later be "filtering what crosses it, not
// restructuring it": that is why the seam is a function of a tool NAME, with a
// static table behind it, rather than a rule baked into the call path.

// Reach says where a tool family may be executed from.
type Reach int

const (
	// ReachGatewayOnly is the zero value, and it denies. The tool holds a
	// gateway credential, administers the gateway, or reads and writes the
	// gateway's own files.
	ReachGatewayOnly Reach = iota
	// ReachNode means a node's agent may call it, executed on the gateway with
	// the gateway's context. These are the tools that carry no credential, or
	// whose whole purpose is to reach the user's Slack thread — which is on the
	// gateway by construction.
	ReachNode
	// ReachLocal means the tool is meaningless or wrong when executed on the
	// gateway for a node. It is not "denied for safety": serving it would
	// SUCCEED and do the wrong thing on the wrong machine.
	ReachLocal
)

// Family is one classified tool family — the namespace before the first dot,
// which is exactly the granularity an agent's `tools:` allowlist selects at.
type Family struct {
	Name  string
	Reach Reach
	// Why is the reason, in the terms a reviewer needs to disagree with it. It
	// is a field rather than a comment because the partition test prints it.
	Why string
}

// Families is the authoritative classification. Every registry family must
// appear here; internal/app's drift guard fails when one does not.
var Families = []Family{
	// ---- a node may reach these -------------------------------------------
	{Name: "ping", Reach: ReachNode, Why: "answers from anywhere and closes over nothing"},
	{Name: "version", Reach: ReachNode, Why: "returns a build string; the gateway's version is what a node wants to know"},
	{Name: "ask", Reach: ReachNode, Why: "puts a question in the user's own Slack thread — which only the gateway can do, so serving it here is the point rather than a concession"},
	{Name: "present_plan", Reach: ReachNode, Why: "same as ask: it renders Block Kit into the initiating thread and refuses cleanly with no thread"},

	// ---- the gateway's alone ----------------------------------------------
	{Name: "slack", Reach: ReachGatewayOnly, Why: "holds the daemon's bot token, and send-msg's attachment/blocks arguments are unrooted GATEWAY filesystem paths it will read and upload"},
	{Name: "jobs", Reach: ReachGatewayOnly, Why: "jobs.run is exec on the gateway host and carries the in-process delegator; jobs.define is the write half of it"},
	{Name: "cfg", Reach: ReachGatewayOnly, Why: "rewrites the gateway's configuration store, including who is authorised to use the gateway"},
	{Name: "setup", Reach: ReachGatewayOnly, Why: "writes LaunchAgents and replaces the running binary"},
	{Name: "node", Reach: ReachGatewayOnly, Why: "mints and revokes the credentials that authenticate nodes; a node reaching it is fleet-wide privilege escalation"},
	{Name: "journal", Reach: ReachGatewayOnly, Why: "reads and prunes every conversation of every user on the gateway"},
	{Name: "troubleshoot", Reach: ReachGatewayOnly, Why: "zips the gateway's config dir, journal and logs to a caller-supplied path"},
	{Name: "restart", Reach: ReachGatewayOnly, Why: "restarts the gateway daemon"},

	// ---- meaningless or wrong remotely ------------------------------------
	{Name: "auth", Reach: ReachLocal, Why: "auth.request writes the granted credential into the ENVIRONMENT of the process that asked; served gateway-side it would succeed and put the credential where the node's agent never looks"},
	// The four native groups are synthesized per agent from the agent's own
	// workspace root and are never registered, so a node's copies are already
	// local and nothing here can proxy them. They are classified anyway: if one
	// were ever registered globally, the zero value would have made it
	// gateway-only, and a gateway-served `attach` returns an absolute host path
	// the gateway then uploads — arbitrary gateway file exfiltration.
	{Name: GroupFiles, Reach: ReachLocal, Why: "synthesized per agent, rooted at the NODE's workspace"},
	{Name: GroupTerminal, Reach: ReachLocal, Why: "a shell on the gateway is not the shell the agent means"},
	{Name: GroupSkills, Reach: ReachLocal, Why: "served from the node's own binary and skills directory"},
	{Name: GroupAttach, Reach: ReachLocal, Why: "returns an absolute host path for upload; gateway-side that is arbitrary gateway file exfiltration"},
	{Name: GroupManage, Reach: ReachLocal, Why: "a skills-visibility token that registers no tool"},
}

// FamilyOf returns the family a tool name belongs to: the namespace before the
// first dot, or the whole name when it has none. This is the same rule
// registryMatches selects by, so the partition is expressed at exactly the
// granularity an allowlist can.
func FamilyOf(toolName string) string {
	if i := strings.IndexByte(toolName, '.'); i >= 0 {
		return toolName[:i]
	}
	return toolName
}

// ReachOf classifies one tool by name. An unclassified family reads as
// ReachGatewayOnly with a reason saying so, because the safe answer and the
// honest answer are the same one.
func ReachOf(toolName string) (Reach, string) {
	family := FamilyOf(toolName)
	for _, f := range Families {
		if f.Name == family {
			return f.Reach, f.Why
		}
	}
	return ReachGatewayOnly, "unclassified: no verdict in toolset.Families, so it is refused rather than assumed harmless"
}

// NodeMayReach reports whether a runtime node's agent may call this tool over
// the tool channel.
func NodeMayReach(toolName string) bool {
	reach, _ := ReachOf(toolName)
	return reach == ReachNode
}
