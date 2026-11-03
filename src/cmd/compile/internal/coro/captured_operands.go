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
)

// capturedOperandCall records the actual statement and compiler-created
// wrapper. A range callback's independent ordinary source can lack that wrapper;
// its cleanup copy then binds the selected function to the ordinary frozen value.
type capturedOperandCall struct {
	source  *ir.Func
	wrapper *ir.Func
	created bool
}

// capturedOperandBody gives the selected function value its own explicit
// parameter. Its environment is read only when this activation starts, after
// registration/spawn. In particular a nil go callee must panic in the child and
// a nil deferred callee must remain legal until cleanup invokes it.
func capturedOperandBody(body *ir.Func, sourceType *types.Type, captures []*types.Type) *ir.Func {
	if body == nil || len(body.ClosureVars) != len(captures) {
		return nil
	}
	pos := body.Pos()
	selected := ir.NewNameAt(pos, typecheck.Lookup(".coro.selected"), sourceType)
	selected.Class, selected.Curfn = ir.PPARAM, body
	selected.SetTypecheck(1)
	field := types.NewField(pos, selected.Sym(), sourceType)
	field.Nname = selected
	params := append([]*types.Field{field}, body.Type().Params()...)
	body.Nname.SetType(types.NewSignature(nil, params, body.Type().Results()))
	body.Dcl = append([]*ir.Name{selected}, body.Dcl...)
	variables := append([]*ir.Name(nil), body.ClosureVars...)
	cells := make(map[*ir.Name]*ir.Name)
	var prefix ir.Nodes
	ir.WithFunc(body, func() {
		// This load retains the ordinary nil-function check even when every
		// captured value or every source result is subsequently dead.
		pc := typecheck.TempAt(pos, body, types.Types[types.TUINTPTR])
		prefix = append(prefix, ir.NewDecl(pos, ir.ODCL, pc), ir.NewAssignStmt(pos, pc,
			ir.FuncPC(pos, typecheck.Conv(selected, types.Types[types.TINTER]), obj.ABIInternal)))
		// captureTypes uses a source declaration. Here the clone still has
		// precisely the source captures, so record their stable types before
		// turning its body into a static helper.
		factoryCaptureTypes[body] = captures
		values := callableCaptureArguments(pos, body, selected)
		delete(factoryCaptureTypes, body)
		for i, variable := range variables {
			cell := typecheck.TempAt(variable.Pos(), body, types.NewPtr(captures[i]))
			cells[variable] = cell
			prefix = append(prefix, ir.NewDecl(variable.Pos(), ir.ODCL, cell), ir.NewAssignStmt(variable.Pos(), cell, values[i]))
		}
	})
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
		ir.EditChildren(node, edit)
		return node
	}
	for i, statement := range body.Body {
		body.Body[i] = edit(statement)
	}
	body.Body = append(prefix, body.Body...)
	body.ClosureVars = nil
	body.SetNeedctxt(false)
	ir.WithFunc(body, func() { typecheck.Stmts(body.Body) })
	return body
}

// specializeCapturedOperands lifts one proved local source callee at actual
// go/defer statements. The original function value and its operands stay in
// their frontend snapshots. The private helper runs the complete source body,
// including its results, direct recover, panic, Goexit and suspending work.
// Normal effect, factory, defer and machine-budget admission still apply.
func specializeCapturedOperands(funcs []*ir.Func, reader *BodyReader,
	bodies map[*ir.Func]func() *ir.Func) (map[*ir.Func]bool, map[*ir.GoDeferStmt]*capturedOperandCall) {
	rewritten := make(map[*ir.Func]bool)
	sites := make(map[*ir.GoDeferStmt]*capturedOperandCall)
	if reader == nil || reader.Clone == nil || reader.PinCaptures == nil {
		return rewritten, sites
	}
	var resolved map[*ir.Func]map[*ir.Name][]*ir.Func
	var expressions map[ir.Node][]*ir.Func
	helpers := make(map[*ir.Func]*ir.Func)
	generated := make(map[*ir.Func]bool)
	count := 0
	work := append([]*ir.Func(nil), funcs...)
	queued := make(map[*ir.Func]bool)
	for _, fn := range work {
		queued[fn] = true
	}
	for index := 0; index < len(work); index++ {
		caller := work[index]
		if !reader.Have(caller) && bodies[caller] == nil {
			continue
		}
		ir.Visit(caller, func(node ir.Node) {
			statement, ok := node.(*ir.GoDeferStmt)
			if !ok || statement.Op() != ir.OGO && statement.Op() != ir.ODEFER {
				return
			}
			call, ok := statement.Call.(*ir.CallExpr)
			if !ok || call.Op() != ir.OCALLFUNC {
				return
			}
			if resolved == nil {
				resolved, expressions = packageFunctionExpressionTargets(typecheck.Target.Funcs, nil)
			}
			inner := unwrapGoCall(call)
			var wrapper *ir.Func
			if inner != call {
				wrapper = call.Fun.(*ir.ClosureExpr).Func
			} else {
				// Direct source literals already have declaration-owned cleanup
				// and native child factories. Keep those existing entry contracts.
				if _, literal := call.Fun.(*ir.ClosureExpr); literal || len(call.Args) != 0 {
					return
				}
			}
			var source *ir.Func
			if name := ir.StaticCalleeName(ir.StaticValue(inner.Fun)); name != nil {
				source = name.Func
			} else if name, ok := inner.Fun.(*ir.Name); ok && len(resolved[caller][name]) == 1 {
				source = resolved[caller][name][0]
			} else if len(expressions[inner.Fun]) == 1 {
				source = expressions[inner.Fun][0]
			}
			if source == nil || generated[source] || !capturedFactorySupported(source) ||
				source.RangeParent != nil || source.Sym().Pkg != types.LocalPkg ||
				source.Dupok() || source.Wrapper() || source.ABIWrapper() || source.Type().NumRecvs() != 0 ||
				source.Pragma & ^ir.Noinline != 0 || !reader.Have(source) ||
				len(inner.Args) != source.Type().NumParams() || !smallParameterBody(source) {
				return
			}
			helper, known := helpers[source]
			if !known {
				if count >= 64 || !reader.PinCaptures(source) {
					return
				}
				count++
				captures := append([]*types.Type(nil), captureTypes(source)...)
				sig := source.Type()
				sym := types.LocalPkg.Lookup(fmt.Sprintf("%s.operand%d", source.Sym().Name, count))
				helper = capturedOperandBody(reader.Clone(source, sym), sig, captures)
				helpers[source] = helper
				if helper == nil {
					return
				}
				typecheck.Target.Funcs = append(typecheck.Target.Funcs, helper)
				generated[helper] = true
				bodies[helper] = func() *ir.Func {
					return capturedOperandBody(reader.Clone(source, types.LocalPkg.Lookup(sym.Name+".go")), sig, captures)
				}
				for _, fn := range typecheck.Target.Funcs {
					if !queued[fn] {
						queued[fn] = true
						work = append(work, fn)
					}
				}
			}
			if helper == nil {
				return
			}
			created := wrapper == nil
			if created {
				pos := statement.Pos()
				selected := typecheck.TempAt(pos, caller, call.Fun.Type())
				statement.PtrInit().Append(ir.TakeInit(call)...)
				freeze := ir.NewAssignStmt(pos, selected, call.Fun)
				selected.Defn = freeze
				statement.PtrInit().Append(ir.NewDecl(pos, ir.ODCL, selected), freeze)
				wrapper = ir.NewClosureFunc(pos, pos, statement.Op(), types.NewSignature(nil, nil, nil), caller, typecheck.Target, 0)
				wrapper.DeclareParams(true)
				wrapper.SetWrapper(true)
				frozen := ir.NewClosureVar(pos, wrapper, selected)
				wrapper.Body = ir.Nodes{typecheck.Call(pos, helper.Nname, ir.Nodes{frozen}, false)}
				call.Fun, call.Args = wrapper.OClosure, nil
				call.SetOp(ir.OCALL)
				call.SetType(nil)
				call.SetTypecheck(0)
				ir.WithFunc(caller, func() { typecheck.Expr(call) })
			} else {
				actual := wrapper.Body[0].(*ir.CallExpr)
				actual.Args = append(ir.Nodes{actual.Fun}, actual.Args...)
				actual.Fun = helper.Nname
				actual.SetOp(ir.OCALL)
				actual.SetType(nil)
				actual.SetTypecheck(0)
				ir.WithFunc(wrapper, func() { typecheck.Stmt(actual) })
			}
			wrapper.WrappedFunc = helper
			ir.WithFunc(caller, func() { typecheck.Stmts(statement.Init()) })
			sites[statement] = &capturedOperandCall{source: source, wrapper: wrapper, created: created}
			if reader.Have(caller) {
				rewritten[caller] = true
			}
			resolved = nil
		})
	}
	return rewritten, sites
}
