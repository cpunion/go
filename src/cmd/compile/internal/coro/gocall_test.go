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

type goCallFixture struct {
	caller  *ir.Func
	child   *ir.Func
	wrapper *ir.Func
	call    *ir.CallExpr
	inner   *ir.CallExpr
	outer   *ir.Name
	capture *ir.Name
}

func newGoCallFixture(t *testing.T) goCallFixture {
	t.Helper()
	prepareLowerTest(t)
	pkg := types.NewPkg("example.com/coro/gocall/"+t.Name(), "gocall")
	types.LocalPkg = pkg
	typecheck.Target = new(ir.Package)
	parameter := types.NewField(src.NoXPos, pkg.Lookup("value"), types.Types[types.TINT])
	child := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("child"),
		types.NewSignature(nil, []*types.Field{parameter}, nil))
	child.DeclareParams(true)
	typecheck.Target.Funcs = append(typecheck.Target.Funcs, child)
	caller := newLowerTestFunc(pkg, "caller")
	outer := caller.NewLocal(src.NoXPos, pkg.Lookup("argument"), types.Types[types.TINT])
	wrapper := ir.NewClosureFunc(src.NoXPos, src.NoXPos, ir.OGO,
		types.NewSignature(nil, nil, nil), caller, typecheck.Target, 0)
	wrapper.DeclareParams(true)
	wrapper.SetWrapper(true)
	wrapper.WrappedFunc = child
	capture := ir.NewClosureVar(src.NoXPos, wrapper, outer)
	inner := ir.NewCallExpr(src.NoXPos, ir.OCALLFUNC, child.Nname, ir.Nodes{capture})
	inner.SetTypecheck(1)
	wrapper.Body = ir.Nodes{inner}
	call := ir.NewCallExpr(src.NoXPos, ir.OCALLFUNC, wrapper.OClosure, nil)
	call.SetTypecheck(1)
	call.GoDefer = true
	return goCallFixture{caller, child, wrapper, call, inner, outer, capture}
}

func TestUnwrapGoCall(t *testing.T) {
	prepareLowerTest(t)
	oldPkg, oldTarget := types.LocalPkg, typecheck.Target
	defer func() { types.LocalPkg, typecheck.Target = oldPkg, oldTarget }()
	f := newGoCallFixture(t)
	got := unwrapGoCall(f.call)
	if got == f.call || got == f.inner || got.Fun != f.child.Nname ||
		len(got.Args) != 1 || got.Args[0] != f.outer {
		t.Fatalf("unwrapped call = %v, want child with caller's argument", got)
	}
	if f.call.Fun != f.wrapper.OClosure || f.inner.Args[0] != f.capture {
		t.Fatal("unwrapping modified the ordinary Go fallback")
	}
	f.inner.Args = ir.Nodes{typedInt(src.NoXPos, types.Types[types.TINT], 42)}
	f.wrapper.ClosureVars = nil
	got = unwrapGoCall(f.call)
	if got == f.call || ir.Int64Val(got.Args[0]) != 42 {
		t.Fatal("capture-free wrapper did not preserve its literal argument")
	}
}

func TestUnwrapGoCallFallback(t *testing.T) {
	prepareLowerTest(t)
	oldPkg, oldTarget := types.LocalPkg, typecheck.Target
	defer func() { types.LocalPkg, typecheck.Target = oldPkg, oldTarget }()
	for _, test := range []struct {
		name string
		edit func(goCallFixture)
	}{
		{"ordinary", func(f goCallFixture) { f.call.Fun = f.child.Nname }},
		{"arguments", func(f goCallFixture) { f.call.Args = ir.Nodes{f.outer} }},
		{"call-init", func(f goCallFixture) { f.call.SetInit(ir.Nodes{newLowerTestReturn()}) }},
		{"closure-init", func(f goCallFixture) { f.wrapper.OClosure.SetInit(ir.Nodes{newLowerTestReturn()}) }},
		{"source-literal", func(f goCallFixture) { f.wrapper.SetWrapper(false) }},
		{"dynamic-target", func(f goCallFixture) { f.wrapper.WrappedFunc = nil }},
		{"extra-body", func(f goCallFixture) { f.wrapper.Body = append(f.wrapper.Body, newLowerTestReturn()) }},
		{"non-call", func(f goCallFixture) { f.wrapper.Body = ir.Nodes{newLowerTestReturn()} }},
		{"interface-call", func(f goCallFixture) { f.inner.SetOp(ir.OCALLINTER) }},
		{"body-init", func(f goCallFixture) { f.inner.SetInit(ir.Nodes{newLowerTestReturn()}) }},
		{"unknown-callee", func(f goCallFixture) { f.inner.Fun = f.outer }},
		{"wrong-target", func(f goCallFixture) { f.wrapper.WrappedFunc = f.caller }},
		{"missing-outer", func(f goCallFixture) { f.capture.Outer = nil }},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newGoCallFixture(t)
			test.edit(f)
			if got := unwrapGoCall(f.call); got != f.call {
				t.Fatal("unsupported wrapper lost its ordinary Go call")
			}
		})
	}
}

func TestGoCallPlan(t *testing.T) {
	for _, test := range []struct {
		name      string
		yieldInit bool
	}{{"direct-init", false}, {"suspending-init", true}} {
		t.Run(test.name, func(t *testing.T) {
			testGoCallPlan(t, test.yieldInit)
		})
	}
}

func testGoCallPlan(t *testing.T, yieldInit bool) {
	t.Helper()
	prepareLowerTest(t)
	oldPkg, oldTarget := types.LocalPkg, typecheck.Target
	defer func() { types.LocalPkg, typecheck.Target = oldPkg, oldTarget }()
	f := newGoCallFixture(t)
	yield := ir.NewFunc(src.NoXPos, src.NoXPos, ir.Pkgs.Runtime.Lookup("Gosched"),
		types.NewSignature(nil, nil, nil))
	yield.DeclareParams(true)
	f.child.Body = ir.Nodes{newLowerTestCall(yield), newLowerTestReturn()}
	declaration := ir.NewDecl(src.NoXPos, ir.ODCL, f.outer)
	assignment := ir.NewAssignStmt(src.NoXPos, f.outer,
		typedInt(src.NoXPos, types.Types[types.TINT], 42))
	assignment.Def = true
	f.outer.Defn = assignment
	spawn := ir.NewGoDeferStmt(src.NoXPos, ir.OGO, f.call)
	spawn.SetInit(ir.Nodes{declaration, assignment})
	if yieldInit {
		spawn.SetInit(ir.Nodes{declaration, newLowerTestCall(yield), assignment})
	}
	spawn.SetTypecheck(1)
	f.caller.Body = ir.Nodes{spawn, newLowerTestCall(yield), newLowerTestReturn()}
	plan, err := Analyze([]*ir.Func{f.caller, f.child, f.wrapper}, nil)
	if err != nil {
		t.Fatal(err)
	}
	edge := edgeForCall(plan.Functions[f.caller], f.call)
	if edge.Callee != f.child || edge.Kind != GoCall || edge.Node != f.call {
		t.Fatalf("spawn edge = %+v, want original site and named child", edge)
	}
	if err := plan.Verify(); err != nil {
		t.Fatal(err)
	}
	result, err := Lower(plan)
	if err != nil || result.Lowered != 2 {
		t.Fatalf("lower = (%+v, %v), want caller and child lowered", result, err)
	}
	assignments := 0
	for _, generated := range typecheck.Target.Funcs {
		ir.Visit(generated, func(node ir.Node) {
			assignment, ok := node.(*ir.AssignStmt)
			if ok && assignment.Y != nil && assignment.Y.Op() == ir.OLITERAL &&
				assignment.Y.Type().IsInteger() && ir.Int64Val(assignment.Y) == 42 {
				assignments++
			}
		})
	}
	if assignments != 1 {
		t.Fatalf("go argument initializations = %d, want 1", assignments)
	}
}
