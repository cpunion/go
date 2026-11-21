// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/base"
	"cmd/compile/internal/ir"
	"cmd/compile/internal/typecheck"
	"cmd/compile/internal/types"
	"slices"
)

// An invariant return has one yield at depth zero. Each recursive edge
// forwards every parameter unchanged except for subtracting one from depth.
// The unwind adds a sum that depends only on those invariant parameters.
type invariantReturn struct {
	depth      int
	base, step []invariantReturnTerm
}

// Parameter indices are one-based; zero denotes a literal or no condition.
type invariantReturnTerm struct {
	param, condition int
	deref            bool
	literal          int64
}

func planInvariantReturn(f *Function) *affineReturnPlan {
	if f == nil || f.Func == nil || f.Primary != CoroPrimary || base.Flag.Cfg.Instrumenting {
		return nil
	}
	fn := f.Func
	sig := fn.Type()
	integer := types.Types[types.TINT]
	if fn.Pragma != 0 || fn.OClosure != nil || fn.Needctxt() || len(fn.ClosureVars) != 0 ||
		sig.HasShape() || sig.NumParams() < 2 || len(sig.RecvParams()) != sig.NumParams() ||
		sig.NumResults() != 1 || sig.Result(0).Type != integer || len(fn.Body) < 2 {
		return nil
	}
	// The only permitted Init is the declaration of the local that receives
	// the recursive result when the call stands in its own statement.
	assign, _ := fn.Body[1].(*ir.AssignStmt)
	for _, stmt := range fn.Body {
		if ir.Any(stmt, func(n ir.Node) bool {
			if len(n.Init()) == 0 {
				return false
			}
			if assign == nil || n != assign || !assign.Def || len(assign.Init()) != 1 {
				return true
			}
			decl, ok := assign.Init()[0].(*ir.Decl)
			return !ok || decl.Op() != ir.ODCL || decl.X != assign.X
		}) {
			return nil
		}
	}
	branch, ok := fn.Body[0].(*ir.IfStmt)
	if !ok || len(branch.Else) != 0 || len(branch.Body) != 2 {
		return nil
	}
	test, ok := branch.Cond.(*ir.BinaryExpr)
	if !ok || test.Op() != ir.OEQ && test.Op() != ir.OLE {
		return nil
	}
	depth, ok := test.X.(*ir.Name)
	if !ok || depth.Type() != integer {
		return nil
	}
	if zero, ok := affineLiteral(test.Y); !ok || zero != 0 {
		return nil
	}
	invariant := &invariantReturn{}
	params := make(map[*ir.Name]int)
	for i, field := range sig.Params() {
		name, ok := field.Nname.(*ir.Name)
		if !ok {
			return nil
		}
		if name.Canonical() == depth.Canonical() {
			invariant.depth = i + 1
			continue
		}
		params[name.Canonical()] = i + 1
	}
	if invariant.depth == 0 {
		return nil
	}
	yield := sourceGosched(branch.Body[0])
	baseResult, ok := branch.Body[1].(*ir.ReturnStmt)
	if yield == nil || !ok || len(baseResult.Results) != 1 ||
		!appendInvariantSum(&invariant.base, baseResult.Results[0], params, 0, nil) {
		return nil
	}
	ret, ok := fn.Body[len(fn.Body)-1].(*ir.ReturnStmt)
	if !ok || len(ret.Results) != 1 {
		return nil
	}
	var child *ir.Name
	var call *ir.CallExpr
	if len(fn.Body) > 2 {
		if assign == nil || !assign.Def {
			return nil
		}
		child, _ = assign.X.(*ir.Name)
		call, _ = assign.Y.(*ir.CallExpr)
		if child == nil || child.Class != ir.PAUTO || child.Type() != integer || call == nil {
			return nil
		}
		for _, stmt := range fn.Body[2 : len(fn.Body)-1] {
			if !appendInvariantAdd(&invariant.step, stmt, child, params) {
				return nil
			}
		}
	}
	var found bool
	takeResult := func(n ir.Node) bool {
		if child != nil {
			if !affineInput(n, child) || found {
				return false
			}
		} else {
			c, ok := n.(*ir.CallExpr)
			if !ok || found {
				return false
			}
			call = c
		}
		found = true
		return true
	}
	if !appendInvariantSum(&invariant.step, ret.Results[0], params, 0, takeResult) ||
		!found || call.Op() != ir.OCALLFUNC || len(call.Args) != sig.NumParams() {
		return nil
	}
	for i, arg := range call.Args {
		name := sig.Param(i).Nname.(*ir.Name)
		if i+1 != invariant.depth {
			if !affineInput(arg, name) {
				return nil
			}
			continue
		}
		sub, ok := arg.(*ir.BinaryExpr)
		if !ok || sub.Op() != ir.OSUB || !affineInput(sub.X, name) {
			return nil
		}
		if one, ok := affineLiteral(sub.Y); !ok || one != 1 {
			return nil
		}
	}
	targetName := ir.StaticCalleeName(call.Fun)
	if targetName == nil {
		return nil
	}
	target, _ := targetName.Defn.(*ir.Func)
	if target == nil {
		return nil
	}
	return &affineReturnPlan{input: depth, target: target, yield: yield, source: f, invariant: invariant}
}

func appendInvariantSum(terms *[]invariantReturnTerm, n ir.Node, params map[*ir.Name]int,
	condition int, takeResult func(ir.Node) bool) bool {
	if n.Type() != types.Types[types.TINT] {
		return false
	}
	if add, ok := n.(*ir.BinaryExpr); ok && add.Op() == ir.OADD {
		return appendInvariantSum(terms, add.X, params, condition, takeResult) &&
			appendInvariantSum(terms, add.Y, params, condition, takeResult)
	}
	if takeResult != nil && takeResult(n) {
		return true
	}
	term := invariantReturnTerm{condition: condition}
	if value, ok := affineLiteral(n); ok {
		term.literal = value
	} else {
		if load, ok := n.(*ir.StarExpr); ok && load.Op() == ir.ODEREF {
			term.deref = true
			n = load.X
		}
		name, ok := n.(*ir.Name)
		if !ok || params[name.Canonical()] == 0 {
			return false
		}
		term.param = params[name.Canonical()]
	}
	*terms = append(*terms, term)
	return true
}

func appendInvariantAdd(terms *[]invariantReturnTerm, stmt ir.Node, child *ir.Name, params map[*ir.Name]int) bool {
	condition := 0
	if branch, ok := stmt.(*ir.IfStmt); ok {
		test, ok := branch.Cond.(*ir.Name)
		if !ok || test.Type() != types.Types[types.TBOOL] || params[test.Canonical()] == 0 ||
			len(branch.Body) != 1 || len(branch.Else) != 0 {
			return false
		}
		condition, stmt = params[test.Canonical()], branch.Body[0]
	}
	add, ok := stmt.(*ir.AssignOpStmt)
	return ok && add.AsOp == ir.OADD && affineInput(add.X, child) &&
		appendInvariantSum(terms, add.Y, params, condition, nil)
}

func sameInvariantReturn(p, q *affineReturnPlan) bool {
	a, b := p.invariant, q.invariant
	if a == nil || b == nil {
		return a == b
	}
	return a.depth == b.depth && slices.Equal(a.base, b.base) && slices.Equal(a.step, b.step)
}

func applyInvariantReturn(plan *Plan, old *lowerCandidate, p *affineReturnPlan) *lowerCandidate {
	fn := old.function.Func
	invariant := p.invariant
	pos, integer := fn.Pos(), types.Types[types.TINT]
	param := func(i int) ir.Node { return fn.Type().Param(i - 1).Nname.(*ir.Name) }
	zero := func() ir.Node { return typedInt(pos, integer, 0) }
	original, declarations := fn.Body, fn.Dcl
	yield := ir.Copy(p.yield).(*ir.CallExpr)
	var checks ir.Node = ir.NewBinaryExpr(pos, ir.OGE, p.input, zero())
	checked := make(map[int]bool)
	for _, term := range slices.Concat(invariant.base, invariant.step) {
		if term.deref && !checked[term.param] {
			checked[term.param] = true
			p := param(term.param)
			nonnull := ir.NewBinaryExpr(pos, ir.ONE, p, ir.NewNilExpr(pos, p.Type()))
			checks = ir.NewLogicalExpr(pos, ir.OANDAND, checks, nonnull)
		}
	}

	// All loads remain after the yield. The original unwind has no intervening
	// synchronization or stores, so its successful non-atomic int loads can
	// be reused. Leave nil dereferences on the original path at their source
	// positions. At depth zero only the base's operands are evaluated.
	value := func(term invariantReturnTerm) ir.Node {
		if term.param == 0 {
			return typedInt(pos, integer, term.literal)
		}
		n := param(term.param)
		if term.deref {
			n = typedDeref(pos, n, integer)
		}
		return n
	}
	var sum ir.Node = zero()
	for _, term := range invariant.base {
		sum = ir.NewBinaryExpr(pos, ir.OADD, sum, value(term))
	}
	baseValue := typecheck.TempAt(pos, fn, integer)
	step := typecheck.TempAt(pos, fn, integer)
	positive := ir.Nodes{ir.NewDecl(pos, ir.ODCL, step), ir.NewAssignStmt(pos, step, zero())}
	for _, term := range invariant.step {
		var add ir.Node = ir.NewAssignOpStmt(pos, ir.OADD, step, value(term))
		if term.condition != 0 {
			add = ir.NewIfStmt(pos, param(term.condition), ir.Nodes{add}, nil)
		}
		positive.Append(add)
	}
	product := ir.NewBinaryExpr(pos, ir.OMUL, p.input, step)
	positive.Append(ir.NewReturnStmt(pos, ir.Nodes{ir.NewBinaryExpr(pos, ir.OADD, baseValue, product)}))
	fast := ir.NewIfStmt(pos, checks, ir.Nodes{
		yield,
		ir.NewDecl(pos, ir.ODCL, baseValue),
		ir.NewAssignStmt(pos, baseValue, sum),
		ir.NewIfStmt(pos, ir.NewBinaryExpr(pos, ir.OGT, p.input, zero()), positive, nil),
		ir.NewReturnStmt(pos, ir.Nodes{baseValue}),
	}, nil)
	ir.WithFunc(fn, func() { typecheck.Stmt(fast) })
	fn.Body = append(ir.Nodes{fast}, original...)

	f := *old.function
	id := SiteID(1)
	for _, site := range f.Sites {
		id = max(id, site.ID+1)
	}
	f.Sites = append(slices.Clone(f.Sites), Site{ID: id, Kind: SiteYield, Node: yield})
	for _, edge := range f.Edges {
		if edge.Node == p.yield {
			edge.Node = yield
			f.Edges = append(slices.Clone(f.Edges), edge)
			break
		}
	}
	candidate, err := newLowerCandidate(plan, &f)
	if err != nil {
		fn.Body, fn.Dcl = original, declarations
		return old
	}
	candidate.ordinary = old.ordinary
	candidate.heapLocals = old.heapLocals
	return candidate
}
