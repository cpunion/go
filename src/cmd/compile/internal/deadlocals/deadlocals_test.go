// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package deadlocals

import (
	"cmd/compile/internal/base"
	"cmd/compile/internal/ir"
	"cmd/compile/internal/typecheck"
	"cmd/compile/internal/types"
	"cmd/internal/obj"
	"cmd/internal/obj/x86"
	"cmd/internal/src"
	"testing"
)

func init() {
	types.PtrSize = 8
	types.RegSize = 8
	types.MaxWidth = 1 << 50
	base.Ctxt = obj.Linknew(&x86.Linkamd64)
	typecheck.InitUniverse()
}

func TestAssignmentInit(t *testing.T) {
	for _, list := range []bool{false, true} {
		t.Run(map[bool]string{false: "assignment", true: "assignment-list"}[list], func(t *testing.T) {
			pkg := types.NewPkg("test/assignmentinit/"+t.Name(), "assignmentinit")
			fn := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("f"), types.NewSignature(nil, nil, nil))
			fn.DeclareParams(true)
			x := fn.NewLocal(src.NoXPos, pkg.Lookup("x"), types.Types[types.TINT])
			y := fn.NewLocal(src.NoXPos, pkg.Lookup("y"), types.Types[types.TINT])
			dead := fn.NewLocal(src.NoXPos, pkg.Lookup("dead"), types.Types[types.TINT])
			xdef := ir.NewAssignStmt(src.NoXPos, x, ir.NewInt(src.NoXPos, 1))
			deaddef := ir.NewAssignStmt(src.NoXPos, dead, ir.NewInt(src.NoXPos, 2))
			callee := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("use"), types.NewSignature(nil, nil, nil))
			call := ir.NewCallExpr(src.NoXPos, ir.OCALLFUNC, callee.Nname, ir.Nodes{x})
			var assign ir.InitNode
			if list {
				assign = ir.NewAssignListStmt(src.NoXPos, ir.OAS2, ir.Nodes{y}, ir.Nodes{ir.NewInt(src.NoXPos, 3)})
			} else {
				assign = ir.NewAssignStmt(src.NoXPos, y, ir.NewInt(src.NoXPos, 3))
			}
			assign.SetInit(ir.Nodes{call})
			fn.Body = ir.Nodes{xdef, deaddef, assign}
			Funcs([]*ir.Func{fn})

			if xdef.X != x {
				t.Fatal("assignment to variable used by initializer was removed")
			}
			if !ir.IsBlank(deaddef.X) {
				t.Fatal("unrelated unused assignment was retained")
			}
			if init := assign.Init(); len(init) != 1 || init[0] != call || call.Args[0] != x {
				t.Fatal("initializer side effect or argument was changed")
			}
			var target ir.Node
			switch assign := assign.(type) {
			case *ir.AssignStmt:
				target = assign.X
			case *ir.AssignListStmt:
				target = assign.Lhs[0]
			}
			if !ir.IsBlank(target) {
				t.Fatal("unused assignment with initializer was retained")
			}
		})
	}
}
