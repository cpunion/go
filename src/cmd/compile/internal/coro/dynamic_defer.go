// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/ir"
	"cmd/compile/internal/typecheck"
	"cmd/compile/internal/types"
	"cmd/internal/obj"
	"fmt"
	"slices"
)

// prepareDynamicDeferWrapper runs only after all lowering dependencies have
// been admitted, before collecting factory locals. A normalized func() defer
// needs a private wrapper to dispatch its frozen callee during cleanup.
func prepareDynamicDeferWrapper(deferred *lowerDefer, parent *ir.Func) {
	if _, wrapped := deferred.call.Fun.(*ir.ClosureExpr); wrapped {
		return
	}
	pos := deferred.statement.Pos()
	value := typecheck.TempAt(pos, parent, deferred.call.Fun.Type())
	deferred.statement.PtrInit().Append(ir.NewDecl(pos, ir.ODCL, value),
		ir.NewAssignStmt(pos, value, deferred.call.Fun))
	wrapper := ir.NewClosureFunc(pos, pos, ir.ODEFER,
		types.NewSignature(nil, nil, nil), parent, typecheck.Target, 0)
	wrapper.DeclareParams(true)
	wrapper.SetWrapper(true)
	fun := ir.NewClosureVar(pos, wrapper, value)
	wrapper.Body = ir.Nodes{typecheck.Call(pos, fun, nil, false)}
	deferred.call.Fun = wrapper.OClosure
	deferred.snapshotCallee = false
}

// rewriteDynamicDeferCall dispatches at cleanup, after registration froze all
// operands. Each selected entry has its own validated defer contract. In
// particular an ordinary call cannot substitute for a terminal factory, and
// the task scope used by a plain direct recover cannot admit indirect recover.
func rewriteDynamicDeferCall(deferred *lowerDefer, token *ir.Name,
	factories map[*ir.Func]*ir.Func) error {
	closure, ok := deferred.call.Fun.(*ir.ClosureExpr)
	if !ok || len(closure.Func.Body) != 1 {
		return fmt.Errorf("dynamic terminal defer has no operand wrapper")
	}
	wrapper := closure.Func
	call, ok := wrapper.Body[0].(*ir.CallExpr)
	if !ok || call.Op() != ir.OCALLFUNC || len(call.Init()) != 0 {
		return fmt.Errorf("dynamic terminal defer wrapper has no frozen call")
	}
	pos := deferred.statement.Pos()
	var capturedToken *ir.Name
	pc := typecheck.TempAt(pos, wrapper, types.Types[types.TUINTPTR])
	body := ir.Nodes{
		ir.NewDecl(pos, ir.ODCL, pc),
		ir.NewAssignStmt(pos, pc, ir.FuncPC(pos,
			typecheck.Conv(call.Fun, types.Types[types.TINTER]), obj.ABIInternal)),
	}
	var dispatch ir.Nodes
	for i := len(deferred.terminalTargets) - 1; i >= 0; i-- {
		entry := deferred.terminalTargets[i]
		target := entry.target
		args := slices.Clone(call.Args)
		var selected ir.Nodes
		if entry.factory {
			if capturedToken == nil {
				capturedToken = ir.NewClosureVar(pos, wrapper, token)
			}
			factory := factories[target]
			if factory == nil {
				return fmt.Errorf("dynamic terminal defer is missing factory %s", ir.PkgFuncName(target))
			}
			for _, result := range target.Type().Results() {
				temp := typecheck.TempAt(pos, wrapper, result.Type)
				selected = append(selected, ir.NewDecl(pos, ir.ODCL, temp))
				args = append(args, typecheck.NodAddrAt(pos, temp))
			}
			resume := typecheck.Call(pos, factory.Nname, args, false)
			selected = append(selected, typecheck.Call(pos,
				typecheck.LookupRuntime("coroDeferRun"), ir.Nodes{capturedToken, resume}, false))
		} else {
			selected = ir.Nodes{typecheck.Call(pos, target.Nname, args, target.Type().IsVariadic())}
		}
		if i == len(deferred.terminalTargets)-1 {
			// FuncPC checks nil only when cleanup invokes this wrapper.
			// The closed-set proof makes the final non-nil target exhaustive.
			dispatch = selected
			continue
		}
		targetPC := ir.FuncPC(pos, typecheck.Conv(target.Nname,
			types.Types[types.TINTER]), obj.ABIInternal)
		dispatch = ir.Nodes{ir.NewIfStmt(pos,
			ir.NewBinaryExpr(pos, ir.OEQ, pc, targetPC), selected, dispatch)}
	}
	wrapper.Body = append(body, dispatch...)
	ir.WithFunc(wrapper, func() { typecheck.Stmts(wrapper.Body) })
	return nil
}
