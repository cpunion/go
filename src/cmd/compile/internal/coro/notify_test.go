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

func notifyTestSignature(pkg *types.Pkg) *types.Type {
	name := ir.NewDeclNameAt(src.NoXPos, ir.OTYPE, pkg.Lookup("notifyList"))
	list := types.NewNamed(name)
	name.SetType(list)
	list.SetUnderlying(types.NewStruct(nil))
	return types.NewSignature(nil, []*types.Field{
		types.NewField(src.NoXPos, nil, types.NewPtr(list)),
		types.NewField(src.NoXPos, nil, types.Types[types.TUINT32]),
	}, nil)
}

func TestNotifyRecipe(t *testing.T) {
	prepareLowerTest(t)
	if got := SiteNotify.String(); got != "notify" {
		t.Fatalf("notification site name = %q", got)
	}
	pkg := types.NewPkg("sync", "sync")
	typ := notifyTestSignature(pkg)
	fn := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("runtime_notifyListWait"), typ)
	if recipe, ok := operationRecipe(fn); !ok || recipe.Kind != SiteNotify || recipe.Effect != MaySuspend {
		t.Fatalf("recipe = %+v, %t", recipe, ok)
	}
	if notifyOperation(nil) != "" {
		t.Fatal("nil call is a notification operation")
	}
	call := newLowerTestCall(fn)
	call.Args = make(ir.Nodes, 2)
	if got := waitOperation(call); got != "coroNotifyListWait" {
		t.Fatalf("operation = %q", got)
	}
	call.Args = nil
	if got := notifyOperation(call); got != "" {
		t.Fatalf("changed arity operation = %q", got)
	}
	for _, bad := range []*types.Type{
		nil, types.Types[types.TINT], types.NewSignature(nil, nil, nil),
		types.NewSignature(nil, typ.Params(), []*types.Field{types.NewField(src.NoXPos, nil, types.Types[types.TBOOL])}),
		types.NewSignature(nil, []*types.Field{
			types.NewField(src.NoXPos, nil, types.Types[types.TUNSAFEPTR]), typ.Param(1),
		}, nil),
		types.NewSignature(nil, []*types.Field{
			types.NewField(src.NoXPos, nil, types.NewPtr(types.Types[types.TUINT32])), typ.Param(1),
		}, nil),
		types.NewSignature(nil, []*types.Field{
			types.NewField(src.NoXPos, nil, types.NewPtr(types.NewStruct(nil))), typ.Param(1),
		}, nil),
		types.NewSignature(nil, []*types.Field{typ.Param(0), types.NewField(src.NoXPos, nil, types.Types[types.TUINT64])}, nil),
		notifyTestSignature(types.NewPkg("example.com/sync", "sync")),
	} {
		fn := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("runtime_notifyListWait"), bad)
		if recipe, ok := operationRecipe(fn); !ok || recipe.Kind != SiteAwait || recipe.Effect != MaySuspend {
			t.Fatalf("changed signature recipe = %+v, %t", recipe, ok)
		}
	}
}

func TestLowerNotify(t *testing.T) {
	prepareLowerTest(t)
	oldTarget, oldLocalPkg, oldCurFunc := typecheck.Target, types.LocalPkg, ir.CurFunc
	t.Cleanup(func() {
		typecheck.Target, types.LocalPkg, ir.CurFunc = oldTarget, oldLocalPkg, oldCurFunc
	})
	typecheck.Target = new(ir.Package)
	pkg := types.NewPkg("sync", "sync")
	types.LocalPkg = pkg
	typ := notifyTestSignature(pkg)
	primitive := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("runtime_notifyListWait"), typ)
	primitive.DeclareParams(true)
	fn := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("wait"), typ)
	fn.DeclareParams(true)
	typecheck.Target.Funcs = append(typecheck.Target.Funcs, fn)
	call := newLowerTestCall(primitive)
	for _, arg := range fn.Dcl {
		call.Args = append(call.Args, arg)
	}
	counter := typecheck.TempAt(src.NoXPos, fn, types.Types[types.TINT])
	call.SetInit(ir.Nodes{ir.NewDecl(src.NoXPos, ir.ODCL, counter),
		ir.NewAssignStmt(src.NoXPos, counter, typedInt(src.NoXPos, types.Types[types.TINT], 17))})
	fn.Body = ir.Nodes{call, newLowerTestReturn()}
	plan, err := Analyze([]*ir.Func{fn}, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := Lower(plan, nil)
	if err != nil || result.Lowered != 1 || result.Skipped != 0 {
		t.Fatalf("Lower = %+v, %v", result, err)
	}
	found := 0
	for _, generated := range typecheck.Target.Funcs {
		ir.VisitList(generated.Body, func(node ir.Node) {
			helper, ok := node.(*ir.CallExpr)
			if !ok || symbolName(ir.StaticCalleeName(ir.StaticValue(helper.Fun))) != "runtime.coroNotifyListWait" {
				return
			}
			found++
			conversion, ok := helper.Args[1].(*ir.ConvExpr)
			if !ok || conversion.Type() != types.Types[types.TUNSAFEPTR] {
				t.Fatal("notification list is not converted to its runtime pointer ABI")
			}
			// Inspect the shared wait-state contract using the captured source
			// argument, before the runtime-only unsafe.Pointer conversion.
			helper.Args[1] = conversion.X
			checkSemaphoreTransition(t, generated, helper, true)
			helper.Args[1] = conversion
		})
	}
	if found != 1 {
		t.Fatalf("generated %d notification calls, want 1", found)
	}
	other := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("otherWait"), typ)
	call.Fun = other.Nname
	if _, err := newLowerCandidate(plan, plan.Functions[fn]); err == nil ||
		!strings.Contains(err.Error(), "unsupported notification site") {
		t.Fatalf("changed target error = %v", err)
	}
}
