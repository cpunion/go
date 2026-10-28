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

// Hidden Init lists are source effects even when the visible expression
// still matches the recurrence. Guard each nested expression separately.
func TestAffineReturnHiddenEffects(t *testing.T) {
	prepareLowerTest(t)
	oldPkg, oldTarget := types.LocalPkg, typecheck.Target
	defer func() { types.LocalPkg, typecheck.Target = oldPkg, oldTarget }()
	for _, where := range []string{"pure", "condition", "zero", "subtract", "one", "add", "step", "call", "yield", "branch", "base", "return"} {
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
			sub := ir.NewBinaryExpr(pos, ir.OSUB, n, one)
			call := ir.NewCallExpr(pos, ir.OCALLFUNC, fn.Nname, ir.Nodes{sub})
			add := ir.NewBinaryExpr(pos, ir.OADD, call, step)
			gosched := ir.NewFunc(pos, pos, types.NewPkg("runtime", "runtime").Lookup("Gosched"), types.NewSignature(nil, nil, nil))
			yield := ir.NewCallExpr(pos, ir.OCALLFUNC, gosched.Nname, nil)
			base := ir.NewReturnStmt(pos, ir.Nodes{typedInt(pos, integer, 3)})
			branch := ir.NewIfStmt(pos, condition, ir.Nodes{yield, base}, nil)
			ret := ir.NewReturnStmt(pos, ir.Nodes{add})
			fn.Body = ir.Nodes{branch, ret}
			nodes := map[string]ir.Node{"condition": condition, "zero": zero, "subtract": sub, "one": one, "add": add, "step": step, "call": call, "yield": yield, "branch": branch, "base": base, "return": ret}
			if where != "pure" {
				nodes[where].(ir.InitNode).SetInit(ir.Nodes{ir.NewAssignStmt(pos, n, typedInt(pos, integer, 99))})
			}
			p := planAffineReturn(&Function{Func: fn, Primary: CoroPrimary})
			if (p != nil) != (where == "pure") {
				t.Fatalf("effect at %s: admitted=%v", where, p != nil)
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
