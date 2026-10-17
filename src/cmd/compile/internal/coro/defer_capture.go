// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import "cmd/compile/internal/ir"

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
