// Package depthcap implements rule XDRL002: a function that recursively walks a
// recursive Stellar XDR structure (ScVal, ScMap, ScVec, SorobanAuthorizedInvocation)
// must carry an explicit depth bound. Untrusted ledger data can nest these
// structures deeply; an unbounded walk lets any transaction submitter exhaust the
// indexer's stack or memory (see stellar/wallet-backend ingestion-halt fixes and
// the RFP section 3 requirement on depth and size caps).
package depthcap

import (
	"go/ast"
	"go/types"
	"strings"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/inspect"
	"golang.org/x/tools/go/ast/inspector"
)

// RuleID is the stable identifier reported with every finding.
const RuleID = "XDRL002"

// xdrPackages are the import paths whose XDR types are considered.
var xdrPackages = map[string]bool{
	"github.com/stellar/go-stellar-sdk/xdr": true,
	"github.com/stellar/go/xdr":             true,
}

// recursiveTypes are XDR types that can contain themselves.
var recursiveTypes = map[string]bool{
	"ScVal": true, "ScMap": true, "ScVec": true, "ScMapEntry": true,
	"SorobanAuthorizedInvocation": true, "SorobanAuthorizationEntry": true,
}

var Analyzer = &analysis.Analyzer{
	Name:     "depthcap",
	Doc:      RuleID + ": recursive walks over XDR ScVal or authorization trees must take a depth bound",
	Requires: []*analysis.Analyzer{inspect.Analyzer},
	Run:      run,
}

func run(pass *analysis.Pass) (any, error) {
	ins := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)
	ins.Preorder([]ast.Node{(*ast.FuncDecl)(nil)}, func(n ast.Node) {
		fn := n.(*ast.FuncDecl)
		obj, ok := pass.TypesInfo.Defs[fn.Name].(*types.Func)
		if !ok || fn.Body == nil {
			return
		}
		sig := obj.Type().(*types.Signature)
		xdrType, hasDepth := "", false
		for i := 0; i < sig.Params().Len(); i++ {
			t := sig.Params().At(i).Type()
			if name := recursiveXDRName(t); name != "" && xdrType == "" {
				xdrType = name
			}
			if isInteger(t) {
				hasDepth = true
			}
		}
		if xdrType == "" || hasDepth || !callsItself(pass, fn.Body, obj) {
			return
		}
		pass.Reportf(fn.Name.Pos(), "%s: %s recursively walks xdr.%s without a depth parameter; add a depth bound and reject inputs that exceed it", RuleID, fn.Name.Name, xdrType)
	})
	return nil, nil
}

func recursiveXDRName(t types.Type) string {
	for {
		switch u := t.(type) {
		case *types.Pointer:
			t = u.Elem()
			continue
		case *types.Slice:
			t = u.Elem()
			continue
		}
		break
	}
	named, ok := t.(*types.Named)
	if !ok || named.Obj().Pkg() == nil {
		return ""
	}
	path := strings.TrimSuffix(named.Obj().Pkg().Path(), "/")
	if xdrPackages[path] && recursiveTypes[named.Obj().Name()] {
		return named.Obj().Name()
	}
	return ""
}

func isInteger(t types.Type) bool {
	b, ok := t.Underlying().(*types.Basic)
	return ok && b.Info()&types.IsInteger != 0
}

func callsItself(pass *analysis.Pass, body *ast.BlockStmt, self *types.Func) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || found {
			return !found
		}
		var id *ast.Ident
		switch f := call.Fun.(type) {
		case *ast.Ident:
			id = f
		case *ast.SelectorExpr:
			id = f.Sel
		}
		if id != nil && pass.TypesInfo.Uses[id] == self {
			found = true
		}
		return true
	})
	return found
}
