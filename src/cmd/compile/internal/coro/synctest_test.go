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

func TestOrdinaryBody(t *testing.T) {
	prepareLowerTest(t)
	oldPkg, oldTarget := types.LocalPkg, typecheck.Target
	defer func() { types.LocalPkg, typecheck.Target = oldPkg, oldTarget }()
	pkg := types.NewPkg("example.com/coro/ordinarybody", "ordinarybody")
	types.LocalPkg, typecheck.Target = pkg, new(ir.Package)
	makeFunction := func(name string) *ir.Func {
		recv := types.NewField(src.NoXPos, pkg.Lookup("recv"), types.Types[types.TINT])
		param := types.NewField(src.NoXPos, pkg.Lookup("param"), types.Types[types.TINT])
		result := types.NewField(src.NoXPos, pkg.Lookup("result"), types.Types[types.TINT])
		fn := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup(name),
			types.NewSignature(recv, []*types.Field{param}, []*types.Field{result}))
		fn.DeclareParams(true)
		return fn
	}
	fn, ordinary := makeFunction("native"), makeFunction("ordinary")
	local := ordinary.NewLocal(src.NoXPos, pkg.Lookup("local"), types.Types[types.TINT])
	child := ir.NewClosureFunc(src.NoXPos, src.NoXPos, ir.OCLOSURE,
		types.NewSignature(nil, nil, nil), ordinary, typecheck.Target, 0)
	child.DeclareParams(true)
	child.RangeParent = ordinary
	paramCapture := ir.NewClosureVar(src.NoXPos, child, ordinary.Dcl[1])
	localCapture := ir.NewClosureVar(src.NoXPos, child, local)
	nested := ir.NewClosureFunc(src.NoXPos, src.NoXPos, ir.OCLOSURE,
		types.NewSignature(nil, nil, nil), child, typecheck.Target, 0)
	nested.DeclareParams(true)
	nestedCapture := ir.NewClosureVar(src.NoXPos, nested, paramCapture)
	nested.Body = ir.Nodes{
		ir.NewAssignStmt(src.NoXPos, nestedCapture, typedInt(src.NoXPos, nestedCapture.Type(), 1)),
		newLowerTestReturn(),
	}
	child.Body = ir.Nodes{
		typecheck.Call(src.NoXPos, nested.OClosure, nil, false),
		ir.NewAssignStmt(src.NoXPos, localCapture, typedInt(src.NoXPos, localCapture.Type(), 2)),
		newLowerTestReturn(),
	}
	ordinary.Body = ir.Nodes{
		ir.NewDecl(src.NoXPos, ir.ODCL, local),
		ir.NewAssignStmt(src.NoXPos, local, ordinary.Dcl[0]),
		ir.NewAssignStmt(src.NoXPos, ordinary.Dcl[2], ordinary.Dcl[1]),
		typecheck.Call(src.NoXPos, child.OClosure, nil, false),
	}
	nativeReturn := newLowerTestReturn()
	fn.Body = ir.Nodes{nativeReturn}
	addOrdinaryBody(fn, ordinary)
	branch, ok := fn.Body[0].(*ir.IfStmt)
	if !ok || len(branch.Else) != 1 || branch.Else[0] != nativeReturn ||
		symbolName(ir.StaticCalleeName(branch.Cond.(*ir.CallExpr).Fun)) != "runtime.coroUseOrdinary" {
		t.Fatalf("public entry = %v, want context test and unchanged native branch", fn.Body)
	}
	if local.Curfn != fn || child.ClosureParent != fn || child.RangeParent != fn ||
		nested.ClosureParent != child {
		t.Fatal("ordinary local or closure has the wrong owner")
	}
	if paramCapture.Outer != fn.Dcl[1] || paramCapture.Defn != fn.Dcl[1] ||
		nestedCapture.Outer != paramCapture || nestedCapture.Defn != fn.Dcl[1] ||
		localCapture.Outer != local || localCapture.Defn != local {
		t.Fatal("ordinary parameter/local capture chain was not remapped")
	}
	if branch.Body[len(branch.Body)-1].Op() != ir.ORETURN {
		t.Fatal("ordinary void fallthrough can enter the native branch")
	}
	for _, stmt := range branch.Body {
		ir.Visit(stmt, func(node ir.Node) {
			if name, ok := node.(*ir.Name); ok && name.Curfn == ordinary {
				t.Errorf("ordinary body retained temporary owner: %v", name)
			}
		})
	}
}

func TestLowerOrdinaryBodyUnavailable(t *testing.T) {
	prepareLowerTest(t)
	oldPkg, oldTarget := types.LocalPkg, typecheck.Target
	defer func() { types.LocalPkg, typecheck.Target = oldPkg, oldTarget }()
	pkg := types.NewPkg("example.com/coro/unavailablebody", "unavailablebody")
	types.LocalPkg, typecheck.Target = pkg, new(ir.Package)
	fn := newLowerTestFunc(pkg, "leaf")
	yield := newLowerTestCall(testFunc("yield"))
	fn.Body = ir.Nodes{yield, newLowerTestReturn()}
	function := &Function{
		Func: fn, Local: MaySuspend, Effect: MaySuspend, Primary: CoroPrimary,
		Sites: []Site{{ID: 1, Kind: SiteYield, Node: yield}},
	}
	called := false
	result, err := Lower(&Plan{Functions: map[*ir.Func]*Function{fn: function}},
		&BodyReader{Have: func(got *ir.Func) bool { called = got == fn; return false },
			Read: func(*ir.Func) *ir.Func { t.Fatal("unavailable body was read"); return nil }})
	if err != nil || !called || result.Lowered != 0 || result.Skipped != 1 ||
		len(result.Diagnostics) != 1 || !strings.Contains(result.Diagnostics[0], "ordinary body unavailable") {
		t.Fatalf("Lower = (%+v, %v), reader called = %v", result, err, called)
	}
	if fn.Body[0] != yield {
		t.Fatal("missing ordinary body changed the source function")
	}
}

func TestLowerOrdinaryBody(t *testing.T) {
	prepareLowerTest(t)
	oldPkg, oldTarget := types.LocalPkg, typecheck.Target
	defer func() { types.LocalPkg, typecheck.Target = oldPkg, oldTarget }()
	pkg := types.NewPkg("example.com/coro/lowerordinary", "lowerordinary")
	types.LocalPkg, typecheck.Target = pkg, new(ir.Package)
	fn := newLowerTestFunc(pkg, "leaf")
	yield := newLowerTestCall(testFunc("yield"))
	fn.Body = ir.Nodes{yield, newLowerTestReturn()}
	function := &Function{
		Func: fn, Local: MaySuspend, Effect: MaySuspend, Primary: CoroPrimary,
		Sites: []Site{{ID: 1, Kind: SiteYield, Node: yield}},
	}
	ordinary := newLowerTestFunc(pkg, "ordinary")
	ordinary.Body = ir.Nodes{newLowerTestReturn()}
	result, err := Lower(&Plan{Functions: map[*ir.Func]*Function{fn: function}},
		&BodyReader{Have: func(*ir.Func) bool { return true },
			Read: func(*ir.Func) *ir.Func { return ordinary }})
	if err != nil || result.Lowered != 1 || result.Skipped != 0 {
		t.Fatalf("Lower = (%+v, %v), want one lowered function", result, err)
	}
	if len(fn.Body) != 1 || fn.Body[0].Op() != ir.OIF {
		t.Fatal("lowering did not install the ordinary source branch")
	}
}

func TestLowerOrdinaryBodyDependencies(t *testing.T) {
	prepareLowerTest(t)
	oldPkg, oldTarget := types.LocalPkg, typecheck.Target
	defer func() { types.LocalPkg, typecheck.Target = oldPkg, oldTarget }()
	pkg := types.NewPkg("example.com/coro/ordinarydependencies", "ordinarydependencies")
	types.LocalPkg, typecheck.Target = pkg, new(ir.Package)
	leaf := newLowerTestFunc(pkg, "leaf")
	yield := newLowerTestCall(testFunc("yield"))
	leaf.Body = ir.Nodes{yield, newLowerTestReturn()}
	caller := newLowerTestFunc(pkg, "caller")
	call := newLowerTestCall(leaf)
	caller.Body = ir.Nodes{call, newLowerTestReturn()}
	plan := &Plan{Functions: map[*ir.Func]*Function{
		leaf: {Func: leaf, Local: MaySuspend, Effect: MaySuspend, Primary: CoroPrimary,
			Sites: []Site{{ID: 1, Kind: SiteYield, Node: yield}}},
		caller: {Func: caller, Effect: MaySuspend, Primary: CoroPrimary,
			Sites: []Site{{ID: 1, Kind: SiteAwait, Node: call}},
			Edges: []Edge{{Kind: DirectCall, Callee: leaf, CalleeName: symbolName(leaf.Nname), Node: call}}},
	}}
	functions := len(typecheck.Target.Funcs)
	result, err := Lower(plan, &BodyReader{
		Have: func(fn *ir.Func) bool { return fn != leaf },
		Read: func(*ir.Func) *ir.Func {
			t.Fatal("rejected dependency materialized an ordinary body and orphaned its closures")
			return nil
		},
	})
	if err != nil || result.Lowered != 0 || result.Skipped != 2 || len(typecheck.Target.Funcs) != functions {
		t.Fatalf("Lower = (%+v, %v), functions = %d, want %d", result, err, len(typecheck.Target.Funcs), functions)
	}
}

func TestLowerOrdinaryBodyMissingAdmitted(t *testing.T) {
	prepareLowerTest(t)
	oldPkg, oldTarget := types.LocalPkg, typecheck.Target
	defer func() { types.LocalPkg, typecheck.Target = oldPkg, oldTarget }()
	pkg := types.NewPkg("example.com/coro/ordinarymissing", "ordinarymissing")
	types.LocalPkg, typecheck.Target = pkg, new(ir.Package)
	fn := newLowerTestFunc(pkg, "leaf")
	yield := newLowerTestCall(testFunc("yield"))
	fn.Body = ir.Nodes{yield, newLowerTestReturn()}
	function := &Function{Func: fn, Local: MaySuspend, Effect: MaySuspend, Primary: CoroPrimary,
		Sites: []Site{{ID: 1, Kind: SiteYield, Node: yield}}}
	_, err := Lower(&Plan{Functions: map[*ir.Func]*Function{fn: function}}, &BodyReader{
		Have: func(*ir.Func) bool { return true },
		Read: func(*ir.Func) *ir.Func { return nil },
	})
	if err == nil || !strings.Contains(err.Error(), "missing admitted ordinary body") {
		t.Fatalf("Lower error = %v, want missing admitted body", err)
	}
}
