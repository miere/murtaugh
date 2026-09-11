// Package renderclockanalyzer is a go/analysis pass enforcing the layer rule the
// gateway/runtime-node split rests on: a chatRenderer implementation may not
// observe time.
//
// # The rule, and why a comment was not enough
//
// The Slack write path sees RENDERED OUTPUT, not events, and some events are
// deliberately never rendered — a status heartbeat keeps a long tool's turn alive
// and produces no renderer call at all. A liveness detector attached there is
// therefore blind during healthy work, which forces it to be sized against the
// longest legitimate silence rather than against actual inactivity; sized that way
// it can only fire long after Slack has closed the message on its own (#170
// Concern 4). Liveness belongs at the inbound event edge, where every frame
// resets it, and every edge that feeds a renderer owns one: the chat turn loop
// resets its window before its type switch (eventTranslator is that switch
// extracted, and carries the window with it), and backgroundEventsRouter.Handle
// resets its stretch's window before its own switch (#192). The rule is about
// which LAYER may hold a timer, not about how many there are — a background
// stretch is fed by a different edge, and had no timer at all until it got its
// own.
//
// That argument is easy to agree with and easy to forget. The renderer holds a
// message open and knows when a section was last written to, so "just seal it if
// nothing arrives for a while" will keep looking like the obvious local fix. This
// pass makes it a build failure instead of a discussion.
//
// # What it flags
//
// Every reference to package `time` — a call, a type, a constant — in the BODY
// or the SIGNATURE of any method of a named type that implements the package's
// `chatRenderer` interface, plus any field of such a type whose type mentions
// package `time`. Detection is type-precise: the resolved object's package is
// compared, so an import alias or a dot-import does not evade it.
//
// Helper METHODS on the renderer are covered, not just the eight interface
// methods, because that is where a clock would actually be smuggled in.
//
// # What it does not flag, said plainly
//
// The pass follows RECEIVERS, not call graphs or data flow, so anything the
// renderer reaches the clock THROUGH is out of reach — a free function called
// from a renderer method, and equally a collaborator it holds and delegates to:
// give a helper struct a `last time.Time` and a `stale()` method, have the
// renderer call `r.h.stale()`, and nothing here fires. That is the more
// plausible smuggling route of the two, and it is not covered.
//
// What IS covered is the direct, local mistake: a clock on the renderer itself,
// in a field, in a parameter, or in a body. The rule is a guard against that,
// not a proof.
//
// The sinks BELOW the renderer are deliberately out of scope. The line the rule
// draws is the one that matters: the layer that decides SEGMENTATION AND
// TERMINALS cannot look at a clock.
//
// # Scoping, and why the guard cannot decay silently
//
// The pass keys off an interface NAME, because the rule is about one specific
// seam in one package rather than a shape. A rename would ordinarily turn the
// guard into a no-op that still passes CI, so the reverse is also checked: a
// package whose path ends in internal/slack/gateway that does NOT declare
// chatRenderer is itself reported. The guard fails loudly rather than evaporating.
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

// rendererInterface is the seam the rule protects. Named rather than shape-matched
// (see the package doc); guardedPackageSuffix is what stops that being fragile.
const rendererInterface = "chatRenderer"

// guardedPackageSuffix is the package that MUST declare rendererInterface. A
// fixture package declaring its own chatRenderer is checked too; only this one is
// required to.
const guardedPackageSuffix = "/internal/slack/gateway"

// Analyzer reports uses of package time inside a chatRenderer implementation.
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
	// A field is the quietest way in: `lastPaint time.Time` needs no call site to
	// review. Checked from the type rather than the AST so an embedded or aliased
	// spelling cannot hide it.
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
		// The SIGNATURE is checked as well as the body, so "inside any method" is
		// true of the whole method: `func (r *sectionRenderer) sealAfter(d
		// time.Duration)` hands the layer a clock without ever naming time in a
		// body, and a body-only pass would wave it through.
		reportClockUses(pass, fn.Type)
		if fn.Body != nil {
			reportClockUses(pass, fn.Body)
		}
	})
	return nil, nil
}

// reportClockUses flags every identifier resolving to package time under n.
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

// rendererIface returns the package's own chatRenderer interface, or nil when it
// declares none.
func rendererIface(pkg *types.Package) *types.Interface {
	obj := pkg.Scope().Lookup(rendererInterface)
	if obj == nil {
		return nil
	}
	iface, _ := obj.Type().Underlying().(*types.Interface)
	return iface
}

// implementers returns the package's named non-interface types satisfying iface,
// by value or by pointer. Both are checked because the sole real implementation
// (*sectionRenderer) satisfies it only as a pointer, while a test fake may not.
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

// isGuardedMethod reports whether fn is a method on one of the guarded types.
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

// reportClockFields flags a struct field whose type mentions package time.
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

// mentionsTime reports whether t is, or is built out of, a type declared in
// package time. seen breaks the cycle a self-referential type would otherwise
// create.
func mentionsTime(t types.Type, seen map[types.Type]bool) bool {
	if t == nil || seen[t] {
		return false
	}
	seen[t] = true
	switch tt := t.(type) {
	case *types.Named:
		// Stop at the name: a struct that merely CONTAINS a time is that type's
		// business, and descending into it would flag every transitive holder.
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

// inTimePackage reports whether obj is declared in package time.
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

// packagePos is somewhere inside the package to hang the "interface is gone"
// diagnostic on. Any file will do; the message is about the package.
func packagePos(pass *analysis.Pass) token.Pos {
	if len(pass.Files) > 0 {
		return pass.Files[0].Package
	}
	return token.NoPos
}
