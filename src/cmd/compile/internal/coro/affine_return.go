// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/ir"
	"cmd/compile/internal/typecheck"
	"cmd/compile/internal/types"
	"go/constant"
)

// An affine return plan proves an exact scalar recurrence over machine int.
// Every function in the closed call graph must return the same base and
// add the same literal step after calling its target with n-1. The only
// source effect is the base-case Gosched, which remains a single yield.
// Helpers, pointers, closures, defers, spawns, floats and hidden Init effects
// are excluded; the ordinary fallback keeps its original recursive body.
type affineReturnPlan struct {
	input      *ir.Name
	target     *ir.Func
	condition  ir.Node
	yield      *ir.CallExpr
	base, step int64
	source     *Function
}

func affineLiteral(node ir.Node) (int64, bool) {
	lit, ok := node.(*ir.BasicLit)
	if !ok || lit.Val().Kind() != constant.Int {
		return 0, false
	}
	return constant.Int64Val(lit.Val())
}
func affineInput(node ir.Node, input *ir.Name) bool {
	name, ok := node.(*ir.Name)
	return ok && name.Canonical() == input.Canonical()
}
func planAffineReturn(f *Function) *affineReturnPlan {
	if f == nil || f.Func == nil || f.Primary != CoroPrimary {
		return nil
	}
	fn := f.Func
	sig := fn.Type()
	if fn.Pragma != 0 || fn.OClosure != nil || fn.Needctxt() || len(fn.ClosureVars) != 0 || sig.HasShape() || sig.NumParams() != 1 || len(sig.RecvParams()) != 1 || sig.NumResults() != 1 || sig.Param(0).Type != types.Types[types.TINT] || sig.Result(0).Type != types.Types[types.TINT] || len(fn.Body) != 2 {
		return nil
	}
	// Frontend expressions can carry effects in Init even when their visible
	// operation is a name, literal, comparison, or arithmetic expression.
	// The closed form is valid only when the entire source tree has none.
	for _, stmt := range fn.Body {
		if ir.Any(stmt, func(n ir.Node) bool { return len(n.Init()) != 0 }) {
			return nil
		}
	}
	input, ok := sig.Param(0).Nname.(*ir.Name)
	if !ok {
		return nil
	}
	branch, ok := fn.Body[0].(*ir.IfStmt)
	if !ok || len(branch.Init()) != 0 || len(branch.Else) != 0 || len(branch.Body) != 2 {
		return nil
	}
	condition, ok := branch.Cond.(*ir.BinaryExpr)
	if !ok || condition.Op() != ir.OLE || !affineInput(condition.X, input) {
		return nil
	}
	zero, ok := affineLiteral(condition.Y)
	if !ok || zero != 0 {
		return nil
	}
	yield, ok := branch.Body[0].(*ir.CallExpr)
	if !ok || yield.Op() != ir.OCALLFUNC || len(yield.Args) != 0 || len(yield.Init()) != 0 {
		return nil
	}
	name, ok := yield.Fun.(*ir.Name)
	if !ok || name.Sym() == nil || name.Sym().Pkg == nil || name.Sym().Pkg.Path != "runtime" || name.Sym().Name != "Gosched" {
		return nil
	}
	baseRet, ok := branch.Body[1].(*ir.ReturnStmt)
	if !ok || len(baseRet.Init()) != 0 || len(baseRet.Results) != 1 {
		return nil
	}
	base, ok := affineLiteral(baseRet.Results[0])
	if !ok {
		return nil
	}
	ret, ok := fn.Body[1].(*ir.ReturnStmt)
	if !ok || len(ret.Init()) != 0 || len(ret.Results) != 1 {
		return nil
	}
	add, ok := ret.Results[0].(*ir.BinaryExpr)
	if !ok || add.Op() != ir.OADD {
		return nil
	}
	call, ok := add.X.(*ir.CallExpr)
	step, integer := affineLiteral(add.Y)
	if !ok || !integer {
		call, ok = add.Y.(*ir.CallExpr)
		step, integer = affineLiteral(add.X)
	}
	if !ok || !integer || call.Op() != ir.OCALLFUNC || len(call.Init()) != 0 || len(call.Args) != 1 {
		return nil
	}
	targetName := ir.StaticCalleeName(call.Fun)
	if targetName == nil {
		return nil
	}
	target, _ := targetName.Defn.(*ir.Func)
	if target == nil {
		return nil
	}
	sub, ok := call.Args[0].(*ir.BinaryExpr)
	if !ok || sub.Op() != ir.OSUB || !affineInput(sub.X, input) {
		return nil
	}
	one, ok := affineLiteral(sub.Y)
	if !ok || one != 1 {
		return nil
	}
	return &affineReturnPlan{input: input, target: target, condition: condition, yield: yield, base: base, step: step, source: f}
}
func affineReturnClosed(p *affineReturnPlan, plans map[*ir.Func]*affineReturnPlan, candidates map[*ir.Func]*lowerCandidate) bool {
	seen := make(map[*ir.Func]bool)
	for fn := p.source.Func; !seen[fn]; {
		seen[fn] = true
		q := plans[fn]
		if q == nil || candidates[fn] == nil || q.base != p.base || q.step != p.step {
			return false
		}
		fn = q.target
	}
	return true
}
func applyAffineReturn(plan *Plan, old *lowerCandidate, p *affineReturnPlan) *lowerCandidate {
	if old.ordinary == nil {
		return old
	}
	fn := old.function.Func
	pos := fn.Pos()
	original := fn.Body
	positive := ir.Copy(p.yield).(*ir.CallExpr)
	baseReturn := ir.NewReturnStmt(pos, ir.Nodes{typedInt(pos, types.Types[types.TINT], p.base)})
	product := ir.NewBinaryExpr(pos, ir.OMUL, p.input, typedInt(pos, types.Types[types.TINT], p.step))
	product.SetType(types.Types[types.TINT])
	product.SetTypecheck(1)
	sum := ir.NewBinaryExpr(pos, ir.OADD, typedInt(pos, types.Types[types.TINT], p.base), product)
	sum.SetType(types.Types[types.TINT])
	sum.SetTypecheck(1)
	fn.Body = ir.Nodes{ir.NewIfStmt(pos, p.condition, ir.Nodes{p.yield, baseReturn}, nil), positive, ir.NewReturnStmt(pos, ir.Nodes{sum})}
	ir.WithFunc(fn, func() { typecheck.Stmts(fn.Body) })
	f := *old.function
	f.Recursive = false
	f.Sites = []Site{{ID: 1, Kind: SiteYield, Node: p.yield}, {ID: 2, Kind: SiteYield, Node: positive}}
	f.Edges = nil
	for _, edge := range old.function.Edges {
		if edge.Node == p.yield {
			f.Edges = append(f.Edges, edge)
			next := edge
			next.Node = positive
			f.Edges = append(f.Edges, next)
		}
	}
	candidate, err := newLowerCandidate(plan, &f)
	if err != nil {
		fn.Body = original
		return old
	}
	candidate.ordinary = old.ordinary
	candidate.heapLocals = old.heapLocals
	candidate.affineRoot = p
	return candidate
}

// A public entry runs on an ordinary Go stack. The exact single-yield proof
// lets it execute its Gosched and scalar result directly, avoiding a root
// scheduler and frame. Native callers still use the generated factory/resume;
// addOrdinaryBody subsequently installs the independent source fallback for
// locked-thread and synctest callers. This is an entry optimization, not an
// execution of a native continuation by the public caller.
func lowerAffineRootEntry(candidate *lowerCandidate) {
	p := candidate.affineRoot
	if p == nil || candidate.ordinary == nil || candidate.factoryABI != FactoryABI3 {
		return
	}
	fn := candidate.function.Func
	pos := fn.Pos()
	integer := types.Types[types.TINT]
	input := fn.Type().Param(0).Nname.(*ir.Name)
	literal := func(n int64) ir.Node { return typedInt(pos, integer, n) }
	yield := func() ir.Node { return ir.NewCallExpr(pos, ir.OCALLFUNC, p.yield.Fun, nil) }
	condition := ir.NewBinaryExpr(pos, ir.OLE, input, literal(0))
	product := ir.NewBinaryExpr(pos, ir.OMUL, input, literal(p.step))
	sum := ir.NewBinaryExpr(pos, ir.OADD, literal(p.base), product)
	fn.Body = ir.Nodes{
		ir.NewIfStmt(pos, condition, ir.Nodes{yield(), ir.NewReturnStmt(pos, ir.Nodes{literal(p.base)})}, nil),
		yield(), ir.NewReturnStmt(pos, ir.Nodes{sum}),
	}
	ir.WithFunc(fn, func() { typecheck.Stmts(fn.Body) })
}
