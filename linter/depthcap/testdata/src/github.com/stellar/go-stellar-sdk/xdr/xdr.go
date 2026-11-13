// Package xdr is a minimal stub of the Stellar XDR types used by the analyzer tests.
package xdr

type ScVal struct {
	Vec *ScVec
	Map *ScMap
}
type ScVec []ScVal
type ScMapEntry struct{ Key, Val ScVal }
type ScMap []ScMapEntry
type SorobanAuthorizedInvocation struct{ SubInvocations []SorobanAuthorizedInvocation }
