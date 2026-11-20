// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import "cmd/compile/internal/ir"

// fixedDeferArgumentWrappers identifies normalized defer wrappers whose
// captured operands are private temporaries declared at their registration.
// A fixed site executes at most once per activation, so its resume slots
// already preserve those argument values through cleanup.
func (candidate *lowerCandidate) fixedDeferArgumentWrappers() map[*ir.Func]bool {
	if candidate.dynamicDefers {
		return nil
	}
	var wrappers map[*ir.Func]bool
	for _, deferred := range candidate.defers {
		if deferred.sourceLiteral || deferred.open || deferred.rangeAnchor != nil ||
			deferred.namedRecover || deferred.directGoexit || deferred.runTerminal ||
			deferred.rewriteTerminal != 0 || len(deferred.resumable) != 0 ||
			len(deferred.terminalTargets) != 0 {
			continue
		}
		closure, ok := deferred.call.Fun.(*ir.ClosureExpr)
		if !ok || !closure.Func.Wrapper() || closure.Func.WrappedFunc == nil {
			continue
		}
		private := make(map[*ir.Name]bool)
		for _, init := range deferred.statement.Init() {
			if decl, ok := init.(*ir.Decl); ok && decl.X.AutoTemp() && !decl.X.Addrtaken() {
				private[decl.X.Canonical()] = true
			}
		}
		for _, capture := range closure.Func.ClosureVars {
			if !private[capture.Canonical()] {
				ok = false
				break
			}
		}
		if ok {
			if wrappers == nil {
				wrappers = make(map[*ir.Func]bool)
			}
			wrappers[closure.Func] = true
		}
	}
	return wrappers
}

// refreshNestedClosureCaptures follows the new canonical owner after an
// enclosing deferred literal's captures have moved into the resume frame.
func refreshNestedClosureCaptures(fn *ir.Func) {
	ir.Visit(fn, func(node ir.Node) {
		if closure, ok := node.(*ir.ClosureExpr); ok {
			for _, variable := range closure.Func.ClosureVars {
				variable.Defn = variable.Outer.Canonical()
			}
			refreshNestedClosureCaptures(closure.Func)
		}
	})
}
