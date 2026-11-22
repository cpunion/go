// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/base"
	"cmd/compile/internal/ir"
	"cmd/compile/internal/typecheck"
	"cmd/compile/internal/types"
	"cmd/internal/src"
	"slices"
)

// An ordered return replaces a serial descent and unwind with a single
// yield followed by ascending iterations. The descent only initializes a
// private array and forwards the arguments. The unwind keeps the original
// expression trees and scalar writes, including the call to each array reader.
type orderedReturn struct {
	depth      int
	array      *rootArrayPlan
	child      *ir.Name
	base, step ir.Nodes
	baseOps    []orderedReturnOp
	stepOps    []orderedReturnOp
}

// Parameter and child names are compared by their role across functions in
// the closed call graph. Array readers must have the exact same target.
type orderedReturnOp struct {
	op      ir.Op
	param   int
	literal int64
	reader  *ir.Name
}

func planOrderedReturn(f *Function) *affineReturnPlan {
	if f == nil || f.Func == nil || f.Primary != CoroPrimary || base.Flag.Cfg.Instrumenting {
		return nil
	}
	fn := f.Func
	sig := fn.Type()
	integer := types.Types[types.TINT]
	if fn.Pragma != 0 || fn.OClosure != nil || fn.Needctxt() || len(fn.ClosureVars) != 0 ||
		sig.HasShape() || sig.NumParams() != len(sig.RecvParams()) ||
		sig.NumResults() != 1 || sig.Result(0).Type != integer || len(fn.Body) < 5 {
		return nil
	}
	array := planFrameArray(f, 1, 0)
	branch, ok := fn.Body[2].(*ir.IfStmt)
	if array == nil || !ok || len(branch.Else) != 0 || len(branch.Body) < 2 {
		return nil
	}
	condition, ok := branch.Cond.(*ir.BinaryExpr)
	if !ok || condition.Op() != ir.OEQ && condition.Op() != ir.OLE {
		return nil
	}
	depth, ok := condition.X.(*ir.Name)
	if !ok || depth.Type() != integer || !isInvariantDepthByte(array.write.Y, depth) {
		return nil
	}
	if zero, ok := affineLiteral(condition.Y); !ok || zero != 0 {
		return nil
	}
	assign, ok := fn.Body[3].(*ir.AssignStmt)
	if !ok || !assign.Def || len(assign.Init()) != 1 {
		return nil
	}
	child, ok := assign.X.(*ir.Name)
	decl, declared := assign.Init()[0].(*ir.Decl)
	call, called := assign.Y.(*ir.CallExpr)
	if !ok || child.Class != ir.PAUTO || child.Type() != integer ||
		!declared || decl.Op() != ir.ODCL || decl.X != child || !called {
		return nil
	}
	for _, node := range fn.Body[2:] {
		if ir.Any(node, func(n ir.Node) bool { return n != assign && len(n.Init()) != 0 }) {
			return nil
		}
	}
	params := make(map[*ir.Name]int)
	for i, field := range sig.Params() {
		name, ok := field.Nname.(*ir.Name)
		if !ok || name.Type() != integer && !isOrderedPointer(name.Type()) {
			return nil
		}
		params[name.Canonical()] = i + 1
	}
	yield := sourceGosched(branch.Body[0])
	depthIndex := params[depth.Canonical()]
	if yield == nil || depthIndex == 0 {
		return nil
	}
	target := invariantReturnTarget(sig, depthIndex, call)
	if target == nil {
		return nil
	}

	p := &orderedReturn{depth: depthIndex, array: array, child: child, base: branch.Body[1:], step: fn.Body[4:]}
	var valid bool
	p.baseOps, valid = p.operations(p.base, params, false)
	if !valid {
		return nil
	}
	p.stepOps, valid = p.operations(p.step, params, true)
	if !valid {
		return nil
	}
	return &affineReturnPlan{input: depth, target: target, yield: yield, source: f, ordered: p}
}

func isOrderedPointer(t *types.Type) bool {
	return t.IsPtr() && t.Elem() == types.Types[types.TINT]
}

func (p *orderedReturn) operations(body ir.Nodes, params map[*ir.Name]int, step bool) ([]orderedReturnOp, bool) {
	ret, ok := body[len(body)-1].(*ir.ReturnStmt)
	if !ok || len(ret.Results) != 1 {
		return nil, false
	}
	var operations []orderedReturnOp
	pointer := func(n ir.Node) int {
		load, ok := n.(*ir.StarExpr)
		if !ok || load.Op() != ir.ODEREF || load.Type() != types.Types[types.TINT] {
			return 0
		}
		if name, ok := load.X.(*ir.Name); ok {
			return params[name.Canonical()]
		}
		return 0
	}
	var expression func(ir.Node) bool
	expression = func(n ir.Node) bool {
		if n.Type() != types.Types[types.TINT] {
			return false
		}
		switch n := n.(type) {
		case *ir.BinaryExpr:
			if !rootArrayArithmetic(n.Op()) {
				return false
			}
			operations = append(operations, orderedReturnOp{op: n.Op()})
			return expression(n.X) && expression(n.Y)
		case *ir.CallExpr:
			if _, ok := p.array.uses[n]; ok {
				operations = append(operations, orderedReturnOp{op: ir.OCALLFUNC, reader: ir.StaticCalleeName(n.Fun)})
				return true
			}
		case *ir.StarExpr:
			if index := pointer(n); index != 0 {
				operations = append(operations, orderedReturnOp{op: ir.ODEREF, param: index})
				return true
			}
		case *ir.Name:
			index := params[n.Canonical()]
			if index != 0 || step && affineInput(n, p.child) {
				operations = append(operations, orderedReturnOp{op: ir.ONAME, param: index})
				return true
			}
		case *ir.BasicLit:
			if value, ok := affineLiteral(n); ok {
				operations = append(operations, orderedReturnOp{op: ir.OLITERAL, literal: value})
				return true
			}
		}
		return false
	}
	for _, node := range body[:len(body)-1] {
		op := ir.OAS
		var left, right ir.Node
		switch stmt := node.(type) {
		case *ir.AssignStmt:
			left, right = stmt.X, stmt.Y
		case *ir.AssignOpStmt:
			op, left, right = stmt.AsOp, stmt.X, stmt.Y
			if !rootArrayArithmetic(op) {
				return nil, false
			}
		default:
			return nil, false
		}
		index := pointer(left)
		if index == 0 {
			return nil, false
		}
		operations = append(operations, orderedReturnOp{op: op, param: index})
		if !expression(right) {
			return nil, false
		}
	}
	operations = append(operations, orderedReturnOp{op: ir.ORETURN})
	valid := expression(ret.Results[0])
	return operations, valid
}

func sameOrderedReturn(a, b *orderedReturn) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.depth == b.depth && a.array.writeIndex == b.array.writeIndex &&
		types.Identical(a.array.array.Type(), b.array.array.Type()) &&
		slices.Equal(a.baseOps, b.baseOps) && slices.Equal(a.stepOps, b.stepOps)
}

func applyOrderedReturn(plan *Plan, old *lowerCandidate, p *affineReturnPlan) *lowerCandidate {
	fn, ordered := old.function.Func, p.ordered
	pos, integer := fn.Pos(), types.Types[types.TINT]
	literal := func(n int64) ir.Node { return typedInt(pos, integer, n) }
	original, declarations := fn.Body, fn.Dcl
	yield := ir.Copy(p.yield).(*ir.CallExpr)
	var checks ir.Node = ir.NewBinaryExpr(pos, ir.OGE, p.input, literal(0))
	for _, field := range fn.Type().Params() {
		if name := field.Nname.(*ir.Name); isOrderedPointer(name.Type()) {
			nonnull := ir.NewBinaryExpr(pos, ir.ONE, name, ir.NewNilExpr(pos, name.Type()))
			checks = ir.NewLogicalExpr(pos, ir.OANDAND, checks, nonnull)
		}
	}

	// The reader cannot observe or retain the private array's address and
	// cannot write it. Reuse its initial zeroes at every depth.
	array := typecheck.TempAt(pos, fn, ordered.array.array.Type())
	index := typecheck.TempAt(pos, fn, integer)
	total := typecheck.TempAt(pos, fn, integer)
	var unwind func(ir.Node, ir.Node) ir.Node
	unwind = func(node, depth ir.Node) ir.Node {
		if name, ok := node.(*ir.Name); ok {
			switch name.Canonical() {
			case p.input.Canonical():
				return depth
			case ordered.child.Canonical():
				return total
			case ordered.array.array:
				return array
			}
		}
		ir.EditChildren(node, func(child ir.Node) ir.Node { return unwind(child, depth) })
		return node
	}
	emit := func(depth ir.Node, source ir.Nodes, body *ir.Nodes) {
		for _, node := range source {
			if ret, ok := node.(*ir.ReturnStmt); ok {
				result := unwind(ir.DeepCopy(src.NoXPos, ret.Results[0]), depth)
				body.Append(ir.NewAssignStmt(ret.Pos(), total, result))
				continue
			}
			body.Append(unwind(ir.DeepCopy(src.NoXPos, node), depth))
		}
	}
	fastBody := ir.Nodes{yield,
		ir.NewDecl(pos, ir.ODCL, array), ir.NewAssignStmt(pos, array, ir.NewZero(pos, array.Type())),
		ir.NewDecl(pos, ir.ODCL, total)}
	emit(literal(0), ordered.base, &fastBody)
	write := ir.NewIndexExpr(pos, array, literal(ordered.array.writeIndex))
	value := ir.NewConvExpr(pos, ir.OCONV, array.Type().Elem(), index)
	body := ir.Nodes{ir.NewAssignStmt(pos, write, value)}
	emit(index, ordered.step, &body)
	// Test for the original depth before incrementing. A final depth of
	// MaxInt must not wrap the loop index.
	body.Append(ir.NewIfStmt(pos, ir.NewBinaryExpr(pos, ir.OEQ, index, p.input),
		ir.Nodes{ir.NewBranchStmt(pos, ir.OBREAK, nil)}, nil))
	loop := ir.NewForStmt(pos, nil, nil, ir.NewAssignOpStmt(pos, ir.OADD, index, literal(1)), body, false)
	fastBody.Append(ir.NewIfStmt(pos, ir.NewBinaryExpr(pos, ir.OGT, p.input, literal(0)), ir.Nodes{
		ir.NewDecl(pos, ir.ODCL, index), ir.NewAssignStmt(pos, index, literal(1)), loop,
	}, nil), ir.NewReturnStmt(pos, ir.Nodes{total}))
	fast := ir.NewIfStmt(pos, checks, fastBody, nil)
	return applySingleYieldFast(plan, old, p, original, declarations, yield, fast)
}
