// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import "cmd/compile/internal/ir"

// planScalarSharedArrays replaces the shared buffer only when every reader
// in the lineage can use a byte snapshot. The private factory signature must
// agree at both ends of each borrowing call.
func planScalarSharedArrays(candidates map[*ir.Func]*lowerCandidate) {
	for _, candidate := range candidates {
		if plan := candidate.sharedArrayPlan; plan != nil {
			candidate.scalarArrayReads = scalarArrayReturnReads(candidate.function.Func, plan)
		}
	}
	for changed := true; changed; {
		changed = false
		for _, parent := range candidates {
			for _, child := range parent.sharedArrayAwaits {
				if (parent.scalarArrayReads == nil) == (child.scalarArrayReads == nil) {
					continue
				}
				parent.scalarArrayReads, child.scalarArrayReads = nil, nil
				changed = true
			}
		}
	}
}

// scalarArrayReturnReads records the expression operands that consume the
// array. Leave statement uses and expression initializers to reconstruction.
func scalarArrayReturnReads(fn *ir.Func, plan *rootArrayPlan) []*ir.Node {
	var reads []*ir.Node
	seen := make(map[*ir.CallExpr]bool, len(plan.uses))
	var visitOperand func(*ir.Node)
	visitOperand = func(operand *ir.Node) {
		node := *operand
		if node == nil || len(node.Init()) != 0 {
			return
		}
		switch n := node.(type) {
		case *ir.CallExpr:
			if plan.uses[n] != nil {
				seen[n] = true
				reads = append(reads, operand)
			}
		case *ir.BinaryExpr:
			visitOperand(&n.X)
			visitOperand(&n.Y)
		case *ir.ConvExpr:
			visitOperand(&n.X)
		case *ir.UnaryExpr:
			visitOperand(&n.X)
		}
	}
	ir.VisitList(fn.Body, func(node ir.Node) {
		if ret, ok := node.(*ir.ReturnStmt); ok {
			for i := range ret.Results {
				visitOperand(&ret.Results[i])
			}
		}
	})
	if len(seen) != len(plan.uses) {
		return nil
	}
	return reads
}

// The original write still computes the snapshot before any suspension.
// The reader is an integer expression over constant indices, so substitute
// the saved byte and the zeros of the entry-initialized array at the call site.
func scalarArrayRead(plan *rootArrayPlan, call *ir.CallExpr, snapshot *ir.Name) ir.Node {
	result := plan.uses[call]
	var replace func(ir.Node) ir.Node
	replace = func(node ir.Node) ir.Node {
		if index, ok := node.(*ir.IndexExpr); ok {
			if ir.Int64Val(index.Index) == plan.writeIndex {
				return snapshot
			}
			return typedInt(call.Pos(), index.Type(), 0)
		}
		ir.EditChildren(node, replace)
		return node
	}
	return replace(ir.DeepCopy(call.Pos(), result))
}
