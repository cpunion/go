// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/ir"
	"cmd/compile/internal/typecheck"
	"cmd/compile/internal/types"
)

// plainSpawnFactorySupported admits a static go target that can finish on the
// executor without yielding or requiring terminal-control rewriting. Its
// ordinary entry remains unchanged, including its inline body.
func plainSpawnFactorySupported(function *Function) bool {
	return function != nil && function.Primary == PlainPrimary &&
		function.Effect == NoSuspend && function.Exec == 0 &&
		function.Terminal == 0 && resumeFactorySupported(function.Func) &&
		function.Func.Type().NumResults() == 0 && len(function.Func.Body) != 0
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
	args := make(ir.Nodes, len(factory.Type().Params()))
	for i, field := range factory.Type().Params() {
		parameter := field.Nname.(*ir.Name)
		args[i] = ir.NewClosureVar(pos, resume, parameter)
	}
	// An implicit panic unwinds to the runtime and reenters this resume for
	// terminal propagation. Never invoke the ordinary body a second time.
	ir.WithFunc(resume, func() {
		// Reuse the frontend's static callee representation, including a
		// method expression's explicit receiver and a variadic slice.
		call := typecheck.Call(pos, ir.Copy(callee), args, fn.Type().IsVariadic())
		action := typecheck.TempAt(pos, resume, types.Types[types.TUINT8])
		query := typecheck.Call(pos, typecheck.LookupRuntime("coroTerminalAction"),
			ir.Nodes{resume.Dcl[0]}, false)
		pending := ir.NewBinaryExpr(pos, ir.ONE, action,
			typedInt(pos, types.Types[types.TUINT8], int64(actionInvalid)))
		resume.Body = ir.Nodes{
			ir.NewAssignStmt(pos, action, query),
			ir.NewIfStmt(pos, pending,
				ir.Nodes{ir.NewReturnStmt(pos, ir.Nodes{action})}, nil),
			call,
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
