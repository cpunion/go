// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/ir"
	"cmd/compile/internal/typecheck"
	"cmd/compile/internal/types"
	"cmd/internal/src"
	"slices"
	"strings"
	"testing"
)

func TestLocalFunctionTargets(t *testing.T) {
	prepareLowerTest(t)
	for _, test := range []struct {
		name string
		want int
	}{
		{"branches", 2}, {"converted", 1}, {"parallel", 2}, {"element", 1}, {"zero", 0},
		{"alias", 0}, {"unknown", 0}, {"multi-result", 0}, {"parameter", 0},
		{"global", 0}, {"address-flag", 0}, {"address", 0}, {"capture", 0},
		{"range", 0}, {"type-case", 0}, {"closure", 0}, {"shape", 0}, {"method", 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			pkg := types.NewPkg("example.com/coro/dynamicproof/"+test.name, "p")
			fn := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("parent"), types.NewSignature(nil, nil, nil))
			first := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("first"), types.NewSignature(nil, nil, nil))
			second := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("second"), types.NewSignature(nil, nil, nil))
			local := ir.NewNameAt(src.NoXPos, pkg.Lookup("f"), first.Type())
			local.Class, local.Curfn = ir.PAUTO, fn
			fn.Dcl = []*ir.Name{local}
			assign := ir.NewAssignStmt(src.NoXPos, local, first.Nname)
			fn.Body = ir.Nodes{assign}
			switch test.name {
			case "branches":
				branch := ir.NewIfStmt(src.NoXPos, ir.NewBool(src.NoXPos, true), ir.Nodes{ir.NewAssignStmt(src.NoXPos, local, second.Nname)}, nil)
				branch.SetInit(ir.Nodes{ir.NewAssignStmt(src.NoXPos, local, ir.NewNilExpr(src.NoXPos, local.Type()))})
				fn.Body = append(fn.Body, branch)
			case "converted":
				assign.Y = ir.NewConvExpr(src.NoXPos, ir.OCONVNOP, local.Type(), first.Nname)
			case "parallel":
				fn.Body = append(fn.Body, ir.NewAssignListStmt(src.NoXPos, ir.OAS2, ir.Nodes{local}, ir.Nodes{second.Nname}))
			case "element":
				array := ir.NewNameAt(src.NoXPos, pkg.Lookup("array"), types.NewArray(local.Type(), 1))
				element := ir.NewIndexExpr(src.NoXPos, array, ir.NewInt(src.NoXPos, 0))
				fn.Body = append(fn.Body, ir.NewAssignStmt(src.NoXPos, element, second.Nname))
			case "zero":
				assign.Y = nil
			case "alias":
				assign.Y = local
			case "unknown":
				assign.Y = newLowerTestCall(second)
			case "multi-result":
				fn.Body = append(fn.Body, ir.NewAssignListStmt(src.NoXPos, ir.OAS2FUNC, ir.Nodes{local}, ir.Nodes{second.Nname}))
			case "parameter":
				local.Class = ir.PPARAM
			case "global":
				local.Class = ir.PEXTERN
			case "address-flag":
				local.SetAddrtaken(true)
			case "address":
				fn.Body = append(fn.Body, ir.NewAddrExpr(src.NoXPos, local))
			case "capture":
				closure := &ir.ClosureExpr{Func: second}
				second.ClosureVars = []*ir.Name{ir.NewClosureVar(src.NoXPos, second, local)}
				fn.Body = append(fn.Body, closure)
			case "range":
				stmt := ir.NewRangeStmt(src.NoXPos, nil, nil, nil, nil, false)
				stmt.Key = local
				fn.Body = append(fn.Body, stmt)
			case "type-case":
				clause := ir.NewCaseStmt(src.NoXPos, nil, fn.Body)
				clause.Var = local
				fn.Body = ir.Nodes{ir.NewSwitchStmt(src.NoXPos, nil, []*ir.CaseClause{clause})}
			case "closure":
				second.OClosure = &ir.ClosureExpr{Func: second}
				assign.Y = second.OClosure
			case "shape":
				first.Type().SetHasShape(true)
			case "method":
				recv := types.NewField(src.NoXPos, nil, types.Types[types.TINT])
				first.Nname.SetType(types.NewSignature(recv, nil, nil))
			}
			got := localFunctionTargets(fn)[local]
			if len(got) != test.want {
				t.Fatalf("targets = %v, want %d", got, test.want)
			}
			if len(got) > 1 && (!slices.Contains(got, first) || !slices.Contains(got, second)) {
				t.Fatal("missing target")
			}
		})
	}
}

func TestDynamicCallEffects(t *testing.T) {
	prepareLowerTest(t)
	pkg := types.NewPkg("example.com/coro/dynamiceffects", "p")
	a, b := testFunc("dynamicA"), testFunc("dynamicB")
	a.Nname.SetSym(pkg.Lookup("a"))
	b.Nname.SetSym(pkg.Lookup("b"))
	p := &Plan{Functions: map[*ir.Func]*Function{
		a: {Func: a, Effect: MaySuspend, Exec: NeedsSystemABI, Terminal: MayPanic},
		b: {Func: b, Exec: NeedsTaskContext, Terminal: MayGoexit},
	}}
	edge := Edge{Targets: []*ir.Func{a, b}}
	got, known := p.edgeSummary(edge)
	if !known || got.Effect != MaySuspend || got.Exec != NeedsSystemABI|NeedsTaskContext || got.Terminal != MayPanic|MayGoexit || !p.edgeNeedsCoroEntry(edge) {
		t.Fatalf("summary=%+v known=%t", got, known)
	}
	p.Functions[a], p.Functions[b] = &Function{Func: a}, &Function{Func: b}
	if p.edgeNeedsCoroEntry(edge) {
		t.Fatal("closed plain targets unnecessarily require a coroutine entry")
	}
	delete(p.Functions, b)
	if _, known := p.edgeSummary(edge); known || !p.edgeNeedsCoroEntry(Edge{Targets: []*ir.Func{b}}) {
		t.Fatal("unknown target lost conservative effect")
	}
}

func TestLocalFunctionAliasTargets(t *testing.T) {
	prepareLowerTest(t)
	for _, test := range []struct {
		name string
		want int
	}{
		{"chain", 1}, {"converted", 1}, {"diamond", 2}, {"parallel", 2},
		{"seeded-cycle", 2}, {"self", 1}, {"empty-cycle", 0}, {"nil", 0},
		{"nil-and-seed", 1}, {"duplicate", 1}, {"isolated-unknown", 1},
		{"late-unknown", 0}, {"cycle-unknown", 0}, {"parameter", 0},
		{"global", 0}, {"other-function", 0}, {"address", 0}, {"capture", 0},
		{"range", 0}, {"type-case", 0}, {"multi-result", 0}, {"closure", 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			pkg := types.NewPkg("example.com/coro/aliasproof/"+test.name, "p")
			fn := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("parent"), types.NewSignature(nil, nil, nil))
			first := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("first"), fn.Type())
			second := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("second"), fn.Type())
			local := func(symbol string) *ir.Name {
				name := ir.NewNameAt(src.NoXPos, pkg.Lookup(symbol), first.Type())
				name.Class, name.Curfn = ir.PAUTO, fn
				fn.Dcl = append(fn.Dcl, name)
				return name
			}
			a, b, c, d := local("a"), local("b"), local("c"), local("d")
			assign := func(lhs, rhs ir.Node) *ir.AssignStmt { return ir.NewAssignStmt(src.NoXPos, lhs, rhs) }
			fn.Body = ir.Nodes{assign(a, first.Nname), assign(b, a), assign(c, b), assign(d, c)}
			switch test.name {
			case "converted":
				fn.Body[1].(*ir.AssignStmt).Y = ir.NewConvExpr(src.NoXPos, ir.OCONVNOP, b.Type(), a)
			case "diamond":
				fn.Body = append(fn.Body, assign(c, a), assign(b, second.Nname))
			case "parallel":
				fn.Body = append(fn.Body, ir.NewAssignListStmt(src.NoXPos, ir.OAS2, ir.Nodes{a, b}, ir.Nodes{b, second.Nname}))
			case "seeded-cycle":
				fn.Body = append(fn.Body, assign(a, c), assign(b, second.Nname))
			case "self":
				fn.Body = append(fn.Body, assign(b, b))
			case "empty-cycle":
				fn.Body[0] = assign(a, c)
			case "nil":
				fn.Body[0] = assign(a, ir.NewNilExpr(src.NoXPos, a.Type()))
			case "nil-and-seed":
				fn.Body[0] = assign(a, nil)
				fn.Body = append(fn.Body, assign(b, second.Nname))
			case "duplicate":
				fn.Body = append(fn.Body, assign(b, a), assign(b, a))
			case "isolated-unknown":
				fn.Body = append(fn.Body, assign(local("unrelated"), newLowerTestCall(second)))
			case "late-unknown", "cycle-unknown":
				fn.Body = append(fn.Body, assign(a, newLowerTestCall(second)), assign(d, first.Nname))
				if test.name == "cycle-unknown" {
					fn.Body = append(fn.Body, assign(a, d))
				}
			case "parameter":
				a.Class = ir.PPARAM
			case "global":
				a.Class = ir.PEXTERN
			case "other-function":
				a.Curfn = second
			case "address":
				fn.Body = append(fn.Body, ir.NewAddrExpr(src.NoXPos, a))
			case "capture":
				second.ClosureVars = []*ir.Name{ir.NewClosureVar(src.NoXPos, second, a)}
				fn.Body = append(fn.Body, &ir.ClosureExpr{Func: second})
			case "range":
				stmt := ir.NewRangeStmt(src.NoXPos, nil, nil, nil, nil, false)
				stmt.Value = a
				fn.Body = append(fn.Body, stmt)
			case "type-case":
				clause := ir.NewCaseStmt(src.NoXPos, nil, fn.Body)
				clause.Var = a
				fn.Body = ir.Nodes{ir.NewSwitchStmt(src.NoXPos, nil, []*ir.CaseClause{clause})}
			case "multi-result":
				fn.Body = append(fn.Body, ir.NewAssignListStmt(src.NoXPos, ir.OAS2FUNC, ir.Nodes{a}, ir.Nodes{second.Nname}))
			case "closure":
				fn.Body = append(fn.Body, assign(a, &ir.ClosureExpr{Func: second}))
			}
			got := localFunctionTargets(fn)[d]
			if len(got) != test.want || test.want == 2 && (got[0] != first || got[1] != second) {
				t.Fatalf("targets=%v, want %d sorted targets", got, test.want)
			}
		})
	}
}

func TestLowerDynamicCall(t *testing.T) {
	prepareLowerTest(t)
	oldPkg, oldTarget := types.LocalPkg, typecheck.Target
	defer func() { types.LocalPkg, typecheck.Target = oldPkg, oldTarget }()
	pkg := types.NewPkg("example.com/coro/dynamiclower", "p")
	types.LocalPkg, typecheck.Target = pkg, new(ir.Package)
	child := func(name string) *ir.Func {
		fn := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup(name), types.NewSignature(nil, []*types.Field{
			types.NewField(src.NoXPos, pkg.Lookup("argument"), types.Types[types.TINT]),
		}, nil))
		fn.Nname.Defn = fn
		fn.DeclareParams(true)
		typecheck.Target.Funcs = append(typecheck.Target.Funcs, fn)
		return fn
	}
	first, second, parent := child("first"), child("second"), newLowerTestFunc(pkg, "parent")
	yield := ir.NewFunc(src.NoXPos, src.NoXPos, types.NewPkg("runtime", "runtime").Lookup("Gosched"), types.NewSignature(nil, nil, nil))
	first.Body = ir.Nodes{newLowerTestCall(yield), newLowerTestReturn()}
	panicStmt := ir.NewUnaryExpr(src.NoXPos, ir.OPANIC, ir.NewNilExpr(src.NoXPos, types.Types[types.TINTER]))
	panicStmt.SetTypecheck(1)
	second.Body = ir.Nodes{newLowerTestCall(yield), panicStmt}
	local := ir.NewNameAt(src.NoXPos, pkg.Lookup("f"), first.Type())
	local.Class, local.Curfn = ir.PAUTO, parent
	parent.Dcl = append(parent.Dcl, local)
	alias := ir.NewNameAt(src.NoXPos, pkg.Lookup("alias"), first.Type())
	alias.Class, alias.Curfn = ir.PAUTO, parent
	parent.Dcl = append(parent.Dcl, alias)
	argument := typedInt(src.NoXPos, types.Types[types.TINT], 42)
	call := ir.NewCallExpr(src.NoXPos, ir.OCALLFUNC, alias, ir.Nodes{argument})
	call.SetTypecheck(1)
	parent.Body = ir.Nodes{ir.NewDecl(src.NoXPos, ir.ODCL, local), ir.NewAssignStmt(src.NoXPos, local, first.Nname),
		ir.NewIfStmt(src.NoXPos, ir.NewBool(src.NoXPos, true), ir.Nodes{ir.NewAssignStmt(src.NoXPos, local, second.Nname)}, nil),
		ir.NewDecl(src.NoXPos, ir.ODCL, alias), ir.NewAssignStmt(src.NoXPos, alias, local), call, newLowerTestReturn()}
	plan, err := Analyze([]*ir.Func{first, second, parent}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var dynamic Edge
	for _, edge := range plan.Functions[parent].Edges {
		if edge.Node == call {
			dynamic = edge
		}
	}
	if dynamic.Callee != nil || len(dynamic.Targets) != 2 || dynamic.Unknown {
		t.Fatalf("edge=%+v", dynamic)
	}
	var dump strings.Builder
	plan.Dump(&dump)
	if !strings.Contains(dump.String(), "closed-targets caller="+ir.PkgFuncName(parent)+" targets="+ir.PkgFuncName(first)+","+ir.PkgFuncName(second)) {
		t.Fatalf("missing deterministic target proof in dump:\n%s", &dump)
	}
	got, err := Lower(plan, nil)
	if err != nil || got.Lowered != 3 {
		t.Fatalf("Lower=(%+v,%v)", got, err)
	}
	if call.Fun != alias || len(call.Args) != 1 || call.Args[0] != argument {
		t.Fatal("original dynamic call changed")
	}
	if plan.Functions[first].Factory != FactoryABI3 || plan.Functions[second].Factory != FactoryABI1 {
		t.Fatal("dispatch did not exercise distinct typed factory contracts")
	}
	found := map[string]bool{}
	for _, fn := range typecheck.Target.Funcs {
		ir.VisitList(fn.Body, func(n ir.Node) {
			if call, ok := n.(*ir.CallExpr); ok {
				if name := ir.StaticCalleeName(call.Fun); name != nil {
					found[name.Sym().Name] = true
				}
			}
		})
	}
	for _, name := range []string{"first.coro", "second.coro", "coroAwaitFrame"} {
		if !found[name] {
			t.Errorf("missing dispatch entry %s", name)
		}
	}
	if strings.Contains(strings.Join(got.Diagnostics, "\n"), "dynamic await") {
		t.Fatal(got.Diagnostics)
	}
}

func TestDynamicCallAnalysis(t *testing.T) {
	prepareLowerTest(t)
	for _, operation := range []bool{false, true} {
		name := "unknown-import"
		if operation {
			name = "operation-recipe"
		}
		t.Run(name, func(t *testing.T) {
			pkg := types.NewPkg("example.com/coro/dynamicanalysis/"+name, "p")
			fn := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("parent"), types.NewSignature(nil, nil, nil))
			targetPkg := types.NewPkg("example.com/coro/unknown", "unknown")
			targetName := "Missing"
			if operation {
				targetPkg = types.NewPkg("runtime", "runtime")
				targetName = "Gosched"
			}
			target := ir.NewFunc(src.NoXPos, src.NoXPos, targetPkg.Lookup(targetName), types.NewSignature(nil, nil, nil))
			local := ir.NewNameAt(src.NoXPos, pkg.Lookup("f"), target.Type())
			local.Class, local.Curfn = ir.PAUTO, fn
			fn.Dcl = []*ir.Name{local}
			call := ir.NewCallExpr(src.NoXPos, ir.OCALLFUNC, local, nil)
			call.SetTypecheck(1)
			fn.Body = ir.Nodes{ir.NewAssignStmt(src.NoXPos, local, target.Nname), call}
			plan, err := Analyze([]*ir.Func{fn}, nil)
			if err != nil {
				t.Fatal(err)
			}
			function := plan.Functions[fn]
			edge := function.Edges[0]
			if !edge.Unknown || function.Effect != MaySuspend || !slices.Contains(function.Seeds, UnknownCall) || len(function.Sites) != 1 || function.Sites[0].Kind != SiteAwait {
				t.Fatalf("lost unknown-call boundary: %+v", function)
			}
			if operation && len(edge.Targets) != 0 || !operation && len(edge.Targets) != 1 {
				t.Fatalf("targets=%v", edge.Targets)
			}
		})
	}
}
