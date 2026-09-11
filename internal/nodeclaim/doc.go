// Package nodeclaim turns a node's own configuration into the advertisement it
// sends the gateway, and notices when that configuration changes.
//
// It is the node side of #195. The gateway's half is internal/nodehost's
// registry; the wire form is agentwire.Advertisement; this package is the only
// thing that decides what a node's configuration MEANS as a claim.
//
// # A node advertises what it serves, not what it has configured
//
// The two differ and the difference is not cosmetic. A node's configuration can
// define six agent profiles while the process serves one — cmd/murtaugh-runtime
// picks a single agent, because the protocol carries no agent name and a link
// therefore IS an agent. Advertising the other five would be a claim the gateway
// could act on and the node could not honour, and the failure would surface as a
// conversation landing on a machine that answers with an error.
//
// The same rule prunes the claims: a channel rule routing to a profile this node
// does not serve is dropped before it is sent. It is not the gateway's job to
// discover that a claim is hollow.
//
// # What the advertisement deliberately does not carry
//
// **`allow_anyone`.** It waives the gateway's own access list, and a node admin
// writing it could open the gateway to the whole workspace from their laptop.
//
// **`reply_on_thread`.** It decides whether a channel's messages thread, which
// decides the conversation key, which is what a pin is keyed by. It belongs to
// the gateway for the same reason the agent name does.
//
// **DMs.** Every node's configuration resolves a DM to *some* agent — that is
// what chat.defaults.agent is — so a DM claim would be a claim every node makes
// about every DM, which selects nothing. #170's algorithm already answers this
// without a claim: a conversation no node claims round-robins across the fleet,
// which is exactly the intended behaviour for a DM. There is deliberately no
// default node and none is needed.
//
// # The node has no configuration reload, and this is not one
//
// cmd/murtaugh-runtime reads its configuration once and builds its agent from
// that snapshot. Watcher does NOT change that, and must not: both backend
// families latch their toolset — native at its first Initialize, acp and
// claude_code when their aggregator registers a first session — and the redial
// loop reuses the agent.Client captured at startup. Rebuilding it under a live
// connection would strand every open session behind a client nothing points at
// any more. So this watcher re-reads the CLAIM SET and nothing else. Changing
// which agent a node serves still takes a restart, and that is stated here
// rather than discovered from a node that quietly stopped answering.
//
// # It applies changes unconditionally, and that is the difference from the
// gateway's watcher
//
// internal/app's config watcher shows an admin a diff and rolls the edit back if
// nobody approves. That flow needs Slack, an admin, and a Block Kit card, none
// of which a node has — and more to the point it would be the wrong policy: a
// node admin editing their own node is the authority on that node, which is what
// #170 means by two administrative roles. What is reused is the pure part:
// config.SnapshotChanged over config.Store.Snapshot, which compares renderings
// so a store that re-encoded a body without changing its meaning does not read
// as a change.
//
// **One trap while the configuration split (#170 item 12) is unbuilt.** A
// gateway and a node on one machine share ~/.config/murtaugh/config.yaml today.
// So a "node-side" edit made on a --role both box is also seen by the GATEWAY's
// watcher, which will post an approval card and, if nobody answers, write the
// edit back out again. The claim will change and then change back. That is the
// shared store, not this watcher.
package nodeclaim
