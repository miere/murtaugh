// Package nodetoken mints, stores the hash of, and verifies the bearer token a
// runtime node presents to the gateway (#190, part of the split in #170).
//
// # The token IS the identity
//
// A node never tells the gateway who it is. It presents a token; the gateway
// looks up which node and which user that token was minted for. This is the
// whole point of the design and it is why there is no `node_id` field anywhere
// in the wire format: inside a fleet, a node that announces its own identity
// can announce someone else's, and every authorisation decision downstream
// inherits the lie.
//
// # The token's shape, and why it has two parts
//
//	mrtg_node_<selector>_<secret>
//
// The SELECTOR is a public credential id. It is what the store is keyed by, so
// verification is one indexed lookup rather than a scan.
//
// The SECRET is 256 bits from crypto/rand. Only its SHA-256 digest is stored,
// and the presented secret's digest is compared against the stored one with
// crypto/subtle.
//
// A single-part token — "the digest IS the primary key" — would follow the same
// SHAPE the rest of this repo's side stores take: the identifying value used
// directly as the key, with no separate opaque id (config.JobRunClaim is keyed
// by (job, occurrence), the two values themselves). The analogy stops at the
// shape — no side store here holds a digest of anything. It was rejected
// here for one reason: it performs the comparison inside the database index,
// which leaves the constant-time comparison #190 asks for with nothing to
// compare. Splitting the credential is what makes that requirement real rather
// than decorative: the lookup is on a value that is not a secret, and the
// secret is compared in Go, in constant time.
//
// SHA-256 and not a password KDF, deliberately: the secret is a high-entropy
// random string, not a password, so there is no dictionary to slow an attacker
// down through. A KDF here would buy nothing and cost every handshake.
//
// # The prefix
//
// `mrtg_node_` exists so a leaked token is greppable in a log and findable by a
// secret scanner. It is also what internal/troubleshoot redacts on.
//
// # Where the plaintext is allowed to exist
//
// Mint returns it, once. Nothing in this package stores it in a struct that
// outlives the call, and no method on config.NodeTokenStore returns one — the
// record carries a digest and never the secret. The only durable copy is the
// one the operator puts on the node (see PathFor and WriteFile: mode 0600, in
// the config directory, where the sandbox rule can name it — and read PathFor
// on what that does and does not protect it from).
package nodetoken
