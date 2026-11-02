// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/ir"
	"cmd/compile/internal/typecheck"
	"cmd/compile/internal/types"
	"cmd/internal/src"
	"strings"
	"testing"
)

func TestRangeStatePanicRecipe(t *testing.T) {
	prepareLowerTest(t)
	oldTarget := typecheck.Target
	typecheck.Target = new(ir.Package)
	t.Cleanup(func() { typecheck.Target = oldTarget })
	field := func(typ *types.Type) *types.Field { return types.NewField(src.NoXPos, nil, typ) }
	integer := types.Types[types.TINT]
	signature := types.NewSignature(nil, []*types.Field{field(integer)}, nil)
	helper := ir.NewFunc(src.NoXPos, src.NoXPos, ir.Pkgs.Runtime.Lookup("panicrangestate"), signature)
	got, ok := operationRecipe(helper)
	if !ok || got != (OperationRecipe{Terminal: MayPanic}) {
		t.Fatalf("runtime protocol panic recipe = %+v, %v", got, ok)
	}
	for _, test := range []struct {
		name string
		sig  *types.Type
	}{
		{"no-argument", types.NewSignature(nil, nil, nil)},
		{"two-arguments", types.NewSignature(nil, []*types.Field{field(integer), field(integer)}, nil)},
		{"pointer", types.NewSignature(nil, []*types.Field{field(types.NewPtr(integer))}, nil)},
		{"result", types.NewSignature(nil, []*types.Field{field(integer)}, []*types.Field{field(integer)})},
		{"receiver", types.NewSignature(field(types.NewPtr(types.NewStruct(nil))), []*types.Field{field(integer)}, nil)},
		{"slice", types.NewSignature(nil, []*types.Field{field(types.NewSlice(integer))}, nil)},
	} {
		t.Run(test.name, func(t *testing.T) {
			fn := ir.NewFunc(src.NoXPos, src.NoXPos, helper.Sym(), test.sig)
			if recipe, ok := operationRecipe(fn); ok {
				t.Fatalf("unexpected runtime declaration acquired recipe %+v", recipe)
			}
		})
	}
	pkg := types.NewPkg("example.com/coro/iteratoridentity", "iteratoridentity")
	spoof := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("panicrangestate"), signature)
	if _, ok := operationRecipe(spoof); ok {
		t.Fatal("source spelling acquired private runtime capability")
	}
	caller := newLowerTestFunc(pkg, "caller")
	call := newLowerTestCall(helper)
	call.Args = ir.Nodes{ir.NewInt(src.NoXPos, 1)}
	caller.Body = ir.Nodes{call}
	plan, err := Analyze([]*ir.Func{caller}, nil)
	if err != nil {
		t.Fatal(err)
	}
	function := plan.Functions[caller]
	if function.Effect != NoSuspend || function.Terminal != MayPanic || len(function.Sites) != 0 || len(function.Edges) != 1 || function.Edges[0].Unknown || plan.edgeNeedsCoroEntry(function.Edges[0]) {
		t.Fatalf("protocol panic lost plain terminal semantics: %+v", function)
	}
}

func TestRejectRangeDeferOwnership(t *testing.T) {
	prepareLowerTest(t)
	oldTarget := typecheck.Target
	typecheck.Target = new(ir.Package)
	t.Cleanup(func() { typecheck.Target = oldTarget })
	pkg := types.NewPkg("example.com/coro/rangedeferownership", "rangedeferownership")
	fn := newLowerTestFunc(pkg, "caller")
	leaf := newLowerTestFunc(pkg, "cleanup")
	leaf.Body = ir.Nodes{newLowerTestReturn()}
	call := newLowerTestCall(leaf)
	deferred := ir.NewGoDeferStmt(src.NoXPos, ir.ODEFER, call)
	deferred.DeferAt = ir.NewNilExpr(src.NoXPos, types.Types[types.TUNSAFEPTR])
	yield := ir.NewFunc(src.NoXPos, src.NoXPos, ir.Pkgs.Runtime.Lookup("Gosched"), types.NewSignature(nil, nil, nil))
	fn.Body = ir.Nodes{deferred, newLowerTestCall(yield), newLowerTestReturn()}
	plan, err := Analyze([]*ir.Func{fn, leaf}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = newLowerCandidate(plan, plan.Functions[fn])
	if err == nil || !strings.Contains(err.Error(), "range defer belongs to enclosing activation") {
		t.Fatalf("enclosing range defer ownership = %v", err)
	}
}
