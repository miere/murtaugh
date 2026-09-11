// Package renderclockanalyzer keeps clocks out of chatRenderer: some events, like
// heartbeats, never reach a renderer, so a timer there misreads healthy work.
package renderclockanalyzer

import (
	"go/ast"
	"go/token"
	"go/types"
	"strings"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/inspect"
	"golang.org/x/tools/go/ast/inspector"
)

const rendererInterface = "chatRenderer"

const guardedPackageSuffix = "/internal/slack/gateway"

var Analyzer = &analysis.Analyzer{
	Name:     "renderclock",
	Doc:      "flags any use of package time inside a chatRenderer implementation; liveness is measured at the inbound event edge, never on the Slack write path",
	Requires: []*analysis.Analyzer{inspect.Analyzer},
	Run:      run,
}

const diagnostic = "a chatRenderer implementation must not observe time: the Slack write path sees rendered output, not events, so a detector here is blind during healthy work (#170 Concern 4). Put the timer at the inbound event edge that feeds this renderer (eventTranslator for a chat turn, backgroundEventsRouter for a background stretch) instead"

func run(pass *analysis.Pass) (any, error) {
	iface := rendererIface(pass.Pkg)
	if iface == nil {
		if strings.HasSuffix(pass.Pkg.Path(), guardedPackageSuffix) {
			pass.Reportf(packagePos(pass), "the renderclock guard is scoped to the %s interface, which this package no longer declares; rename the guard along with it or the rule silently stops being enforced", rendererInterface)
		}
		return nil, nil
	}
	guarded := implementers(pass.Pkg, iface)
	if len(guarded) == 0 {
		return nil, nil
	}
	for _, named := range guarded {
		reportClockFields(pass, named)
	}
	insp, ok := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)
	if !ok {
		return nil, nil
	}
	insp.Preorder([]ast.Node{(*ast.FuncDecl)(nil)}, func(n ast.Node) {
		fn := n.(*ast.FuncDecl)
		if fn.Recv == nil || !isGuardedMethod(pass, fn, guarded) {
			return
		}
		reportClockUses(pass, fn.Type)
		if fn.Body != nil {
			reportClockUses(pass, fn.Body)
		}
	})
	return nil, nil
}

func reportClockUses(pass *analysis.Pass, n ast.Node) {
	ast.Inspect(n, func(n ast.Node) bool {
		ident, ok := n.(*ast.Ident)
		if !ok {
			return true
		}
		if inTimePackage(pass.TypesInfo.Uses[ident]) {
			pass.Reportf(ident.Pos(), "%s", diagnostic)
		}
		return true
	})
}

func rendererIface(pkg *types.Package) *types.Interface {
	obj := pkg.Scope().Lookup(rendererInterface)
	if obj == nil {
		return nil
	}
	iface, _ := obj.Type().Underlying().(*types.Interface)
	return iface
}

func implementers(pkg *types.Package, iface *types.Interface) []*types.Named {
	var out []*types.Named
	for _, name := range pkg.Scope().Names() {
		tn, ok := pkg.Scope().Lookup(name).(*types.TypeName)
		if !ok || tn.IsAlias() {
			continue
		}
		named, ok := tn.Type().(*types.Named)
		if !ok {
			continue
		}
		if _, isIface := named.Underlying().(*types.Interface); isIface {
			continue
		}
		if types.Implements(named, iface) || types.Implements(types.NewPointer(named), iface) {
			out = append(out, named)
		}
	}
	return out
}

func isGuardedMethod(pass *analysis.Pass, fn *ast.FuncDecl, guarded []*types.Named) bool {
	def, _ := pass.TypesInfo.Defs[fn.Name].(*types.Func)
	if def == nil {
		return false
	}
	sig, ok := def.Type().(*types.Signature)
	if !ok || sig.Recv() == nil {
		return false
	}
	recv := namedOf(sig.Recv().Type())
	if recv == nil {
		return false
	}
	for _, named := range guarded {
		if named.Obj() == recv.Obj() {
			return true
		}
	}
	return false
}

func reportClockFields(pass *analysis.Pass, named *types.Named) {
	st, ok := named.Underlying().(*types.Struct)
	if !ok {
		return
	}
	for i := range st.NumFields() {
		field := st.Field(i)
		if mentionsTime(field.Type(), map[types.Type]bool{}) {
			pass.Reportf(field.Pos(), "%s", diagnostic)
		}
	}
}

func mentionsTime(t types.Type, seen map[types.Type]bool) bool {
	if t == nil || seen[t] {
		return false
	}
	seen[t] = true
	switch tt := t.(type) {
	case *types.Named:
		return inTimePackage(tt.Obj())
	case *types.Pointer:
		return mentionsTime(tt.Elem(), seen)
	case *types.Slice:
		return mentionsTime(tt.Elem(), seen)
	case *types.Array:
		return mentionsTime(tt.Elem(), seen)
	case *types.Chan:
		return mentionsTime(tt.Elem(), seen)
	case *types.Map:
		return mentionsTime(tt.Key(), seen) || mentionsTime(tt.Elem(), seen)
	case *types.Signature:
		return tupleMentionsTime(tt.Params(), seen) || tupleMentionsTime(tt.Results(), seen)
	default:
		return false
	}
}

func tupleMentionsTime(tup *types.Tuple, seen map[types.Type]bool) bool {
	for i := range tup.Len() {
		if mentionsTime(tup.At(i).Type(), seen) {
			return true
		}
	}
	return false
}

func inTimePackage(obj types.Object) bool {
	return obj != nil && obj.Pkg() != nil && obj.Pkg().Path() == "time"
}

func namedOf(t types.Type) *types.Named {
	for {
		switch tt := t.(type) {
		case *types.Pointer:
			t = tt.Elem()
		case *types.Named:
			return tt
		default:
			return nil
		}
	}
}

func packagePos(pass *analysis.Pass) token.Pos {
	if len(pass.Files) > 0 {
		return pass.Files[0].Package
	}
	return token.NoPos
}
