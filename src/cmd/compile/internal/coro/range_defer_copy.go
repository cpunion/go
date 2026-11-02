// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/ir"
	"cmd/compile/internal/typecheck"
	"cmd/compile/internal/types"
	"slices"
)

// copyRangeDeferBody gives an ordinary callback a separate native cleanup body.
// The two bodies share only their enclosing operand/capture cells. Rewriting
// panic, recover or Goexit in the native copy must never modify the ordinary
// function that can still be registered against an ordinary range anchor.
// bindings maps the source body's captures to the corresponding ordinary
// callback cells; it does not evaluate any of those cells.
func copyRangeDeferBody(source *ir.Func, parent *ir.Func,
	bindings map[*ir.Name]*ir.Name) *ir.ClosureExpr {
	names := make(map[*ir.Name]*ir.Name)
	nodes := make(map[ir.Node]ir.Node)
	functions := make(map[*ir.Func]*ir.Func)
	var locals []*ir.Name
	for capture, outer := range bindings {
		names[capture.Outer] = outer
	}
	fields := func(source []*types.Field) []*types.Field {
		result := make([]*types.Field, len(source))
		for i, field := range source {
			copy := *field
			copy.Nname = nil
			result[i] = &copy
		}
		return result
	}
	var copyNode func(ir.Node, *ir.Func) ir.Node
	var copyFunction func(*ir.Func, *ir.Func) *ir.Func
	copyFunction = func(old, outer *ir.Func) *ir.Func {
		if fn := functions[old]; fn != nil {
			return fn
		}
		sig := old.Type()
		// Deferred source functions and their nested literals have no receiver.
		// Fresh signature fields keep DeclareParams from replacing the source
		// function's parameter declarations through shared type metadata.
		typ := types.NewSignature(nil, fields(sig.Params()), fields(sig.Results()))
		fn := ir.NewClosureFunc(old.Pos(), old.OClosure.Pos(), ir.ODEFER,
			typ, outer, typecheck.Target, 0)
		functions[old] = fn
		names[old.Nname] = fn.Nname
		nodes[old] = fn
		fn.DeclareParams(true)
		fn.Pragma = old.Pragma
		fn.Endlineno = old.Endlineno
		fn.Label = old.Label
		fn.Parents, fn.Marks = slices.Clone(old.Parents), slices.Clone(old.Marks)
		fn.NumDefers, fn.NumReturns = old.NumDefers, old.NumReturns
		fn.SetHasDefer(old.HasDefer())
		fn.SetWrapper(old.Wrapper())
		fn.WrappedFunc = old.WrappedFunc
		fn.SetClosureResultsLost(old.ClosureResultsLost())
		fn.SetOpenCodedDeferDisallowed(old.OpenCodedDeferDisallowed())
		if old.RangeParent != nil {
			fn.RangeParent = old.RangeParent
			if replacement := functions[old.RangeParent]; replacement != nil {
				fn.RangeParent = replacement
			}
		}
		parameters := len(sig.RecvParams()) + sig.NumResults()
		for i, variable := range old.Dcl {
			if i < parameters {
				names[variable] = fn.Dcl[i]
				continue
			}
			// Name.Copy intentionally panics. A fresh Name value retains source
			// flags while dropping analysis state and storage from the old body.
			copy := new(ir.Name)
			*copy = *variable
			copy.Curfn, copy.Defn = fn, nil
			copy.Opt, copy.Heapaddr = nil, nil
			fn.Dcl = append(fn.Dcl, copy)
			names[variable] = copy
			locals = append(locals, variable)
		}
		for _, capture := range old.ClosureVars {
			cell := capture.Outer
			if replacement := bindings[capture]; old == source && replacement != nil {
				cell = replacement
			} else if replacement := names[cell]; replacement != nil {
				cell = replacement
			}
			names[capture] = ir.NewClosureVar(capture.Pos(), fn, cell)
		}
		fn.Body = make(ir.Nodes, len(old.Body))
		for i, statement := range old.Body {
			fn.Body[i] = copyNode(statement, fn)
		}
		return fn
	}
	copyNode = func(node ir.Node, owner *ir.Func) ir.Node {
		if node == nil {
			return nil
		}
		if name, ok := node.(*ir.Name); ok {
			if replacement := names[name]; replacement != nil {
				return replacement
			}
			return name
		}
		switch node.Op() {
		case ir.OLITERAL, ir.ONIL, ir.OTYPE:
			return node
		}
		copy := ir.Copy(node)
		nodes[node] = copy
		if closure, ok := copy.(*ir.ClosureExpr); ok {
			closure.Func = copyFunction(closure.Func, owner)
			closure.SetType(closure.Func.Type())
			closure.Func.OClosure = closure
		}
		ir.EditChildren(copy, func(child ir.Node) ir.Node { return copyNode(child, owner) })
		return copy
	}
	fn := copyFunction(source, parent)
	for _, old := range locals {
		if replacement := nodes[old.Defn]; replacement != nil {
			names[old].Defn = replacement
		} else if name, ok := old.Defn.(*ir.Name); ok && names[name] != nil {
			names[old].Defn = names[name]
		}
		if old.Heapaddr != nil {
			names[old].Heapaddr = names[old.Heapaddr]
		}
	}
	return fn.OClosure
}
