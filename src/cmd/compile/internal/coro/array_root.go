// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/ir"
	"cmd/compile/internal/typecheck"
	"cmd/compile/internal/types"
	"go/constant"
)

// rootArrayPlan records a deliberately narrow reconstruction proof. It does
// not rewrite IR: the original zero and byte conversion must retain their
// evaluation points when a later lowering uses this proof.
type rootArrayPlan struct {
	array      *ir.Name
	zero       *ir.AssignStmt
	write      *ir.AssignStmt
	writeIndex int64
	uses       map[*ir.CallExpr]*ir.Func
}

// rootArrayLowering keeps a small traced frame for each invocation and one
// private array inside its physical root. Borrowed pointer fields use the
// existing clearing paths; root caches account for the complete allocation.
type rootArrayLowering struct {
	plan     *rootArrayPlan
	public   *ir.Func
	root     *ir.Name
	buffer   *ir.Name
	snapshot *ir.Name
}

func prepareRootArray(candidate *lowerCandidate) {
	if !candidate.localSelfFrameLoop() || candidate.selfSpawn {
		return
	}
	plan := planRootArray(candidate.function)
	if plan == nil {
		return
	}
	fn, public := candidate.function.Func, candidate.factory
	pos := fn.Pos()
	// Preserve the exported factory signature. A public factory always starts
	// a new lineage; testing ctx alone would incorrectly share storage across
	// independent goroutines spawned by another native coroutine.
	params := make([]*types.Field, 0, public.Type().NumParams()+1)
	for _, field := range public.Type().Params() {
		params = append(params, types.NewField(field.Pos, field.Sym, field.Type))
	}
	params = append(params, types.NewField(pos, typecheck.Lookup(".arrayRoot"), types.Types[types.TBOOL]))
	results := make([]*types.Field, 0, public.Type().NumResults())
	for _, field := range public.Type().Results() {
		results = append(results, types.NewField(field.Pos, nil, field.Type))
	}
	implementation := ir.NewFunc(pos, pos, fn.Sym().Pkg.Lookup(fn.Sym().Name+".coro.array"),
		types.NewSignature(nil, params, results))
	implementation.SetDupok(fn.Dupok())
	implementation.Nname.Defn = implementation
	implementation.DeclareParams(true)
	typecheck.Target.Funcs = append(typecheck.Target.Funcs, implementation)
	remap := make(map[*ir.Name]*ir.Name)
	var args ir.Nodes
	for i, field := range public.Type().Params() {
		old := field.Nname.(*ir.Name)
		remap[old] = implementation.Type().Param(i).Nname.(*ir.Name)
		args = append(args, old)
	}
	args = append(args, ir.NewBool(pos, true))
	ir.WithFunc(public, func() {
		public.Body = ir.Nodes{ir.NewReturnStmt(pos, ir.Nodes{
			typecheck.Call(pos, implementation.Nname, args, false),
		})}
		typecheck.Stmts(public.Body)
	})
	for source, name := range candidate.parameters {
		candidate.parameters[source] = remap[name]
	}
	for i, name := range candidate.resultPtrs {
		candidate.resultPtrs[i] = remap[name]
	}
	for _, name := range candidate.resultValues {
		name.Curfn = implementation
		implementation.Dcl = append(implementation.Dcl, name)
	}
	// Only result temporaries had been appended to the public factory's Dcl.
	// They now belong to the implementation; retain its public parameters.
	for i := 0; i < len(public.Dcl); {
		if public.Dcl[i].Curfn == implementation {
			public.Dcl = append(public.Dcl[:i], public.Dcl[i+1:]...)
		} else {
			i++
		}
	}
	candidate.factory = implementation
	root := implementation.Type().Param(len(params) - 1).Nname.(*ir.Name)
	buffer := typecheck.TempAt(pos, implementation, types.NewPtr(plan.array.Type()))
	snapshot := typecheck.TempAt(plan.write.Pos(), fn, types.Types[types.TUINT8])
	candidate.rootArray = &rootArrayLowering{plan, public, root, buffer, snapshot}

	// Keep the original byte conversion at its source evaluation point. The
	// original zero has no effects, and the array's only write is this one.
	// Fresh root storage is zero; restoring this byte before each proven leaf
	// read therefore reconstructs the complete original array without another
	// full-size zero. The leaf itself, including its full array type, is kept.
	plan.write.X = snapshot
	plan.write.SetInit(ir.Nodes{ir.NewDecl(snapshot.Pos(), ir.ODCL, snapshot)})
	fn.Body = fn.Body[1:]
	for call := range plan.uses {
		index := ir.NewIndexExpr(call.Pos(), buffer,
			typedInt(call.Pos(), types.Types[types.TINT], plan.writeIndex))
		call.Args[0] = buffer
		ir.WithFunc(fn, func() {
			call.SetInit(ir.Nodes{typecheck.Stmt(ir.NewAssignStmt(call.Pos(), index, snapshot))})
		})
	}
	for i, name := range fn.Dcl {
		if name.Canonical() == plan.array {
			fn.Dcl = append(fn.Dcl[:i], fn.Dcl[i+1:]...)
			break
		}
	}
}

// planRootArray recognizes an entry-zeroed byte array followed by one pure
// constant-index write. Its address can subsequently reach only a local leaf
// that reads constant elements and returns a scalar. In particular, a callee
// cannot publish or compare the array's identity, mutate it, or retain it.
// Sharing reconstructed storage along a serial self-await lineage needs this
// stronger property in addition to an ordinary escape-analysis result.
func planRootArray(function *Function) *rootArrayPlan {
	fn := function.Func
	if len(fn.Body) < 3 || len(fn.Type().RecvParams()) > 4 || fn.Type().NumResults() != 1 ||
		!fn.Type().Result(0).Type.IsInteger() {
		return nil
	}
	// Bound the extra private factory selector to register-sized arguments;
	// aggregate transfers retain the existing lowering and budget checks.
	for _, input := range fn.Type().RecvParams() {
		typ := input.Type
		types.CalcSize(typ)
		if typ.Size() > int64(types.PtrSize) || !(typ.IsInteger() || typ.IsBoolean() || typ.IsPtr() || typ.IsChan()) {
			return nil
		}
	}
	zero, ok := fn.Body[0].(*ir.AssignStmt)
	if !ok {
		return nil
	}
	array, ok := zero.X.(*ir.Name)
	if !ok || !entryZeroedFrameLocals(fn.Body)[array.Canonical()] ||
		array.Type().Elem().Kind() != types.TUINT8 {
		return nil
	}
	types.CalcSize(array.Type())
	if size := array.Type().Size(); size < 512 || size > 8<<10 {
		return nil
	}
	write, ok := fn.Body[1].(*ir.AssignStmt)
	if !ok || len(write.Init()) != 0 {
		return nil
	}
	index, ok := write.X.(*ir.IndexExpr)
	if !ok || index.X != array || len(index.Init()) != 0 ||
		!ir.IsConst(index.Index, constant.Int) || len(index.Index.Init()) != 0 ||
		!rootArrayInput(write.Y) {
		return nil
	}
	offset := ir.Int64Val(index.Index)
	if offset < 0 || offset >= array.Type().NumElem() {
		return nil
	}
	plan := &rootArrayPlan{array: array.Canonical(), zero: zero, write: write,
		writeIndex: offset, uses: make(map[*ir.CallExpr]*ir.Func)}
	var inspect func(ir.Node) bool
	inspect = func(node ir.Node) bool {
		if node == zero || node == write {
			return true
		}
		switch node.Op() {
		case ir.OCLOSURE, ir.OGO, ir.ODEFER:
			return false
		}
		if name, ok := node.(*ir.Name); ok && name.Canonical() == plan.array {
			return false
		}
		if call, ok := node.(*ir.CallExpr); ok && len(call.Init()) == 0 && len(call.Args) == 1 {
			address, ok := call.Args[0].(*ir.AddrExpr)
			if ok && address.X == array && len(address.Init()) == 0 {
				callee := edgeForCall(function, call).Callee
				if !rootArrayLeaf(callee, array.Type()) || ir.StaticCalleeName(call.Fun) != callee.Nname ||
					ir.Any(call.Fun, func(n ir.Node) bool { return len(n.Init()) != 0 }) {
					return false
				}
				plan.uses[call] = callee
				return true
			}
		}
		return !ir.DoChildren(node, func(child ir.Node) bool { return !inspect(child) })
	}
	for _, node := range fn.Body {
		if !inspect(node) {
			return nil
		}
	}
	if len(plan.uses) == 0 {
		return nil
	}
	return plan
}

func rootArrayInput(node ir.Node) bool {
	if node == nil || len(node.Init()) != 0 {
		return false
	}
	if ir.IsConst(node, constant.Int) {
		return true
	}
	switch node := node.(type) {
	case *ir.Name:
		return node.Class == ir.PPARAM && node.Type().IsInteger()
	case *ir.ConvExpr:
		return (node.Op() == ir.OCONV || node.Op() == ir.OCONVNOP) &&
			node.Type().IsInteger() && rootArrayInput(node.X)
	case *ir.BinaryExpr:
		return rootArrayArithmetic(node.Op()) && rootArrayInput(node.X) && rootArrayInput(node.Y)
	}
	return false
}

func rootArrayArithmetic(op ir.Op) bool {
	switch op {
	case ir.OADD, ir.OSUB, ir.OMUL, ir.OAND, ir.OOR, ir.OXOR, ir.OANDNOT:
		return true
	}
	return false
}

func rootArrayLeaf(fn *ir.Func, array *types.Type) bool {
	if fn == nil || len(fn.Body) != 1 || fn.Type().NumRecvs() != 0 ||
		fn.Type().NumParams() != 1 || fn.Type().NumResults() != 1 ||
		!fn.Type().Result(0).Type.IsInteger() {
		return false
	}
	input := fn.Type().Param(0)
	if !input.Type.IsPtr() || !types.Identical(input.Type.Elem(), array) {
		return false
	}
	parameter, ok := input.Nname.(*ir.Name)
	if !ok {
		return false
	}
	ret, ok := fn.Body[0].(*ir.ReturnStmt)
	if !ok || len(ret.Init()) != 0 || len(ret.Results) != 1 {
		return false
	}
	var value func(ir.Node) bool
	value = func(node ir.Node) bool {
		if node == nil || len(node.Init()) != 0 {
			return false
		}
		if ir.IsConst(node, constant.Int) {
			return true
		}
		switch node := node.(type) {
		case *ir.ConvExpr:
			return (node.Op() == ir.OCONV || node.Op() == ir.OCONVNOP) &&
				node.Type().IsInteger() && value(node.X)
		case *ir.BinaryExpr:
			return rootArrayArithmetic(node.Op()) && value(node.X) && value(node.Y)
		case *ir.IndexExpr:
			base := node.X
			if dereference, ok := base.(*ir.StarExpr); ok && len(dereference.Init()) == 0 {
				base = dereference.X
			}
			name, ok := base.(*ir.Name)
			return ok && name.Canonical() == parameter.Canonical() &&
				ir.IsConst(node.Index, constant.Int) && ir.Int64Val(node.Index) >= 0 &&
				len(node.Index.Init()) == 0 && ir.Int64Val(node.Index) < array.NumElem()
		}
		return false
	}
	return value(ret.Results[0])
}
