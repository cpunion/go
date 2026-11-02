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

func TestNormalizeReceiveExpressions(t *testing.T) {
	prepareLowerTest(t)
	oldPkg, oldTarget := types.LocalPkg, typecheck.Target
	defer func() { types.LocalPkg, typecheck.Target = oldPkg, oldTarget }()
	for _, mode := range []string{"return", "add", "direct", "short-circuit", "or-circuit", "prior-call", "loop"} {
		t.Run(mode, func(t *testing.T) {
			pkg := types.NewPkg("example.com/coro/receives/"+mode, "p")
			types.LocalPkg, typecheck.Target = pkg, new(ir.Package)
			fn := newLowerTestFunc(pkg, "receive")
			channel := fn.NewLocal(src.NoXPos, pkg.Lookup("c"), types.NewChan(types.Types[types.TINT], types.Cboth))
			recv := ir.NewUnaryExpr(src.NoXPos, ir.ORECV, channel)
			recv.SetType(types.Types[types.TINT])
			recv.SetTypecheck(1)
			var expr ir.Node = recv
			switch mode {
			case "add":
				expr = ir.NewBinaryExpr(src.NoXPos, ir.OADD, recv, typedInt(src.NoXPos, types.Types[types.TINT], 1))
				expr.SetType(types.Types[types.TINT])
			case "short-circuit", "or-circuit", "loop":
				condition := ir.NewBinaryExpr(src.NoXPos, ir.ONE, recv, typedInt(src.NoXPos, types.Types[types.TINT], 0))
				condition.SetType(types.Types[types.TBOOL])
				condition.SetTypecheck(1)
				op := ir.OANDAND
				if mode == "or-circuit" {
					op = ir.OOROR
				}
				expr = ir.NewLogicalExpr(src.NoXPos, op, ir.NewBool(src.NoXPos, false), condition)
				expr.SetType(types.Types[types.TBOOL])
				expr.SetTypecheck(1)
				if mode == "loop" {
					expr = condition
				}
			case "prior-call":
				plain := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("plain"), types.NewSignature(nil, nil, []*types.Field{types.NewField(src.NoXPos, nil, types.Types[types.TINT])}))
				call := newLowerTestCall(plain)
				call.SetType(types.Types[types.TINT])
				expr = ir.NewBinaryExpr(src.NoXPos, ir.OADD, call, recv)
				expr.SetType(types.Types[types.TINT])
			}
			var stmt ir.Node = ir.NewReturnStmt(src.NoXPos, ir.Nodes{expr})
			if mode == "direct" {
				stmt = ir.NewAssignStmt(src.NoXPos, fn.NewLocal(src.NoXPos, pkg.Lookup("v"), recv.Type()), recv)
			} else if mode == "loop" {
				stmt = ir.NewForStmt(src.NoXPos, nil, expr, nil, nil, false)
			}
			fn.Body = ir.Nodes{stmt}
			function := &Function{Func: fn, Primary: CoroPrimary, Sites: []Site{{Kind: SiteChannel, Node: recv}}}
			normalizeSuspensionExpressions(function)
			if mode == "short-circuit" || mode == "or-circuit" {
				if len(stmt.Init()) != 2 {
					t.Fatal("conditional receive has no ordered branch")
				}
				first := stmt.Init()[0].(*ir.AssignStmt)
				guard := stmt.Init()[1].(*ir.IfStmt)
				condition := guard.Cond
				if mode == "or-circuit" {
					condition = condition.(*ir.UnaryExpr).X
				}
				second := guard.Body[0].(*ir.AssignStmt)
				if condition != first.X || second.X != first.X || first.X.Name().Defn != first || len(second.Init()) != 1 || second.Init()[0].(*ir.AssignStmt).Y != recv {
					t.Fatal("receive escaped its selected branch or lost its result cell")
				}
				normalizeSuspensionExpressions(function)
				if len(stmt.Init()) != 2 || len(second.Init()) != 1 {
					t.Fatal("conditional receive normalization is not idempotent")
				}
				return
			}
			if mode == "loop" {
				loop := stmt.(*ir.ForStmt)
				if loop.Cond != nil || len(loop.Init()) != 0 || len(loop.Body) != 1 {
					t.Fatal("receive moved out of its repeated condition")
				}
				guard := loop.Body[0].(*ir.IfStmt)
				if len(guard.Init()) != 1 || guard.Init()[0].(*ir.AssignStmt).Y != recv || guard.Cond.Op() != ir.ONOT || guard.Body[0].Op() != ir.OBREAK {
					t.Fatal("loop receive lost its evaluation or exit edge")
				}
				normalizeSuspensionExpressions(function)
				if len(loop.Body) != 1 || len(guard.Init()) != 1 {
					t.Fatal("loop receive normalization is not idempotent")
				}
				return
			}
			want := mode == "return" || mode == "add"
			if got := len(stmt.Init()) == 1; got != want {
				t.Fatalf("receive extracted=%t want=%t", got, want)
			}
			if want {
				assignment, ok := stmt.Init()[0].(*ir.AssignStmt)
				if !ok || assignment.Y != recv || assignment.X.Type() != recv.Type() || assignment.X.Name().Defn != assignment || containsCall(stmt.(*ir.ReturnStmt).Results[0], recv) {
					t.Fatal("receive or its typed result was duplicated or lost")
				}
				normalizeSuspensionExpressions(function)
				if len(stmt.Init()) != 1 {
					t.Fatal("normalization duplicated a receive")
				}
			}
		})
	}
}
