// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/ir"
	"cmd/compile/internal/staticinit"
	"cmd/compile/internal/typecheck"
	"cmd/compile/internal/types"
)

// normalizeSuspensionExpressions exposes single-value receives and awaited
// operations at their original evaluation points, including guarded branches and
// repeated loop headers. Sole multi-result returns keep independent result cells.
//
// The normal ordering pass runs after escape analysis, but coroutine lowering
// must run before it. Keep this pass deliberately smaller: it only introduces
// assignments that expose suspension points, and declines transformations
// that would move a call across an observable operation.
func normalizeSuspensionExpressions(function *Function) {
	if function == nil || function.Func == nil ||
		function.Primary != CoroPrimary {
		return
	}

	operations := make(map[ir.Node]bool)
	for _, site := range function.Sites {
		if site.Node == nil || site.Node.Type() == nil {
			continue
		}
		switch site.Kind {
		case SiteAwait:
			if _, ok := site.Node.(*ir.CallExpr); ok {
				operations[site.Node] = true
			}
		case SiteChannel:
			if site.Node.Op() == ir.ORECV {
				operations[site.Node] = true
			}
		}
	}

	oldCurFunc := ir.CurFunc
	ir.CurFunc = function.Func
	if function.continuationLabels == nil {
		function.continuationLabels = make(map[*types.Sym]*ir.LabelStmt)
	}
	normalizeCallList(function.Func.Body, function.Func, operations, function.continuationLabels)
	ir.CurFunc = oldCurFunc
}

func normalizeCallList(list ir.Nodes, fn *ir.Func,
	operations map[ir.Node]bool, labels ...map[*types.Sym]*ir.LabelStmt) {
	for _, stmt := range list {
		normalizeCallStatement(stmt, fn, operations, labels...)
	}
}

func normalizeCallStatement(stmt ir.Node, fn *ir.Func,
	operations map[ir.Node]bool, labels ...map[*types.Sym]*ir.LabelStmt) {
	initNode, ok := stmt.(ir.InitNode)
	if !ok {
		return
	}
	prefix := ir.TakeInit(stmt)
	normalizeCallList(prefix, fn, operations, labels...)

	var root ir.Node
	switch stmt := stmt.(type) {
	case *ir.BlockStmt:
		initNode.SetInit(prefix)
		normalizeCallList(stmt.List, fn, operations, labels...)
		return
	case *ir.IfStmt:
		var init ir.Nodes
		stmt.Cond, init = normalizeCallExpression(stmt.Cond, fn, operations, false, labels...)
		initNode.SetInit(append(prefix, init...))
		normalizeCallList(stmt.Body, fn, operations, labels...)
		normalizeCallList(stmt.Else, fn, operations, labels...)
		return
	case *ir.ForStmt:
		// Keep condition execution inside the loop. In particular continue
		// must still pass through Post before reaching the next condition;
		// moving condition initializers to the loop's Init would run them once.
		initNode.SetInit(prefix)
		if stmt.Cond != nil && ir.Any(stmt.Cond, func(node ir.Node) bool {
			return operations[node]
		}) {
			not := ir.NewUnaryExpr(stmt.Cond.Pos(), ir.ONOT, stmt.Cond)
			stop := ir.NewBranchStmt(stmt.Pos(), ir.OBREAK, nil)
			guard := typecheck.Stmt(ir.NewIfStmt(stmt.Pos(), not, ir.Nodes{stop}, nil))
			stmt.Body = append(ir.Nodes{guard}, stmt.Body...)
			stmt.Cond = nil
		}
		if stmt.Post != nil {
			normalizeCallStatement(stmt.Post, fn, operations, labels...)
		}
		normalizeCallList(stmt.Body, fn, operations, labels...)
		return
	case *ir.LabelStmt:
		if stmt.LoopVarContinue() && len(labels) != 0 {
			labels[0][stmt.Label] = stmt
		}
		initNode.SetInit(prefix)
		return
	case *ir.RangeStmt:
		// Keep the operand and bindings at their original evaluation points.
		// Only body statements execute once for each selected iteration.
		initNode.SetInit(prefix)
		normalizeCallList(stmt.Body, fn, operations, labels...)
		return
	case *ir.SelectStmt:
		// Communications are chosen before any case body runs. Normalize
		// awaits inside the chosen body without moving them into dispatch.
		initNode.SetInit(prefix)
		for _, clause := range stmt.Cases {
			normalizeCallList(clause.Body, fn, operations, labels...)
		}
		return
	case *ir.SwitchStmt:
		initNode.SetInit(prefix)
		for _, clause := range stmt.Cases {
			normalizeCallList(clause.Body, fn, operations, labels...)
		}
		return
	case *ir.ReturnStmt, *ir.AssignStmt, *ir.AssignListStmt:
		root = stmt
	case *ir.AssignOpStmt:
		root = stmt
	case *ir.CallExpr:
		root = stmt
	default:
		initNode.SetInit(prefix)
		return
	}

	if assignment, ok := root.(*ir.AssignListStmt); ok {
		ordered := normalizeTupleAssignmentOrder(assignment, fn, operations)
		normalizeCallList(ordered, fn, operations, labels...)
		prefix = append(prefix, ordered...)
	}
	if assignment, ok := root.(*ir.AssignOpStmt); ok && ir.Any(root, func(node ir.Node) bool { return operations[node] }) {
		// Order calls/receives in the destination before the RHS, without
		// taking its address or reading its value. Ordinary Go leaves simple
		// loads and the eventual nil/bounds/store checks after RHS calls.
		var ordered ir.Nodes
		assignment.X = orderedCallValue(assignment.X, fn, &ordered)
		assignment.Y = orderedCallValue(assignment.Y, fn, &ordered)
		normalizeCallList(ordered, fn, operations, labels...)
		prefix = append(prefix, ordered...)
	}
	_, init := normalizeCallExpression(root, fn, operations, true, labels...)
	initNode.SetInit(append(prefix, init...))
}

func normalizeCallExpression(root ir.Node, fn *ir.Func,
	operations map[ir.Node]bool, directStatement bool, labels ...map[*types.Sym]*ir.LabelStmt) (ir.Node, ir.Nodes) {
	ordered := normalizeCallOperandOrder(root, fn, operations)
	var order []ir.Node
	var visit func(ir.Node)
	visit = func(node ir.Node) {
		if node == nil {
			return
		}
		ir.DoChildren(node, func(child ir.Node) bool {
			visit(child)
			return false
		})
		if operations[node] || ordered[node] {
			// An ordered call may have no result type. Its complete operand
			// prefix still belongs in the worklist: normalizing an inner
			// suspension can remove the last suspension from that prefix,
			// while the original operand's defining assignment remains.
			order = append(order, node)
		} else if logical, ok := node.(*ir.LogicalExpr); ok && ir.Any(logical.Y, func(n ir.Node) bool {
			return operations[n]
		}) {
			order = append(order, node)
		} else if inlined, ok := node.(*ir.InlinedCallExpr); ok && len(labels) != 0 &&
			len(inlined.ReturnVars) == 1 && ir.Any(inlined, func(n ir.Node) bool {
			return operations[n]
		}) {
			order = append(order, node)
		} else if node.Op() != ir.OINLCALL && node.Type() != nil && hasSuspensionInit(node, operations) {
			order = append(order, node)
		}
	}
	visit(root)

	var init ir.Nodes
	// Exposing a call's initializer can make an argument initializer safe on
	// the next pass. Each change removes an initializer or suspension from root,
	// so this reaches a fixed point without duplicating an evaluation.
	for {
		changed := false
		for _, node := range order {
			if logical, ok := node.(*ir.LogicalExpr); ok && safeCallPrefix(root, logical) {
				// Preserve the conditional evaluation point by assigning the
				// left operand first and executing the complete right operand
				// only in its selected branch. Recursive normalization keeps
				// inline control flow and argument initializers in that branch.
				logical = typecheck.Expr(logical).(*ir.LogicalExpr)
				value := typecheck.TempAt(logical.Pos(), fn, logical.Type())
				first := ir.NewAssignStmt(logical.Pos(), value, logical.X)
				first.SetInit(ir.Nodes{ir.NewDecl(logical.Pos(), ir.ODCL, value)})
				value.Defn = first
				var condition ir.Node = value
				if logical.Op() == ir.OOROR {
					condition = ir.NewUnaryExpr(logical.Pos(), ir.ONOT, value)
				}
				second := ir.NewAssignStmt(logical.Pos(), value, logical.Y)
				branch := ir.NewIfStmt(logical.Pos(), condition, ir.Nodes{second}, nil)
				body := append(ir.TakeInit(logical), typecheck.Stmt(first), typecheck.Stmt(branch))
				normalizeCallList(body, fn, operations, labels...)
				// Typechecking an auto temporary updates Defn for each write.
				// Keep its declaration's initial assignment as the definition;
				// ordinary reassignment analysis still sees the guarded write.
				value.Defn = first
				init = append(init, body...)
				root = replaceNormalizedExpression(root, logical, value)
				changed = true
				continue
			}
			if inlined, ok := node.(*ir.InlinedCallExpr); ok && len(inlined.Body) != 0 &&
				safeCallPrefix(root, inlined) {
				// Move the complete inline execution at its original evaluation
				// point, retaining branches and local return jumps. Never hoist
				// an individual await out of the inline body's control flow.
				body := append(ir.TakeInit(inlined), inlined.Body.Take()...)
				var ownLabels func(ir.Node) bool
				ownLabels = func(n ir.Node) bool {
					// A nested inline expression owns its own jumps. If it
					// needs normalization, its expansion registers them later.
					if n.Op() == ir.OINLCALL {
						return false
					}
					if label, ok := n.(*ir.LabelStmt); ok {
						labels[0][label.Label] = label
					}
					return ir.DoChildren(n, ownLabels)
				}
				for _, statement := range body {
					ownLabels(statement)
				}
				normalizeCallList(body, fn, operations, labels...)
				init = append(init, body...)
				root = replaceNormalizedExpression(root, inlined, inlined.SingleResult())
				changed = true
				continue
			}
			if (hasSuspensionInit(node, operations) || ordered[node]) && safeCallPrefix(root, node) {
				// The frontend expands f(g()) into tuple assignments in an
				// argument conversion's Init list or the call's Init list.
				// Expose the entire ordered prefix before normalizing f,
				// including earlier callee evaluation. A non-suspending f
				// needs the same treatment.
				prefix := ir.TakeInit(node)
				delete(ordered, node)
				normalizeCallList(prefix, fn, operations, labels...)
				init = append(init, prefix...)
				changed = true
			}
			if !operations[node] {
				continue
			}
			if directStatement && directSuspensionStatement(root, node) {
				continue
			}
			if hasNestedSuspension(node, operations) ||
				!safeCallPrefix(root, node) {
				continue
			}
			if call, ok := node.(*ir.CallExpr); ok && call.Type().IsFuncArgStruct() {
				ret, ok := root.(*ir.ReturnStmt)
				if !ok || len(ret.Results) != 1 || ret.Results[0] != call ||
					call.Fun.Type().NumResults() != fn.Type().NumResults() || fn.Type().NumResults() < 2 {
					continue
				}
				// The call is the only return expression. Its result destinations
				// must be independent typed cells, evaluated before assigning the
				// function's output slots and running its deferred cleanup.
				var values, declarations ir.Nodes
				for _, result := range fn.Type().Results() {
					temp := typecheck.TempAt(call.Pos(), fn, result.Type)
					values.Append(temp)
					declarations.Append(ir.NewDecl(call.Pos(), ir.ODCL, temp))
				}
				assign := ir.NewAssignListStmt(call.Pos(), ir.OAS2, values, ir.Nodes{call})
				assign.SetInit(declarations)
				for _, value := range values {
					value.Name().Defn = assign
				}
				init.Append(typecheck.Stmt(assign))
				ret.Results = values
				changed = true
				continue
			}

			temp := typecheck.TempAt(node.Pos(), fn, node.Type())
			assign := ir.NewAssignStmt(node.Pos(), temp, node)
			assign.PtrInit().Append(ir.NewDecl(node.Pos(), ir.ODCL, temp))
			temp.Defn = assign
			init.Append(typecheck.Stmt(assign))
			root = replaceNormalizedExpression(root, node, temp)
			changed = true
		}
		if !changed {
			break
		}
	}
	return root, init
}

func hasSuspensionInit(node ir.Node, operations map[ir.Node]bool) bool {
	for _, stmt := range node.Init() {
		if ir.Any(stmt, func(node ir.Node) bool {
			return operations[node]
		}) {
			return true
		}
	}
	return false
}

func directSuspensionStatement(root, operation ir.Node) bool {
	switch root := root.(type) {
	case *ir.CallExpr:
		return root == operation
	case *ir.AssignStmt:
		return root.Y == operation
	case *ir.AssignListStmt:
		return len(root.Rhs) == 1 && root.Rhs[0] == operation
	}
	return false
}

func hasNestedSuspension(root ir.Node, operations map[ir.Node]bool) bool {
	return ir.Any(root, func(node ir.Node) bool {
		return node != root && operations[node]
	})
}

// safeCallPrefix reports whether hoisting target before root preserves every
// observable operation that precedes target. Short-circuit right operands are
// conditional and therefore cannot be hoisted.
func safeCallPrefix(root, target ir.Node) bool {
	if root == target {
		return true
	}
	// The ordinary ordering pass leaves simple destination loads and store
	// checks until after RHS calls. Keep that behavior here: taking &p.x or
	// snapshotting p would move a nil check or a variable read before target.
	// Calls, indices and other complex destinations retain the conservative
	// prefix check below until their ordered operands can be exposed too.
	// Initializers and tuple/receive protocols retain their complete original
	// ordering. In particular a two-result receive cannot become a scalar.
	if len(root.Init()) == 0 {
		switch assign := root.(type) {
		case *ir.AssignStmt:
			if assign.X.Op() != ir.ONAME && lateAssignmentTarget(assign.X) && containsCall(assign.Y, target) {
				return safeCallPrefix(assign.Y, target)
			}
		case *ir.AssignListStmt:
			if assign.Op() != ir.OAS2 {
				break
			}
			late, complex := true, false
			for _, lhs := range assign.Lhs {
				late = late && lateAssignmentTarget(lhs)
				complex = complex || lhs.Op() != ir.ONAME
			}
			if late && complex {
				for _, rhs := range assign.Rhs {
					if containsCall(rhs, target) {
						return safeCallPrefix(rhs, target)
					}
					if staticinit.AnySideEffects(rhs) {
						return false
					}
				}
			}
		}
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

// lateAssignmentTarget contains only variable, pointer and field loads. None
// has a separately ordered call or receive; even an intermediate nil pointer
// must be checked by the original assignment, after its RHS has run.
func lateAssignmentTarget(node ir.Node) bool {
	if node == nil || len(node.Init()) != 0 {
		return false
	}
	switch node := node.(type) {
	case *ir.Name:
		return true
	case *ir.StarExpr:
		return lateAssignmentTarget(node.X)
	case *ir.SelectorExpr:
		return (node.Op() == ir.ODOT || node.Op() == ir.ODOTPTR) && lateAssignmentTarget(node.X)
	}
	return false
}

func containsCall(root, target ir.Node) bool {
	return ir.Any(root, func(node ir.Node) bool {
		return node == target
	})
}

func replaceCall(root ir.Node, old *ir.CallExpr, replacement ir.Node) ir.Node {
	return replaceNormalizedExpression(root, old, replacement)
}

func replaceNormalizedExpression(root, old, replacement ir.Node) ir.Node {
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
