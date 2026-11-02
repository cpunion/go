// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/ir"
	"cmd/compile/internal/typecheck"
	"cmd/compile/internal/types"
	"cmd/internal/src"
)

// rangeDeferRecordType keeps ordinary registrations and range markers in one
// traced owner stack. An anchor distinguishes a marker from an ordinary nil
// function, which must still panic only when cleanup invokes it.
func rangeDeferRecordType() *types.Type {
	typ := types.NewStruct([]*types.Field{
		types.NewField(src.NoXPos, typecheck.Lookup("fn"), types.NewSignature(nil, nil, nil)),
		types.NewField(src.NoXPos, typecheck.Lookup("anchor"), types.Types[types.TINTER]),
	})
	typ.SetNoalg(true)
	types.CalcSize(typ)
	return typ
}

// lowerDeferStackCleanup pops each ordinary call before invocation. A range
// marker instead remains registered while Next removes one pending callback.
// Terminal restart can therefore resume the same marker without losing the
// remainder or moving its callbacks past another ordinary defer record.
func lowerDeferStackCleanup(pos src.XPos, resume *ir.Func, ctx ir.Node,
	stack ir.Node, token ir.Node, namedRecover, records bool) *ir.ForStmt {
	element := stack.Type().Elem()
	function := element
	if records {
		function = element.Field(0).Type
	}
	deferred := typecheck.TempAt(pos, resume, function)
	stackLen := func() ir.Node { return ir.NewUnaryExpr(pos, ir.OLEN, stack) }
	last := func() ir.Node {
		return ir.NewBinaryExpr(pos, ir.OSUB, stackLen(), ir.NewInt(pos, 1))
	}
	loadLast := func() ir.Node { return ir.NewIndexExpr(pos, stack, last()) }
	pop := func() ir.Nodes {
		return ir.Nodes{
			ir.NewAssignStmt(pos, loadLast(), ir.NewZero(pos, element)),
			ir.NewAssignStmt(pos, stack, ir.NewSliceExpr(pos, ir.OSLICE, stack, nil, last(), nil)),
		}
	}
	var body ir.Nodes
	if !records {
		body = append(body, ir.NewAssignStmt(pos, deferred, loadLast()))
		body = append(body, pop()...)
	} else {
		record := typecheck.TempAt(pos, resume, element)
		field := func(index int) ir.Node {
			selection := element.Field(index)
			node := ir.NewSelectorExpr(pos, ir.ODOT, record, selection.Sym)
			node.Selection = selection
			node.SetType(selection.Type)
			node.SetTypecheck(1)
			return node
		}
		ok := typecheck.TempAt(pos, resume, types.Types[types.TBOOL])
		next := typecheck.Call(pos, typecheck.LookupRuntime("coroRangeDeferNext"),
			ir.Nodes{ctx, field(1)}, false)
		drained := append(pop(), ir.NewBranchStmt(pos, ir.OCONTINUE, nil))
		marker := ir.Nodes{
			ir.NewAssignListStmt(pos, ir.OAS2, ir.Nodes{deferred, ok}, ir.Nodes{next}),
			ir.NewIfStmt(pos, ir.NewUnaryExpr(pos, ir.ONOT, ok), drained, nil),
		}
		ordinary := append(ir.Nodes{ir.NewAssignStmt(pos, deferred, field(0))}, pop()...)
		body = append(body,
			ir.NewAssignStmt(pos, record, loadLast()),
			ir.NewIfStmt(pos, ir.NewBinaryExpr(pos, ir.ONE, field(1),
				ir.NewNilExpr(pos, types.Types[types.TINTER])), marker, ordinary),
		)
	}
	invoke := typecheck.Call(pos, typecheck.LookupRuntime("coroDeferInvoke"), ir.Nodes{deferred}, false)
	if namedRecover {
		invoke = typecheck.Call(pos, typecheck.LookupRuntime("coroDeferCall"), ir.Nodes{token, deferred}, false)
	}
	body = append(body, invoke)
	condition := ir.NewBinaryExpr(pos, ir.ONE, stackLen(), ir.NewInt(pos, 0))
	return ir.NewForStmt(pos, nil, condition, nil, body, false)
}
