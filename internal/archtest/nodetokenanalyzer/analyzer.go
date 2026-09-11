// Package nodetokenanalyzer bans == on token digests: == leaks timing, and no
// behavioural test can tell it apart from a constant-time compare.
package nodetokenanalyzer

import (
	"go/ast"
	"go/token"
	"go/types"
	"strings"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/inspect"
	"golang.org/x/tools/go/ast/inspector"
)

const digestType = "Digest"

const guardedPackageSuffix = "/internal/nodetoken"

var Analyzer = &analysis.Analyzer{
	Name:     "nodetoken",
	Doc:      "flags any ==/!= or bytes.Equal comparison of a node-token Digest; the secret must be compared with crypto/subtle so verification does not leak how much of a guess was right",
	Requires: []*analysis.Analyzer{inspect.Analyzer},
	Run:      run,
}

const diagnostic = "a node-token Digest must be compared with crypto/subtle.ConstantTimeCompare (nodetoken.Equal), never with == or a byte-wise helper: verification is an unauthenticated path, and an early return leaks how many leading characters of a guess were right (#190)"

var equalHelpers = map[string]map[string]bool{
	"bytes":   {"Equal": true, "Compare": true},
	"strings": {"Compare": true, "EqualFold": true},
}

func run(pass *analysis.Pass) (any, error) {
	digest := digestNamed(pass.Pkg)
	if digest == nil {
		if strings.HasSuffix(pass.Pkg.Path(), guardedPackageSuffix) {
			pass.Reportf(packagePos(pass), "the node-token comparison guard is scoped to the %s type, which this package no longer declares; rename the guard along with it or the rule silently stops being enforced", digestType)
		}
		return nil, nil
	}
	insp, ok := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)
	if !ok {
		return nil, nil
	}
	insp.Preorder([]ast.Node{(*ast.BinaryExpr)(nil), (*ast.CallExpr)(nil)}, func(n ast.Node) {
		switch node := n.(type) {
		case *ast.BinaryExpr:
			if node.Op != token.EQL && node.Op != token.NEQ {
				return
			}
			if isDigest(pass, digest, node.X) || isDigest(pass, digest, node.Y) {
				pass.Reportf(node.Pos(), "%s", diagnostic)
			}
		case *ast.CallExpr:
			if !isEqualHelper(pass, node) {
				return
			}
			for _, arg := range node.Args {
				if isDigest(pass, digest, arg) {
					pass.Reportf(node.Pos(), "%s", diagnostic)
					return
				}
			}
		}
	})
	return nil, nil
}

func digestNamed(pkg *types.Package) *types.Named {
	obj := pkg.Scope().Lookup(digestType)
	if obj == nil {
		return nil
	}
	tn, ok := obj.(*types.TypeName)
	if !ok || tn.IsAlias() {
		return nil
	}
	named, _ := tn.Type().(*types.Named)
	return named
}

func isDigest(pass *analysis.Pass, digest *types.Named, expr ast.Expr) bool {
	tv, ok := pass.TypesInfo.Types[unwrapConversions(pass, expr)]
	if !ok || tv.Type == nil {
		return false
	}
	named, ok := tv.Type.(*types.Named)
	if !ok {
		return false
	}
	return named.Obj() == digest.Obj()
}

func unwrapConversions(pass *analysis.Pass, expr ast.Expr) ast.Expr {
	for {
		call, ok := expr.(*ast.CallExpr)
		if !ok || len(call.Args) != 1 {
			return expr
		}
		if tv, ok := pass.TypesInfo.Types[call.Fun]; !ok || !tv.IsType() {
			return expr
		}
		expr = call.Args[0]
	}
}

func isEqualHelper(pass *analysis.Pass, call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	fn, ok := pass.TypesInfo.Uses[sel.Sel].(*types.Func)
	if !ok || fn.Pkg() == nil {
		return false
	}
	return equalHelpers[fn.Pkg().Name()][fn.Name()]
}

func packagePos(pass *analysis.Pass) token.Pos {
	if len(pass.Files) == 0 {
		return token.NoPos
	}
	return pass.Files[0].Package
}
