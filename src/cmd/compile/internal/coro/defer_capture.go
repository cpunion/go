// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import "cmd/compile/internal/ir"

func (deferred *lowerDefer) plainDeferCall() bool {
	return !deferred.sourceLiteral && !deferred.open && deferred.rangeAnchor == nil &&
		!deferred.namedRecover && !deferred.directGoexit && !deferred.runTerminal &&
		deferred.rewriteTerminal == 0 && len(deferred.resumable) == 0 &&
		len(deferred.terminalTargets) == 0
}

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
		if !deferred.plainDeferCall() {
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

// explicitFrameDefers only admits fixed registrations whose cleanup can
// use values from frame slots. Mutable and escaping capture cells in
// deferred literals still need the closure factory.
func (candidate *lowerCandidate) explicitFrameDefers(wrappers map[*ir.Func]bool) bool {
	if candidate.dynamicDefers || candidate.rangeOwner != nil {
		return false
	}
	for _, deferred := range candidate.defers {
		if !deferred.plainDeferCall() || deferred.snapshotCallee {
			return false
		}
		if closure, ok := deferred.call.Fun.(*ir.ClosureExpr); ok {
			if !wrappers[closure.Func] {
				return false
			}
		} else if ir.StaticCalleeName(deferred.call.Fun) == nil {
			return false
		}
	}
	return true
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
