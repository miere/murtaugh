// Package creds is a fixture declaring its own Digest, exercising the shapes the
// guard is meant to catch and the shapes it deliberately leaves alone.
package creds

import (
	"bytes"
	"crypto/subtle"
	"strings"
)

type Digest string

// Ident is a non-secret identifier. Comparing one is ordinary and must not be
// flagged — that is the scoping claim, that the rule follows the TYPE rather
// than the package.
type Ident string

func good(a, b Digest) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func bad(a, b Digest) bool {
	return a == b // want `must be compared with crypto/subtle`
}

func badNotEqual(a, b Digest) bool {
	return a != b // want `must be compared with crypto/subtle`
}

// One side converted from a plain string still has a Digest operand, so it is
// still reported.
func badConverted(a Digest, raw string) bool {
	return a == Digest(raw) // want `must be compared with crypto/subtle`
}

// A struct field is the quietest spelling: no conversion, no helper, and the
// type is only visible through the declaration.
type record struct {
	Hash Digest
}

func badField(r record, a Digest) bool {
	return r.Hash == a // want `must be compared with crypto/subtle`
}

// Inference hides the type from the reader but not from the type checker.
func badInferred(a, b Digest) bool {
	x := a
	y := b
	return x == y // want `must be compared with crypto/subtle`
}

// A conversion is how a digest is spelled at the moment it is compared the
// wrong way, so the operand is peeled before its type is judged.
func badConversionBothSides(a, b Digest) bool {
	return string(a) == string(b) // want `must be compared with crypto/subtle`
}

// `==` under another name.
func badBytesEqual(a, b Digest) bool {
	return bytes.Equal([]byte(a), []byte(b)) // want `must be compared with crypto/subtle`
}

func badStringsCompare(a, b Digest) bool {
	return strings.Compare(string(a), string(b)) == 0 // want `must be compared with crypto/subtle`
}

func badEqualFold(a, b Digest) bool {
	return strings.EqualFold(string(a), string(b)) // want `must be compared with crypto/subtle`
}

// --- the stated limits, pinned as assertions rather than left as oversights ---

// Comparing an ordinary identifier is not the rule's business.
func identsAreFine(a, b Ident) bool {
	return a == b
}

// A digest laundered through an intermediate variable has no Digest operand
// left by the time the comparison happens. The pass does not chase values
// through data flow and does not pretend to.
func launderedIsNotCaught(a, b Digest) bool {
	x, y := string(a), string(b)
	return x == y
}

// A map lookup compares digests inside the runtime. Not a comparison
// expression, not reported.
func mapLookupIsNotCaught(known map[Digest]bool, a Digest) bool {
	return known[a]
}
