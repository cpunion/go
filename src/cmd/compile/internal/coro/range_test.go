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
