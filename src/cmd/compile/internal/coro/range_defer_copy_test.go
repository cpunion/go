// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/ir"
	"cmd/compile/internal/typecheck"
	"cmd/compile/internal/types"
	"cmd/internal/src"
	"testing"
)

func TestCopyRangeDeferBody(t *testing.T) {
	prepareLowerTest(t)
	oldTarget := typecheck.Target
	typecheck.Target = new(ir.Package)
	t.Cleanup(func() { typecheck.Target = oldTarget })
	pkg := types.NewPkg("example.com/coro/rangedefercopy", "p")
	integer := types.Types[types.TINT]
	originalParent := newLowerTestFunc(pkg, "original")
	ordinaryParent := newLowerTestFunc(pkg, "ordinary")
	originalCell := typecheck.TempAt(src.NoXPos, originalParent, integer)
	ordinaryCell := typecheck.TempAt(src.NoXPos, ordinaryParent, integer)
	source := ir.NewClosureFunc(src.NoXPos, src.NoXPos, ir.ODEFER,
		types.NewSignature(nil, nil, nil), originalParent, typecheck.Target, 0)
	source.DeclareParams(true)
	captured := ir.NewClosureVar(src.NoXPos, source, originalCell)
	local := typecheck.TempAt(src.NoXPos, source, integer)
	assign := ir.NewAssignStmt(src.NoXPos, local, captured)
	assign.Def = true
	local.Defn = assign
	child := ir.NewClosureFunc(src.NoXPos, src.NoXPos, ir.OCLOSURE,
		types.NewSignature(nil,
			[]*types.Field{types.NewField(src.NoXPos, pkg.Lookup("delta"), integer)},
			[]*types.Field{types.NewField(src.NoXPos, nil, integer)}),
		source, typecheck.Target, 0)
	child.DeclareParams(true)
	child.RangeParent = source
	childCell := ir.NewClosureVar(src.NoXPos, child, local)
	childOuter := ir.NewClosureVar(src.NoXPos, child, captured)
	child.Body = ir.Nodes{ir.NewReturnStmt(src.NoXPos, ir.Nodes{
		ir.NewBinaryExpr(src.NoXPos, ir.OADD, child.Dcl[0],
			ir.NewBinaryExpr(src.NoXPos, ir.OADD, childCell, childOuter)),
	})}
	childValue := typecheck.TempAt(src.NoXPos, source, child.Type())
	bindChild := ir.NewAssignStmt(src.NoXPos, childValue, child.OClosure)
	bindChild.Def = true
	childValue.Defn = bindChild
	panicNode := ir.NewUnaryExpr(src.NoXPos, ir.OPANIC,
		ir.NewNilExpr(src.NoXPos, types.Types[types.TINTER]))
	panicNode.SetTypecheck(1)
	source.Body = ir.Nodes{
		ir.NewDecl(src.NoXPos, ir.ODCL, local), assign,
		ir.NewDecl(src.NoXPos, ir.ODCL, childValue), bindChild, panicNode,
	}
	source.Parents = []ir.ScopeID{0}
	source.Label = 19
	originalParameter := child.Type().Param(0).Nname
	copy := copyRangeDeferBody(source, ordinaryParent, map[*ir.Name]*ir.Name{captured: ordinaryCell}).Func
	if source.Body[1] != assign || source.Body[4] != panicNode ||
		source.ClosureVars[0] != captured || captured.Outer != originalCell {
		t.Fatal("native copy modified ordinary source body or captures")
	}
	if copy == source || copy.ClosureParent != ordinaryParent || copy.Label != 19 ||
		copy.ClosureVars[0] == captured || copy.ClosureVars[0].Outer != ordinaryCell {
		t.Fatal("native copy did not bind independent enclosing operand cells")
	}
	newLocal := copy.Dcl[0]
	newAssign := copy.Body[1].(*ir.AssignStmt)
	if newLocal == local || newLocal.Curfn != copy || newLocal.Defn != newAssign ||
		newAssign.X != newLocal || newAssign.Y != copy.ClosureVars[0] || local.Defn != assign {
		t.Fatal("native/ordinary local declaration or defining assignment was shared")
	}
	newChild := copy.Body[3].(*ir.AssignStmt).Y.(*ir.ClosureExpr).Func
	if newChild == child || newChild.ClosureParent != copy || newChild.RangeParent != copy ||
		newChild.OClosure.Type() != newChild.Type() ||
		newChild.ClosureVars[0].Outer != newLocal || newChild.ClosureVars[1].Outer != copy.ClosureVars[0] ||
		newChild.Type().Param(0).Nname != newChild.Dcl[0] || child.Type().Param(0).Nname != originalParameter {
		t.Fatal("nested capture, parameter signature or range owner was not independently rebound")
	}
	for _, old := range []*ir.Name{local, captured, child.Dcl[0], child.Dcl[1], childCell, childOuter} {
		if ir.Any(copy, func(node ir.Node) bool { return node == old }) ||
			ir.Any(newChild, func(node ir.Node) bool { return node == old }) {
			t.Fatalf("native copy retained a declaration from the ordinary body: %v", old)
		}
	}
	// Terminal rewriting is the reason a second body is required. The original
	// still has its ordinary panic even after the native cleanup records the
	// logical outer activation's panic through its captured owner token.
	token := typecheck.TempAt(src.NoXPos, ordinaryParent, types.Types[types.TUNSAFEPTR])
	tokenCapture := ir.NewClosureVar(src.NoXPos, copy, token)
	if err := rewriteDeferTerminal(copy, tokenCapture, MayPanic); err != nil {
		t.Fatal(err)
	}
	if ir.Any(copy, func(node ir.Node) bool { return node.Op() == ir.OPANIC }) ||
		!ir.Any(source, func(node ir.Node) bool { return node == panicNode }) {
		t.Fatal("terminal rewrite crossed native/ordinary body ownership")
	}
	ir.WithFunc(newChild, func() { typecheck.Stmts(newChild.Body) })
	ir.WithFunc(copy, func() { typecheck.Stmts(copy.Body) })
	copy.Parents[0] = 7
	if source.Parents[0] != 0 {
		t.Fatal("source debug scope metadata was shared with native copy")
	}
}
