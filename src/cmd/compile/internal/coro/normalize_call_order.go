// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/ir"
	"cmd/compile/internal/typecheck"
)

// orderedCallValue exposes calls and receives as the ordinary ordering pass
// does. Simple loads remain in their original expression: in particular a
// function variable is read after argument calls, while a function-producing
// call runs before them. No destination address, nil check or bounds check is
// introduced here.
func orderedCallValue(node ir.Node, fn *ir.Func, prefix *ir.Nodes) ir.Node {
	if node == nil || node.Op() == ir.OCLOSURE {
		return node
	}
	*prefix = append(*prefix, ir.TakeInit(node)...)
	if node.Op() != ir.OANDAND && node.Op() != ir.OOROR && node.Op() != ir.OINLCALL {
		ir.EditChildren(node, func(child ir.Node) ir.Node {
			return orderedCallValue(child, fn, prefix)
		})
	}
	switch node.Op() {
	case ir.OINDEXMAP:
		index := node.(*ir.IndexExpr)
		key := index.Index
		kt := typecheck.MapKeyRuntimeType(index.X.Type())
		copyKey := !ir.IsAddressable(key)
		if kt != nil {
			nt := key.Type()
			// A fast scalar key is converted at its eventual use. A fast
			// aggregate instead needs an early memory temporary for the
			// runtime's scalar load, even if the aggregate is addressable.
			copyKey = nt.Kind() != kt.Kind() &&
				!(nt.IsPtrShaped() && kt.IsPtrShaped()) &&
				!(nt.IsInteger() && kt.IsInteger())
		}
		if copyKey {
			index.Index = orderedCallTemp(key, fn, prefix)
		}
	case ir.OCALLFUNC, ir.OCALLINTER, ir.ORECV, ir.OANDAND, ir.OOROR, ir.OINLCALL:
		if node.Type() == nil || node.Type().IsFuncArgStruct() {
			return node
		}
		return orderedCallTemp(node, fn, prefix)
	}
	return node
}

func orderedCallTemp(node ir.Node, fn *ir.Func, prefix *ir.Nodes) ir.Node {
	value := typecheck.TempAt(node.Pos(), fn, node.Type())
	assign := ir.NewAssignStmt(node.Pos(), value, node)
	assign.SetInit(ir.Nodes{ir.NewDecl(node.Pos(), ir.ODCL, value)})
	value.Defn = assign
	*prefix = append(*prefix, typecheck.Stmt(assign))
	return value
}

// Keep the complete ordered operand prefix on its call. Existing suspension
// normalization can then expose that prefix at the call's original guarded
// evaluation point, including non-suspending calls before a suspension.
func normalizeCallOperandOrder(root ir.Node, fn *ir.Func, operations map[ir.Node]bool) map[ir.Node]bool {
	ordered := make(map[ir.Node]bool)
	var calls []*ir.CallExpr
	ir.Visit(root, func(node ir.Node) {
		if call, ok := node.(*ir.CallExpr); ok &&
			(call.Op() == ir.OCALLFUNC || call.Op() == ir.OCALLINTER) &&
			(operations[call] || hasNestedSuspension(call, operations)) {
			calls = append(calls, call)
		}
	})
	for _, call := range calls {
		if !safeCallPrefix(root, call) {
			continue
		}
		prefix := ir.TakeInit(call)
		call.Fun = orderedCallValue(call.Fun, fn, &prefix)
		for i, argument := range call.Args {
			call.Args[i] = orderedCallValue(argument, fn, &prefix)
		}
		call.SetInit(prefix)
		ordered[call] = len(prefix) != 0
	}
	return ordered
}

// Frontend tuple conversions retain the producer in a result expression's
// Init. Move that complete prefix ahead of the assignment only after exposing
// its LHS calls/receives in order. The original simultaneous assignment still
// performs stores and their checks after the producer has returned.
func normalizeTupleAssignmentOrder(stmt *ir.AssignListStmt, fn *ir.Func,
	operations map[ir.Node]bool) ir.Nodes {
	complex := false
	for _, target := range stmt.Lhs {
		complex = complex || target.Op() != ir.ONAME
		if ir.Any(target, func(node ir.Node) bool { return node.Op() == ir.OINDEXMAP }) {
			return nil
		}
	}
	if !complex || len(stmt.Rhs) != len(stmt.Lhs) {
		return nil
	}
	var producer *ir.CallExpr
	for _, result := range stmt.Rhs {
		ir.Visit(result, func(node ir.Node) {
			if call, ok := node.(*ir.CallExpr); ok && operations[call] &&
				call.Type() != nil && call.Type().IsFuncArgStruct() &&
				call.Fun.Type().NumResults() == len(stmt.Lhs) {
				producer = call
			}
		})
	}
	if producer == nil {
		return nil
	}
	if _, ok := normalizedResultProjection(producer, stmt, len(stmt.Lhs)); !ok {
		return nil
	}
	var prefix ir.Nodes
	for i, target := range stmt.Lhs {
		stmt.Lhs[i] = orderedCallValue(target, fn, &prefix)
	}
	for i, result := range stmt.Rhs {
		stmt.Rhs[i] = orderedCallValue(result, fn, &prefix)
	}
	return prefix
}
