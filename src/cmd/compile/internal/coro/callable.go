// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/ir"
	"cmd/compile/internal/typecheck"
	"cmd/compile/internal/types"
	"cmd/internal/src"
)

// Captured source entries use private typed capture parameters before their
// source parameters. They are not exported as context-free factory entries.
// Keep their original types: lowering an enclosing function may subsequently
// represent one of these cells as an explicitly captured pointer.
var factoryCaptureTypes = make(map[*ir.Func][]*types.Type)

func capturedFactorySupported(fn *ir.Func) bool {
	if fn == nil || fn.OClosure == nil || fn.Wrapper() ||
		fn.Type() == nil || fn.Type().HasShape() || len(fn.ClosureVars) == 0 {
		return false
	}
	for _, variable := range fn.ClosureVars {
		if variable.Type() == nil || variable.Type().HasShape() ||
			variable.Canonical().Sym().Name == typecheck.LocalDictName {
			return false
		}
	}
	return true
}

func callableFactorySupported(fn *ir.Func) bool {
	return resumeFactorySupported(fn) || capturedFactorySupported(fn)
}

func captureTypes(fn *ir.Func) []*types.Type {
	if captures, ok := factoryCaptureTypes[fn]; ok {
		return captures
	}
	if !capturedFactorySupported(fn) {
		return nil
	}
	captures := make([]*types.Type, len(fn.ClosureVars))
	for i, variable := range fn.ClosureVars {
		captures[i] = variable.Type()
	}
	return captures
}

func sourceCaptureArguments(fn *ir.Func) ir.Nodes {
	var args ir.Nodes
	for _, variable := range fn.ClosureVars {
		// Taking the address also makes the frontend closure retain the same
		// shared cell. A native invocation never takes a snapshot of its value.
		variable.Canonical().SetAddrtaken(true)
		args = append(args, typecheck.NodAddr(variable))
	}
	return args
}

// callableCaptureArguments reads only the environment of a proved source
// closure, after the selected function value and all operands have been frozen.
// Its public entry takes each capture's address, so escape analysis gives every
// environment field the corresponding *T representation. The source function
// value and each resulting *T remain ordinary traced Go values.
func callableCaptureArguments(pos src.XPos, fn *ir.Func, value ir.Node) ir.Nodes {
	captures := captureTypes(fn)
	if len(captures) == 0 {
		return nil
	}
	fields := make([]*types.Field, len(captures)+1)
	fields[0] = types.NewField(pos, typecheck.Lookup("F"), types.Types[types.TUINTPTR])
	for i, typ := range captures {
		fields[i+1] = types.NewField(pos, typecheck.LookupNum("X", i), types.NewPtr(typ))
	}
	environment := types.NewStruct(fields)
	environment.SetNoalg(true)
	types.CalcSize(environment)
	data := ir.NewUnaryExpr(pos, ir.OIDATA, typecheck.Conv(value, types.Types[types.TINTER]))
	data.SetType(types.Types[types.TUNSAFEPTR])
	data.SetTypecheck(1)
	pointer := typecheck.ConvNop(data, types.NewPtr(environment))
	args := make(ir.Nodes, len(captures))
	for i := range captures {
		args[i] = explicitFrameField(pos, pointer, fields[i+1])
	}
	return args
}

// replaceCapturedCell passes a shared *T through a nested closure instead of
// asking that closure to capture the resume expression *cell. Preserve the
// original environment field position. The replacement pointer is captured by
// value, giving the same *T environment field as the original reference capture.
func replaceCapturedCell(fn *ir.Func, variable, cell *ir.Name) {
	index := -1
	for i, candidate := range fn.ClosureVars {
		if candidate == variable {
			index = i
			break
		}
	}
	if index < 0 {
		panic("missing captured cell")
	}
	pointer := ir.NewClosureVar(variable.Pos(), fn, cell)
	fn.ClosureVars[index] = pointer
	fn.ClosureVars = fn.ClosureVars[:len(fn.ClosureVars)-1]
	var edit func(ir.Node) ir.Node
	edit = func(node ir.Node) ir.Node {
		if node == variable {
			return typedDeref(node.Pos(), pointer, variable.Type())
		}
		if closure, ok := node.(*ir.ClosureExpr); ok {
			for _, child := range append([]*ir.Name(nil), closure.Func.ClosureVars...) {
				if child.Outer == variable {
					replaceCapturedCell(closure.Func, child, pointer)
				}
			}
		}
		ir.EditChildren(node, edit)
		return node
	}
	for i, statement := range fn.Body {
		fn.Body[i] = edit(statement)
	}
}
