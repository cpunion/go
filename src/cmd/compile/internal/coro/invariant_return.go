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
// Each unwind step adds invariant terms or sums of the current depth.
type invariantReturn struct {
	depth      int
	base, step []invariantReturnTerm
}

// Parameter indices are one-based; zero denotes a literal or no condition.
type invariantReturnTerm struct {
	param, condition   int
	deref, integerCast bool
	depth              invariantDepth
	literal            int64
}

type invariantDepth uint8

const (
	invariantNoDepth invariantDepth = iota
	invariantFullDepth
	invariantByteDepth
	invariantDepthCount
)

type invariantReturnOperands struct {
	depth  *ir.Name
	params map[*ir.Name]int
	array  *rootArrayPlan
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
	body := fn.Body
	var array *rootArrayPlan
	if _, startsWithTest := body[0].(*ir.IfStmt); !startsWithTest {
		array = planFrameArray(f, 1)
		if array == nil || len(body) < 4 {
			return nil
		}
		body = body[2:]
	}
	// The only permitted Init is the declaration of the local that receives
	// the recursive result when the call stands in its own statement.
	assign, _ := body[1].(*ir.AssignStmt)
	for _, stmt := range body {
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
	branch, ok := body[0].(*ir.IfStmt)
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
	operands := invariantReturnOperands{depth: depth, params: make(map[*ir.Name]int), array: array}
	for i, field := range sig.Params() {
		name, ok := field.Nname.(*ir.Name)
		if !ok {
			return nil
		}
		if name.Canonical() == depth.Canonical() {
			invariant.depth = i + 1
			continue
		}
		operands.params[name.Canonical()] = i + 1
	}
	if invariant.depth == 0 || array != nil && !isInvariantDepthByte(array.write.Y, depth) {
		return nil
	}
	yield := sourceGosched(branch.Body[0])
	baseResult, ok := branch.Body[1].(*ir.ReturnStmt)
	if yield == nil || !ok || len(baseResult.Results) != 1 ||
		!operands.appendSum(&invariant.base, baseResult.Results[0], 0, nil) {
		return nil
	}
	ret, ok := body[len(body)-1].(*ir.ReturnStmt)
	if !ok || len(ret.Results) != 1 {
		return nil
	}
	var child *ir.Name
	var call *ir.CallExpr
	if len(body) > 2 {
		if assign == nil || !assign.Def {
			return nil
		}
		child, _ = assign.X.(*ir.Name)
		call, _ = assign.Y.(*ir.CallExpr)
		if child == nil || child.Class != ir.PAUTO || child.Type() != integer || call == nil {
			return nil
		}
		for _, stmt := range body[2 : len(body)-1] {
			if !operands.appendAdd(&invariant.step, stmt, child) {
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
	if !operands.appendSum(&invariant.step, ret.Results[0], 0, takeResult) ||
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

func invariantIntegerCast(n ir.Node) (*ir.ConvExpr, bool) {
	conv, ok := n.(*ir.ConvExpr)
	return conv, ok && (conv.Op() == ir.OCONV || conv.Op() == ir.OCONVNOP) &&
		conv.Type().IsInteger() && conv.X.Type().IsInteger()
}

func isInvariantDepthByte(n ir.Node, depth *ir.Name) bool {
	conv, ok := invariantIntegerCast(n)
	return ok && conv.Type().Kind() == types.TUINT8 && affineInput(conv.X, depth)
}

func (operands invariantReturnOperands) depthTerm(n ir.Node) invariantDepth {
	if affineInput(n, operands.depth) {
		return invariantFullDepth
	}
	if conv, ok := invariantIntegerCast(n); ok && isInvariantDepthByte(conv.X, operands.depth) {
		return invariantByteDepth
	}
	call, ok := n.(*ir.CallExpr)
	if !ok || operands.array == nil {
		return invariantNoDepth
	}
	leaf := operands.array.uses[call]
	if leaf == nil {
		return invariantNoDepth
	}
	// The array proof already establishes purity, a constant index, and an
	// in-bounds read from the private buffer with one byte initialized.
	result := leaf.Body[0].(*ir.ReturnStmt).Results[0]
	if conv, ok := invariantIntegerCast(result); ok {
		if index, ok := conv.X.(*ir.IndexExpr); ok && ir.Int64Val(index.Index) == operands.array.writeIndex {
			return invariantByteDepth
		}
	}
	return invariantNoDepth
}

func (operands invariantReturnOperands) appendSum(terms *[]invariantReturnTerm, n ir.Node,
	condition int, takeResult func(ir.Node) bool) bool {
	if n.Type() != types.Types[types.TINT] {
		return false
	}
	if add, ok := n.(*ir.BinaryExpr); ok && add.Op() == ir.OADD {
		return operands.appendSum(terms, add.X, condition, takeResult) &&
			operands.appendSum(terms, add.Y, condition, takeResult)
	}
	if depth := operands.depthTerm(n); depth != invariantNoDepth {
		*terms = append(*terms, invariantReturnTerm{depth: depth, condition: condition})
		return true
	}
	if takeResult != nil && takeResult(n) {
		return true
	}
	term := invariantReturnTerm{condition: condition}
	if value, ok := affineLiteral(n); ok {
		term.literal = value
	} else {
		if conv, ok := invariantIntegerCast(n); ok {
			term.integerCast = true
			n = conv.X
		}
		if load, ok := n.(*ir.StarExpr); ok && load.Op() == ir.ODEREF {
			term.deref = true
			n = load.X
		}
		name, ok := n.(*ir.Name)
		if !ok || operands.params[name.Canonical()] == 0 {
			return false
		}
		term.param = operands.params[name.Canonical()]
	}
	*terms = append(*terms, term)
	return true
}

func (operands invariantReturnOperands) appendAdd(terms *[]invariantReturnTerm, stmt ir.Node, child *ir.Name) bool {
	condition := 0
	if branch, ok := stmt.(*ir.IfStmt); ok {
		test, ok := branch.Cond.(*ir.Name)
		if !ok || test.Type() != types.Types[types.TBOOL] || operands.params[test.Canonical()] == 0 ||
			len(branch.Body) != 1 || len(branch.Else) != 0 {
			return false
		}
		condition, stmt = operands.params[test.Canonical()], branch.Body[0]
	}
	add, ok := stmt.(*ir.AssignOpStmt)
	return ok && add.AsOp == ir.OADD && affineInput(add.X, child) &&
		operands.appendSum(terms, add.Y, condition, nil)
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
			n = typedDeref(pos, n, n.Type().Elem())
		}
		if term.integerCast {
			n = ir.NewConvExpr(pos, ir.OCONV, integer, n)
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
	addTerm := func(target *ir.Name, input ir.Node, condition int) {
		var add ir.Node = ir.NewAssignOpStmt(pos, ir.OADD, target, input)
		if condition != 0 {
			add = ir.NewIfStmt(pos, param(condition), ir.Nodes{add}, nil)
		}
		positive.Append(add)
	}
	var depthTerms []invariantReturnTerm
	for _, term := range invariant.step {
		if term.depth != invariantNoDepth {
			depthTerms = append(depthTerms, term)
		} else {
			addTerm(step, value(term), term.condition)
		}
	}
	product := ir.NewBinaryExpr(pos, ir.OMUL, p.input, step)
	var total ir.Node = ir.NewBinaryExpr(pos, ir.OADD, baseValue, product)
	if len(depthTerms) != 0 {
		result := typecheck.TempAt(pos, fn, integer)
		positive.Append(ir.NewDecl(pos, ir.ODCL, result), ir.NewAssignStmt(pos, result, total))
		var sums [invariantDepthCount]*ir.Name
		for _, term := range depthTerms {
			if sums[term.depth] == nil {
				sums[term.depth] = invariantDepthSum(fn, p.input, term.depth, &positive)
			}
			addTerm(result, sums[term.depth], term.condition)
		}
		total = result
	}
	positive.Append(ir.NewReturnStmt(pos, ir.Nodes{total}))
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

// invariantDepthSum emits the sum of a depth projection from one to n.
// The fast entry checks that n is nonnegative before reaching this code.
func invariantDepthSum(fn *ir.Func, depth *ir.Name, kind invariantDepth, body *ir.Nodes) *ir.Name {
	pos, integer := fn.Pos(), types.Types[types.TINT]
	literal := func(v int64) ir.Node { return typedInt(pos, integer, v) }
	binary := func(op ir.Op, left, right ir.Node) ir.Node { return ir.NewBinaryExpr(pos, op, left, right) }
	local := func(value ir.Node) *ir.Name {
		name := typecheck.TempAt(pos, fn, integer)
		body.Append(ir.NewDecl(pos, ir.ODCL, name), ir.NewAssignStmt(pos, name, value))
		return name
	}
	var value ir.Node
	switch kind {
	case invariantFullDepth:
		// Divide before multiplying to preserve the sum even when n+1
		// or n*(n+1) exceeds the machine int range.
		half := local(binary(ir.ORSH, depth, literal(1)))
		pairs := binary(ir.OMUL, half, binary(ir.OADD, depth, literal(1)))
		odd := binary(ir.OMUL, binary(ir.OAND, depth, literal(1)), binary(ir.OADD, half, literal(1)))
		value = binary(ir.OADD, pairs, odd)
	case invariantByteDepth:
		const period = 1 << 8
		cycles := binary(ir.ORSH, depth, literal(8))
		whole := binary(ir.OMUL, cycles, literal(period*(period-1)/2))
		remainder := local(binary(ir.OAND, depth, literal(period-1)))
		partial := binary(ir.OMUL, remainder, binary(ir.OADD, remainder, literal(1)))
		value = binary(ir.OADD, whole, binary(ir.ORSH, partial, literal(1)))
	}
	return local(value)
}
