// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/ir"
	"cmd/compile/internal/typecheck"
	"cmd/compile/internal/types"
)

// captureParameterBody lifts a serialized closure body into a private static
// helper. Its explicit *T prefix preserves the selected source environment;
// ordinary fallback copies must perform the same conversion.
func captureParameterBody(body *ir.Func, captures []*types.Type) *ir.Func {
	if body == nil || len(body.ClosureVars) != len(captures) {
		return nil
	}
	vars := append([]*ir.Name(nil), body.ClosureVars...)
	params := make([]*types.Field, 0, len(captures)+body.Type().NumParams())
	cells := make(map[*ir.Name]*ir.Name)
	var declarations []*ir.Name
	for i, typ := range captures {
		field := types.NewField(body.Pos(), typecheck.LookupNum(".coro.capture", i), types.NewPtr(typ))
		name := ir.NewNameAt(body.Pos(), field.Sym, field.Type)
		name.Class = ir.PPARAM
		name.Curfn = body
		name.SetTypecheck(1)
		field.Nname = name
		params = append(params, field)
		declarations = append(declarations, name)
		cells[vars[i]] = name
	}
	params = append(params, body.Type().Params()...)
	body.Nname.SetType(types.NewSignature(nil, params, body.Type().Results()))
	body.Dcl = append(declarations, body.Dcl...)
	var edit func(ir.Node) ir.Node
	edit = func(node ir.Node) ir.Node {
		if name, ok := node.(*ir.Name); ok {
			if cell := cells[name]; cell != nil {
				return typedDeref(node.Pos(), cell, name.Type())
			}
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
	body.ClosureVars = nil
	body.SetNeedctxt(false)
	ir.WithFunc(body, func() { typecheck.Stmts(body.Body) })
	return body
}

// pinCapturedParameterCells makes the source closure use the same *T fields
// read by private dispatch. This runs before capture/escape analysis. Taking a
// capture's address does not evaluate user code or read its current value.
func pinCapturedParameterCells(source *ir.Func) {
	var prefix ir.Nodes
	ir.WithFunc(source, func() {
		for _, variable := range source.ClosureVars {
			variable.Canonical().SetAddrtaken(true)
			prefix = append(prefix, typecheck.Stmt(ir.NewAssignStmt(variable.Pos(), ir.BlankNode, typecheck.NodAddr(variable))))
		}
	})
	source.Body = append(prefix, source.Body...)
}

// rewriteCapturedParameterCall freezes the function value and every operand
// before any environment read. Nil calls therefore still evaluate operands,
// and distinct instances of one proved source target retain distinct cells.
func rewriteCapturedParameterCall(caller *ir.Func, call *ir.CallExpr, source, helper *ir.Func) {
	prefix := ir.TakeInit(call)
	freeze := func(value ir.Node) *ir.Name {
		name := typecheck.TempAt(value.Pos(), caller, value.Type())
		assign := ir.NewAssignStmt(value.Pos(), name, value)
		assign.SetInit(ir.Nodes{ir.NewDecl(value.Pos(), ir.ODCL, name)})
		name.Defn = assign
		prefix = append(prefix, typecheck.Stmt(assign))
		return name
	}
	ir.WithFunc(caller, func() {
		selected := freeze(call.Fun)
		args := make(ir.Nodes, len(call.Args))
		for i, arg := range call.Args {
			args[i] = freeze(arg)
		}
		call.Args = append(callableCaptureArguments(call.Pos(), source, selected), args...)
		call.Fun = helper.Nname
		call.SetInit(prefix)
	})
}
