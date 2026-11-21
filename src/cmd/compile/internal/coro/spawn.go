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

// directSpawnEntrySupported keeps the public source go statement when the
// caller has no continuation work of its own. The ordinary goroutine created
// by go enters the child's public entry, which still checks its native gate
// and execution context. Calls from an active native frame use the producer's
// existing factory.
func directSpawnEntrySupported(plan *Plan, candidates map[*ir.Func]*lowerCandidate,
	candidate *lowerCandidate) bool {
	fn := candidate.function
	if fn.Effect != NoSuspend || fn.Func.Type().NumResults() != 0 ||
		len(fn.Sites) != 1 || fn.Sites[0].Kind != SiteSpawn ||
		candidate.rangeOwner != nil || candidate.rangeBody != nil {
		return false
	}
	body := candidate.ordinary.Body
	if len(body) == 2 {
		if ret, ok := body[1].(*ir.ReturnStmt); ok && len(ret.Init()) == 0 && len(ret.Results) == 0 {
			body = body[:1]
		}
	}
	if len(body) != 1 || body[0].Op() != ir.OGO {
		return false
	}
	call, ok := fn.Sites[0].Node.(*ir.CallExpr)
	if !ok {
		return false
	}
	edge := edgeForCall(fn, call)
	if edge.Kind != GoCall || edge.Callee == nil || edge.ValueCall || len(edge.Targets) != 0 {
		return false
	}
	// Reading the source body also copies any local closure literal. Only
	// a named entry still refers to the same lowered child in that copy.
	if edge.Callee.OClosure != nil {
		return false
	}
	if child := candidates[edge.Callee]; child != nil {
		return child.function.Effect == MaySuspend
	}
	if plan.Functions[edge.Callee] != nil {
		return false
	}
	summary, known := plan.edgeSummary(edge)
	return known && summary.Primary() == CoroPrimary &&
		summary.Effect == MaySuspend && summary.Factory != NoFactory
}

// plainSpawnFactorySupported admits a static go target that can finish on the
// executor without yielding or requiring terminal-control rewriting. Until
// execution summaries describe bounded plain calls, reject calls and loops.
// The ordinary entry remains unchanged, including its inline body.
func plainSpawnFactorySupported(function *Function) bool {
	return function != nil && resumeFactorySupported(function.Func) && plainFactoryBodySupported(function)
}

// A normal callback has no direct-defer recover rights. Keep the existing
// bounded leaf requirements. Captured recover callbacks use a private helper
// with the source environment's traced pointer cells as explicit parameters.
// Transitive recover still needs a separate terminal-control proof.
func plainCallbackFactorySupported(function *Function) bool {
	if function == nil || !callableFactorySupported(function.Func) || function.LocalTerminal != function.Terminal ||
		!plainFactoryBodySupportedWithTerminal(function, UsesRecover) {
		return false
	}
	if resumeFactorySupported(function.Func) {
		return true
	}
	if function.Terminal != UsesRecover || !capturedFactorySupported(function.Func) {
		return false
	}
	// The ordinary helper call includes every capture cell, as well as the
	// original inputs and result destinations. Preserve the adapter's small
	// call bound; the linked machine proof still owns the actual stack cost.
	typ := function.Func.Type()
	types.CalcSize(types.NewFuncArgs(typ))
	return typ.ArgWidth()+int64(len(captureTypes(function.Func))+typ.NumResults())*int64(types.PtrSize) <= 128
}

// A captured plain callback cannot use the ordinary-call adapter: that adapter
// has no source closure environment. Its demanded private factory instead uses
// the regular typed capture lowering, without publishing a context-free ABI.
func capturedPlainFactorySupported(function *Function) bool {
	return function != nil && capturedFactorySupported(function.Func) && plainFactoryBodySupported(function)
}

func plainFactoryBodySupported(function *Function) bool {
	return plainFactoryBodySupportedWithTerminal(function, 0)
}

func plainFactoryBodySupportedWithTerminal(function *Function, allowed TerminalFlags) bool {
	if !(function != nil && function.Primary == PlainPrimary &&
		function.Effect == NoSuspend && function.Exec == 0 &&
		function.Terminal & ^allowed == 0 &&
		len(function.Func.Body) != 0 &&
		len(function.Edges) == 0) {
		return false
	}
	if typ := function.Func.Type(); typ.NumResults() != 0 {
		// The adapter needs call and result-copy temporaries. Keep
		// their source ABI footprint, plus the factory's result pointers,
		// small until larger values can bypass the ordinary return ABI.
		// This is not a bound on arbitrary source bodies or helper calls.
		const maxCallSize = 128
		types.CalcSize(types.NewFuncArgs(typ))
		if typ.ArgWidth()+int64(typ.NumResults())*int64(types.PtrSize) > maxCallSize {
			return false
		}
	}
	return !ir.Any(function.Func, func(node ir.Node) bool {
		switch node.Op() {
		case ir.OFOR, ir.ORANGE, ir.OGOTO:
			return true
		}
		return false
	})
}

// normalRecoverCallback supplies the normal-call variant of a bounded plain
// callback. Calling the public entry here would expose the enclosing cleanup's
// runtime recover bridge. A separate private copy makes its direct recover
// expressions nil, retaining all other source effects and the public entry's
// direct-defer behavior. Never rewrite shared source IR or publish this variant
// as a defer entry.
func normalRecoverCallback(fn *ir.Func, reader *BodyReader) *ir.Func {
	if reader == nil || reader.Clone == nil || !reader.Have(fn) {
		return nil
	}
	if capturedFactorySupported(fn) && (reader.PinCaptures == nil || !reader.PinCaptures(fn)) {
		return nil
	}
	copy := reader.Clone(fn, types.LocalPkg.Lookup(fn.Sym().Name+".coro.callback"))
	if copy == nil {
		return nil
	}
	// Keep the public closure entry and environment intact. Only this
	// private ordinary helper receives the capture pointers already decoded
	// by typed callback dispatch, in their original field order.
	cells := make(map[*ir.Name]*ir.Name)
	var fields []*types.Field
	var parameters []*ir.Name
	for i, variable := range copy.ClosureVars {
		pos := variable.Pos()
		parameter := ir.NewNameAt(pos, typecheck.LookupNum(".coro.capture", i), types.NewPtr(variable.Type()))
		parameter.Class, parameter.Curfn = ir.PPARAM, copy
		parameter.SetTypecheck(1)
		field := types.NewField(pos, parameter.Sym(), parameter.Type())
		field.Nname = parameter
		fields = append(fields, field)
		parameters = append(parameters, parameter)
		cells[variable] = parameter
	}
	if len(fields) != 0 {
		copy.Nname.SetType(types.NewSignature(nil, append(fields, copy.Type().Params()...), copy.Type().Results()))
		copy.Dcl = append(parameters, copy.Dcl...)
	}
	found := false
	var edit func(ir.Node) ir.Node
	edit = func(node ir.Node) ir.Node {
		if name, ok := node.(*ir.Name); ok && cells[name] != nil {
			return typedDeref(node.Pos(), cells[name], name.Type())
		}
		if closure, ok := node.(*ir.ClosureExpr); ok {
			for _, variable := range append([]*ir.Name(nil), closure.Func.ClosureVars...) {
				if cell := cells[variable.Outer]; cell != nil {
					replaceCapturedCell(closure.Func, variable, cell)
				}
			}
		}
		if node.Op() == ir.ORECOVER {
			found = true
			return ir.InitExpr(ir.TakeInit(node), ir.NewNilExpr(node.Pos(), node.Type()))
		}
		ir.EditChildren(node, edit)
		return node
	}
	for i, statement := range copy.Body {
		copy.Body[i] = edit(statement)
	}
	if !found {
		return nil
	}
	copy.ClosureVars = nil
	copy.SetNeedctxt(false)
	ir.WithFunc(copy, func() { typecheck.Stmts(copy.Body) })
	typecheck.Target.Funcs = append(typecheck.Target.Funcs, copy)
	return copy
}

// newPlainSpawnFactory adapts an ordinary entry to the existing closure factory
// ABI. Only locally demanded go targets get an adapter; ordinary calls do not
// enter a scheduler or allocate a resume closure.
func newPlainSpawnFactory(fn *ir.Func, callee ir.Node) *ir.Func {
	factory := newResumeFactory(fn, FactoryABI1)
	pos := fn.Pos()
	resume := ir.NewClosureFunc(pos, pos, ir.OCLOSURE, stacklessResumeType(),
		factory, typecheck.Target, 0)
	resume.DeclareParams(true)
	recordNativeStackEntry(resume, nativeStackGates[factory])
	recordSourceResume(resume, fn, nil, nil)
	inputs := len(captureTypes(fn)) + len(fn.Type().RecvParams())
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
