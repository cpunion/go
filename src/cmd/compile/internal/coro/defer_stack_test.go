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

func TestDeferStackMarkerCleanup(t *testing.T) {
	prepareLowerTest(t)
	oldTarget := typecheck.Target
	typecheck.Target = new(ir.Package)
	t.Cleanup(func() { typecheck.Target = oldTarget })
	for _, records := range []bool{false, true} {
		for _, recover := range []bool{false, true} {
			fn := newLowerTestFunc(types.NewPkg("example.com/coro/marker", "p"), "cleanup")
			ctx := typecheck.TempAt(src.NoXPos, fn, types.Types[types.TUNSAFEPTR])
			token := typecheck.TempAt(src.NoXPos, fn, types.Types[types.TUNSAFEPTR])
			element := types.NewSignature(nil, nil, nil)
			if records {
				element = rangeDeferRecordType()
				if !element.HasPointers() || element.Field(0).Type.Kind() != types.TFUNC || !element.Field(1).Type.IsEmptyInterface() {
					t.Fatal("range marker lost its traced callback or anchor")
				}
			}
			stack := typecheck.TempAt(src.NoXPos, fn, types.NewSlice(element))
			loop := lowerDeferStackCleanup(src.NoXPos, fn, ctx, stack, token, recover, records)
			fn.Body = ir.Nodes{loop}
			ir.WithFunc(fn, func() { typecheck.Stmts(fn.Body) })
			invocation := loop.Body[len(loop.Body)-1].(*ir.CallExpr)
			want := "coroDeferInvoke"
			if recover {
				want = "coroDeferCall"
				if invocation.Args[0] != token {
					t.Fatal("named recover cleanup lost its enclosing token")
				}
			}
			if ir.StaticCalleeName(invocation.Fun).Sym().Name != want {
				t.Fatal("cleanup selected an unexpected defer invocation scope")
			}
			if !records {
				if len(loop.Body) != 4 || loop.Body[1].(*ir.AssignStmt).X.Op() != ir.OINDEX || loop.Body[2].(*ir.AssignStmt).X != stack {
					t.Fatal("ordinary cleanup no longer clears/pops before invoking")
				}
				continue
			}
			if len(loop.Body) != 3 {
				t.Fatal("range cleanup changed its marker/invocation ordering")
			}
			branch := loop.Body[1].(*ir.IfStmt)
			next := branch.Body[0].(*ir.AssignListStmt).Rhs[0].(*ir.CallExpr)
			if ir.StaticCalleeName(next.Fun).Sym().Name != "coroRangeDeferNext" || next.Args[0] != ctx {
				t.Fatal("range cleanup lost its exact owner activation query")
			}
			// The marker is popped only when Next says it is fully drained.
			// A successful callback must reach invocation with the marker and
			// all remaining callback roots still in the owner stack.
			drained := branch.Body[1].(*ir.IfStmt)
			if len(branch.Body) != 2 || len(drained.Body) != 3 || len(drained.Else) != 0 ||
				drained.Body[0].(*ir.AssignStmt).X.Op() != ir.OINDEX ||
				drained.Body[1].(*ir.AssignStmt).X != stack || drained.Body[2].Op() != ir.OCONTINUE {
				t.Fatal("marker removal is not restricted to the drained branch")
			}
			if len(branch.Else) != 3 || branch.Else[1].(*ir.AssignStmt).X.Op() != ir.OINDEX ||
				branch.Else[2].(*ir.AssignStmt).X != stack {
				t.Fatal("ordinary records lost their clear/pop before invocation")
			}
		}
	}
}
