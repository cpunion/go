// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import "cmd/compile/internal/ir"

// hasTypeSwitch also finds type switches inside copied or inlined expressions.
// CaseClause.Var must remain a Name until walk initializes the selected case.
// Explicit frame rewriting cannot replace it with a field selector.
func hasTypeSwitch(body ir.Nodes) bool {
	for _, stmt := range body {
		if ir.Any(stmt, func(node ir.Node) bool {
			stmt, ok := node.(*ir.SwitchStmt)
			return ok && stmt.Tag != nil && stmt.Tag.Op() == ir.OTYPESW
		}) {
			return true
		}
	}
	return false
}

// expressionSwitchNeedsLowering leaves ordinary expression switches intact
// unless a case needs continuation control.
func expressionSwitchNeedsLowering(stmt *ir.SwitchStmt, sites map[ir.Node]bool) bool {
	for _, clause := range stmt.Cases {
		for _, body := range clause.Body {
			if ir.Any(body, func(node ir.Node) bool {
				return sites[node] || node.Op() == ir.ORETURN ||
					node.Op() == ir.ODEFER || node.Op() == ir.OCONTINUE
			}) {
				return true
			}
		}
	}
	return false
}

// switchHeaderHasSite rejects dispatch expressions that would themselves need
// continuation states. Ordinary expressions retain the frontend's ordering and
// comparison metadata; lowering only replaces the selected case body.
func switchHeaderHasSite(stmt *ir.SwitchStmt, sites map[ir.Node]bool) bool {
	hasSite := func(node ir.Node) bool { return sites[node] }
	if stmt.Tag != nil && ir.Any(stmt.Tag, hasSite) {
		return true
	}
	for _, clause := range stmt.Cases {
		for _, node := range clause.Init() {
			if ir.Any(node, hasSite) {
				return true
			}
		}
		for _, node := range clause.List {
			if ir.Any(node, hasSite) {
				return true
			}
		}
	}
	return false
}
