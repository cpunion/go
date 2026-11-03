// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/ir"
	"cmd/compile/internal/typecheck"
	"cmd/compile/internal/types"
	"fmt"
)

// lowerOpenAwait freezes the source function and its arguments in Go order.
// A matching independently admitted companion starts an ordinary structured
// native await. Every other entry executes once on the logical goroutine's
// ordinary host, with the same frozen request and result destinations.
func lowerOpenAwait(call *ir.CallExpr, statement ir.Node, ctx ir.Node,
	resume *ir.Func, edit func(ir.Node) ir.Node) (ir.Nodes, ir.Node, error) {
	return lowerOpenCall(call, statement, ctx, resume, edit, false)
}

// A go statement has already frozen its operands in the source goroutine.
// Both native and opaque children are independently tracked logical tasks.
func lowerOpenSpawn(call *ir.CallExpr, ctx ir.Node, resume *ir.Func,
	edit func(ir.Node) ir.Node) (ir.Nodes, ir.Node, error) {
	return lowerOpenCall(unwrapGoCall(call), nil, ctx, resume, edit, true)
}

func lowerOpenCall(call *ir.CallExpr, statement ir.Node, ctx ir.Node,
	resume *ir.Func, edit func(ir.Node) ir.Node, spawn bool) (ir.Nodes, ir.Node, error) {
	if call.Op() != ir.OCALLFUNC || call.Fun.Type().Kind() != types.TFUNC {
		return nil, nil, fmt.Errorf("open function await has no function value")
	}
	pos := call.Pos()
	signature := callableTypeSignature(call.Fun.Type())
	requestType := callableRequestType(signature)
	// Discarded go results belong to the child, never the parent's locals.
	targets := make(ir.Nodes, signature.NumResults())
	if !spawn {
		var err error
		targets, err = callResultTargets(call, statement, signature.NumResults())
		if err != nil {
			return nil, nil, err
		}
	}
	if len(call.Args) != signature.NumParams() {
		return nil, nil, fmt.Errorf("open function argument list is not normalized")
	}
	var setup ir.Nodes
	freeze := func(value ir.Node) *ir.Name {
		temp := typecheck.TempAt(pos, resume, value.Type())
		setup = append(setup, ir.NewDecl(pos, ir.ODCL, temp), ir.NewAssignStmt(pos, temp, value))
		return temp
	}
	fun := freeze(edit(call.Fun))
	args := make(ir.Nodes, len(call.Args))
	for i, arg := range call.Args {
		args[i] = freeze(edit(arg))
	}
	request := freeze(typedNew(pos, requestType))
	fields := requestType.Fields()
	for i, arg := range args {
		setup = append(setup, ir.NewAssignStmt(pos, explicitFrameField(pos, request, fields[i]), arg))
	}
	for i, target := range targets {
		var pointer ir.Node
		if target == nil {
			pointer = typedNew(pos, signature.Result(i).Type)
		} else {
			pointer = typecheck.NodAddr(edit(target))
		}
		setup = append(setup, ir.NewAssignStmt(pos, explicitFrameField(pos, request, fields[len(args)+i]), pointer))
	}
	ptr := types.Types[types.TUNSAFEPTR]
	descriptor := typecheck.TempAt(pos, resume, ptr)
	environment := typecheck.TempAt(pos, resume, ptr)
	lookup := typecheck.Call(pos, typecheck.LookupRuntime("coroLookupFunction"),
		ir.Nodes{typecheck.Conv(typecheck.Conv(fun, signature), types.Types[types.TINTER])}, false)
	setup = append(setup,
		ir.NewDecl(pos, ir.ODCL, descriptor),
		ir.NewDecl(pos, ir.ODCL, environment),
		ir.NewAssignListStmt(pos, ir.OAS2, ir.Nodes{descriptor, environment}, ir.Nodes{lookup}))
	action := typecheck.TempAt(pos, resume, types.Types[types.TUINT8])
	frame := typecheck.TempAt(pos, resume, ptr)
	child := typecheck.TempAt(pos, resume, stacklessResumeType())
	setup = append(setup, ir.NewDecl(pos, ir.ODCL, action), ir.NewDecl(pos, ir.ODCL, frame), ir.NewDecl(pos, ir.ODCL, child))
	makeFrame := typecheck.Call(pos, typecheck.LookupRuntime("coroCallableFactory"),
		ir.Nodes{ctx, descriptor, environment, typecheck.ConvNop(request, ptr)}, false)
	native := ir.Nodes{
		ir.NewAssignListStmt(pos, ir.OAS2, ir.Nodes{frame, child}, ir.Nodes{makeFrame}),
		typecheck.Call(pos, typecheck.LookupRuntime("coroAwaitFrame"), ir.Nodes{ctx, frame, child}, false),
		ir.NewAssignStmt(pos, action, typedInt(pos, types.Types[types.TUINT8], int64(actionWait))),
	}

	spawnAction := func(call ir.Node) ir.Nodes {
		wantYield := typecheck.TempAt(pos, resume, types.Types[types.TBOOL])
		return ir.Nodes{
			ir.NewDecl(pos, ir.ODCL, wantYield),
			ir.NewAssignStmt(pos, wantYield, call),
			ir.NewAssignStmt(pos, action, typedInt(pos, types.Types[types.TUINT8], int64(actionInvalid))),
			ir.NewIfStmt(pos, wantYield, ir.Nodes{
				ir.NewAssignStmt(pos, action, typedInt(pos, types.Types[types.TUINT8], int64(actionYield))),
			}, nil),
		}
	}
	if spawn {
		native = append(ir.Nodes{
			ir.NewAssignListStmt(pos, ir.OAS2, ir.Nodes{frame, child}, ir.Nodes{makeFrame}),
		}, spawnAction(typecheck.Call(pos,
			typecheck.LookupRuntime("coroSpawnFrame"), ir.Nodes{ctx, frame, child}, false))...)
	}

	// The thunk is instantiated only in the fallback branch. Its captures
	// retain the typed request and original function environment across GC;
	// it never reevaluates an operand or restarts the enclosing source root.
	thunk := ir.NewClosureFunc(pos, pos, ir.OCLOSURE,
		types.NewSignature(nil, nil, nil), resume, typecheck.Target, 0)
	thunk.DeclareParams(true)
	recordSourceBridge(thunk)
	frozenFun := ir.NewClosureVar(pos, thunk, fun)
	frozenRequest := ir.NewClosureVar(pos, thunk, request)
	ir.WithFunc(thunk, func() {
		arguments := make(ir.Nodes, len(args))
		for i := range arguments {
			arguments[i] = explicitFrameField(pos, frozenRequest, fields[i])
		}
		invoke := typecheck.Call(pos, frozenFun, arguments, call.IsDDD)
		if len(targets) == 0 {
			thunk.Body = ir.Nodes{invoke}
		} else {
			destinations := make(ir.Nodes, len(targets))
			for i := range destinations {
				destinations[i] = ir.NewStarExpr(pos, explicitFrameField(pos, frozenRequest, fields[len(args)+i]))
			}
			thunk.Body = ir.Nodes{ir.NewAssignListStmt(pos, ir.OAS2, destinations, ir.Nodes{invoke})}
		}
		typecheck.Stmts(thunk.Body)
	})
	ordinaryName := "coroHostCall"
	if spawn {
		ordinaryName = "coroSpawnOpaque"
	}
	ordinary := typecheck.Call(pos, typecheck.LookupRuntime(ordinaryName), ir.Nodes{ctx, thunk.OClosure}, false)
	fallback := ir.Nodes{ir.NewAssignStmt(pos, action, ordinary)}
	if spawn {
		fallback = spawnAction(ordinary)
	}
	var dispatch ir.Node = ir.NewIfStmt(pos,
		ir.NewBinaryExpr(pos, ir.ONE, descriptor, ir.NewNilExpr(pos, ptr)), native, fallback)
	if spawn {
		// Execute the frontend's original nil-go startup on the ordinary
		// host. The registering task must return Wait before this thunk can
		// run; a raw GoStmt on the fixed executor has no stack proof.
		nilThunk := ir.NewClosureFunc(pos, pos, ir.OCLOSURE,
			types.NewSignature(nil, nil, nil), resume, typecheck.Target, 0)
		nilThunk.DeclareParams(true)
		recordSourceBridge(nilThunk)
		nilFun := ir.NewClosureVar(pos, nilThunk, fun)
		nilRequest := ir.NewClosureVar(pos, nilThunk, request)
		ir.WithFunc(nilThunk, func() {
			arguments := make(ir.Nodes, len(args))
			for i := range arguments {
				arguments[i] = explicitFrameField(pos, nilRequest, fields[i])
			}
			invoke := typecheck.Call(pos, nilFun, arguments, call.IsDDD)
			nilThunk.Body = ir.Nodes{typecheck.Stmt(ir.NewGoDeferStmt(pos, ir.OGO, invoke))}
		})
		nilStart := typecheck.Call(pos, typecheck.LookupRuntime("coroHostCall"), ir.Nodes{ctx, nilThunk.OClosure}, false)
		dispatch = ir.NewIfStmt(pos,
			ir.NewBinaryExpr(pos, ir.ONE, fun, ir.NewNilExpr(pos, fun.Type())),
			ir.Nodes{dispatch}, ir.Nodes{ir.NewAssignStmt(pos, action, nilStart)})
	}

	setup = append(setup, dispatch)
	return setup, action, nil
}
