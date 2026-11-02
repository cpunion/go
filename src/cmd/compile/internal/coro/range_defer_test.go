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

func TestRangeDeferAnchorIdentity(t *testing.T) {
	prepareLowerTest(t)
	field := func(typ *types.Type) *types.Field { return types.NewField(src.NoXPos, nil, typ) }
	sig := types.NewSignature(nil, nil, []*types.Field{field(types.Types[types.TINTER])})
	helper := ir.NewFunc(src.NoXPos, src.NoXPos, ir.Pkgs.Runtime.Lookup("deferrangefunc"), sig)
	if !rangeDeferAnchorDeclaration(helper) {
		t.Fatal("lost exact compiler-owned range anchor declaration")
	}
	if recipe, ok := operationRecipe(helper); !ok || recipe != (OperationRecipe{Kind: SiteRangeDefer, Exec: NeedsTaskContext}) {
		t.Fatalf("anchor requires its dedicated task-context marker lowering: %+v, %t", recipe, ok)
	}
	for _, test := range []struct {
		name string
		sig  *types.Type
	}{
		{"no-result", types.NewSignature(nil, nil, nil)},
		{"extra-result", types.NewSignature(nil, nil, []*types.Field{field(types.Types[types.TINTER]), field(types.Types[types.TBOOL])})},
		{"argument", types.NewSignature(nil, []*types.Field{field(types.Types[types.TINT])}, []*types.Field{field(types.Types[types.TINTER])})},
		{"pointer-result", types.NewSignature(nil, nil, []*types.Field{field(types.Types[types.TUNSAFEPTR])})},
		{"receiver", types.NewSignature(field(types.NewPtr(types.NewStruct(nil))), nil, []*types.Field{field(types.Types[types.TINTER])})},
	} {
		t.Run(test.name, func(t *testing.T) {
			if rangeDeferAnchorDeclaration(ir.NewFunc(src.NoXPos, src.NoXPos, helper.Sym(), test.sig)) {
				t.Fatal("unexpected signature acquired private range-anchor identity")
			}
		})
	}
	pkg := types.NewPkg("example.com/coro/rangeanchorspoof", "p")
	if rangeDeferAnchorDeclaration(nil) ||
		rangeDeferAnchorDeclaration(ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("deferrangefunc"), sig)) ||
		rangeDeferAnchorDeclaration(ir.NewFunc(src.NoXPos, src.NoXPos, ir.Pkgs.Runtime.Lookup("otherAnchor"), sig)) {
		t.Fatal("non-runtime declaration acquired private range-anchor identity")
	}
}

func TestSynchronousRangeIterator(t *testing.T) {
	prepareLowerTest(t)
	oldTarget := typecheck.Target
	typecheck.Target = new(ir.Package)
	t.Cleanup(func() { typecheck.Target = oldTarget })
	for _, mode := range []string{
		"direct", "conditional", "loop", "unrelated-closure", "two-direct",
		"global", "forward", "alias", "replace", "address", "return", "capture",
		"go", "defer", "normalized-go", "normalized-defer", "go-defer-flag",
		"call-init", "callee-also-argument", "unused", "second-unused", "second-escapes",
	} {
		t.Run(mode, func(t *testing.T) {
			pkg := types.NewPkg("example.com/coro/synchronousrange/"+mode, "p")
			callback := types.NewSignature(nil,
				[]*types.Field{types.NewField(src.NoXPos, nil, types.Types[types.TINTER])},
				[]*types.Field{types.NewField(src.NoXPos, nil, types.Types[types.TBOOL])})
			fields := []*types.Field{types.NewField(src.NoXPos, pkg.Lookup("yield"), callback)}
			if mode == "two-direct" || mode == "second-unused" || mode == "second-escapes" {
				fields = append(fields, types.NewField(src.NoXPos, pkg.Lookup("other"), callback))
			}
			var results []*types.Field
			if mode == "return" {
				results = []*types.Field{types.NewField(src.NoXPos, nil, callback)}
			}
			fn := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("iterator"), types.NewSignature(nil, fields, results))
			fn.DeclareParams(true)
			parameter := fn.Dcl[0]
			call := ir.NewCallExpr(src.NoXPos, ir.OCALLFUNC, parameter,
				ir.Nodes{ir.NewNilExpr(src.NoXPos, types.Types[types.TINTER])})
			call.SetType(types.Types[types.TBOOL])
			fn.Body = ir.Nodes{call}
			global := ir.NewNameAt(src.NoXPos, pkg.Lookup("saved"), callback)
			global.Class = ir.PEXTERN
			store := ir.NewAssignStmt(src.NoXPos, global, parameter)
			closure := func(capture bool) *ir.ClosureExpr {
				sig := types.NewSignature(nil, nil, nil)
				cl := ir.NewClosureFunc(src.NoXPos, src.NoXPos, ir.OCLOSURE, sig, fn, typecheck.Target, 0)
				if capture {
					ir.NewClosureVar(src.NoXPos, cl, parameter)
				}
				return cl.OClosure
			}
			closureAssignment := func(capture bool) ir.Node {
				cl := closure(capture)
				local := typecheck.TempAt(src.NoXPos, fn, cl.Type())
				return ir.NewAssignStmt(src.NoXPos, local, cl)
			}
			want := false
			switch mode {
			case "direct":
				want = true
			case "conditional":
				fn.Body = ir.Nodes{ir.NewIfStmt(src.NoXPos, ir.NewBool(src.NoXPos, true), ir.Nodes{call}, nil)}
				want = true
			case "loop":
				fn.Body = ir.Nodes{ir.NewForStmt(src.NoXPos, nil, ir.NewBool(src.NoXPos, true), nil, ir.Nodes{call}, false)}
				want = true
			case "unrelated-closure":
				fn.Body = append(fn.Body, closureAssignment(false))
				want = true
			case "global":
				fn.Body = append(fn.Body, store)
				if !readOnlyCallbackParameters(fn) {
					t.Fatal("global escape oracle no longer distinguishes readonly from synchronous lifetime")
				}
			case "forward":
				helper := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("save"),
					types.NewSignature(nil, []*types.Field{types.NewField(src.NoXPos, nil, callback)}, nil))
				forward := newLowerTestCall(helper)
				forward.Args = ir.Nodes{parameter}
				fn.Body = append(fn.Body, forward)
				if !readOnlyCallbackParameters(fn) {
					t.Fatal("forwarding oracle no longer distinguishes readonly from synchronous lifetime")
				}
			case "alias":
				local := typecheck.TempAt(src.NoXPos, fn, callback)
				fn.Body = append(fn.Body, ir.NewAssignStmt(src.NoXPos, local, parameter))
			case "replace":
				fn.Body = append(fn.Body, ir.NewAssignStmt(src.NoXPos, parameter, ir.NewNilExpr(src.NoXPos, callback)))
			case "address":
				local := typecheck.TempAt(src.NoXPos, fn, types.NewPtr(callback))
				fn.Body = append(fn.Body, ir.NewAssignStmt(src.NoXPos, local, ir.NewAddrExpr(src.NoXPos, parameter)))
			case "return":
				fn.Body = append(fn.Body, ir.NewReturnStmt(src.NoXPos, ir.Nodes{parameter}))
			case "capture":
				fn.Body = append(fn.Body, closureAssignment(true))
			case "go", "defer":
				op := ir.OGO
				if mode == "defer" {
					op = ir.ODEFER
				}
				fn.Body = ir.Nodes{ir.NewGoDeferStmt(src.NoXPos, op, call)}
			case "normalized-go", "normalized-defer":
				op := ir.OGO
				if mode == "normalized-defer" {
					op = ir.ODEFER
				}
				wrapped := ir.NewCallExpr(src.NoXPos, ir.OCALLFUNC, closure(true), nil)
				fn.Body = ir.Nodes{ir.NewGoDeferStmt(src.NoXPos, op, wrapped)}
			case "go-defer-flag":
				call.GoDefer = true
			case "call-init":
				call.SetInit(ir.Nodes{store})
			case "callee-also-argument":
				call.Args = ir.Nodes{ir.NewConvExpr(src.NoXPos, ir.OCONVIFACE, types.Types[types.TINTER], parameter)}
			case "unused":
				fn.Body = nil
			case "two-direct":
				fn.Body = append(fn.Body, ir.NewCallExpr(src.NoXPos, ir.OCALLFUNC, fn.Dcl[1],
					ir.Nodes{ir.NewNilExpr(src.NoXPos, types.Types[types.TINTER])}))
				want = true
			case "second-unused":
			case "second-escapes":
				fn.Body = append(fn.Body, ir.NewAssignStmt(src.NoXPos, global, fn.Dcl[1]))
			}
			if got := synchronousRangeIterator(fn); got != want {
				t.Fatalf("synchronous-only callback lifetime = %t, want %t", got, want)
			}
		})
	}
	if synchronousRangeIterator(nil) || synchronousRangeIterator(newLowerTestFunc(types.NewPkg("example.com/coro/nocallback", "p"), "empty")) {
		t.Fatal("missing callback acquired a synchronous range-iterator proof")
	}
}
