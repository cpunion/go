// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/ir"
	"cmd/compile/internal/reflectdata"
	"cmd/compile/internal/typecheck"
	"cmd/compile/internal/types"
)

type fusedChunkProof struct {
	literal ir.Node
	factory *ir.CallExpr
	await   *ir.BlockStmt
	resume  *ir.Func
}

// newFusedChunkFactory builds a local structured-await entry using the same
// typed stores as the original factory. The caller has already evaluated the
// arguments. A reservation miss reuses the original factory and await ABI.
func newFusedChunkFactory(candidate *lowerCandidate, resume *ir.Func,
	oldFrame, oldZero *ir.Name, prologue, initializers ir.Nodes) *ir.Func {
	locals := chunkFactoryLocals(prologue)
	if locals == nil || !chunkFactoryStoresKnown(candidate.factory, oldFrame, oldZero, locals, initializers) {
		return nil
	}
	source := candidate.function.Func
	factory := candidate.factory
	pos := source.Pos()
	params := make([]*types.Field, factory.Type().NumParams())
	for i, field := range factory.Type().Params() {
		params[i] = types.NewField(field.Pos, field.Sym, field.Type)
	}
	action := types.Types[types.TUINT8]
	t := types.NewSignature(nil, params, []*types.Field{types.NewField(pos, nil, action)})
	fast := ir.NewFunc(pos, pos, source.Sym().Pkg.Lookup(source.Sym().Name+".coro.chunk"), t)
	fast.SetDupok(source.Dupok())
	fast.Nname.Defn = fast
	fast.DeclareParams(true)
	ir.WithFunc(fast, func() {
		frame := typecheck.TempAt(pos, fast, oldFrame.Type())
		bindings := map[*ir.Name]ir.Node{oldFrame: frame}
		if oldZero != nil {
			bindings[oldZero] = ir.NewBool(pos, true)
		}
		for name, value := range locals {
			if value == nil {
				value = typecheck.Expr(ir.NewZero(name.Pos(), name.Type()))
			}
			bindings[name] = value
		}
		args := make(ir.Nodes, t.NumParams())
		for i := range args {
			name := t.Param(i).Nname.(*ir.Name)
			bindings[factory.Type().Param(i).Nname.(*ir.Name)] = name
			args[i] = name
		}
		ctx := args[0]
		unsafePointer := types.Types[types.TUNSAFEPTR]
		chunk := types.NewArray(candidate.frameType, candidate.frameChunkLength)
		take := typecheck.Call(pos, typecheck.LookupRuntime("coroPrepareCompatibleFusedChunk"),
			ir.Nodes{ctx, ir.Copy(resume.OClosure), typedInt(pos, types.Types[types.TUINTPTR], candidate.frameType.Size()),
				reflectdata.TypePtrAt(pos, chunk)}, false)
		fast.Body = ir.Nodes{
			ir.NewDecl(pos, ir.ODCL, frame),
			ir.NewAssignStmt(pos, frame, typecheck.ConvNop(take, frame.Type())),
		}
		reuse := copyChunkFactoryStores(initializers, bindings)
		enter := typecheck.Call(pos, typecheck.LookupRuntime("coroEnterCompatibleFusedChunk"),
			ir.Nodes{ctx, typecheck.ConvNop(frame, unsafePointer), ir.Copy(resume.OClosure)}, false)
		reuse = append(reuse, ir.NewReturnStmt(pos, ir.Nodes{enter}))
		found := ir.NewBinaryExpr(pos, ir.ONE, frame, ir.NewNilExpr(pos, frame.Type()))
		fast.Body = append(fast.Body, ir.NewIfStmt(pos, found, reuse, nil))

		coldFrame := typecheck.TempAt(pos, fast, unsafePointer)
		childResume := typecheck.TempAt(pos, fast, stacklessResumeType())
		request := typecheck.Call(pos, typecheck.LookupRuntime("coroRequestFusedFrame"),
			ir.Nodes{ctx, ir.NewBool(pos, true)}, false)
		cold := typecheck.Call(pos, factory.Nname, args, false)
		await := typecheck.Call(pos, typecheck.LookupRuntime("coroAwaitFusedFrame"),
			ir.Nodes{ctx, coldFrame, childResume}, false)
		fast.Body = append(fast.Body, request,
			ir.NewDecl(pos, ir.ODCL, coldFrame),
			ir.NewDecl(pos, ir.ODCL, childResume),
			ir.NewAssignListStmt(pos, ir.OAS2, ir.Nodes{coldFrame, childResume}, ir.Nodes{cold}),
			ir.NewReturnStmt(pos, ir.Nodes{await}))
		typecheck.Stmts(fast.Body)
	})
	typecheck.Target.Funcs = append(typecheck.Target.Funcs, fast)
	return fast
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
