// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/ir"
	"cmd/compile/internal/staticinit"
	"cmd/compile/internal/typecheck"
)

// normalizeSingleResultCalls makes nested single-result awaits explicit.
//
// The normal ordering pass runs after escape analysis, but coroutine lowering
// must run before it. Keep this pass deliberately smaller: it only introduces
// assignments that expose suspension points, and declines transformations
// that would move a call across an observable operation.
func normalizeSingleResultCalls(function *Function) {
	if function == nil || function.Func == nil ||
		function.Primary != CoroPrimary {
		return
	}

	calls := make(map[*ir.CallExpr]bool)
	for _, site := range function.Sites {
		call, ok := site.Node.(*ir.CallExpr)
		if !ok || site.Kind != SiteAwait || call.Type() == nil ||
			call.Type().IsFuncArgStruct() {
			continue
		}
		calls[call] = true
	}
	if len(calls) == 0 {
		return
	}

	oldCurFunc := ir.CurFunc
	ir.CurFunc = function.Func
	normalizeCallList(function.Func.Body, function.Func, calls)
	ir.CurFunc = oldCurFunc
}

func normalizeCallList(list ir.Nodes, fn *ir.Func,
	calls map[*ir.CallExpr]bool) {
	for _, stmt := range list {
		normalizeCallStatement(stmt, fn, calls)
	}
}

func normalizeCallStatement(stmt ir.Node, fn *ir.Func,
	calls map[*ir.CallExpr]bool) {
	initNode, ok := stmt.(ir.InitNode)
	if !ok {
		return
	}
	init := ir.TakeInit(stmt)
	normalizeCallList(init, fn, calls)
	initNode.SetInit(init)

	var root ir.Node
	switch stmt := stmt.(type) {
	case *ir.BlockStmt:
		normalizeCallList(stmt.List, fn, calls)
		return
	case *ir.IfStmt:
		stmt.Cond, init = normalizeCallExpression(stmt.Cond, fn, calls, false)
		stmt.PtrInit().Append(init...)
		normalizeCallList(stmt.Body, fn, calls)
		normalizeCallList(stmt.Else, fn, calls)
		return
	case *ir.ForStmt:
		// A for condition is reevaluated on every iteration, whereas the
		// statement Init list runs only once. Leave nested condition and post
		// calls for a later control-flow normalization.
		normalizeCallList(stmt.Body, fn, calls)
		return
	case *ir.ReturnStmt, *ir.AssignStmt, *ir.AssignListStmt:
		root = stmt
	case *ir.CallExpr:
		root = stmt
	default:
		return
	}

	_, init = normalizeCallExpression(root, fn, calls, true)
	initNode.PtrInit().Append(init...)
}

func normalizeCallExpression(root ir.Node, fn *ir.Func,
	calls map[*ir.CallExpr]bool, directStatement bool) (ir.Node, ir.Nodes) {
	var order []*ir.CallExpr
	var visit func(ir.Node)
	visit = func(node ir.Node) {
		if node == nil {
			return
		}
		ir.DoChildren(node, func(child ir.Node) bool {
			visit(child)
			return false
		})
		if call, ok := node.(*ir.CallExpr); ok && calls[call] {
			order = append(order, call)
		}
	}
	visit(root)

	var init ir.Nodes
	for _, call := range order {
		if directStatement && directCallStatement(root, call) {
			continue
		}
		if hasNestedCall(call, calls) ||
			!safeCallPrefix(root, call) {
			continue
		}

		temp := typecheck.TempAt(call.Pos(), fn, call.Type())
		assign := ir.NewAssignStmt(call.Pos(), temp, call)
		assign.PtrInit().Append(ir.NewDecl(call.Pos(), ir.ODCL, temp))
		temp.Defn = assign
		init.Append(typecheck.Stmt(assign))
		root = replaceCall(root, call, temp)
	}
	return root, init
}

func directCallStatement(root ir.Node, call *ir.CallExpr) bool {
	switch root := root.(type) {
	case *ir.CallExpr:
		return root == call
	case *ir.AssignStmt:
		return root.Y == call
	case *ir.AssignListStmt:
		return len(root.Rhs) == 1 && root.Rhs[0] == call
	}
	return false
}

func hasNestedCall(root *ir.CallExpr, calls map[*ir.CallExpr]bool) bool {
	return ir.Any(root, func(node ir.Node) bool {
		call, ok := node.(*ir.CallExpr)
		return ok && call != root && calls[call]
	})
}

// safeCallPrefix reports whether hoisting target before root preserves every
// observable operation that precedes target. Short-circuit right operands are
// conditional and therefore cannot be hoisted.
func safeCallPrefix(root ir.Node, target *ir.CallExpr) bool {
	if root == target {
		return true
	}
	if root.Op() == ir.OINLCALL {
		// An inlined call body can contain branches and returns that are not
		// represented by the expression child order.
		return false
	}

	safe := true
	found := false
	ir.DoChildren(root, func(child ir.Node) bool {
		if !containsCall(child, target) {
			if staticinit.AnySideEffects(child) {
				safe = false
				return true
			}
			return false
		}
		if logical, ok := root.(*ir.LogicalExpr); ok &&
			logical.Y == child {
			safe = false
			found = true
			return true
		}
		found = true
		safe = safeCallPrefix(child, target)
		return true
	})
	return found && safe
}

func containsCall(root ir.Node, target *ir.CallExpr) bool {
	return ir.Any(root, func(node ir.Node) bool {
		return node == target
	})
}

func replaceCall(root ir.Node, old *ir.CallExpr, replacement ir.Node) ir.Node {
	if root == old {
		return replacement
	}
	var edit func(ir.Node) ir.Node
	edit = func(node ir.Node) ir.Node {
		if node == old {
			return replacement
		}
		ir.EditChildren(node, edit)
		return node
	}
	ir.EditChildren(root, edit)
	return root
}
