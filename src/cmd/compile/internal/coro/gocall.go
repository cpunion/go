// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import "cmd/compile/internal/ir"

// unwrapGoCall returns the named call inside a frontend-generated go wrapper when
// its captured operands can be read from the caller's frozen temporaries.
// The original wrapper and go statement init remain untouched, so rejected
// lowering candidates keep the frontend's ordinary Go implementation.
func unwrapGoCall(call *ir.CallExpr) *ir.CallExpr {
	closure, ok := call.Fun.(*ir.ClosureExpr)
	if !ok || len(call.Args) != 0 || len(call.Init()) != 0 ||
		len(closure.Init()) != 0 {
		return call
	}
	wrapper := closure.Func
	if !wrapper.Wrapper() || wrapper.WrappedFunc == nil ||
		len(wrapper.Body) != 1 {
		return call
	}
	target, ok := wrapper.Body[0].(*ir.CallExpr)
	if !ok || target.Op() != ir.OCALLFUNC || len(target.Init()) != 0 {
		return call
	}
	name := ir.StaticCalleeName(ir.StaticValue(target.Fun))
	if name == nil || name.Func != wrapper.WrappedFunc {
		return call
	}
	captures := make(map[*ir.Name]*ir.Name, len(wrapper.ClosureVars))
	for _, variable := range wrapper.ClosureVars {
		if variable.Outer == nil {
			return call
		}
		captures[variable] = variable.Outer
	}
	var copy func(ir.Node) ir.Node
	copy = func(node ir.Node) ir.Node {
		if name, ok := node.(*ir.Name); ok {
			if outer := captures[name]; outer != nil {
				return outer
			}
			return name
		}
		node = ir.Copy(node)
		ir.EditChildren(node, copy)
		return node
	}
	return copy(target).(*ir.CallExpr)
}
