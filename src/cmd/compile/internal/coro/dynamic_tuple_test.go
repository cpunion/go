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

func TestTupleFunctionTargets(t *testing.T) {
	prepareLowerTest(t)
	for _, test := range []struct {
		name         string
		first, third []string
	}{
		{"direct", []string{"first"}, []string{"second"}},
		{"branches", []string{"first", "second"}, []string{"first", "second"}},
		{"forward", []string{"second"}, []string{"first"}},
		{"cycle", []string{"first", "second"}, []string{"first", "second"}},
		{"cycle-unknown", []string{"first", "second"}, nil},
		{"empty-cycle", nil, nil}, {"bodyless", nil, nil}, {"imported", nil, nil},
		{"unknown-first", nil, []string{"second"}}, {"unknown-third", []string{"first"}, nil},
		{"unknown-middle", []string{"first"}, []string{"second"}},
		{"nil-third", []string{"first"}, nil}, {"dynamic-producer", nil, nil},
		{"wrong-arity", nil, nil}, {"bad-return", nil, nil},
		{"named-bare", []string{"first"}, []string{"second"}},
		{"named-explicit", []string{"first", "second"}, []string{"first", "second"}},
		{"named-forward", []string{"second"}, []string{"first"}},
		{"named-assign", []string{"second"}, []string{"first"}},
		{"named-swap", []string{"first", "second"}, []string{"first", "second"}},
		{"named-swap-unknown", nil, nil}, {"named-return-init", []string{"first"}, nil},
		{"named-zero", []string{"first"}, nil},
		{"named-capture", []string{"first"}, nil}, {"named-address", []string{"first"}, nil},
		{"named-address-flag", []string{"first"}, nil}, {"named-missing", []string{"first"}, nil},
		{"named-late-unknown", []string{"first"}, nil}, {"named-type-case", []string{"first"}, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			pkg := types.NewPkg("example.com/coro/tupleproof/"+test.name, "p")
			plain := types.NewSignature(nil, nil, nil)
			first := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("first"), plain)
			second := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("second"), plain)
			producer := func(symbol string, named bool) *ir.Func {
				fields := []*types.Field{types.NewField(src.NoXPos, nil, plain),
					types.NewField(src.NoXPos, nil, types.Types[types.TBOOL]), types.NewField(src.NoXPos, nil, plain)}
				if named {
					for i, field := range fields {
						field.Sym = pkg.LookupNum("out", i)
					}
				}
				fn := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup(symbol), types.NewSignature(nil, nil, fields))
				fn.DeclareParams(true)
				return fn
			}
			choose, hop := producer("choose", strings.HasPrefix(test.name, "named-")), producer("hop", false)
			parent := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("parent"), plain)
			local := func(fn *ir.Func, symbol string, typ *types.Type) *ir.Name {
				n := ir.NewNameAt(src.NoXPos, pkg.Lookup(symbol), typ)
				n.Class, n.Curfn = ir.PAUTO, fn
				fn.Dcl = append(fn.Dcl, n)
				return n
			}
			call := func(fn *ir.Func) *ir.CallExpr {
				n := newLowerTestCall(fn)
				n.SetType(fn.Type().ResultsTuple())
				return n
			}
			assign := func(lhs, rhs ir.Node) *ir.AssignStmt { return ir.NewAssignStmt(src.NoXPos, lhs, rhs) }
			ret := func(a, b ir.Node) *ir.ReturnStmt {
				return ir.NewReturnStmt(src.NoXPos, ir.Nodes{a, ir.NewBool(src.NoXPos, true), b})
			}
			f, g := local(parent, "f", plain), local(parent, "g", plain)
			flag := local(parent, "flag", types.Types[types.TBOOL])
			binding := ir.NewAssignListStmt(src.NoXPos, ir.OAS2FUNC, ir.Nodes{f, flag, g}, ir.Nodes{call(choose)})
			parent.Body = ir.Nodes{binding}
			choose.Body, hop.Body = ir.Nodes{ret(first.Nname, second.Nname)}, ir.Nodes{ret(second.Nname, first.Nname)}
			unknown := local(choose, "input", plain)
			unknown.Class = ir.PPARAM
			funcs := []*ir.Func{parent, choose, hop, first, second}
			if strings.HasPrefix(test.name, "named-") {
				a, b := choose.Type().Result(0).Nname.(*ir.Name), choose.Type().Result(2).Nname.(*ir.Name)
				choose.Body = ir.Nodes{assign(a, first.Nname), assign(b, second.Nname), newLowerTestReturn()}
				switch test.name {
				case "named-explicit":
					choose.Body[2] = ret(second.Nname, first.Nname)
				case "named-forward":
					choose.Body = ir.Nodes{ir.NewReturnStmt(src.NoXPos, ir.Nodes{call(hop)})}
				case "named-assign":
					choose.Body = ir.Nodes{ir.NewAssignListStmt(src.NoXPos, ir.OAS2FUNC,
						ir.Nodes{a, choose.Type().Result(1).Nname.(*ir.Name), b}, ir.Nodes{call(hop)}), newLowerTestReturn()}
				case "named-swap", "named-swap-unknown":
					choose.Body = append(choose.Body, ir.NewAssignListStmt(src.NoXPos, ir.OAS2, ir.Nodes{a, b}, ir.Nodes{b, a}))
					if test.name == "named-swap-unknown" {
						choose.Body = append(choose.Body, assign(b, unknown))
					}
				case "named-return-init":
					choose.Body[2].(*ir.ReturnStmt).SetInit(ir.Nodes{assign(b, unknown)})
				case "named-zero":
					choose.Body = ir.Nodes{assign(a, first.Nname), newLowerTestReturn()}
				case "named-capture":
					second.ClosureVars = []*ir.Name{ir.NewClosureVar(src.NoXPos, second, b)}
					choose.Body = append(choose.Body, &ir.ClosureExpr{Func: second})
				case "named-address":
					choose.Body = append(choose.Body, ir.NewAddrExpr(src.NoXPos, b))
				case "named-address-flag":
					b.SetAddrtaken(true)
				case "named-missing":
					choose.Type().Result(2).Nname = nil
				case "named-late-unknown":
					choose.Body = append(choose.Body, assign(b, unknown))
				case "named-type-case":
					clause := ir.NewCaseStmt(src.NoXPos, nil, choose.Body)
					clause.Var = b
					choose.Body = ir.Nodes{ir.NewSwitchStmt(src.NoXPos, nil, []*ir.CaseClause{clause})}
				}
			} else {
				switch test.name {
				case "branches":
					choose.Body = append(choose.Body, ret(second.Nname, first.Nname))
				case "forward":
					choose.Body = ir.Nodes{ir.NewReturnStmt(src.NoXPos, ir.Nodes{call(hop)})}
				case "cycle", "cycle-unknown", "empty-cycle":
					choose.Body = append(choose.Body, ir.NewReturnStmt(src.NoXPos, ir.Nodes{call(hop)}))
					hop.Body = append(hop.Body, ir.NewReturnStmt(src.NoXPos, ir.Nodes{call(choose)}))
					if test.name == "empty-cycle" {
						choose.Body, hop.Body = choose.Body[1:], hop.Body[1:]
					} else if test.name == "cycle-unknown" {
						hop.Body = append(hop.Body, ret(first.Nname, unknown))
					}
				case "bodyless":
					choose.Body = nil
					parent.Body = append(parent.Body, assign(f, first.Nname), assign(g, second.Nname))
				case "imported":
					funcs = slices.DeleteFunc(funcs, func(fn *ir.Func) bool { return fn == choose })
				case "unknown-first":
					choose.Body = ir.Nodes{ret(unknown, second.Nname)}
				case "unknown-third":
					choose.Body = ir.Nodes{ret(first.Nname, unknown)}
				case "unknown-middle":
					input := local(choose, "flagInput", types.Types[types.TBOOL])
					input.Class = ir.PPARAM
					choose.Body[0].(*ir.ReturnStmt).Results[1] = input
				case "nil-third":
					choose.Body = ir.Nodes{ret(first.Nname, ir.NewNilExpr(src.NoXPos, plain))}
				case "wrong-arity":
					binding.Lhs = ir.Nodes{f, g}
				case "bad-return":
					choose.Body = ir.Nodes{ir.NewReturnStmt(src.NoXPos, ir.Nodes{first.Nname})}
				case "dynamic-producer":
					p := local(parent, "producer", choose.Type())
					parent.Body = append(ir.Nodes{assign(p, choose.Nname), assign(p, hop.Nname)}, parent.Body...)
					binding.Rhs[0].(*ir.CallExpr).Fun = p
				}
			}
			got := packageFunctionTargets(funcs)[parent]
			for _, check := range []struct {
				name *ir.Name
				want []string
			}{{f, test.first}, {g, test.third}} {
				var names []string
				for _, fn := range got[check.name] {
					names = append(names, fn.Sym().Name)
				}
				if !slices.Equal(names, check.want) {
					t.Errorf("%s targets=%v, want %v", check.name.Sym().Name, names, check.want)
				}
			}
		})
	}
}

func TestLowerTupleFunctionReturn(t *testing.T) {
	prepareLowerTest(t)
	oldPkg, oldTarget := types.LocalPkg, typecheck.Target
	defer func() { types.LocalPkg, typecheck.Target = oldPkg, oldTarget }()
	pkg := types.NewPkg("example.com/coro/tuplelower", "p")
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
	producerType := func() *types.Type {
		return types.NewSignature(nil, nil, []*types.Field{types.NewField(src.NoXPos, nil, plain), types.NewField(src.NoXPos, nil, types.Types[types.TBOOL])})
	}
	choose, forward := newFunction("choose", producerType()), newFunction("forward", producerType())
	parent := newFunction("parent", plain)
	yield := ir.NewFunc(src.NoXPos, src.NoXPos, types.NewPkg("runtime", "runtime").Lookup("Gosched"), plain)
	first.Body, second.Body = ir.Nodes{newLowerTestCall(yield), newLowerTestReturn()}, ir.Nodes{newLowerTestCall(yield), newLowerTestReturn()}
	ret := func(value ir.Node) *ir.ReturnStmt {
		return ir.NewReturnStmt(src.NoXPos, ir.Nodes{value, ir.NewBool(src.NoXPos, true)})
	}
	choose.Body = ir.Nodes{newLowerTestCall(yield), ir.NewIfStmt(src.NoXPos, ir.NewBool(src.NoXPos, true), ir.Nodes{ret(first.Nname)}, nil), ret(second.Nname)}
	inner := newLowerTestCall(choose)
	inner.SetType(choose.Type().ResultsTuple())
	forwarded := ir.NewReturnStmt(src.NoXPos, ir.Nodes{inner})
	forward.Body = ir.Nodes{forwarded}
	f := parent.NewLocal(src.NoXPos, pkg.Lookup("f"), plain)
	producer := newLowerTestCall(forward)
	producer.SetType(forward.Type().ResultsTuple())
	call := ir.NewCallExpr(src.NoXPos, ir.OCALLFUNC, f, nil)
	call.SetTypecheck(1)
	parent.Body = ir.Nodes{ir.NewDecl(src.NoXPos, ir.ODCL, f), ir.NewAssignListStmt(src.NoXPos, ir.OAS2FUNC, ir.Nodes{f, ir.BlankNode}, ir.Nodes{producer}), call, newLowerTestReturn()}
	plan, err := Analyze([]*ir.Func{parent, forward, choose, second, first}, nil)
	if err != nil {
		t.Fatal(err)
	}
	edge := edgeForCall(plan.Functions[parent], call)
	if edge.Unknown || !slices.Equal(edge.Targets, []*ir.Func{first, second}) {
		t.Fatalf("tuple target edge=%+v", edge)
	}
	normalizeSingleResultCalls(plan.Functions[forward])
	if len(forwarded.Results) != 2 || len(forwarded.Init()) != 1 {
		t.Fatal("sole tuple return did not become independent typed result cells")
	}
	assignment, ok := forwarded.Init()[0].(*ir.AssignListStmt)
	if !ok || assignment.Op() != ir.OAS2FUNC || len(assignment.Init()) != 2 || len(assignment.Rhs) != 1 || assignment.Rhs[0] != inner {
		t.Fatal("tuple call or result declarations were not retained")
	}
	for i, value := range forwarded.Results {
		if value != assignment.Lhs[i] || value.Name().Defn != assignment || value.Type() != forward.Type().Result(i).Type {
			t.Fatal("tuple return destinations differ from declared typed temporaries")
		}
	}
	// Lower runs normalization again, so it also verifies that the already
	// explicit tuple return is stable. Frame collection consumes declarations.
	result, err := Lower(plan, nil)
	if err != nil || result.Lowered != 5 || len(result.Diagnostics) != 0 {
		t.Fatalf("Lower=(%+v,%v)", result, err)
	}
	if inner.Fun != choose.Nname || producer.Fun != forward.Nname || call.Fun != f {
		t.Fatal("source call identities were changed")
	}
}

func TestNormalizeTupleReturnBoundary(t *testing.T) {
	prepareLowerTest(t)
	oldPkg, oldTarget := types.LocalPkg, typecheck.Target
	defer func() { types.LocalPkg, typecheck.Target = oldPkg, oldTarget }()
	pkg := types.NewPkg("example.com/coro/tupleboundary", "p")
	types.LocalPkg, typecheck.Target = pkg, new(ir.Package)
	for _, mode := range []string{"assignment", "argument", "argument-statement", "wrong-arity", "multiple-expressions"} {
		t.Run(mode, func(t *testing.T) {
			fields := func() []*types.Field {
				return []*types.Field{types.NewField(src.NoXPos, nil, types.Types[types.TINT]),
					types.NewField(src.NoXPos, nil, types.Types[types.TBOOL])}
			}
			callee := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup(mode+"callee"), types.NewSignature(nil, nil, fields()))
			caller := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup(mode+"caller"), types.NewSignature(nil, nil, fields()))
			caller.DeclareParams(true)
			call := newLowerTestCall(callee)
			call.SetType(callee.Type().ResultsTuple())
			ret := ir.NewReturnStmt(src.NoXPos, ir.Nodes{call})
			var root ir.Node = ret
			switch mode {
			case "assignment":
				root = ir.NewAssignListStmt(src.NoXPos, ir.OAS2FUNC, ir.Nodes{ir.BlankNode, ir.BlankNode}, ir.Nodes{call})
			case "argument", "argument-statement":
				wrap := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("wrap"), types.NewSignature(nil, fields(), fields()))
				outer := ir.NewCallExpr(src.NoXPos, ir.OCALLFUNC, wrap.Nname, ir.Nodes{call})
				outer.SetType(wrap.Type().ResultsTuple())
				outer.SetTypecheck(1)
				ret.Results[0] = outer
				if mode == "argument-statement" {
					root = outer
				}
			case "wrong-arity":
				caller.Nname.SetType(types.NewSignature(nil, nil, fields()[:1]))
			case "multiple-expressions":
				ret.Results = append(ret.Results, ir.NewBool(src.NoXPos, true))
			}
			caller.Body = ir.Nodes{root}
			before := len(caller.Dcl)
			normalizeSingleResultCalls(&Function{Func: caller, Effect: MaySuspend, Primary: CoroPrimary,
				Sites: []Site{{ID: 1, Kind: SiteAwait, Node: call}}})
			if len(root.Init()) != 0 || len(caller.Dcl) != before || !containsCall(root, call) {
				t.Fatal("tuple call was moved outside the supported sole-return boundary")
			}
		})
	}
}
