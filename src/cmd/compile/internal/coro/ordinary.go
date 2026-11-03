// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/base"
	"cmd/compile/internal/ir"
	"cmd/compile/internal/typecheck"
	"cmd/compile/internal/types"
)

// BodyReader supplies independent source bodies for ordinary fallback. Have
// must not materialize IR: candidates can still be rejected by dependency
// checks. Read supplies admitted ordinary entries or restores rejected callers
// whose working bodies contain private specialization calls.
type BodyReader struct {
	Have func(*ir.Func) bool
	Read func(*ir.Func) *ir.Func
	// Clone reads a source body under a compiler-owned helper symbol.
	Clone func(*ir.Func, *types.Sym) *ir.Func
	// PinCaptures gives every serialized copy of this source closure the
	// same traced *T environment fields before escape analysis.
	PinCaptures func(*ir.Func) bool
}

// A captured source temporary can survive as a declaration after its defining
// assignment has moved into a typed cell or frame field. That assignment no
// longer defines the original name. Keep static-value queries from following
// the obsolete link, including for interface operands frozen by defer wrappers.
func clearRewrittenVariableDefinitions(funcs []*ir.Func) {
	for _, fn := range funcs {
		for _, name := range fn.Dcl {
			if name.Class != ir.PAUTO && name.Class != ir.PAUTOHEAP {
				continue
			}
			valid := false
			switch definition := name.Defn.(type) {
			case *ir.AssignStmt:
				valid = definition.X == name
			case *ir.AssignListStmt:
				for _, lhs := range definition.Lhs {
					valid = valid || lhs == name
				}
			default:
				continue
			}
			if !valid {
				name.Defn = nil
			}
		}
	}
}

// addOrdinaryBody keeps the source body in the public entry, rather than
// calling a helper: an extra frame would change direct recover semantics.
// Native factories bypass this entry and never execute the ordinary body.
func addOrdinaryBody(fn, ordinary *ir.Func, anchors ...*ir.Name) {
	remapOrdinaryBody(fn, ordinary)
	condition := typecheck.Call(fn.Pos(), typecheck.LookupRuntime("coroUseOrdinary"), nil, false)
	gate := ir.NewIndexExpr(fn.Pos(), nativeSourceGates[fn], ir.NewInt(fn.Pos(), 0))
	condition = ir.NewLogicalExpr(fn.Pos(), ir.OOROR, condition,
		ir.NewBinaryExpr(fn.Pos(), ir.ONE, gate, ir.NewInt(fn.Pos(), 0)))
	for _, anchor := range anchors {
		condition = ir.NewLogicalExpr(fn.Pos(), ir.OOROR, condition,
			ir.NewUnaryExpr(fn.Pos(), ir.ONOT, typecheck.Call(fn.Pos(), typecheck.LookupRuntime("coroRangeDeferIsNative"), ir.Nodes{anchor}, false)))
	}
	// Separately loaded Go modules have no closed linked-image stack proof.
	// Retain their factory ABI and summaries, while keeping public calls ordinary.
	if base.Flag.Dynlink != nil && *base.Flag.Dynlink || base.Flag.LinkShared != nil && *base.Flag.LinkShared {
		condition = ir.NewBool(fn.Pos(), true)
	}
	fn.Body = ir.Nodes{ir.NewIfStmt(fn.Pos(), condition, ordinary.Body, fn.Body)}
	ir.WithFunc(fn, func() { typecheck.Stmts(fn.Body) })
}

// remapOrdinaryBody installs independent declarations and captures without
// adding a call frame. Compiler-private callback helper entries always use
// these ordinary bodies; only their validated factories enter native tasks.
func remapOrdinaryBody(fn, ordinary *ir.Func) {
	parameters := make(map[*ir.Name]*ir.Name)
	for i, variable := range ordinary.ClosureVars {
		parameters[variable] = fn.ClosureVars[i]
	}
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
	fn.Parents, fn.Marks = ordinary.Parents, ordinary.Marks
}
