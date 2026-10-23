// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/ir"
	"cmd/compile/internal/reflectdata"
	"cmd/compile/internal/typecheck"
	"cmd/compile/internal/types"
	"unicode/utf8"
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

// indexedRangeLoop keeps a typed snapshot of an array, array pointer or slice
// header across resumes. Array values are copied; pointers and slice headers
// retain the original backing array.
// The hidden counter and length are independent of assignments in the body;
// element loads still observe writes to the snapshot's backing array. Use an
// ordinary index rather than walkRange's transient uintptr induction variable,
// since any body may suspend while the garbage collector runs.
func indexedRangeLoop(stmt *ir.RangeStmt, fn *ir.Func) *ir.ForStmt {
	pos := stmt.Pos()
	if typ := typecheck.RangeExprType(stmt.X.Type()); typ.IsArray() &&
		(stmt.Value == nil || ir.IsBlank(stmt.Value)) {
		// The frontend has already rewritten constant-length key-only
		// ranges to integer ranges. Evaluate the remaining expression
		// once for its length without copying or reading its elements.
		copy := *stmt
		copy.X = typecheck.Expr(ir.NewUnaryExpr(pos, ir.OLEN, stmt.X))
		return integerRangeLoop(&copy, fn)
	}
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
			if ir.IsBlank(stmt.Key) {
				body.Prepend(ir.NewAssignStmt(pos, stmt.Value, elementValue))
			} else {
				// Freeze the element before storing the key: the key's
				// storage can alias a backing-array element. Keep this
				// temporary in the typed frame rather than an OAS2 stack
				// temporary, which can overflow a nosplit resume.
				snapshot := typecheck.TempAt(pos, fn, stmt.Value.Type())
				init.Append(ir.NewDecl(pos, ir.ODCL, snapshot))
				body.Prepend(ir.NewAssignStmt(pos, snapshot, elementValue),
					ir.NewAssignStmt(pos, stmt.Key, keyValue),
					ir.NewAssignStmt(pos, stmt.Value, snapshot))
			}
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

// stringRangeLoop follows walkRange's UTF-8 decoding while keeping the string
// and byte offsets in typed frame storage across suspensions. Advance the hidden
// offset before the source body so continue resumes at the next rune, even when
// an invalid encoding consumes just one byte. The exposed key is the old offset.
func stringRangeLoop(stmt *ir.RangeStmt, fn *ir.Func) *ir.ForStmt {
	pos := stmt.Pos()
	text := typecheck.TempAt(pos, fn, types.Types[types.TSTRING])
	index := typecheck.TempAt(pos, fn, types.Types[types.TINT])
	key := typecheck.TempAt(pos, fn, types.Types[types.TINT])
	value := typecheck.TempAt(pos, fn, types.RuneType)
	init := append(ir.Nodes(nil), stmt.Init()...)
	for _, name := range []*ir.Name{text, index, key, value} {
		init.Append(ir.NewDecl(pos, ir.ODCL, name))
	}
	init.Append(ir.NewAssignStmt(pos, text, typecheck.Conv(stmt.X, types.Types[types.TSTRING])),
		ir.NewAssignStmt(pos, index, nil))
	condition := ir.NewBinaryExpr(pos, ir.OLT, index, ir.NewUnaryExpr(pos, ir.OLEN, text))
	byteValue := ir.NewIndexExpr(pos, text, index)
	byteValue.SetBounded(true)
	decode := typecheck.Call(pos, typecheck.LookupRuntime("decoderune"), ir.Nodes{text, index}, false)
	step := ir.NewIfStmt(pos,
		ir.NewBinaryExpr(pos, ir.OLE, value, ir.NewInt(pos, utf8.RuneSelf-1)),
		ir.Nodes{ir.NewAssignStmt(pos, index, ir.NewBinaryExpr(pos, ir.OADD, index, ir.NewInt(pos, 1)))},
		ir.Nodes{ir.NewAssignListStmt(pos, ir.OAS2, ir.Nodes{value, index}, ir.Nodes{decode})})
	body := ir.Nodes{ir.NewAssignStmt(pos, key, index),
		ir.NewAssignStmt(pos, value, typecheck.Conv(byteValue, types.RuneType)), step}
	convert := func(dst, src, typeWord, srcRType ir.Node) ir.Node {
		if dst.Type().Kind() == types.TBLANK || types.Identical(dst.Type(), src.Type()) {
			return src
		}
		conversion := ir.NewConvExpr(pos, ir.OCONV, dst.Type(), src)
		conversion.TypeWord, conversion.SrcRType = typeWord, srcRType
		return conversion
	}
	if stmt.Value != nil && !ir.IsBlank(stmt.Value) {
		body.Append(ir.NewAssignListStmt(pos, ir.OAS2, ir.Nodes{stmt.Key, stmt.Value}, ir.Nodes{
			convert(stmt.Key, key, stmt.KeyTypeWord, stmt.KeySrcRType),
			convert(stmt.Value, value, stmt.ValueTypeWord, stmt.ValueSrcRType)}))
	} else if stmt.Key != nil && !ir.IsBlank(stmt.Key) {
		body.Append(ir.NewAssignStmt(pos, stmt.Key, convert(stmt.Key, key, stmt.KeyTypeWord, stmt.KeySrcRType)))
	}
	body.Append(stmt.Body...)
	loop := ir.NewForStmt(pos, nil, condition, nil, body, false)
	loop.SetInit(init)
	loop.Label = stmt.Label
	return loop
}

// mapRangeLoop uses the ordinary map iterator in typed frame storage. Its
// pointers keep the map, tables and current entry alive while the body parks.
// Initialize the same storage afresh on every entry, including empty maps;
// never copy an initialized iterator or restart it after a suspension.
func mapRangeLoop(stmt *ir.RangeStmt, fn *ir.Func) *ir.ForStmt {
	pos := stmt.Pos()
	typ := stmt.X.Type()
	iterType := reflectdata.MapIterType()
	iter := typecheck.TempAt(pos, fn, iterType)
	init := append(ir.Nodes(nil), stmt.Init()...)
	init.Append(ir.NewDecl(pos, ir.ODCL, iter), ir.NewAssignStmt(pos, iter, nil),
		typecheck.Call(pos, typecheck.LookupRuntime("mapIterStart", typ.Key(), typ.Elem(), iterType),
			ir.Nodes{reflectdata.RangeMapRType(pos, stmt), stmt.X, typecheck.NodAddr(iter)}, false))
	key := ir.NewSelectorExpr(pos, ir.ODOT, iter, iterType.Field(0).Sym)
	condition := ir.NewBinaryExpr(pos, ir.ONE, key, typecheck.NodNil())
	post := typecheck.Call(pos, typecheck.LookupRuntime("mapIterNext", iterType), ir.Nodes{typecheck.NodAddr(iter)}, false)
	var body, destinations, values ir.Nodes
	simple := true
	for i, dst := range []ir.Node{stmt.Key, stmt.Value} {
		if dst == nil || ir.IsBlank(dst) {
			continue
		}
		operandType := typ.Key()
		word, rtype := stmt.KeyTypeWord, stmt.KeySrcRType
		if i == 1 {
			operandType = typ.Elem()
			word, rtype = stmt.ValueTypeWord, stmt.ValueSrcRType
		}
		pointer := ir.NewSelectorExpr(pos, ir.ODOT, iter, iterType.Field(i).Sym)
		var value ir.Node = ir.NewStarExpr(pos, typecheck.ConvNop(pointer, types.NewPtr(operandType)))
		value = typecheck.Expr(value)
		if !types.Identical(dst.Type(), operandType) {
			conversion := ir.NewConvExpr(pos, ir.OCONV, dst.Type(), value)
			conversion.TypeWord, conversion.SrcRType = word, rtype
			value = conversion
		}
		_, name := dst.(*ir.Name)
		simple = simple && name
		destinations.Append(dst)
		values.Append(value)
	}
	if simple {
		// Freeze both operands before either destination store, using
		// frame temporaries even for large indirect map keys and values.
		for i, dst := range destinations {
			snapshot := typecheck.TempAt(pos, fn, dst.Type())
			init.Append(ir.NewDecl(pos, ir.ODCL, snapshot))
			body.Append(ir.NewAssignStmt(pos, snapshot, values[i]))
			values[i] = snapshot
		}
		for i, dst := range destinations {
			body.Append(ir.NewAssignStmt(pos, dst, values[i]))
		}
	} else if len(destinations) == 2 {
		// Keep operand evaluation in the ordinary assignment order,
		// including side effects in indexed destinations.
		body.Append(ir.NewAssignListStmt(pos, ir.OAS2, destinations, values))
	} else {
		body.Append(ir.NewAssignStmt(pos, destinations[0], values[0]))
	}
	body.Append(stmt.Body...)
	loop := ir.NewForStmt(pos, nil, condition, post, body, false)
	loop.SetInit(init)
	loop.Label = stmt.Label
	return loop
}
