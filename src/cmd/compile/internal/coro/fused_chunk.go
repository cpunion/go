// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import "cmd/compile/internal/ir"

type fusedChunkProof struct {
	literal ir.Node
	callee  *ir.Func
	factory *ir.CallExpr
	await   *ir.BlockStmt
	resume  *ir.Func
}

// chunkFactoryLocals accepts only implicit zero values and constant assignments.
// These locals can be substituted into the identical stores for a reused slot.
func chunkFactoryLocals(prologue ir.Nodes) map[*ir.Name]ir.Node {
	locals := make(map[*ir.Name]ir.Node)
	for _, stmt := range prologue {
		switch stmt := stmt.(type) {
		case *ir.Decl:
			if stmt.X.Class != ir.PAUTO || len(stmt.Init()) != 0 {
				return nil
			}
			locals[stmt.X] = nil
		case *ir.AssignStmt:
			name, ok := stmt.X.(*ir.Name)
			_, declared := locals[name]
			_, literal := stmt.Y.(*ir.BasicLit)
			if !ok || !declared || (!literal && stmt.Y != nil && !ir.IsZero(stmt.Y)) ||
				ir.Any(stmt, func(n ir.Node) bool { return len(n.Init()) != 0 }) {
				return nil
			}
			locals[name] = stmt.Y
		default:
			return nil
		}
	}
	return locals
}

func chunkFactoryStoresKnown(factory *ir.Func, oldFrame, oldZero *ir.Name,
	locals map[*ir.Name]ir.Node, initializers ir.Nodes) bool {
	known := func(name *ir.Name) bool {
		if name == oldFrame || name == oldZero {
			return true
		}
		if _, ok := locals[name]; ok {
			return true
		}
		for _, field := range factory.Type().Params() {
			if name == field.Nname {
				return true
			}
		}
		return false
	}
	for _, stmt := range initializers {
		if ir.Any(stmt, func(n ir.Node) bool {
			name, ok := n.(*ir.Name)
			return ok && !known(name)
		}) {
			return false
		}
	}
	return true
}

func copyChunkFactoryStores(initializers ir.Nodes, bindings map[*ir.Name]ir.Node) ir.Nodes {
	var copyStore func(ir.Node) ir.Node
	copyStore = func(n ir.Node) ir.Node {
		if name, ok := n.(*ir.Name); ok {
			if replacement := bindings[name]; replacement != nil {
				return ir.DeepCopy(name.Pos(), replacement)
			}
			return n
		}
		copy := ir.Copy(n)
		ir.EditChildren(copy, copyStore)
		return copy
	}
	stores := make(ir.Nodes, len(initializers))
	for i, stmt := range initializers {
		stores[i] = copyStore(stmt)
	}
	return stores
}
