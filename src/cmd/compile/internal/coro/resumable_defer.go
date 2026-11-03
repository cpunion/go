// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/ir"
	"cmd/compile/internal/typecheck"
	"cmd/compile/internal/types"
	"cmd/internal/obj"
	"cmd/internal/src"
	"fmt"
	"slices"
)

func (candidate *lowerCandidate) hasResumableDefers() bool {
	has := func(defers []*lowerDefer) bool {
		return slices.ContainsFunc(defers, func(d *lowerDefer) bool { return len(d.resumable) != 0 })
	}
	if has(candidate.defers) {
		return true
	}
	if candidate.rangeOwner != nil {
		for _, body := range candidate.rangeOwner.bodies {
			if has(body.cleanup.defers) {
				return true
			}
		}
	}
	return false
}

// Keep registrations in the existing traced func() stack. A private starter
// freezes the selected callee and operands, allocates its typed child only at
// invocation, and returns after handing the cleanup activation to the scheduler.
// Ordinary callback registrations use exactly the same enclosing owner token.
func prepareResumableDeferWrapper(deferred *lowerDefer, parent *ir.Func) {
	if closure, ok := deferred.call.Fun.(*ir.ClosureExpr); ok && closure.Func.Wrapper() {
		return
	}
	pos := deferred.statement.Pos()
	value := typecheck.TempAt(pos, parent, deferred.call.Fun.Type())
	deferred.statement.PtrInit().Append(ir.NewDecl(pos, ir.ODCL, value), ir.NewAssignStmt(pos, value, deferred.call.Fun))
	wrapper := ir.NewClosureFunc(pos, pos, ir.ODEFER, types.NewSignature(nil, nil, nil), parent, typecheck.Target, 0)
	wrapper.DeclareParams(true)
	wrapper.SetWrapper(true)
	fun := ir.NewClosureVar(pos, wrapper, value)
	wrapper.Body = ir.Nodes{typecheck.Call(pos, fun, nil, false)}
	deferred.call.Fun = wrapper.OClosure
	deferred.snapshotCallee = false
}

func rewriteResumableDeferCall(deferred *lowerDefer, token *ir.Name, factories map[*ir.Func]*ir.Func) error {
	closure, ok := deferred.call.Fun.(*ir.ClosureExpr)
	if !ok || !closure.Func.Wrapper() || len(closure.Func.Body) != 1 || token == nil {
		return fmt.Errorf("resumable defer has no frozen operand wrapper or owner")
	}
	wrapper := closure.Func
	call, ok := wrapper.Body[0].(*ir.CallExpr)
	if !ok || call.Op() != ir.OCALLFUNC || len(call.Init()) != 0 {
		return fmt.Errorf("resumable defer wrapper has no frozen call")
	}
	pos := deferred.statement.Pos()
	owner := ir.NewClosureVar(pos, wrapper, token)
	var prefix, dispatch ir.Nodes
	var pc *ir.Name
	if deferred.resumableDispatch {
		pc = typecheck.TempAt(pos, wrapper, types.Types[types.TUINTPTR])
		prefix = ir.Nodes{ir.NewDecl(pos, ir.ODCL, pc), ir.NewAssignStmt(pos, pc, ir.FuncPC(pos, typecheck.Conv(call.Fun, types.Types[types.TINTER]), obj.ABIInternal))}
	}
	for i := len(deferred.resumable) - 1; i >= 0; i-- {
		entry := deferred.resumable[i]
		target := entry.target
		args := slices.Clone(call.Args)
		var body ir.Nodes
		if entry.async {
			factory := factories[target]
			abi := resumeFactoryABI(factory, target)
			if abi == NoFactory {
				return fmt.Errorf("resumable defer is missing factory %s", ir.PkgFuncName(target))
			}
			var inputs ir.Nodes
			if abi == FactoryABI3 {
				inputs = append(inputs, ir.NewNilExpr(pos, types.Types[types.TUNSAFEPTR]))
			}
			if len(captureTypes(target)) != 0 {
				inputs = append(inputs, callableCaptureArguments(pos, target, call.Fun)...)
			}
			inputs = append(inputs, args...)
			for _, result := range target.Type().Results() {
				inputs = append(inputs, typedNew(pos, result.Type))
			}
			invoke := typecheck.Call(pos, factory.Nname, inputs, false)
			var frame ir.Node = ir.NewNilExpr(pos, types.Types[types.TUNSAFEPTR])
			var resume ir.Node = invoke
			if abi != FactoryABI1 {
				f := typecheck.TempAt(pos, wrapper, types.Types[types.TUNSAFEPTR])
				r := typecheck.TempAt(pos, wrapper, stacklessResumeType())
				body = append(body, ir.NewDecl(pos, ir.ODCL, f), ir.NewDecl(pos, ir.ODCL, r), ir.NewAssignListStmt(pos, ir.OAS2, ir.Nodes{f, r}, ir.Nodes{invoke}))
				frame, resume = f, r
			}
			body = append(body, typecheck.Call(pos, typecheck.LookupRuntime("coroDeferStartFrame"), ir.Nodes{owner, frame, resume}, false))
		} else if entry.terminal.factory {
			factory := factories[target]
			if factory == nil {
				return fmt.Errorf("mixed defer is missing terminal factory")
			}
			for _, result := range target.Type().Results() {
				args = append(args, typedNew(pos, result.Type))
			}
			body = append(body, typecheck.Call(pos, typecheck.LookupRuntime("coroDeferRun"), ir.Nodes{owner, typecheck.Call(pos, factory.Nname, args, false)}, false))
		} else {
			body = append(body, typecheck.Call(pos, target.Nname, args, target.Type().IsVariadic()))
		}
		if i == len(deferred.resumable)-1 {
			dispatch = body
			continue
		}
		targetPC := ir.FuncPC(pos, typecheck.Conv(target.Nname, types.Types[types.TINTER]), obj.ABIInternal)
		dispatch = ir.Nodes{ir.NewIfStmt(pos, ir.NewBinaryExpr(pos, ir.OEQ, pc, targetPC), body, dispatch)}
	}
	wrapper.Body = append(prefix, dispatch...)
	ir.WithFunc(wrapper, func() { typecheck.Stmts(wrapper.Body) })
	return nil
}

func resumableDeferWait(pos src.XPos, ctx ir.Node) ir.Node {
	return ir.NewIfStmt(pos, typecheck.Call(pos, typecheck.LookupRuntime("coroDeferSuspended"), ir.Nodes{ctx}, false), ir.Nodes{ir.NewReturnStmt(pos, ir.Nodes{typedInt(pos, types.Types[types.TUINT8], int64(actionWait))})}, nil)
}
