package creds

import (
	"bytes"
	"crypto/subtle"
	"strings"
)

type Digest string

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

func badConverted(a Digest, raw string) bool {
	return a == Digest(raw) // want `must be compared with crypto/subtle`
}

type record struct {
	Hash Digest
}

func badField(r record, a Digest) bool {
	return r.Hash == a // want `must be compared with crypto/subtle`
}

func badInferred(a, b Digest) bool {
	x := a
	y := b
	return x == y // want `must be compared with crypto/subtle`
}

func badConversionBothSides(a, b Digest) bool {
	return string(a) == string(b) // want `must be compared with crypto/subtle`
}

func badBytesEqual(a, b Digest) bool {
	return bytes.Equal([]byte(a), []byte(b)) // want `must be compared with crypto/subtle`
}

func badStringsCompare(a, b Digest) bool {
	return strings.Compare(string(a), string(b)) == 0 // want `must be compared with crypto/subtle`
}

func badEqualFold(a, b Digest) bool {
	return strings.EqualFold(string(a), string(b)) // want `must be compared with crypto/subtle`
}

func identsAreFine(a, b Ident) bool {
	return a == b
}

func launderedIsNotCaught(a, b Digest) bool {
	x, y := string(a), string(b)
	return x == y
}

func mapLookupIsNotCaught(known map[Digest]bool, a Digest) bool {
	return known[a]
}
