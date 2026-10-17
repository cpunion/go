// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/ir"
	"slices"
)

// rewriteDeferCapture replaces a deferred literal's value capture by a
// pointer to its registration's storage. Descendant literals must capture
// that pointer too, not retain a chain through the removed value capture.
func rewriteDeferCapture(fn *ir.Func, variable, pointer *ir.Name) {
	var rewrite func(ir.Node) ir.Node
	rewrite = func(node ir.Node) ir.Node {
		if node == variable {
			return typedDeref(node.Pos(), pointer, variable.Type())
		}
		if closure, ok := node.(*ir.ClosureExpr); ok {
			child := closure.Func
			for _, capture := range slices.Clone(child.ClosureVars) {
				if capture.Outer == variable {
					nested := ir.NewClosureVar(capture.Pos(), child, pointer)
					rewriteDeferCapture(child, capture, nested)
				}
			}
		}
		ir.EditChildren(node, rewrite)
		return node
	}
	for i, stmt := range fn.Body {
		fn.Body[i] = rewrite(stmt)
	}
	fn.ClosureVars = slices.DeleteFunc(fn.ClosureVars,
		func(capture *ir.Name) bool { return capture == variable })
}
