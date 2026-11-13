package example

import "github.com/stellar/go-stellar-sdk/xdr"

// Seeded violation: unbounded recursion over ScVal.
func countLeaves(v xdr.ScVal) int { // want `XDRL002: countLeaves recursively walks xdr.ScVal without a depth parameter`
	n := 1
	if v.Vec != nil {
		for _, c := range *v.Vec {
			n += countLeaves(c)
		}
	}
	return n
}

// Seeded violation: unbounded recursion over an authorization tree.
func walkAuth(inv *xdr.SorobanAuthorizedInvocation) { // want `XDRL002: walkAuth recursively walks xdr.SorobanAuthorizedInvocation without a depth parameter`
	for i := range inv.SubInvocations {
		walkAuth(&inv.SubInvocations[i])
	}
}

// Compliant: recursion with an explicit depth bound.
func countLeavesBounded(v xdr.ScVal, depth int) int {
	if depth > 64 {
		return 0
	}
	n := 1
	if v.Vec != nil {
		for _, c := range *v.Vec {
			n += countLeavesBounded(c, depth+1)
		}
	}
	return n
}

// Compliant: not recursive.
func isMap(v xdr.ScVal) bool { return v.Map != nil }
