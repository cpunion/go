// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/ir"
	"cmd/compile/internal/typecheck"
	"cmd/compile/internal/types"
)

// integerRangeLoop exposes an integer range's counter and frozen bound to
// coroutine lowering. As in walkRange, changing the source iteration variable
// does not change the counter, and the bound is evaluated exactly once.
// loopvar.ForCapture has already made distinct iteration variables explicit.
func integerRangeLoop(stmt *ir.RangeStmt, fn *ir.Func) *ir.ForStmt {
	pos := stmt.Pos()
	typ := stmt.X.Type()
	index := typecheck.TempAt(pos, fn, typ)
	limit := typecheck.TempAt(pos, fn, typ)
	init := append(ir.Nodes(nil), stmt.Init()...)
	init.Append(ir.NewDecl(pos, ir.ODCL, index), ir.NewDecl(pos, ir.ODCL, limit),
		ir.NewAssignStmt(pos, index, nil), ir.NewAssignStmt(pos, limit, stmt.X))
	condition := ir.NewBinaryExpr(pos, ir.OLT, index, limit)
	post := ir.NewAssignStmt(pos, index,
		ir.NewBinaryExpr(pos, ir.OADD, index, ir.NewInt(pos, 1)))
	body := append(ir.Nodes(nil), stmt.Body...)
	if stmt.Key != nil && !ir.IsBlank(stmt.Key) {
		var value ir.Node = index
		if !types.Identical(stmt.Key.Type(), typ) {
			conversion := ir.NewConvExpr(pos, ir.OCONV, stmt.Key.Type(), value)
			conversion.TypeWord = stmt.KeyTypeWord
			conversion.SrcRType = stmt.KeySrcRType
			value = conversion
		}
		body.Prepend(ir.NewAssignStmt(pos, stmt.Key, value))
	}
	loop := ir.NewForStmt(pos, nil, condition, post, body, false)
	loop.SetInit(init)
	loop.Label = stmt.Label
	return loop
}
