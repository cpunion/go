// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/ir"
	"cmd/compile/internal/types"
	"slices"
)

type awaitInputReuse struct {
	result *ir.Name
	input  *ir.Name
}

// planSharedArrayAwaitInputs keeps the same slot policy across each borrowing
// lineage. A peer whose input remains live keeps its own result field, and
// mixed proofs must not split an otherwise compatible typed chunk.
func planSharedArrayAwaitInputs(candidates map[*ir.Func]*lowerCandidate) {
	for _, candidate := range candidates {
		candidate.reusedAwaitInput = reusableAwaitInput(candidate)
	}
	for changed := true; changed; {
		changed = false
		for _, parent := range candidates {
			for _, child := range parent.sharedArrayAwaits {
				if (parent.reusedAwaitInput == nil) == (child.reusedAwaitInput == nil) {
					continue
				}
				parent.reusedAwaitInput = nil
				child.reusedAwaitInput = nil
				changed = true
			}
		}
	}
}

// reusableAwaitInput finds a word-sized input whose final use is in the
// outgoing call, so the scalar child result can occupy the same field.
func reusableAwaitInput(candidate *lowerCandidate) *awaitInputReuse {
	if candidate.sharedArrayPlan == nil ||
		len(candidate.branches) != 0 || !keepsReturnInResume(candidate) {
		return nil
	}
	fn := candidate.function.Func
	resultType := fn.Type().Result(0).Type
	if !resultType.IsInteger() || resultType.Size() != int64(types.PtrSize) || len(fn.Body) < 2 {
		return nil
	}
	ret, ok := fn.Body[len(fn.Body)-1].(*ir.ReturnStmt)
	assignment, isAssign := fn.Body[len(fn.Body)-2].(*ir.AssignStmt)
	if !ok || !isAssign || !assignment.Def {
		return nil
	}
	local, ok := assignment.X.(*ir.Name)
	call, isCall := assignment.Y.(*ir.CallExpr)
	if !ok || !isCall || !candidate.fusedAwaits[call] ||
		local.Class != ir.PAUTO || local.Addrtaken() || !local.Type().IsInteger() ||
		local.Type().Size() != int64(types.PtrSize) {
		return nil
	}
	refersTo := func(node ir.Node, name *ir.Name) bool {
		return ir.Any(node, func(n ir.Node) bool {
			v, ok := n.(*ir.Name)
			return ok && v.Canonical() == name.Canonical()
		})
	}
	for _, field := range fn.Type().Params() {
		param, ok := field.Nname.(*ir.Name)
		if !ok || param.Addrtaken() || !types.Identical(param.Type(), local.Type()) ||
			candidate.parameters[param.Canonical()] == nil || refersTo(ret, param) ||
			!slices.ContainsFunc(call.Args, func(arg ir.Node) bool { return refersTo(arg, param) }) {
			continue
		}
		if slices.ContainsFunc(fn.Body, func(stmt ir.Node) bool {
			return ir.Any(stmt, func(n ir.Node) bool {
				cl, ok := n.(*ir.ClosureExpr)
				return ok && slices.ContainsFunc(cl.Func.ClosureVars, func(v *ir.Name) bool {
					return v.Canonical() == param.Canonical() || v.Canonical() == local.Canonical()
				})
			})
		}) {
			continue
		}
		return &awaitInputReuse{result: local.Canonical(), input: candidate.parameters[param.Canonical()]}
	}
	return nil
}
