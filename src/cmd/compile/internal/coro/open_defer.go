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

// Registration has already frozen every operand in the source activation.
// Keep that wrapper in the traced LIFO stack and allocate the invocation's
// typed request only when cleanup pops it. The runtime selects a separately
// proved direct-defer native entry or hands off to the same logical G's host.
func rewriteOpenDeferCall(deferred *lowerDefer, token *ir.Name) error {
	closure, ok := deferred.call.Fun.(*ir.ClosureExpr)
	if !ok || !closure.Func.Wrapper() || len(closure.Func.Body) != 1 || token == nil {
		return fmt.Errorf("open defer has no frozen wrapper or owner")
	}
	wrapper := closure.Func
	call, ok := wrapper.Body[0].(*ir.CallExpr)
	if !ok || !openCallableCall(call) || len(call.Init()) != 0 {
		return fmt.Errorf("open defer wrapper has no frozen function call")
	}
	pos := deferred.statement.Pos()
	signature := openCallableSignature(call)
	if len(call.Args) != signature.NumParams() {
		return fmt.Errorf("open defer arguments are not normalized")
	}
	requestType := callableRequestType(signature)
	request := typecheck.TempAt(pos, wrapper, types.NewPtr(requestType))
	fun := typecheck.TempAt(pos, wrapper, openCallableValue(call).Type())
	owner := ir.NewClosureVar(pos, wrapper, token)
	body := ir.Nodes{
		ir.NewDecl(pos, ir.ODCL, fun), ir.NewAssignStmt(pos, fun, openCallableValue(call)),
		ir.NewDecl(pos, ir.ODCL, request), ir.NewAssignStmt(pos, request, typedNew(pos, requestType)),
	}
	fields := requestType.Fields()
	for i, arg := range call.Args {
		body = append(body, ir.NewAssignStmt(pos, explicitFrameField(pos, request, fields[i]), arg))
	}
	for i, result := range signature.Results() {
		body = append(body, ir.NewAssignStmt(pos, explicitFrameField(pos, request, fields[len(call.Args)+i]), typedNew(pos, result.Type)))
	}
	params := []*types.Field{
		types.NewField(pos, typecheck.Lookup(".coropanic"), types.Types[types.TINTER]),
		types.NewField(pos, typecheck.Lookup(".coropanicking"), types.Types[types.TBOOL]),
		types.NewField(pos, typecheck.Lookup(".coroexiting"), types.Types[types.TBOOL]),
	}
	thunk := ir.NewClosureFunc(pos, pos, ir.OCLOSURE, types.NewSignature(nil, params, nil), wrapper, typecheck.Target, 0)
	thunk.DeclareParams(true)
	recordSourceBridge(thunk)
	frozenFun := ir.NewClosureVar(pos, thunk, fun)
	frozenRequest := ir.NewClosureVar(pos, thunk, request)
	ir.WithFunc(thunk, func() {
		args := make(ir.Nodes, len(call.Args))
		for i := range args {
			args[i] = explicitFrameField(pos, frozenRequest, fields[i])
		}
		invoke := openCallableInvoke(call, frozenFun, args)
		var incoming ir.Nodes
		for _, param := range thunk.Type().Params() {
			incoming = append(incoming, param.Nname.(*ir.Name))
		}
		// This must remain a direct defer of the source function. Calling
		// it inside another deferred closure would remove recover's right.
		thunk.Body = ir.Nodes{ir.NewGoDeferStmt(pos, ir.ODEFER, invoke),
			typecheck.Call(pos, typecheck.LookupRuntime("coroDeferInject"), incoming, false)}
		typecheck.Stmts(thunk.Body)
	})
	if call.Op() == ir.OCALLINTER {
		args := append(ir.Nodes{owner}, openInterfaceArguments(call, fun)...)
		args = append(args, typecheck.ConvNop(request, types.Types[types.TUNSAFEPTR]), thunk.OClosure)
		body = append(body, typecheck.Call(pos, typecheck.LookupRuntime("coroDeferStartInterface"), args, false))
	} else {
		boxed := typecheck.Conv(typecheck.Conv(fun, signature), types.Types[types.TINTER])
		body = append(body, typecheck.Call(pos, typecheck.LookupRuntime("coroDeferStartFunction"),
			ir.Nodes{owner, boxed, typecheck.ConvNop(request, types.Types[types.TUNSAFEPTR]), thunk.OClosure}, false))
	}
	wrapper.Body = body
	ir.WithFunc(wrapper, func() { typecheck.Stmts(wrapper.Body) })
	return nil
}
