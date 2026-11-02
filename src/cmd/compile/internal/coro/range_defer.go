// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/ir"
	"cmd/compile/internal/types"
)

// rangeDeferAnchorDeclaration identifies the exact compiler-owned declaration.
// Recognizing it is not an ordinary-call recipe: its ordinary implementation
// registers a marker on the Go stack and cannot run on a native task stack.
func rangeDeferAnchorDeclaration(fn *ir.Func) bool {
	if fn == nil || fn.Nname == nil || fn.Nname.Sym() == nil ||
		ir.Pkgs.Runtime == nil || fn.Nname.Sym().Pkg != ir.Pkgs.Runtime ||
		fn.Nname.Sym().Name != "deferrangefunc" {
		return false
	}
	sig := fn.Type()
	return sig != nil && sig.Kind() == types.TFUNC && !sig.HasShape() &&
		sig.NumRecvs() == 0 && sig.NumParams() == 0 && sig.NumResults() == 1 &&
		!sig.IsVariadic() && sig.Result(0).Type.IsEmptyInterface()
}

// synchronousRangeIterator proves a narrower lifetime than a closed target set.
// Readonly callback parameters can still escape through a global assignment or
// an ordinary helper. Here every use must be a direct synchronous invocation.
// Native/ordinary interoperability needs a separate owner-anchor entry guard;
// this proof alone never grants a native range-defer recipe.
func synchronousRangeIterator(fn *ir.Func) bool {
	if fn == nil || fn.Type() == nil || fn.Type().Kind() != types.TFUNC {
		return false
	}
	parameters := make(map[*ir.Name]int)
	for _, field := range fn.Type().Params() {
		if field.Type.Kind() != types.TFUNC {
			continue
		}
		name, ok := field.Nname.(*ir.Name)
		if !ok || name.Addrtaken() {
			return false
		}
		parameters[name.Canonical()] = 0
	}
	if len(parameters) == 0 {
		return false
	}
	isParameter := func(name *ir.Name) bool {
		_, ok := parameters[name.Canonical()]
		return ok
	}
	valid := true
	var visit func(ir.Node)
	visit = func(node ir.Node) {
		if node == nil || !valid {
			return
		}
		switch node := node.(type) {
		case *ir.Name:
			if isParameter(node) {
				valid = false
			}
			return
		case *ir.ClosureExpr:
			for _, capture := range node.Func.ClosureVars {
				if isParameter(capture) {
					valid = false
					return
				}
			}
		case *ir.GoDeferStmt:
			// Frontend normalization usually moves the call into a closure.
			// Also reject a direct callback go/defer before normalization.
			if call, ok := node.Call.(*ir.CallExpr); ok {
				if name, ok := call.Fun.(*ir.Name); ok && isParameter(name) {
					valid = false
					return
				}
			}
		case *ir.CallExpr:
			if name, ok := node.Fun.(*ir.Name); ok && isParameter(name) {
				if node.Op() != ir.OCALLFUNC || node.GoDefer {
					valid = false
					return
				}
				parameters[name.Canonical()]++
				// Do not whitelist the Name object: a call can use that exact
				// same IR node again as an argument, where it would escape.
				for _, init := range node.Init() {
					visit(init)
				}
				for _, argument := range node.Args {
					visit(argument)
				}
				visit(node.DeferAt)
				for _, keep := range node.KeepAlive {
					visit(keep)
				}
				return
			}
		}
		ir.DoChildren(node, func(child ir.Node) bool {
			visit(child)
			return false
		})
	}
	for _, statement := range fn.Body {
		visit(statement)
	}
	for _, calls := range parameters {
		if calls == 0 {
			return false
		}
	}
	return valid
}
