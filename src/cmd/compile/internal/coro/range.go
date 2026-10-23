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

// sliceRangeLoop keeps a typed snapshot of the slice header across resumes.
// The hidden counter and length are independent of assignments in the body;
// element loads still observe writes to the original backing array. Use an
// ordinary index rather than walkRange's transient uintptr induction variable,
// since any body may suspend while the garbage collector runs.
func sliceRangeLoop(stmt *ir.RangeStmt, fn *ir.Func) *ir.ForStmt {
	pos := stmt.Pos()
	values := typecheck.TempAt(pos, fn, stmt.X.Type())
	index := typecheck.TempAt(pos, fn, types.Types[types.TINT])
	limit := typecheck.TempAt(pos, fn, types.Types[types.TINT])
	init := append(ir.Nodes(nil), stmt.Init()...)
	init.Append(ir.NewDecl(pos, ir.ODCL, values), ir.NewDecl(pos, ir.ODCL, index),
		ir.NewDecl(pos, ir.ODCL, limit), ir.NewAssignStmt(pos, values, stmt.X),
		ir.NewAssignStmt(pos, index, nil),
		ir.NewAssignStmt(pos, limit, ir.NewUnaryExpr(pos, ir.OLEN, values)))
	condition := ir.NewBinaryExpr(pos, ir.OLT, index, limit)
	post := ir.NewAssignStmt(pos, index,
		ir.NewBinaryExpr(pos, ir.OADD, index, ir.NewInt(pos, 1)))
	body := append(ir.Nodes(nil), stmt.Body...)
	convert := func(dst, value, typeWord, srcRType ir.Node) ir.Node {
		value = typecheck.Expr(value)
		if dst.Type().Kind() == types.TBLANK || types.Identical(dst.Type(), value.Type()) {
			return value
		}
		conversion := ir.NewConvExpr(pos, ir.OCONV, dst.Type(), value)
		conversion.TypeWord, conversion.SrcRType = typeWord, srcRType
		return conversion
	}
	if stmt.Value != nil && !ir.IsBlank(stmt.Value) {
		value := ir.NewIndexExpr(pos, values, index)
		value.SetBounded(true)
		keyValue := convert(stmt.Key, index, stmt.KeyTypeWord, stmt.KeySrcRType)
		elementValue := convert(stmt.Value, value, stmt.ValueTypeWord, stmt.ValueSrcRType)
		_, simpleKey := stmt.Key.(*ir.Name)
		_, simpleValue := stmt.Value.(*ir.Name)
		if simpleKey && simpleValue {
			// Neither destination can affect the hidden header or counter.
			// Separate stores avoid copying a large element through an OAS2
			// stack temporary in a nosplit resume.
			body.Prepend(ir.NewAssignStmt(pos, stmt.Key, keyValue),
				ir.NewAssignStmt(pos, stmt.Value, elementValue))
		} else {
			// A parallel assignment preserves operands such as i, a[i].
			body.Prepend(ir.NewAssignListStmt(pos, ir.OAS2,
				ir.Nodes{stmt.Key, stmt.Value}, ir.Nodes{keyValue, elementValue}))
		}
	} else if stmt.Key != nil && !ir.IsBlank(stmt.Key) {
		body.Prepend(ir.NewAssignStmt(pos, stmt.Key,
			convert(stmt.Key, index, stmt.KeyTypeWord, stmt.KeySrcRType)))
	}
	loop := ir.NewForStmt(pos, nil, condition, post, body, false)
	loop.SetInit(init)
	loop.Label = stmt.Label
	return loop
}
