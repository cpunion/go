// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import "cmd/compile/internal/ir"

// The frontend freezes go/defer operands into distinct fresh temporaries before
// constructing their wrapper. Native frame rewriting later turns those names
// into private pointer cells. Retain only this exact provenance so the normal
// assignment walker need not invent a large stack copy for possible aliasing.
// Keep the parallel assignment intact: its normal Go ordering still determines
// when function values and aggregate variables are read relative to calls.
func recordFreshOperandAssignments(fn *ir.Func) {
	ir.VisitList(fn.Body, func(node ir.Node) {
		statement, ok := node.(*ir.GoDeferStmt)
		if !ok {
			return
		}
		call, ok := statement.Call.(*ir.CallExpr)
		if !ok || unwrapGoCall(call) == call {
			return
		}
		wrapper := call.Fun.(*ir.ClosureExpr).Func
		captures := make(map[*ir.Name]bool)
		for _, variable := range wrapper.ClosureVars {
			if variable.Outer != nil {
				captures[variable.Outer.Canonical()] = true
			}
		}
		ir.VisitList(statement.Init(), func(node ir.Node) {
			assign, ok := node.(*ir.AssignListStmt)
			if !ok || assign.Op() != ir.OAS2 || len(assign.Lhs) != len(assign.Rhs) || len(assign.Lhs) == 0 {
				return
			}
			seen := make(map[*ir.Name]bool)
			for _, destination := range assign.Lhs {
				name, ok := destination.(*ir.Name)
				if !ok || !name.AutoTemp() || name.Curfn != fn || name.Defn != assign ||
					!captures[name.Canonical()] || seen[name.Canonical()] {
					return
				}
				seen[name.Canonical()] = true
			}
			assign.CoroFreshOperands = true
		})
	})
}
