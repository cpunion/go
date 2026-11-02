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

func TestNormalizeSingleResultCalls(t *testing.T) {
	prepareLowerTest(t)

	oldTarget := typecheck.Target
	oldLocalPkg := types.LocalPkg
	oldCurFunc := ir.CurFunc
	defer func() {
		typecheck.Target = oldTarget
		types.LocalPkg = oldLocalPkg
		ir.CurFunc = oldCurFunc
	}()

	pkg := types.NewPkg("example.com/coro/normalize", "normalize")
	types.LocalPkg = pkg
	typecheck.Target = new(ir.Package)

	result := []*types.Field{
		types.NewField(src.NoXPos, nil, types.Types[types.TINT]),
	}
	inner := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("inner"),
		types.NewSignature(nil, nil, result))
	inner.DeclareParams(true)
	value := types.NewField(src.NoXPos, pkg.Lookup("value"),
		types.Types[types.TINT])
	outer := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("outer"),
		types.NewSignature(nil, []*types.Field{value}, result))
	outer.DeclareParams(true)
	caller := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("caller"),
		types.NewSignature(nil, nil, result))
	caller.DeclareParams(true)
	typecheck.Target.Funcs = append(typecheck.Target.Funcs, caller)

	innerCall := newLowerTestCall(inner)
	innerCall.SetType(types.Types[types.TINT])
	outerCall := ir.NewCallExpr(src.NoXPos, ir.OCALLFUNC, outer.Nname,
		ir.Nodes{innerCall})
	outerCall.SetType(types.Types[types.TINT])
	outerCall.SetTypecheck(1)
	ret := ir.NewReturnStmt(src.NoXPos, ir.Nodes{outerCall})
	ret.SetTypecheck(1)
	prefixName := caller.NewLocal(src.NoXPos, pkg.Lookup("prefix"),
		types.Types[types.TINT])
	prefix := ir.NewDecl(src.NoXPos, ir.ODCL, prefixName)
	ret.SetInit(ir.Nodes{prefix})
	clause := ir.NewCaseStmt(src.NoXPos, nil, ir.Nodes{ret})
	stmt := ir.NewSwitchStmt(src.NoXPos, nil, []*ir.CaseClause{clause})
	caller.Body = ir.Nodes{stmt}

	normalizeSingleResultCalls(&Function{
		Func:    caller,
		Effect:  MaySuspend,
		Primary: CoroPrimary,
		Sites: []Site{
			{ID: 1, Kind: SiteAwait, Node: outerCall},
			{ID: 2, Kind: SiteAwait, Node: innerCall},
		},
	})

	if len(ret.Init()) != 3 {
		t.Fatalf("return Init has %d statements, want 3", len(ret.Init()))
	}
	if len(stmt.Init()) != 0 || len(clause.Body) != 1 || clause.Body[0] != ret {
		t.Fatal("case expressions moved into the switch dispatch")
	}
	if ret.Init()[0] != prefix {
		t.Fatalf("first normalized statement = %v, want prefix %v",
			ret.Init()[0], prefix)
	}
	first, ok := ret.Init()[1].(*ir.AssignStmt)
	if !ok || first.Y != innerCall {
		t.Fatalf("second normalized statement = %v, want inner call", ret.Init()[1])
	}
	second, ok := ret.Init()[2].(*ir.AssignStmt)
	if !ok || second.Y != outerCall {
		t.Fatalf("third normalized statement = %v, want outer call", ret.Init()[2])
	}
	if outerCall.Args[0] != first.X || ret.Results[0] != second.X {
		t.Fatalf("normalized temporaries are not used by the enclosing expressions")
	}
	if len(first.Init()) != 1 || first.Init()[0].Op() != ir.ODCL ||
		first.X.Name().Defn != first {
		t.Fatalf("first temporary is not declared and defined by its assignment")
	}
}

func TestNormalizeSingleResultCallSafety(t *testing.T) {
	prepareLowerTest(t)

	oldTarget := typecheck.Target
	oldLocalPkg := types.LocalPkg
	oldCurFunc := ir.CurFunc
	defer func() {
		typecheck.Target = oldTarget
		types.LocalPkg = oldLocalPkg
		ir.CurFunc = oldCurFunc
	}()

	pkg := types.NewPkg("example.com/coro/normalizesafety",
		"normalizesafety")
	types.LocalPkg = pkg
	typecheck.Target = new(ir.Package)

	boolResult := []*types.Field{
		types.NewField(src.NoXPos, nil, types.Types[types.TBOOL]),
	}
	awaited := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("awaited"),
		types.NewSignature(nil, nil, boolResult))
	awaited.DeclareParams(true)
	plain := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("plain"),
		types.NewSignature(nil, nil, boolResult))
	plain.DeclareParams(true)
	argument := types.NewField(src.NoXPos, pkg.Lookup("argument"),
		types.Types[types.TBOOL])
	wrap := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("wrap"),
		types.NewSignature(nil, []*types.Field{argument}, boolResult))
	wrap.DeclareParams(true)
	caller := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("caller"),
		types.NewSignature(nil, nil, boolResult))
	caller.DeclareParams(true)
	typecheck.Target.Funcs = append(typecheck.Target.Funcs, caller)

	newCall := func(fn *ir.Func) *ir.CallExpr {
		call := newLowerTestCall(fn)
		call.SetType(types.Types[types.TBOOL])
		return call
	}

	conditionalCall := newCall(awaited)
	conditional := ir.NewLogicalExpr(src.NoXPos, ir.OANDAND,
		ir.NewBool(src.NoXPos, true), conditionalCall)
	conditional.SetType(types.Types[types.TBOOL])
	conditional.SetTypecheck(1)
	conditionalReturn := ir.NewReturnStmt(src.NoXPos,
		ir.Nodes{conditional})
	conditionalReturn.SetTypecheck(1)

	prefixCall := newCall(awaited)
	prefix := ir.NewLogicalExpr(src.NoXPos, ir.OANDAND,
		newCall(plain), prefixCall)
	prefix.SetType(types.Types[types.TBOOL])
	prefix.SetTypecheck(1)
	prefixReturn := ir.NewReturnStmt(src.NoXPos, ir.Nodes{prefix})
	prefixReturn.SetTypecheck(1)

	directCall := newCall(awaited)
	direct := ir.NewCallExpr(src.NoXPos, ir.OCALLFUNC, wrap.Nname,
		ir.Nodes{directCall})
	direct.SetType(types.Types[types.TBOOL])
	direct.SetTypecheck(1)
	directReturn := ir.NewReturnStmt(src.NoXPos, ir.Nodes{direct})
	directReturn.SetTypecheck(1)

	forCall := newCall(awaited)
	loop := ir.NewForStmt(src.NoXPos, nil, forCall, nil, nil, false)
	loop.SetTypecheck(1)

	ifCall := newCall(awaited)
	ifStmt := ir.NewIfStmt(src.NoXPos, ifCall, nil, nil)
	ifStmt.SetTypecheck(1)

	blockCall := newCall(awaited)
	blockReturn := ir.NewReturnStmt(src.NoXPos, ir.Nodes{blockCall})
	blockReturn.SetTypecheck(1)
	block := ir.NewBlockStmt(src.NoXPos, ir.Nodes{blockReturn})
	block.SetTypecheck(1)

	rangeCall := newCall(awaited)
	rangeIf := ir.NewIfStmt(src.NoXPos, rangeCall, nil, nil)
	rangeStmt := ir.NewRangeStmt(src.NoXPos, nil, nil,
		ir.NewInt(src.NoXPos, 3), ir.Nodes{rangeIf}, false)
	commCall := newCall(awaited)
	channel := caller.NewLocal(src.NoXPos, pkg.Lookup("channel"),
		types.NewChan(types.Types[types.TBOOL], types.Cboth))
	comm := ir.NewSendStmt(src.NoXPos, channel, commCall)
	selectCall := newCall(awaited)
	selectIf := ir.NewIfStmt(src.NoXPos, selectCall, nil, nil)
	defaultCall := newCall(awaited)
	defaultIf := ir.NewIfStmt(src.NoXPos, defaultCall, nil, nil)
	selection := ir.NewSelectStmt(src.NoXPos, []*ir.CommClause{
		ir.NewCommStmt(src.NoXPos, comm, ir.Nodes{selectIf}),
		ir.NewCommStmt(src.NoXPos, nil, ir.Nodes{defaultIf}),
	})

	local := caller.NewLocal(src.NoXPos, pkg.Lookup("local"),
		types.Types[types.TINT])
	decl := ir.NewDecl(src.NoXPos, ir.ODCL, local)
	assignOp := ir.NewAssignOpStmt(src.NoXPos, ir.OADD, local,
		ir.NewInt(src.NoXPos, 1))

	caller.Body = ir.Nodes{
		decl,
		conditionalReturn,
		prefixReturn,
		directReturn,
		loop,
		ifStmt,
		block,
		rangeStmt,
		selection,
		assignOp,
	}
	normalizeSingleResultCalls(&Function{
		Func:    caller,
		Effect:  MaySuspend,
		Primary: CoroPrimary,
		Sites: []Site{
			{ID: 1, Kind: SiteAwait, Node: conditionalCall},
			{ID: 2, Kind: SiteAwait, Node: prefixCall},
			{ID: 3, Kind: SiteAwait, Node: directCall},
			{ID: 4, Kind: SiteAwait, Node: forCall},
			{ID: 5, Kind: SiteAwait, Node: ifCall},
			{ID: 6, Kind: SiteAwait, Node: blockCall},
			{ID: 7, Kind: SiteAwait, Node: rangeCall},
			{ID: 8, Kind: SiteAwait, Node: commCall},
			{ID: 9, Kind: SiteAwait, Node: selectCall},
			{ID: 10, Kind: SiteAwait, Node: defaultCall},
		},
	})

	if len(conditionalReturn.Init()) != 0 {
		t.Fatal("short-circuit right operand was hoisted")
	}
	if len(prefixReturn.Init()) != 0 {
		t.Fatal("call following an observable prefix was hoisted")
	}
	if len(directReturn.Init()) != 1 {
		t.Fatalf("direct nested call Init has %d statements, want 1",
			len(directReturn.Init()))
	}
	if loop.Cond != forCall || len(loop.Init()) != 0 {
		t.Fatal("for condition call was hoisted into the one-time Init list")
	}
	if len(ifStmt.Init()) != 1 || ifStmt.Cond == ifCall {
		t.Fatal("if condition call was not normalized")
	}
	if len(blockReturn.Init()) != 1 {
		t.Fatal("call in block return was not normalized")
	}
	if len(rangeStmt.Init()) != 0 || len(rangeIf.Init()) != 1 || rangeIf.Cond == rangeCall {
		t.Fatal("range body await must stay in its iteration")
	}
	if len(selection.Init()) != 0 || len(comm.Init()) != 0 || comm.Value != commCall {
		t.Fatal("select communication await moved into dispatch")
	}
	if len(selectIf.Init()) != 1 || selectIf.Cond == selectCall ||
		len(defaultIf.Init()) != 1 || defaultIf.Cond == defaultCall {
		t.Fatal("select body awaits must stay in their chosen cases")
	}

	inlinedCall := newCall(awaited)
	inlined := ir.NewInlinedCallExpr(src.NoXPos,
		ir.Nodes{inlinedCall}, ir.Nodes{ir.NewBool(src.NoXPos, true)})
	inlined.SetType(types.Types[types.TBOOL])
	inlined.SetTypecheck(1)
	if safeCallPrefix(inlined, inlinedCall) {
		t.Fatal("call in an inlined control-flow body can be hoisted")
	}

	if root, init := normalizeCallExpression(nil, caller,
		map[*ir.CallExpr]bool{}, false); root != nil || len(init) != 0 {
		t.Fatalf("nil expression normalized to %v, %v", root, init)
	}
}

func TestNormalizeSingleResultCallDirectStatements(t *testing.T) {
	prepareLowerTest(t)

	pkg := types.NewPkg("example.com/coro/normalizedirect",
		"normalizedirect")
	result := []*types.Field{
		types.NewField(src.NoXPos, nil, types.Types[types.TINT]),
	}
	callee := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("callee"),
		types.NewSignature(nil, nil, result))
	callee.DeclareParams(true)
	call := newLowerTestCall(callee)
	call.SetType(types.Types[types.TINT])
	name := ir.NewNameAt(src.NoXPos, pkg.Lookup("value"),
		types.Types[types.TINT])

	if !directCallStatement(call, call) {
		t.Fatal("call statement is not direct")
	}
	if !directCallStatement(ir.NewAssignStmt(src.NoXPos, name, call), call) {
		t.Fatal("assignment call is not direct")
	}
	list := ir.NewAssignListStmt(src.NoXPos, ir.OAS2,
		ir.Nodes{name}, ir.Nodes{call})
	if !directCallStatement(list, call) {
		t.Fatal("assignment-list call is not direct")
	}
	if directCallStatement(ir.NewReturnStmt(src.NoXPos,
		ir.Nodes{call}), call) {
		t.Fatal("return call is unexpectedly direct")
	}

	normalizeSingleResultCalls(nil)
	normalizeSingleResultCalls(&Function{})
	normalizeSingleResultCalls(&Function{Func: callee, Primary: PlainPrimary})
}

func TestNormalizeInlinedAwaitControlFlow(t *testing.T) {
	prepareLowerTest(t)
	oldTarget, oldLocal := typecheck.Target, types.LocalPkg
	defer func() { typecheck.Target = oldTarget; types.LocalPkg = oldLocal }()
	pkg := types.NewPkg("example.com/coro/inlineawait", "p")
	types.LocalPkg = pkg
	typecheck.Target = new(ir.Package)
	callee := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("callee"), types.NewSignature(nil, nil, []*types.Field{types.NewField(src.NoXPos, nil, types.Types[types.TINT])}))
	callee.DeclareParams(true)
	caller := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("caller"), callee.Type())
	caller.DeclareParams(true)
	value := caller.NewLocal(src.NoXPos, pkg.Lookup("value"), types.Types[types.TINT])
	call := newLowerTestCall(callee)
	call.SetType(types.Types[types.TINT])
	assignment := ir.NewAssignStmt(src.NoXPos, value, call)
	label := ir.NewLabelStmt(src.NoXPos, pkg.Lookup("inlineExit"))
	jump := ir.NewBranchStmt(src.NoXPos, ir.OGOTO, label.Label)
	branch := ir.NewIfStmt(src.NoXPos, ir.NewBool(src.NoXPos, true), ir.Nodes{assignment, jump}, nil)
	inlined := ir.NewInlinedCallExpr(src.NoXPos, ir.Nodes{branch, label}, ir.Nodes{value})
	inlined.SetType(value.Type())
	inlined.SetTypecheck(1)
	ret := ir.NewReturnStmt(src.NoXPos, ir.Nodes{inlined})
	caller.Body = ir.Nodes{ret}
	function := &Function{Func: caller, Primary: CoroPrimary, Effect: MaySuspend, Sites: []Site{{ID: 1, Kind: SiteAwait, Node: call}}}
	normalizeSingleResultCalls(function)
	if len(ret.Init()) != 2 || ret.Init()[0] != branch || ret.Init()[1] != label || ret.Results[0] != value {
		t.Fatal("inline execution was not exposed as an intact ordered body")
	}
	if branch.Body[0] != assignment || branch.Body[1] != jump || function.inlineLabels[label.Label] != label {
		t.Fatal("inline return jump or conditional await lost its ownership")
	}
}
