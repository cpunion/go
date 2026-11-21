// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import "cmd/compile/internal/ir"

func (deferred *lowerDefer) plainDeferCall() bool {
	return deferred.synchronousDeferCall() && deferred.rewriteTerminal == 0
}

func (deferred *lowerDefer) synchronousDeferCall() bool {
	return !deferred.sourceLiteral && !deferred.open && deferred.rangeAnchor == nil &&
		!deferred.namedRecover && !deferred.directGoexit && !deferred.runTerminal &&
		len(deferred.resumable) == 0 &&
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

// explicitFrameDefers proves that each registration can store its operands
// or capture cells in explicit frame slots. Direct recover literals still
// invoke cleanup with their own task token.
func (candidate *lowerCandidate) explicitFrameDefers() (map[*ir.Func]bool, bool) {
	function := candidate.function
	if len(candidate.defers) == 0 {
		// A caller can receive a terminal outcome from its children without
		// running cleanup itself. Its normal terminal entry propagates it.
		return nil, function.LocalTerminal == 0 && function.Terminal&^(MayPanic|UsesRecover) == 0
	}
	if candidate.dynamicDefers || candidate.rangeOwner != nil {
		return nil, false
	}
	defers := candidate.fixedDeferArgumentWrappers()
	ownRecover := false
	for _, deferred := range candidate.defers {
		if !deferred.synchronousDeferCall() || deferred.snapshotCallee {
			return nil, false
		}
		if closure, ok := deferred.call.Fun.(*ir.ClosureExpr); ok {
			if closure.Func.Wrapper() {
				if !defers[closure.Func] {
					return nil, false
				}
				continue
			}
			if deferred.rewriteTerminal&^UsesRecover != 0 {
				return nil, false
			}
			if defers == nil {
				defers = make(map[*ir.Func]bool)
			}
			defers[closure.Func] = true
			ownRecover = ownRecover || deferred.rewriteTerminal == UsesRecover
		} else if !deferred.plainDeferCall() || ir.StaticCalleeName(deferred.call.Fun) == nil {
			return nil, false
		}
	}
	if function.Terminal != 0 && (!ownRecover || function.Terminal&^(MayPanic|UsesRecover) != 0 ||
		function.LocalTerminal&UsesRecover != 0) {
		return nil, false
	}
	return defers, true
}

// Only parameters and results live for the whole activation regardless of
// control flow. Other captures keep their per-declaration cells so cleanup
// can reference an earlier execution of a source declaration.
func (candidate *lowerCandidate) borrowDeferCapture(name *ir.Name) bool {
	source := name.Canonical()
	return candidate.heapLocals != nil && !candidate.heapLocals[source] &&
		(candidate.parameters[source] != nil || candidate.results[source] != nil)
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
