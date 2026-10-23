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

func TestIntegerRangeLoop(t *testing.T) {
	prepareLowerTest(t)
	oldTarget, oldLocalPkg, oldCurFunc := typecheck.Target, types.LocalPkg, ir.CurFunc
	defer func() { typecheck.Target, types.LocalPkg, ir.CurFunc = oldTarget, oldLocalPkg, oldCurFunc }()
	typecheck.Target = new(ir.Package)
	pkg := types.NewPkg("example.com/coro/integerrange", "integerrange")
	types.LocalPkg = pkg
	fn := newLowerTestFunc(pkg, "rangeLoop")
	for _, keyType := range []*types.Type{nil, types.Types[types.TINT], types.AnyType} {
		var key ir.Node
		if keyType != nil {
			key = typecheck.TempAt(src.NoXPos, fn, keyType)
		}
		bound := typedInt(src.NoXPos, types.Types[types.TINT], 3)
		body := newLowerTestReturn()
		stmt := ir.NewRangeStmt(src.NoXPos, key, nil, bound, ir.Nodes{body}, false)
		init := ir.NewAssignStmt(src.NoXPos, typecheck.TempAt(src.NoXPos, fn, types.Types[types.TINT]), bound)
		stmt.SetInit(ir.Nodes{init})
		loop := integerRangeLoop(stmt, fn)
		if loop.Init()[0] != init || loop.Body[len(loop.Body)-1] != body || loop.DistinctVars {
			t.Fatal("range init, body or distinct-variable rewrite was lost")
		}
		counter := loop.Cond.(*ir.BinaryExpr).X
		limit := loop.Cond.(*ir.BinaryExpr).Y
		assign := loop.Init()[4].(*ir.AssignStmt)
		if assign.X != limit || assign.Y != bound || loop.Post.(*ir.AssignStmt).X != counter {
			t.Fatal("range counter or frozen bound was lost")
		}
		if key == nil {
			if len(loop.Body) != 1 {
				t.Fatal("keyless range assigned an iteration variable")
			}
			continue
		}
		iteration := loop.Body[0].(*ir.AssignStmt)
		if iteration.X != key {
			t.Fatal("range lost its iteration assignment")
		}
		value := iteration.Y
		if keyType != bound.Type() {
			value = value.(*ir.ConvExpr).X
		}
		if value != counter {
			t.Fatal("range used the source variable as its counter")
		}
	}
}

func TestLowerIntegerRange(t *testing.T) {
	prepareLowerTest(t)
	for _, foreign := range []bool{false, true} {
		name := "suspending"
		if foreign {
			name = "foreign"
		}
		t.Run(name, func(t *testing.T) {
			oldTarget, oldLocalPkg, oldCurFunc := typecheck.Target, types.LocalPkg, ir.CurFunc
			t.Cleanup(func() { typecheck.Target, types.LocalPkg, ir.CurFunc = oldTarget, oldLocalPkg, oldCurFunc })
			typecheck.Target = new(ir.Package)
			pkg := types.NewPkg("example.com/coro/integerrange/"+name, name)
			types.LocalPkg = pkg
			fn := newLowerTestFunc(pkg, "loop")
			var op *ir.Func
			functions := []*ir.Func{fn}
			var directives [][]string
			if foreign {
				op = newLowerTestFunc(pkg, "_Cfunc_step")
				direct := newLowerTestFunc(pkg, "_Cdirect_step")
				functions = append(functions, op, direct)
				directives = [][]string{{"cgo_direct", "v1", "_Cfunc_step", "_Cdirect_step", "step", "mayblock", "-", "void", "-"}}
			} else {
				op = ir.NewFunc(src.NoXPos, src.NoXPos, ir.Pkgs.Runtime.Lookup("Gosched"), types.NewSignature(nil, nil, nil))
				op.DeclareParams(true)
			}
			call := newLowerTestCall(op)
			key := typecheck.TempAt(src.NoXPos, fn, types.Types[types.TINT])
			loop := ir.NewRangeStmt(src.NoXPos, key, nil,
				typedInt(src.NoXPos, types.Types[types.TINT], 3), ir.Nodes{call}, false)
			loop.SetTypecheck(1)
			fn.Body = ir.Nodes{ir.NewDecl(src.NoXPos, ir.ODCL, key), loop, newLowerTestReturn()}
			plan, err := Analyze(functions, directives)
			if err != nil {
				t.Fatal(err)
			}
			candidate, err := newLowerCandidate(plan, plan.Functions[fn])
			if err != nil || canLowerRunToCompletion(candidate) != foreign {
				t.Fatalf("range candidate: %v", err)
			}
			result, err := Lower(plan, nil)
			if err != nil || result.Lowered != 1 {
				t.Fatalf("Lower = %+v, %v", result, err)
			}
			for _, generated := range typecheck.Target.Funcs {
				if generated == fn || generated == op {
					continue
				}
				ir.VisitList(generated.Body, func(node ir.Node) {
					if node.Op() == ir.ORANGE {
						t.Fatal("native entry retained an opaque integer range")
					}
				})
			}
		})
	}
}

func TestLowerSliceRange(t *testing.T) { testLowerIndexedRange(t, "slice") }

func TestLowerMapRange(t *testing.T) { testLowerIndexedRange(t, "map") }

func TestLowerStringRange(t *testing.T) { testLowerIndexedRange(t, "string") }

func TestLowerArrayRange(t *testing.T) {
	for _, kind := range []string{"array", "pointer"} {
		t.Run(kind, func(t *testing.T) { testLowerIndexedRange(t, kind) })
	}
}

func testLowerIndexedRange(t *testing.T, kind string) {
	prepareLowerTest(t)
	for _, foreign := range []bool{false, true} {
		name := "suspending"
		if foreign {
			name = "foreign"
		}
		t.Run(name, func(t *testing.T) {
			oldTarget, oldLocalPkg, oldCurFunc := typecheck.Target, types.LocalPkg, ir.CurFunc
			t.Cleanup(func() { typecheck.Target, types.LocalPkg, ir.CurFunc = oldTarget, oldLocalPkg, oldCurFunc })
			typecheck.Target = new(ir.Package)
			pkg := types.NewPkg("example.com/coro/slicerange/"+name, name)
			types.LocalPkg = pkg
			fn := newLowerTestFunc(pkg, "loop")
			var op *ir.Func
			functions := []*ir.Func{fn}
			var directives [][]string
			if foreign {
				op = newLowerTestFunc(pkg, "_Cfunc_step")
				direct := newLowerTestFunc(pkg, "_Cdirect_step")
				functions = append(functions, op, direct)
				directives = [][]string{{"cgo_direct", "v1", "_Cfunc_step", "_Cdirect_step", "step", "mayblock", "-", "void", "-"}}
			} else {
				op = ir.NewFunc(src.NoXPos, src.NoXPos, ir.Pkgs.Runtime.Lookup("Gosched"), types.NewSignature(nil, nil, nil))
				op.DeclareParams(true)
			}
			call := newLowerTestCall(op)
			key := typecheck.TempAt(src.NoXPos, fn, types.Types[types.TINT])
			typ := types.NewSlice(types.Types[types.TINT])
			if kind == "array" || kind == "pointer" {
				typ = types.NewArray(types.Types[types.TINT], 3)
			}
			if kind == "pointer" {
				typ = types.NewPtr(typ)
			}
			if kind == "map" {
				typ = types.NewMap(types.Types[types.TINT], types.Types[types.TINT])
			}
			if kind == "string" {
				typ = types.Types[types.TSTRING]
			}
			values := typecheck.TempAt(src.NoXPos, fn, typ)
			loop := ir.NewRangeStmt(src.NoXPos, key, nil,
				values, ir.Nodes{call}, false)
			loop.SetTypecheck(1)
			fn.Body = ir.Nodes{ir.NewDecl(src.NoXPos, ir.ODCL, values), ir.NewDecl(src.NoXPos, ir.ODCL, key), loop, newLowerTestReturn()}
			plan, err := Analyze(functions, directives)
			if err != nil {
				t.Fatal(err)
			}
			candidate, err := newLowerCandidate(plan, plan.Functions[fn])
			if err != nil || canLowerRunToCompletion(candidate) != foreign {
				t.Fatalf("range candidate: %v", err)
			}
			result, err := Lower(plan, nil)
			if err != nil || result.Lowered != 1 {
				t.Fatalf("Lower = %+v, %v", result, err)
			}
			for _, generated := range typecheck.Target.Funcs {
				if generated == fn || generated == op {
					continue
				}
				ir.VisitList(generated.Body, func(node ir.Node) {
					if node.Op() == ir.ORANGE {
						t.Fatal("native entry retained an opaque indexed range")
					}
				})
			}
		})
	}
}
