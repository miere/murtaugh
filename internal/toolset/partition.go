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
// internal/app. slack.send_msg and ping are the same type. So the classification
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
	{Name: "help", Reach: ReachNode, Why: "reads tool schemas and holds no credential; refusing it leaves a node's agent guessing at arguments"},
	{Name: "ask", Reach: ReachNode, Why: "puts a question in the user's own Slack thread — which only the gateway can do, so serving it here is the point rather than a concession"},
	{Name: "present_plan", Reach: ReachNode, Why: "same as ask: it renders Block Kit into the initiating thread and refuses cleanly with no thread"},

	// ---- the gateway's alone ----------------------------------------------
	{Name: "slack", Reach: ReachGatewayOnly, Why: "holds the daemon's bot token AND the admin's personal user token, and send_msg's attachment/blocks arguments are unrooted GATEWAY filesystem paths it will read and upload"},
	// slack.send_msg is the one exception, and it is per TOOL rather than per
	// family — see Tools below.
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

// Tool is one classified TOOL, overriding its family's verdict.
//
// Exceptions are a second table and not a second column on Family because they
// must stay rare and must stay conspicuous: an allowlist of whole families with
// two named holes in it is reviewable, and a per-tool table with fifty rows in
// it is the classification having quietly become per-tool.
type Tool struct {
	Name  string
	Reach Reach
	// DenyArgs are arguments refused when the call arrives over the tool
	// channel, and stripped from the schema the node is offered so the model
	// never learns they exist. It is what makes a per-tool exception possible at
	// all: the family verdict is usually about ONE argument rather than about
	// the whole tool, and without this the choice is the whole tool or nothing.
	DenyArgs []string
	Why      string
}

// Tools is the per-tool exception table. Everything not named here takes its
// family's verdict.
var Tools = []Tool{
	// The reason this exception exists is #199. A scheduled job's entire output
	// mechanism is the agent posting for itself — RunAndForget discards the text
	// on purpose — so "post the result to #ops", which is what most job prompts
	// say, stops working the moment the agent is on a node. Leaving it would
	// have shipped a broker-executed job that runs, does its work, and tells
	// nobody: exactly the silent failure the item exists to prevent, arriving by
	// a different door.
	//
	// THREE credentials are in reach of this one tool, and the grant is only
	// safe once all three are accounted for. Counting two of them is how the
	// first version of this exception shipped with the third one open.
	//
	// 1. The BOT token is not an argument and never crosses: the call executes
	// GATEWAY-side with the gateway's own client, like every other ReachNode
	// tool, so a node's agent can ask for a message to be posted and still
	// cannot read the credential that posts it — the same trade `ask` and
	// `present_plan` already make.
	//
	// 2. The gateway's FILESYSTEM, via `attachment` and `blocks`. Both take
	// unrooted GATEWAY paths that the tool reads and uploads, so a node's agent
	// naming one would be reading a file off the gateway.
	//
	// 3. The ADMIN's personal xoxp- token, via `as`. Its own description is
	// "admin posts as the human admin via their Slack user token", and
	// sendmsg.Tool acts on it with `case "admin": client = t.adminClient` — no
	// caller-identity check, and no tools.ApprovalClassifier, so the gate never
	// fires either. A node's agent naming it would post AS THE HUMAN, in any
	// channel that human can post in, from what may be somebody else's laptop.
	// That is not "a job reports its own result": it is impersonation, and it is
	// exactly the capability the family verdict ("holds the daemon's bot token")
	// was written to withhold. The bot token staying gateway-side says nothing
	// about it, because it is a different credential.
	//
	// So (2) and (3) are denied by name and stripped from the schema the node is
	// offered. What is left is: post text, to a channel, as the app.
	//
	// The rest of the family stays gateway-only. Reading a workspace's history
	// (fetch_msgs, fetch_reactions), editing somebody else's message
	// (update_msg), creating channels and writing canvases are not what a job
	// reporting its own result needs, and each is a wider capability than the
	// one being granted here.
	{
		Name: "slack.send_msg", Reach: ReachNode,
		DenyArgs: []string{"attachment", "blocks", "as"},
		Why:      "a headless job's only way to report its result is to post it; the bot token stays gateway-side because the call executes there, while the two arguments that reach PAST it — attachment/blocks (gateway file paths) and as=admin (the admin's own user token) — are denied by name",
	},
}

// toolRule returns the per-tool exception for a name, if there is one.
func toolRule(toolName string) (Tool, bool) {
	for _, t := range Tools {
		if t.Name == toolName {
			return t, true
		}
	}
	return Tool{}, false
}

// DeniedArgs are the arguments a node may not supply to this tool. Empty for
// everything without a per-tool exception, which is everything but one tool.
func DeniedArgs(toolName string) []string {
	rule, ok := toolRule(strings.TrimSpace(toolName))
	if !ok {
		return nil
	}
	return rule.DenyArgs
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
	toolName = strings.TrimSpace(toolName)
	if rule, ok := toolRule(toolName); ok {
		return rule.Reach, rule.Why
	}
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
