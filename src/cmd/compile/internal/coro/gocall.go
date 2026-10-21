// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import "cmd/compile/internal/ir"

// unwrapGoCall returns the call inside a frontend-generated go/defer wrapper when
// its captured operands can be read from the caller's frozen temporaries.
// The original wrapper and statement init remain untouched, so rejected
// lowering candidates keep the frontend's ordinary Go implementation.
func unwrapGoCall(call *ir.CallExpr) *ir.CallExpr {
	closure, ok := call.Fun.(*ir.ClosureExpr)
	if !ok || len(call.Args) != 0 || len(call.Init()) != 0 ||
		len(closure.Init()) != 0 {
		return call
	}
	wrapper := closure.Func
	if !wrapper.Wrapper() || len(wrapper.Body) != 1 {
		return call
	}
	target, ok := wrapper.Body[0].(*ir.CallExpr)
	if !ok || target.Op() != ir.OCALLFUNC || len(target.Init()) != 0 {
		return call
	}
	captures := make(map[*ir.Name]*ir.Name, len(wrapper.ClosureVars))
	for _, variable := range wrapper.ClosureVars {
		if variable.Outer == nil {
			return call
		}
		captures[variable] = variable.Outer
	}
	if wrapper.WrappedFunc != nil {
		name := ir.StaticCalleeName(ir.StaticValue(target.Fun))
		if name == nil || name.Func != wrapper.WrappedFunc {
			return call
		}
	} else {
		// The frontend freezes a dynamic callee into a caller temporary,
		// then captures that value in its wrapper. Only that exact shape
		// is unpacked; source closures and interface wrappers stay ordinary.
		name, ok := target.Fun.(*ir.Name)
		if !ok || captures[name] == nil || captures[name].Curfn != wrapper.ClosureParent {
			return call
		}
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

// unwrapDynamicDeferCall exposes only a validated frontend wrapper's frozen
// dynamic callee. Static and source-literal defers retain their existing call
// plans, including the stricter terminal-control entry contracts.
func unwrapDynamicDeferCall(call *ir.CallExpr) *ir.CallExpr {
	closure, ok := call.Fun.(*ir.ClosureExpr)
	if !ok || closure.Func.WrappedFunc != nil {
		return call
	}
	return unwrapGoCall(call)
}
