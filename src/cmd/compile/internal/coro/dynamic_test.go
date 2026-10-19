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
		{"branches", 2}, {"converted", 1}, {"parallel", 2}, {"zero", 0},
		{"alias", 0}, {"unknown", 0}, {"multi-result", 0}, {"parameter", 0},
		{"global", 0}, {"address-flag", 0}, {"address", 0}, {"capture", 0},
		{"range", 0}, {"closure", 0}, {"shape", 0}, {"method", 0},
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
	delete(p.Functions, b)
	if _, known := p.edgeSummary(edge); known || !p.edgeNeedsCoroEntry(Edge{Targets: []*ir.Func{b}}) {
		t.Fatal("unknown target lost conservative effect")
	}
}

func TestLowerDynamicCall(t *testing.T) {
	prepareLowerTest(t)
	oldPkg, oldTarget := types.LocalPkg, typecheck.Target
	defer func() { types.LocalPkg, typecheck.Target = oldPkg, oldTarget }()
	pkg := types.NewPkg("example.com/coro/dynamiclower", "p")
	types.LocalPkg, typecheck.Target = pkg, new(ir.Package)
	first, second, parent := newLowerTestFunc(pkg, "first"), newLowerTestFunc(pkg, "second"), newLowerTestFunc(pkg, "parent")
	yield := ir.NewFunc(src.NoXPos, src.NoXPos, types.NewPkg("runtime", "runtime").Lookup("Gosched"), types.NewSignature(nil, nil, nil))
	first.Body = ir.Nodes{newLowerTestCall(yield), newLowerTestReturn()}
	second.Body = ir.Nodes{newLowerTestCall(yield), newLowerTestReturn()}
	local := ir.NewNameAt(src.NoXPos, pkg.Lookup("f"), first.Type())
	local.Class, local.Curfn = ir.PAUTO, parent
	parent.Dcl = append(parent.Dcl, local)
	call := ir.NewCallExpr(src.NoXPos, ir.OCALLFUNC, local, nil)
	call.SetTypecheck(1)
	parent.Body = ir.Nodes{ir.NewDecl(src.NoXPos, ir.ODCL, local), ir.NewAssignStmt(src.NoXPos, local, first.Nname),
		ir.NewIfStmt(src.NoXPos, ir.NewBool(src.NoXPos, true), ir.Nodes{ir.NewAssignStmt(src.NoXPos, local, second.Nname)}, nil), call, newLowerTestReturn()}
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
	got, err := Lower(plan, nil)
	if err != nil || got.Lowered != 3 {
		t.Fatalf("Lower=(%+v,%v)", got, err)
	}
	if call.Fun != local || len(call.Args) != 0 {
		t.Fatal("original dynamic call changed")
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
