// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/ir"
	"cmd/compile/internal/typecheck"
	"cmd/compile/internal/types"
	"cmd/internal/src"
	"testing"
)

func TestLowerInlineMarks(t *testing.T) {
	prepareLowerTest(t)
	oldTarget, oldLocalPkg, oldCurFunc := typecheck.Target, types.LocalPkg, ir.CurFunc
	defer func() {
		typecheck.Target, types.LocalPkg, ir.CurFunc = oldTarget, oldLocalPkg, oldCurFunc
	}()
	for _, closure := range []bool{false, true} {
		t.Run(map[bool]string{false: "explicit-frame", true: "closure-frame"}[closure], func(t *testing.T) {
			pkg := types.NewPkg("example.com/coro/inlinemarks/"+t.Name(), "inlinemarks")
			types.LocalPkg = pkg
			typecheck.Target = new(ir.Package)
			fn := newLowerTestFunc(pkg, "marked")
			first := ir.NewInlineMarkStmt(src.NoXPos, 17)
			last := ir.NewInlineMarkStmt(src.NoXPos, 23)
			yield := newLowerTestCall(testFunc("yield"))
			fn.Body = ir.Nodes{first, yield, last, newLowerTestReturn()}
			wantABI := FactoryABI3
			if closure {
				literal := ir.NewClosureFunc(src.NoXPos, src.NoXPos, ir.OCLOSURE,
					types.NewSignature(nil, nil, nil), fn, typecheck.Target, 0)
				literal.DeclareParams(true)
				literal.Body = ir.Nodes{newLowerTestReturn()}
				fn.Body.Prepend(ir.NewAssignStmt(src.NoXPos, ir.BlankNode, literal.OClosure))
				wantABI = FactoryABI1
			}
			function := &Function{
				Func: fn, Local: MaySuspend, Effect: MaySuspend, Primary: CoroPrimary,
				Sites: []Site{{ID: 1, Kind: SiteYield, Node: yield}},
			}
			result, err := Lower(&Plan{Functions: map[*ir.Func]*Function{fn: function}})
			if err != nil || result.Lowered != 1 || function.Factory != wantABI {
				t.Fatalf("Lower = %+v, %v, factory = %v, want %v", result, err, function.Factory, wantABI)
			}
			found := make(map[*ir.InlineMarkStmt]bool)
			for _, generated := range typecheck.Target.Funcs {
				ir.VisitList(generated.Body, func(node ir.Node) {
					if mark, ok := node.(*ir.InlineMarkStmt); ok {
						found[mark] = true
					}
				})
			}
			if !found[first] || !found[last] || first.Index != 17 || last.Index != 23 ||
				first.Typecheck() != 1 || last.Typecheck() != 1 {
				t.Fatal("inline marks were removed, changed, or left unchecked")
			}
		})
	}
}

func TestPreserveCall(t *testing.T) {
	for _, test := range []struct {
		name    string
		summary FuncSummary
		local   bool
		want    bool
	}{
		{name: "localPlain", local: true},
		{name: "localPanic", local: true, summary: FuncSummary{Terminal: MayPanic}, want: true},
		{name: "localSuspend", local: true, summary: FuncSummary{Effect: MaySuspend}, want: true},
		{name: "importedPlain"},
		{name: "importedPanic", summary: FuncSummary{Terminal: MayPanic}},
		{name: "importedPanicFactory", summary: FuncSummary{Terminal: MayPanic, Factory: FactoryABI1}, want: true},
		{name: "importedSuspend", summary: FuncSummary{Effect: MaySuspend}, want: true},
		{name: "importedGoexit", summary: FuncSummary{Terminal: MayGoexit}, want: true},
		{name: "importedSystemABI", summary: FuncSummary{Exec: NeedsSystemABI}, want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fn := testFunc(test.name)
			plan := &Plan{Functions: make(map[*ir.Func]*Function)}
			if test.local {
				plan.Functions[fn] = &Function{
					Func: fn, Effect: test.summary.Effect,
					Exec: test.summary.Exec, Terminal: test.summary.Terminal,
				}
			} else {
				SetSummary(fn, test.summary)
				defer delete(summaries, fn)
			}
			if got := plan.PreserveCall(fn); got != test.want {
				t.Fatalf("PreserveCall = %t, want %t", got, test.want)
			}
		})
	}
	plan := &Plan{}
	if plan.PreserveCall(nil) || plan.PreserveCall(testFunc("unknown")) {
		t.Fatal("call without an operation or summary was preserved")
	}
	operation := ir.NewFunc(src.NoXPos, src.NoXPos,
		types.NewPkg("runtime", "runtime").Lookup("Gosched"),
		types.NewSignature(nil, nil, nil))
	if !plan.PreserveCall(operation) {
		t.Fatal("operation without an imported summary was not preserved")
	}
	foreign := testFunc("_Cfunc_preserve")
	plan = &Plan{
		Functions: map[*ir.Func]*Function{foreign: {Func: foreign}},
		cgoDirect: map[string]cgoDirectCall{
			foreign.Sym().Name: {class: DirectNoBlock},
		},
	}
	if !plan.PreserveCall(foreign) {
		t.Fatal("non-blocking direct C operation was not preserved")
	}
}
