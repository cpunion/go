// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import "cmd/compile/internal/ir"

// hasUnplannedTypeSwitch also finds type switches inside copied or inlined
// expressions. Only switches with a planned dispatch can replace CaseClause.Var
// with a resume-local temporary before explicit frame rewriting.
func hasUnplannedTypeSwitch(body ir.Nodes, planned map[*ir.SwitchStmt]bool) bool {
	for _, stmt := range body {
		if ir.Any(stmt, func(node ir.Node) bool {
			stmt, ok := node.(*ir.SwitchStmt)
			return ok && stmt.Tag != nil && stmt.Tag.Op() == ir.OTYPESW && !planned[stmt]
		}) {
			return true
		}
	}
	return false
}

// Type switches need a planned dispatch to preserve their implicit case names.
// Ordinary expression switches stay intact unless a case needs continuation
// control or contains a type switch that itself needs a planned dispatch.
func switchNeedsLowering(stmt *ir.SwitchStmt, sites map[ir.Node]bool) bool {
	if stmt.Tag != nil && stmt.Tag.Op() == ir.OTYPESW {
		return true
	}
	for _, clause := range stmt.Cases {
		for _, body := range clause.Body {
			if ir.Any(body, func(node ir.Node) bool {
				return sites[node] || node.Op() == ir.ORETURN ||
					node.Op() == ir.ODEFER || node.Op() == ir.OCONTINUE || node.Op() == ir.OTYPESW
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
