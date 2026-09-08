// Package nodetoken is a fixture at the ONE package path the guard requires the
// Digest type to exist at. It deliberately does not declare it, so the pass
// reports the guard's own decay: a rename that quietly turned the rule into a
// no-op would otherwise pass CI.
package nodetoken // want `the node-token comparison guard is scoped to the Digest type, which this package no longer declares`

type hash string

func same(a, b hash) bool { return a == b }
