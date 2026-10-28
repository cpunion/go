// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/ir"
	"cmd/compile/internal/typecheck"
	"cmd/compile/internal/types"
	"cmd/internal/src"
	"go/constant"
	"testing"
)

// Hidden Init lists are source effects even when the visible expression
// still matches the recurrence. Guard each nested expression separately.
func TestAffineReturnHiddenEffects(t *testing.T) {
	prepareLowerTest(t)
	oldPkg, oldTarget := types.LocalPkg, typecheck.Target
	defer func() { types.LocalPkg, typecheck.Target = oldPkg, oldTarget }()
	for _, where := range []string{"pure", "condition", "zero", "subtract", "one", "add", "step", "call", "yield", "branch", "base", "return", "nil-function", "ordinary", "pragma", "missing-input", "not-if", "else", "base-body", "not-condition", "wrong-input", "nonzero", "noninteger", "not-yield", "yield-args", "not-gosched", "not-base-return", "base-results", "base-expression", "not-return", "return-results", "not-add", "no-call", "commuted", "variable-step", "call-args", "dynamic-target", "missing-target", "not-subtraction", "decrement-two", "no-ordinary", "rejected-candidate"} {
		t.Run(where, func(t *testing.T) {
			pos := src.NoXPos
			pkg := types.NewPkg("example.com/affine/"+where, "affine")
			types.LocalPkg, typecheck.Target = pkg, new(ir.Package)
			integer := types.Types[types.TINT]
			fn := ir.NewFunc(pos, pos, pkg.Lookup("sum"), types.NewSignature(nil,
				[]*types.Field{types.NewField(pos, pkg.Lookup("n"), integer)},
				[]*types.Field{types.NewField(pos, nil, integer)}))
			fn.DeclareParams(true)
			fn.Nname.Defn = fn
			n := fn.Type().Param(0).Nname.(*ir.Name)
			zero, one, step := typedInt(pos, integer, 0), typedInt(pos, integer, 1), typedInt(pos, integer, 7)
			condition := ir.NewBinaryExpr(pos, ir.OLE, n, zero)
			condition.SetType(types.Types[types.TBOOL])
			condition.SetTypecheck(1)
			sub := ir.NewBinaryExpr(pos, ir.OSUB, n, one)
			call := ir.NewCallExpr(pos, ir.OCALLFUNC, fn.Nname, ir.Nodes{sub})
			add := ir.NewBinaryExpr(pos, ir.OADD, call, step)
			gosched := ir.NewFunc(pos, pos, types.NewPkg("runtime", "runtime").Lookup("Gosched"), types.NewSignature(nil, nil, nil))
			yield := ir.NewCallExpr(pos, ir.OCALLFUNC, gosched.Nname, nil)
			yield.SetTypecheck(1)
			base := ir.NewReturnStmt(pos, ir.Nodes{typedInt(pos, integer, 3)})
			branch := ir.NewIfStmt(pos, condition, ir.Nodes{yield, base}, nil)
			ret := ir.NewReturnStmt(pos, ir.Nodes{add})
			fn.Body = ir.Nodes{branch, ret}
			nodes := map[string]ir.Node{"condition": condition, "zero": zero, "subtract": sub, "one": one, "add": add, "step": step, "call": call, "yield": yield, "branch": branch, "base": base, "return": ret}

			function := &Function{Func: fn, Primary: CoroPrimary}
			if node := nodes[where]; node != nil {
				node.(ir.InitNode).SetInit(ir.Nodes{ir.NewAssignStmt(pos, n, typedInt(pos, integer, 99))})
			}
			switch where {
			case "nil-function":
				function = nil
			case "ordinary":
				function.Primary = PlainPrimary
			case "pragma":
				fn.Pragma = ir.Noinline
			case "missing-input":
				fn.Type().Param(0).Nname = nil
			case "not-if":
				fn.Body[0] = base
			case "else":
				branch.Else = ir.Nodes{base}
			case "base-body":
				branch.Body = ir.Nodes{yield}
			case "not-condition":
				branch.Cond = n
			case "wrong-input":
				condition.X = zero
			case "nonzero":
				condition.Y = one
			case "noninteger":
				condition.Y = ir.NewBasicLit(pos, types.Types[types.TSTRING], constant.MakeString("zero"))
			case "not-yield":
				branch.Body[0] = base
			case "yield-args":
				yield.Args = ir.Nodes{n}
			case "not-gosched":
				yield.Fun = fn.Nname
			case "not-base-return":
				branch.Body[1] = yield
			case "base-results":
				base.Results = nil
			case "base-expression":
				base.Results = ir.Nodes{n}
			case "not-return":
				fn.Body[1] = branch
			case "return-results":
				ret.Results = nil
			case "not-add":
				ret.Results[0] = n
			case "no-call":
				add.X = zero
			case "commuted":
				add.X, add.Y = step, call
			case "variable-step":
				add.Y = n
			case "call-args":
				call.Args = nil
			case "dynamic-target":
				call.Fun = n
			case "missing-target":
				fn.Nname.Defn = nil
			case "not-subtraction":
				call.Args = ir.Nodes{n}
			case "decrement-two":
				sub.Y = typedInt(pos, integer, 2)
			}
			p := planAffineReturn(function)
			want := where == "pure" || where == "commuted" || where == "no-ordinary" || where == "rejected-candidate"
			if (p != nil) != want {
				t.Fatalf("effect at %s: admitted=%v", where, p != nil)
			}

			if where == "no-ordinary" || where == "rejected-candidate" {
				before := append(ir.Nodes(nil), fn.Body...)
				old := &lowerCandidate{function: &Function{Func: fn, Primary: CoroPrimary, Edges: []Edge{{Node: yield}}}}
				if where == "rejected-candidate" {
					old.ordinary = &ir.Func{}
					fn.SetNeedctxt(true)
				}
				if got := applyAffineReturn(nil, old, p); got != old {
					t.Fatal("failed proof/rebuild replaced candidate")
				}
				if len(fn.Body) != len(before) {
					t.Fatal("rejected transform changed source body")
				}
				for i, n := range before {
					if fn.Body[i] != n {
						t.Fatal("rejected transform lost original body")
					}
				}
			}
		})
	}
}

func TestAffineReturnClosedGraph(t *testing.T) {
	prepareLowerTest(t)
	for _, test := range []struct {
		name string
		want bool
	}{
		{"self", true}, {"mutual", true}, {"incoming-chain", true},
		{"missing-plan", false}, {"missing-candidate", false},
		{"different-base", false}, {"different-step", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			pkg := types.NewPkg("example.com/affinegraph/"+test.name, "affinegraph")
			makeFn := func(name string) *ir.Func {
				return ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup(name), types.NewSignature(nil, nil, nil))
			}
			a, b, c := makeFn("a"), makeFn("b"), makeFn("c")
			plans := map[*ir.Func]*affineReturnPlan{}
			candidates := map[*ir.Func]*lowerCandidate{}
			for _, fn := range []*ir.Func{a, b, c} {
				plans[fn] = &affineReturnPlan{source: &Function{Func: fn}, target: fn, base: 3, step: 7}
				candidates[fn] = &lowerCandidate{}
			}
			if test.name != "self" {
				plans[a].target, plans[b].target = b, a
			}
			switch test.name {
			case "incoming-chain":
				plans[a].target, plans[b].target = b, c
			case "missing-plan":
				delete(plans, b)
			case "missing-candidate":
				delete(candidates, b)
			case "different-base":
				plans[b].base = 4
			case "different-step":
				plans[b].step = 8
			}
			if got := affineReturnClosed(plans[a], plans, candidates); got != test.want {
				t.Fatalf("closed=%v want %v", got, test.want)
			}
		})
	}
}
