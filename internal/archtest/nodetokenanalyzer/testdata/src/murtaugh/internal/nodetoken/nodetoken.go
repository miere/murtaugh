package nodetoken // want `the node-token comparison guard is scoped to the Digest type, which this package no longer declares`

type hash string

func same(a, b hash) bool { return a == b }
