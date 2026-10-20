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

func TestPackageFunctionTargets(t *testing.T) {
	prepareLowerTest(t)
	for _, test := range []struct {
		name string
		want []string
	}{
		{"direct", []string{"first"}}, {"branches", []string{"first", "second"}},
		{"converted", []string{"first"}}, {"local", []string{"first", "second"}},
		{"return-init", []string{"first", "second"}}, {"chain", []string{"second"}},
		{"cycle", []string{"first", "second"}}, {"empty-cycle", nil},
		{"nil", nil}, {"nil-and-seed", []string{"first"}},
		{"unknown", nil}, {"cycle-unknown", nil}, {"parameter", nil},
		{"global", nil}, {"address", nil}, {"capture", nil}, {"closure", nil},
		{"type-case", nil}, {"range", nil}, {"bodyless", nil}, {"bare", nil},
		{"named-result", nil}, {"multiple-results", nil}, {"method", nil},
		{"imported", nil}, {"late-unknown", nil}, {"dynamic-producer", nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			pkg := types.NewPkg("example.com/coro/returnproof/"+test.name, "p")
			plain := types.NewSignature(nil, nil, nil)
			first := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("first"), plain)
			second := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("second"), plain)
			producerType := types.NewSignature(nil, nil, []*types.Field{types.NewField(src.NoXPos, nil, plain)})
			choose := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("choose"), producerType)
			hop := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("hop"), producerType)
			parent := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("parent"), plain)
			local := func(fn *ir.Func, symbol string, typ *types.Type) *ir.Name {
				name := ir.NewNameAt(src.NoXPos, pkg.Lookup(symbol), typ)
				name.Class, name.Curfn = ir.PAUTO, fn
				fn.Dcl = append(fn.Dcl, name)
				return name
			}
			call := func(fn *ir.Func) *ir.CallExpr {
				n := ir.NewCallExpr(src.NoXPos, ir.OCALLFUNC, fn.Nname, nil)
				n.SetType(plain)
				n.SetTypecheck(1)
				return n
			}
			ret := func(value ir.Node) *ir.ReturnStmt { return ir.NewReturnStmt(src.NoXPos, ir.Nodes{value}) }
			assign := func(lhs, rhs ir.Node) *ir.AssignStmt { return ir.NewAssignStmt(src.NoXPos, lhs, rhs) }
			f, alias := local(parent, "f", plain), local(parent, "alias", plain)
			parent.Body = ir.Nodes{assign(f, call(choose)), assign(alias, f)}
			choose.Body, hop.Body = ir.Nodes{ret(first.Nname)}, ir.Nodes{ret(second.Nname)}
			funcs := []*ir.Func{parent, hop, choose, first, second}
			switch test.name {
			case "branches":
				choose.Body = append(choose.Body, ret(second.Nname))
			case "converted":
				choose.Body = ir.Nodes{ret(ir.NewConvExpr(src.NoXPos, ir.OCONVNOP, plain, first.Nname))}
			case "local", "return-init", "parameter", "global", "address", "capture", "type-case", "range":
				a := local(choose, "a", plain)
				result := ret(a)
				choose.Body = ir.Nodes{assign(a, first.Nname), result}
				switch test.name {
				case "local":
					choose.Body = append(choose.Body, assign(a, second.Nname))
				case "return-init":
					result.SetInit(ir.Nodes{assign(a, second.Nname)})
				case "parameter":
					a.Class = ir.PPARAM
				case "global":
					a.Class = ir.PEXTERN
				case "address":
					choose.Body = append(choose.Body, ir.NewAddrExpr(src.NoXPos, a))
				case "capture":
					second.ClosureVars = []*ir.Name{ir.NewClosureVar(src.NoXPos, second, a)}
					choose.Body = append(choose.Body, &ir.ClosureExpr{Func: second})
				case "type-case":
					clause := ir.NewCaseStmt(src.NoXPos, nil, choose.Body)
					clause.Var = a
					choose.Body = ir.Nodes{ir.NewSwitchStmt(src.NoXPos, nil, []*ir.CaseClause{clause})}
				case "range":
					stmt := ir.NewRangeStmt(src.NoXPos, nil, nil, nil, nil, false)
					stmt.Value = a
					choose.Body = append(choose.Body, stmt)
				}
			case "chain":
				choose.Body = ir.Nodes{ret(call(hop))}
			case "cycle", "empty-cycle", "cycle-unknown":
				choose.Body = append(choose.Body, ret(call(hop)))
				hop.Body = append(hop.Body, ret(call(choose)))
				if test.name == "empty-cycle" {
					choose.Body, hop.Body = ir.Nodes{ret(call(hop))}, ir.Nodes{ret(call(choose))}
				} else if test.name == "cycle-unknown" {
					unknown := local(hop, "unknown", plain)
					unknown.Class = ir.PPARAM
					hop.Body = append(hop.Body, ret(unknown))
				}
			case "nil", "nil-and-seed":
				choose.Body = ir.Nodes{ret(ir.NewNilExpr(src.NoXPos, plain))}
				if test.name == "nil-and-seed" {
					parent.Body = append(parent.Body, assign(f, first.Nname))
				}
			case "unknown":
				choose.Body = append(choose.Body, ret(ir.NewNameAt(src.NoXPos, pkg.Lookup("unknown"), plain)))
			case "closure":
				choose.Body = ir.Nodes{ret(&ir.ClosureExpr{Func: second})}
			case "bodyless":
				choose.Body = nil
				parent.Body = append(parent.Body, assign(f, first.Nname))
			case "bare":
				choose.Body = append(choose.Body, newLowerTestReturn())
			case "named-result":
				choose.Nname.SetType(types.NewSignature(nil, nil, []*types.Field{types.NewField(src.NoXPos, pkg.Lookup("result"), plain)}))
			case "multiple-results":
				choose.Nname.SetType(types.NewSignature(nil, nil, []*types.Field{types.NewField(src.NoXPos, nil, plain), types.NewField(src.NoXPos, nil, types.Types[types.TBOOL])}))
			case "method":
				choose.Nname.SetType(types.NewSignature(types.NewField(src.NoXPos, nil, types.Types[types.TINT]), nil, producerType.Results()))
			case "imported":
				funcs = slices.DeleteFunc(funcs, func(fn *ir.Func) bool { return fn == choose })
			case "late-unknown":
				unknown := local(parent, "input", plain)
				unknown.Class = ir.PPARAM
				parent.Body = append(parent.Body, assign(f, unknown))
			case "dynamic-producer":
				producer := local(parent, "producer", producerType)
				parent.Body = append(ir.Nodes{assign(producer, choose.Nname), assign(producer, hop.Nname)}, parent.Body...)
				parent.Body[2].(*ir.AssignStmt).Y.(*ir.CallExpr).Fun = producer
			}
			got := packageFunctionTargets(funcs)[parent][alias]
			names := make([]string, len(got))
			for i, fn := range got {
				names[i] = fn.Sym().Name
			}
			if !slices.Equal(names, test.want) {
				t.Fatalf("targets=%v, want %v", names, test.want)
			}
		})
	}
}

func TestLowerReturnedFunctionCall(t *testing.T) {
	prepareLowerTest(t)
	oldPkg, oldTarget := types.LocalPkg, typecheck.Target
	defer func() { types.LocalPkg, typecheck.Target = oldPkg, oldTarget }()
	pkg := types.NewPkg("example.com/coro/returnlower", "p")
	types.LocalPkg, typecheck.Target = pkg, new(ir.Package)
	plain := types.NewSignature(nil, nil, nil)
	newFunction := func(name string, signature *types.Type) *ir.Func {
		fn := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup(name), signature)
		fn.Nname.Defn = fn
		fn.DeclareParams(true)
		typecheck.Target.Funcs = append(typecheck.Target.Funcs, fn)
		return fn
	}
	first, second := newFunction("first", plain), newFunction("second", plain)
	choose := newFunction("choose", types.NewSignature(nil, nil, []*types.Field{types.NewField(src.NoXPos, nil, plain)}))
	parent := newFunction("parent", plain)
	yield := ir.NewFunc(src.NoXPos, src.NoXPos, types.NewPkg("runtime", "runtime").Lookup("Gosched"), plain)
	first.Body = ir.Nodes{newLowerTestCall(yield), newLowerTestReturn()}
	panicStmt := ir.NewUnaryExpr(src.NoXPos, ir.OPANIC, ir.NewNilExpr(src.NoXPos, types.Types[types.TINTER]))
	panicStmt.SetTypecheck(1)
	second.Body = ir.Nodes{newLowerTestCall(yield), panicStmt}
	choose.Body = ir.Nodes{newLowerTestCall(yield), ir.NewIfStmt(src.NoXPos, ir.NewBool(src.NoXPos, true),
		ir.Nodes{ir.NewReturnStmt(src.NoXPos, ir.Nodes{first.Nname})}, nil), ir.NewReturnStmt(src.NoXPos, ir.Nodes{second.Nname})}
	f := parent.NewLocal(src.NoXPos, pkg.Lookup("f"), plain)
	alias := parent.NewLocal(src.NoXPos, pkg.Lookup("alias"), plain)
	producerCall := newLowerTestCall(choose)
	producerCall.SetType(plain)
	call := ir.NewCallExpr(src.NoXPos, ir.OCALLFUNC, alias, nil)
	call.SetTypecheck(1)
	parent.Body = ir.Nodes{ir.NewDecl(src.NoXPos, ir.ODCL, f), ir.NewDecl(src.NoXPos, ir.ODCL, alias),
		ir.NewAssignStmt(src.NoXPos, f, producerCall), ir.NewAssignStmt(src.NoXPos, alias, f), call, newLowerTestReturn()}
	plan, err := Analyze([]*ir.Func{parent, choose, second, first}, nil)
	if err != nil {
		t.Fatal(err)
	}
	edge := edgeForCall(plan.Functions[parent], call)
	if edge.Unknown || edge.Callee != nil || !slices.Equal(edge.Targets, []*ir.Func{first, second}) {
		t.Fatalf("returned-value edge=%+v", edge)
	}
	result, err := Lower(plan, nil)
	if err != nil || result.Lowered != 4 || len(result.Diagnostics) != 0 {
		t.Fatalf("Lower=(%+v,%v)", result, err)
	}
	if call.Fun != alias || producerCall.Fun != choose.Nname {
		t.Fatal("source calls were changed")
	}
	found := map[string]bool{}
	for _, fn := range typecheck.Target.Funcs {
		ir.VisitList(fn.Body, func(node ir.Node) {
			if call, ok := node.(*ir.CallExpr); ok {
				if name := ir.StaticCalleeName(call.Fun); name != nil {
					found[name.Sym().Name] = true
				}
			}
		})
	}
	for _, name := range []string{"choose.coro", "first.coro", "second.coro", "coroAwaitFrame"} {
		if !found[name] {
			t.Errorf("missing factory/await %s", name)
		}
	}
	var dump strings.Builder
	plan.Dump(&dump)
	if !strings.Contains(dump.String(), "closed-targets caller="+ir.PkgFuncName(parent)) {
		t.Fatal("returned target proof missing from plan")
	}
}
