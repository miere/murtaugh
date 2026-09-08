// Package nodetokenanalyzer is a go/analysis pass enforcing one rule of the node
// credential design (#190): a token digest is never compared with ==.
//
// # The rule, and why a test could not carry it
//
// Verification is an unauthenticated path — the caller is by definition somebody
// who may be guessing — so the comparison of the presented secret's digest
// against the stored one has to take the same time whether the first character
// matched or the last. Go's `==` on strings returns at the first differing byte.
//
// Nothing observable distinguishes the two. `Equal` returns exactly the same
// verdicts either way, so every behavioural test in the package passes with the
// constant-time compare torn out; and a timing test would be flaky on a shared
// machine and would still not be checking what its name claimed. The only guard
// that actually holds is a static one, which is what this is.
//
// # What it flags
//
// Any `==` or `!=` in the guarded package where either operand is a `Digest` —
// as a variable, a struct field, an inferred local, or wrapped in explicit
// conversions (`string(hash) == …`) — plus a call to bytes.Equal,
// bytes.Compare, strings.Compare or strings.EqualFold on one, which are the same
// mistake spelled as a helper.
//
// # What it does not flag, said plainly
//
// A map keyed by Digest compares digests inside the runtime, and that is not
// reported: the pass looks at comparison expressions, not at every operation
// with comparison semantics. Nor does it follow a digest through an intermediate
// variable — `x := string(a); x == y` has no Digest operand left at the
// comparison, and chasing that through arbitrary data flow is not something a
// single-package pass can do honestly. The rule catches the direct, local
// mistake — the shape a well-meaning simplification actually takes — and it is
// not a proof.
//
// # Scoping, and why the guard cannot decay silently
//
// The pass keys off a type NAME in a package PATH. Either could be renamed,
// which would ordinarily turn the guard into a no-op that still passes CI, so
// the reverse is also checked: the guarded package is reported if it does not
// declare the type at all. The guard fails loudly rather than evaporating.
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

// digestType is the named type whose values must never meet an ==.
const digestType = "Digest"

// guardedPackageSuffix is the package that MUST declare digestType. A fixture
// package declaring its own Digest is checked too; only this one is required to.
const guardedPackageSuffix = "/internal/nodetoken"

// Analyzer reports ordinary comparisons of a node-token digest.
var Analyzer = &analysis.Analyzer{
	Name:     "nodetoken",
	Doc:      "flags any ==/!= or bytes.Equal comparison of a node-token Digest; the secret must be compared with crypto/subtle so verification does not leak how much of a guess was right",
	Requires: []*analysis.Analyzer{inspect.Analyzer},
	Run:      run,
}

const diagnostic = "a node-token Digest must be compared with crypto/subtle.ConstantTimeCompare (nodetoken.Equal), never with == or a byte-wise helper: verification is an unauthenticated path, and an early return leaks how many leading characters of a guess were right (#190)"

// equalHelpers are the standard-library comparisons that are `==` under another
// name. They read as harmless, which is exactly why they need naming.
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

// digestNamed returns the package's own Digest type, or nil when it declares
// none.
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

// isDigest reports whether expr — or the value inside any explicit conversions
// wrapping it — has the guarded Digest type.
//
// Types, not syntax: an inferred variable and a struct field both arrive here as
// the same named type. Conversions are peeled because `[]byte(hash)` and
// `string(hash)` are how a digest is spelled at the moment somebody hands it to
// bytes.Equal, and a pass that looked only at the outermost type would see a
// []byte and wave every one of them through.
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

// unwrapConversions peels explicit type conversions off expr. It stops at
// anything that is not a conversion, which is why a digest laundered through an
// intermediate variable (`x := string(a); x == y`) is out of reach — see the
// package doc's statement of the limit.
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

// isEqualHelper reports whether call is one of the standard-library byte-wise
// comparisons listed in equalHelpers.
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

// packagePos returns a position inside the package to hang a package-level
// diagnostic on. Any file will do; the message is about the package.
func packagePos(pass *analysis.Pass) token.Pos {
	if len(pass.Files) == 0 {
		return token.NoPos
	}
	return pass.Files[0].Package
}
