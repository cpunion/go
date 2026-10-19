// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/ir"
	"cmd/compile/internal/typecheck"
	"cmd/compile/internal/types"
)

// PreserveGoCalls keeps an eligible plain target visible inside the frontend's
// go wrapper. Mark only that call: ordinary calls to the target may still inline.
// This must run before inlining, and only when native lowering is enabled.
func (p *Plan) PreserveGoCalls() {
	for _, function := range p.Functions {
		for _, edge := range function.Edges {
			if edge.Kind != GoCall || unwrapGoCall(edge.Node) == edge.Node {
				continue
			}
			child := p.Functions[edge.Callee]
			summary, known := p.edgeSummary(edge)
			if !plainSpawnFactorySupported(child) && !(known &&
				summary.Primary() == PlainPrimary && summary.Effect == NoSuspend &&
				summary.Exec == 0 && summary.Terminal == 0 && summary.Factory != NoFactory) {
				continue
			}
			wrapper := edge.Node.Fun.(*ir.ClosureExpr).Func
			wrapper.Body[0].(*ir.CallExpr).NoInline = true
		}
	}
}

// plainSpawnFactorySupported admits a static go target that can finish on the
// executor without yielding or requiring terminal-control rewriting. Until
// execution summaries describe bounded plain calls, reject calls and loops.
// The ordinary entry remains unchanged, including its inline body.
func plainSpawnFactorySupported(function *Function) bool {
	if !(function != nil && function.Primary == PlainPrimary &&
		function.Effect == NoSuspend && function.Exec == 0 &&
		function.Terminal == 0 && resumeFactorySupported(function.Func) &&
		len(function.Func.Body) != 0 &&
		len(function.Edges) == 0) {
		return false
	}
	return !ir.Any(function.Func, func(node ir.Node) bool {
		switch node.Op() {
		case ir.OFOR, ir.ORANGE, ir.OGOTO:
			return true
		}
		return false
	})
}

// newPlainSpawnFactory adapts an ordinary entry to the existing closure factory
// ABI. Only locally demanded go targets get an adapter; ordinary calls do not
// enter a scheduler or allocate a resume closure.
func newPlainSpawnFactory(fn *ir.Func, callee ir.Node) *ir.Func {
	factory := newResumeFactory(fn, FactoryABI1)
	pos := fn.Pos()
	resume := ir.NewClosureFunc(pos, pos, ir.OCLOSURE, stacklessResumeType(),
		factory, typecheck.Target, 0)
	resume.Pragma |= ir.Nosplit
	resume.DeclareParams(true)
	inputs := len(fn.Type().RecvParams())
	args := make(ir.Nodes, inputs)
	results := make(ir.Nodes, fn.Type().NumResults())
	for i, field := range factory.Type().Params() {
		parameter := field.Nname.(*ir.Name)
		capture := ir.NewClosureVar(pos, resume, parameter)
		if i < inputs {
			args[i] = capture
		} else {
			results[i-inputs] = ir.NewStarExpr(pos, capture)
		}
	}
	// An implicit panic unwinds to the runtime and reenters this resume for
	// terminal propagation. Never invoke the ordinary body a second time.
	ir.WithFunc(resume, func() {
		// Reuse the frontend's static callee representation, including a
		// method expression's explicit receiver and a variadic slice.
		call := typecheck.Call(pos, ir.DeepCopy(pos, callee), args, fn.Type().IsVariadic())
		var invoke ir.Node = call
		// Keep the factory ABI's result destinations meaningful, even though
		// a go statement discards them. They belong to the independent child.
		if len(results) == 1 {
			invoke = ir.NewAssignStmt(pos, results[0], call)
		} else if len(results) > 1 {
			invoke = ir.NewAssignListStmt(pos, ir.OAS2, results, ir.Nodes{call})
		}
		action := typecheck.TempAt(pos, resume, types.Types[types.TUINT8])
		query := typecheck.Call(pos, typecheck.LookupRuntime("coroTerminalAction"),
			ir.Nodes{resume.Dcl[0]}, false)
		pending := ir.NewBinaryExpr(pos, ir.ONE, action,
			typedInt(pos, types.Types[types.TUINT8], int64(actionInvalid)))
		resume.Body = ir.Nodes{
			ir.NewAssignStmt(pos, action, query),
			ir.NewIfStmt(pos, pending,
				ir.Nodes{ir.NewReturnStmt(pos, ir.Nodes{action})}, nil),
			invoke,
			ir.NewReturnStmt(pos, ir.Nodes{
				typedInt(pos, types.Types[types.TUINT8], int64(actionComplete)),
			}),
		}
		typecheck.Stmts(resume.Body)
	})
	factory.Body = ir.Nodes{ir.NewReturnStmt(pos, ir.Nodes{resume.OClosure})}
	ir.WithFunc(factory, func() { typecheck.Stmts(factory.Body) })
	return factory
}
