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

func TestRangeDeferCaptureCells(t *testing.T) {
	prepareLowerTest(t)
	oldTarget := typecheck.Target
	typecheck.Target = new(ir.Package)
	t.Cleanup(func() { typecheck.Target = oldTarget })
	for _, mode := range []string{"literal", "literal-reordered", "wrapper", "missing", "type", "ambiguous", "wrapper-mismatch"} {
		t.Run(mode, func(t *testing.T) {
			pkg := types.NewPkg("example.com/coro/rangebind/"+mode, "p")
			parent := newLowerTestFunc(pkg, "parent")
			other := newLowerTestFunc(pkg, "ordinary")
			signature := types.NewSignature(nil, nil, nil)
			native := ir.NewClosureFunc(src.NoXPos, src.NoXPos, ir.ODEFER, signature, parent, typecheck.Target, 0)
			ordinary := ir.NewClosureFunc(src.NoXPos, src.NoXPos, ir.ODEFER, types.NewSignature(nil, nil, nil), other, typecheck.Target, 0)
			native.DeclareParams(true)
			ordinary.DeclareParams(true)
			cell := func(fn *ir.Func, name string, typ *types.Type) *ir.Name {
				variable := ir.NewNameAt(src.NoXPos, pkg.Lookup(name), typ)
				variable.Class, variable.Curfn = ir.PAUTO, fn
				fn.Dcl = append(fn.Dcl, variable)
				return variable
			}
			x := cell(parent, "x", types.Types[types.TINT])
			y := cell(parent, "y", types.NewPtr(types.Types[types.TINT]))
			ox := cell(other, "x", x.Type())
			oy := cell(other, "y", y.Type())
			nx := ir.NewClosureVar(src.NoXPos, native, x)
			ny := ir.NewClosureVar(src.NoXPos, native, y)
			ir.NewClosureVar(src.NoXPos, ordinary, ox)
			ir.NewClosureVar(src.NoXPos, ordinary, oy)
			want := mode == "literal" || mode == "literal-reordered" || mode == "wrapper"
			switch mode {
			case "literal-reordered":
				ordinary.ClosureVars[0], ordinary.ClosureVars[1] = ordinary.ClosureVars[1], ordinary.ClosureVars[0]
			case "wrapper":
				native.SetWrapper(true)
				ordinary.SetWrapper(true)
				// Body-reader temporary counters need not be equal. The frozen
				// operand positions still bind by their complete signature.
				ordinary.ClosureVars[0].Outer = cell(other, "otherTemp", x.Type())
				ox = ordinary.ClosureVars[0].Outer
			case "missing":
				ordinary.ClosureVars = ordinary.ClosureVars[:1]
			case "type":
				ordinary.ClosureVars[0].SetType(types.Types[types.TSTRING])
			case "ambiguous":
				ordinary.ClosureVars[1].Outer = cell(other, "x", x.Type())
				ordinary.ClosureVars[1].Defn = ordinary.ClosureVars[1].Outer
				ordinary.ClosureVars[1].SetType(x.Type())
			case "wrapper-mismatch":
				native.SetWrapper(true)
			}
			bindings, err := bindRangeDeferCaptureCells(native, ordinary)
			if (err == nil) != want {
				t.Fatalf("ordinary capture binding error=%v, want valid=%t", err, want)
			}
			if want && (bindings[nx] != ox || bindings[ny] != oy) {
				t.Fatal("binding read/reordered an operand instead of selecting its original cell")
			}
			if nx.Outer != x || ny.Outer != y {
				t.Fatal("ordinary binding changed the validated native body's captures")
			}
		})
	}
}
