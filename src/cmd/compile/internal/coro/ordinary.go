// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/ir"
	"cmd/compile/internal/typecheck"
)

// BodyReader supplies independent source bodies for synctest fallback. Have
// must not materialize IR: candidates can still be rejected by dependency
// checks. Read is called only for admitted functions and must return a body.
type BodyReader struct {
	Have func(*ir.Func) bool
	Read func(*ir.Func) *ir.Func
}

// addOrdinaryBody keeps the source body in the public entry, rather than
// calling a helper: an extra frame would change direct recover semantics.
// Native factories bypass this entry and never execute the ordinary body.
func addOrdinaryBody(fn, ordinary *ir.Func) {
	parameters := make(map[*ir.Name]*ir.Name)
	count := len(fn.Type().RecvParams()) + fn.Type().NumResults()
	for i := 0; i < count; i++ {
		parameters[ordinary.Dcl[i]] = fn.Dcl[i]
	}
	for _, name := range ordinary.Dcl[count:] {
		name.Curfn = fn
		fn.Dcl = append(fn.Dcl, name)
	}
	var edit func(ir.Node) ir.Node
	edit = func(node ir.Node) ir.Node {
		if name, ok := node.(*ir.Name); ok {
			if replacement := parameters[name]; replacement != nil {
				return replacement
			}
		}
		if closure, ok := node.(*ir.ClosureExpr); ok {
			child := closure.Func
			if child.ClosureParent == ordinary {
				child.ClosureParent = fn
			}
			if child.RangeParent == ordinary {
				child.RangeParent = fn
			}
			for _, variable := range child.ClosureVars {
				if replacement := parameters[variable.Outer]; replacement != nil {
					variable.Outer = replacement
				}
				variable.Defn = variable.Outer.Canonical()
			}
			ir.EditChildren(child, edit)
		}
		ir.EditChildren(node, edit)
		return node
	}
	for i, stmt := range ordinary.Body {
		ordinary.Body[i] = edit(stmt)
	}
	ordinary.Body = append(ordinary.Body, ir.NewReturnStmt(fn.Pos(), nil))
	condition := typecheck.Call(fn.Pos(), typecheck.LookupRuntime("coroInBubble"), nil, false)
	fn.Body = ir.Nodes{ir.NewIfStmt(fn.Pos(), condition, ordinary.Body, fn.Body)}
	fn.Parents, fn.Marks = ordinary.Parents, ordinary.Marks
	ir.WithFunc(fn, func() { typecheck.Stmts(fn.Body) })
}
