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

func TestExpressionSwitchBoundaries(t *testing.T) {
	prepareLowerTest(t)
	pkg := types.NewPkg("example.com/coro/switchboundary", "switchboundary")
	for _, test := range []struct{ name, want string }{
		{"label", "labeled switch"},
		{"tag", "coroutine site in switch header"},
		{"case", "coroutine site in switch header"},
		{"case-init", "coroutine site in switch header"},
		{"fallthrough", "control flow fallthrough"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fn := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup(test.name), types.NewSignature(nil, nil, nil))
			callee := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("callee"),
				types.NewSignature(nil, nil, []*types.Field{types.NewField(src.NoXPos, nil, types.Types[types.TINT])}))
			call := newLowerTestCall(callee)
			call.SetType(types.Types[types.TINT])
			clause := ir.NewCaseStmt(src.NoXPos, nil, ir.Nodes{newLowerTestReturn()})
			stmt := ir.NewSwitchStmt(src.NoXPos, nil, []*ir.CaseClause{clause})
			switch test.name {
			case "label":
				stmt.Label = pkg.Lookup("label")
			case "tag":
				stmt.Tag = call
			case "case":
				clause.List = ir.Nodes{call}
			case "case-init":
				clause.SetInit(ir.Nodes{call})
			case "fallthrough":
				clause.Body = append(clause.Body, ir.NewBranchStmt(src.NoXPos, ir.OFALL, nil))
			}
			fn.Body = ir.Nodes{stmt}
			_, err := newLowerCandidate(&Plan{}, &Function{Func: fn, Primary: CoroPrimary,
				Sites: []Site{{ID: 1, Kind: SiteAwait, Node: call}}})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestLowerExpressionSwitch(t *testing.T) {
	prepareLowerTest(t)
	oldPkg, oldTarget := types.LocalPkg, typecheck.Target
	defer func() { types.LocalPkg, typecheck.Target = oldPkg, oldTarget }()
	pkg := types.NewPkg("example.com/coro/switchlower", "switchlower")
	types.LocalPkg, typecheck.Target = pkg, new(ir.Package)
	input := types.NewField(src.NoXPos, pkg.Lookup("tag"), types.Types[types.TINT])
	result := types.NewField(src.NoXPos, nil, types.Types[types.TUINT64])
	fn := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("f"), types.NewSignature(nil, []*types.Field{input}, []*types.Field{result}))
	fn.Nname.Defn = fn
	fn.DeclareParams(true)
	yield := ir.NewFunc(src.NoXPos, src.NoXPos, types.NewPkg("runtime", "runtime").Lookup("Gosched"), types.NewSignature(nil, nil, nil))
	ret := func(value int64) *ir.ReturnStmt {
		stmt := ir.NewReturnStmt(src.NoXPos, ir.Nodes{typedInt(src.NoXPos, result.Type, value)})
		stmt.SetTypecheck(1)
		return stmt
	}
	clause := ir.NewCaseStmt(src.NoXPos, ir.Nodes{typedInt(src.NoXPos, input.Type, 0)}, ir.Nodes{newLowerTestCall(yield), ret(1 << 40)})
	stmt := ir.NewSwitchStmt(src.NoXPos, fn.Dcl[0], []*ir.CaseClause{clause, ir.NewCaseStmt(src.NoXPos, nil, ir.Nodes{ret(7)})})
	stmt.SetTypecheck(1)
	fn.Body = ir.Nodes{stmt}
	plan, err := Analyze([]*ir.Func{fn}, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Lower(plan, nil)
	if err != nil || got.Lowered != 1 || got.Skipped != 0 {
		t.Fatalf("Lower = (%+v, %v)", got, err)
	}
	if len(clause.Body) != 2 || clause.Body[0].Op() != ir.OCALLFUNC || clause.Body[1].Op() != ir.ORETURN {
		t.Fatal("dispatch rewrite changed the original case body")
	}
	found := false
	for _, generated := range typecheck.Target.Funcs {
		ir.VisitList(generated.Body, func(node ir.Node) {
			dispatch, ok := node.(*ir.SwitchStmt)
			if !ok || dispatch.Tag == nil || dispatch.Tag.Type() != input.Type {
				return
			}
			found = true
			for _, clause := range dispatch.Cases {
				if len(clause.Body) != 1 || clause.Body[0].Op() != ir.OGOTO {
					t.Fatal("source switch dispatch did not select a continuation")
				}
			}
		})
	}
	if !found {
		t.Fatal("source switch dispatch missing from generated resume")
	}
}

func TestExpressionSwitchContinue(t *testing.T) {
	prepareLowerTest(t)
	pkg := types.NewPkg("example.com/coro/switchcontinue", "switchcontinue")
	fn := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("f"), types.NewSignature(nil, nil, nil))
	yield := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("yield"), types.NewSignature(nil, nil, nil))
	call := newLowerTestCall(yield)
	branch := ir.NewBranchStmt(src.NoXPos, ir.OCONTINUE, nil)
	clause := ir.NewCaseStmt(src.NoXPos, nil, ir.Nodes{branch})
	stmt := ir.NewSwitchStmt(src.NoXPos, nil, []*ir.CaseClause{clause})
	loop := ir.NewForStmt(src.NoXPos, nil, nil, nil, ir.Nodes{stmt}, false)
	fn.Body = ir.Nodes{call, loop}
	candidate, err := newLowerCandidate(&Plan{}, &Function{Func: fn, Primary: CoroPrimary,
		Sites: []Site{{ID: 1, Kind: SiteYield, Node: call}}})
	if err != nil {
		t.Fatal(err)
	}
	if !candidate.switches[stmt] || candidate.branches[branch] != loop {
		t.Fatal("switch continue lost its enclosing loop target")
	}
}

func TestTypeSwitchAdmission(t *testing.T) {
	prepareLowerTest(t)
	pkg := types.NewPkg("example.com/coro/switchtype", "switchtype")
	sig := types.NewSignature(nil, nil, nil)
	for _, name := range []string{"direct", "nested", "inlined", "closure"} {
		t.Run(name, func(t *testing.T) {
			fn := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup(name), sig)
			guard := ir.NewTypeSwitchGuard(src.NoXPos, nil, ir.NewNilExpr(src.NoXPos, types.Types[types.TINTER]))
			stmt := ir.NewSwitchStmt(src.NoXPos, guard, []*ir.CaseClause{ir.NewCaseStmt(src.NoXPos, nil, nil)})
			var body ir.Node = stmt
			switch name {
			case "nested":
				body = ir.NewSwitchStmt(src.NoXPos, nil, []*ir.CaseClause{ir.NewCaseStmt(src.NoXPos, nil, ir.Nodes{stmt})})
			case "inlined":
				body = ir.NewInlinedCallExpr(src.NoXPos, ir.Nodes{stmt}, nil)
			case "closure":
				oldTarget := typecheck.Target
				typecheck.Target = new(ir.Package)
				defer func() { typecheck.Target = oldTarget }()
				closure := ir.NewClosureFunc(src.NoXPos, src.NoXPos, ir.OCLOSURE, sig, fn, typecheck.Target, 0)
				closure.Body = ir.Nodes{stmt}
				body = ir.NewAssignStmt(src.NoXPos, ir.BlankNode, closure.OClosure)
			}
			yield := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("yield"), sig)
			call := newLowerTestCall(yield)
			fn.Body = ir.Nodes{call, body}
			if got, want := hasUnplannedTypeSwitch(fn.Body, nil), name != "closure"; got != want {
				t.Fatalf("hasUnplannedTypeSwitch = %v, want %v", got, want)
			}
			candidate, err := newLowerCandidate(&Plan{}, &Function{Func: fn, Primary: CoroPrimary,
				Sites: []Site{{ID: 1, Kind: SiteYield, Node: call}}})
			if name != "inlined" {
				if err != nil {
					t.Fatal(err)
				}
				if name != "closure" && !candidate.switches[stmt] {
					t.Fatal("type switch has no planned dispatch")
				}
			} else if err == nil || !strings.Contains(err.Error(), "type switch") {
				t.Fatalf("error = %v, want type switch boundary", err)
			}
		})
	}
}

func TestLowerTypeSwitch(t *testing.T) {
	for _, test := range []struct {
		name      string
		addressed bool
	}{{"value", false}, {"address", true}} {
		t.Run(test.name, func(t *testing.T) { testLowerTypeSwitch(t, test.addressed) })
	}
}

func testLowerTypeSwitch(t *testing.T, addressed bool) {
	prepareLowerTest(t)
	oldPkg, oldTarget := types.LocalPkg, typecheck.Target
	defer func() { types.LocalPkg, typecheck.Target = oldPkg, oldTarget }()
	pkg := types.NewPkg("example.com/coro/typeswitchlower", "typeswitchlower")
	types.LocalPkg, typecheck.Target = pkg, new(ir.Package)
	input := types.NewField(src.NoXPos, pkg.Lookup("input"), types.Types[types.TINTER])
	result := types.NewField(src.NoXPos, nil, types.Types[types.TINT])
	fn := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("f"), types.NewSignature(nil, []*types.Field{input}, []*types.Field{result}))
	fn.Nname.Defn = fn
	fn.DeclareParams(true)
	guard := ir.NewTypeSwitchGuard(src.NoXPos, ir.NewIdent(src.NoXPos, pkg.Lookup("x")), fn.Dcl[0])
	guard.SetTypecheck(1)
	variable := fn.NewLocal(src.NoXPos, pkg.Lookup("x"), result.Type)
	variable.Defn = guard
	variable.SetAddrtaken(addressed)
	yield := ir.NewFunc(src.NoXPos, src.NoXPos, types.NewPkg("runtime", "runtime").Lookup("Gosched"), types.NewSignature(nil, nil, nil))
	ret := ir.NewReturnStmt(src.NoXPos, ir.Nodes{variable})
	ret.SetTypecheck(1)
	clause := ir.NewCaseStmt(src.NoXPos, ir.Nodes{ir.TypeNode(result.Type)}, ir.Nodes{newLowerTestCall(yield), ret})
	clause.Var = variable
	stmt := ir.NewSwitchStmt(src.NoXPos, guard, []*ir.CaseClause{clause})
	stmt.SetTypecheck(1)
	zero := ir.NewReturnStmt(src.NoXPos, ir.Nodes{typedInt(src.NoXPos, result.Type, 0)})
	zero.SetTypecheck(1)
	fn.Body = ir.Nodes{stmt, zero}
	plan, err := Analyze([]*ir.Func{fn}, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Lower(plan, nil)
	if err != nil || got.Lowered != 1 || got.Skipped != 0 {
		t.Fatalf("Lower = (%+v, %v)", got, err)
	}
	if clause.Var != variable || len(clause.Body) != 2 {
		t.Fatal("dispatch rewrite changed the source case binding or body")
	}
	found := false
	for _, generated := range typecheck.Target.Funcs {
		ir.VisitList(generated.Body, func(node ir.Node) {
			dispatch, ok := node.(*ir.SwitchStmt)
			if !ok || dispatch.Tag == nil || dispatch.Tag.Op() != ir.OTYPESW {
				return
			}
			found = true
			binding := dispatch.Cases[0].Var
			if binding == variable || binding.Curfn != generated || binding.Type() != result.Type {
				t.Fatal("walk's case binding is not a typed resume-local temporary")
			}
			body := dispatch.Cases[0].Body
			if len(body) != 3 || body[1].Op() != ir.OAS || body[2].Op() != ir.OGOTO {
				t.Fatal("selected binding was not saved before entering its continuation")
			}
			if addressed && body[0].Op() != ir.OAS {
				t.Fatal("addressable binding has no per-selection cell allocation")
			}
		})
	}
	if !found {
		t.Fatal("type switch dispatch missing from generated resume")
	}
}

func TestTypeSwitchClosureBoundary(t *testing.T) {
	prepareLowerTest(t)
	oldPkg, oldTarget := types.LocalPkg, typecheck.Target
	defer func() { types.LocalPkg, typecheck.Target = oldPkg, oldTarget }()
	pkg := types.NewPkg("example.com/coro/typeswitchcapture", "typeswitchcapture")
	types.LocalPkg, typecheck.Target = pkg, new(ir.Package)
	sig := types.NewSignature(nil, nil, nil)
	for _, test := range []struct {
		name                          string
		bound, captures, deferLiteral bool
		want                          bool
	}{
		{name: "unbound"},
		{name: "unrelated", bound: true},
		{name: "ordinary", bound: true, captures: true, want: true},
		{name: "defer", bound: true, captures: true, deferLiteral: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fn := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup(test.name), sig)
			fn.DeclareParams(true)
			variable := fn.NewLocal(src.NoXPos, pkg.Lookup("x"), types.Types[types.TINT])
			guard := ir.NewTypeSwitchGuard(src.NoXPos, nil, ir.NewNilExpr(src.NoXPos, types.Types[types.TINTER]))
			clause := ir.NewCaseStmt(src.NoXPos, nil, nil)
			if test.bound {
				clause.Var = variable
			}
			closure := ir.NewClosureFunc(src.NoXPos, src.NoXPos, ir.OCLOSURE, sig, fn, typecheck.Target, 0)
			captured := variable
			if !test.captures {
				captured = fn.NewLocal(src.NoXPos, pkg.Lookup("other"), types.Types[types.TINT])
			}
			ir.NewClosureVar(src.NoXPos, closure, captured)
			call := ir.NewCallExpr(src.NoXPos, ir.OCALLFUNC, closure.OClosure, nil)
			var defers []*lowerDefer
			if test.deferLiteral {
				clause.Body = ir.Nodes{ir.NewGoDeferStmt(src.NoXPos, ir.ODEFER, call)}
				defers = []*lowerDefer{{call: call}}
			} else {
				clause.Body = ir.Nodes{ir.NewAssignStmt(src.NoXPos, ir.BlankNode, closure.OClosure)}
			}
			stmt := ir.NewSwitchStmt(src.NoXPos, guard, []*ir.CaseClause{clause})
			fn.Body = ir.Nodes{stmt}
			if got := typeSwitchHasOrdinaryClosureCapture(fn, map[*ir.SwitchStmt]bool{stmt: true}, defers); got != test.want {
				t.Fatalf("ordinary closure capture = %v, want %v", got, test.want)
			}
			if test.want {
				yield := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("yield"), sig)
				call := newLowerTestCall(yield)
				fn.Body = append(ir.Nodes{call}, fn.Body...)
				_, err := newLowerCandidate(&Plan{}, &Function{Func: fn, Primary: CoroPrimary,
					Sites: []Site{{ID: 1, Kind: SiteYield, Node: call}}})
				if err == nil || !strings.Contains(err.Error(), "type switch variable captured by closure") {
					t.Fatalf("error = %v, want ordinary case capture boundary", err)
				}
			}
		})
	}
}
