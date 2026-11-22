// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/base"
	"cmd/compile/internal/ir"
	"cmd/compile/internal/reflectdata"
	"cmd/compile/internal/typecheck"
	"cmd/compile/internal/types"
	"go/constant"
	"internal/abi"
)

type zeroSpawnFactory struct {
	resume     *ir.Func
	typ        *types.Type
	allocation *types.Type
	offset     int64
	pc         int64
}

type spawnBatchSite struct {
	block        *ir.BlockStmt
	spawn        ir.Node
	resume       *ir.Func
	callee       *ir.Func
	index, limit ir.Node
	period       int64
	offset       ir.Node
}

// newZeroSpawnFactory admits local factories whose frame
// needs only zero initialization and an initial resume PC.
func newZeroSpawnFactory(candidate *lowerCandidate, resume *ir.Func, factoryFrame *ir.Name, pc *types.Field,
	allocation *types.Type, prologue, stores ir.Nodes) *zeroSpawnFactory {
	frame := candidate.frameType
	if base.Flag.Cfg.Instrumenting || frame.Size() > abi.CoroSpawnBatchFrameBytes || pc == nil ||
		pc.Type != types.Types[types.TUINT32] || candidate.factory.Type().NumParams() != 1 {
		return nil
	}
	locals := chunkFactoryLocals(prologue)
	if locals == nil {
		return nil
	}
	var initialPC ir.Node
	for _, entry := range stores {
		store, ok := entry.(*ir.AssignStmt)
		if !ok || len(store.Init()) != 0 || store.Y == nil || len(store.Y.Init()) != 0 {
			return nil
		}
		dst, ok := store.X.(*ir.SelectorExpr)
		if !ok || dst.X != factoryFrame || dst.Selection == nil || len(dst.Init()) != 0 {
			return nil
		}
		value := store.Y
		if name, ok := value.(*ir.Name); ok {
			value = locals[name]
		}
		if value == nil {
			return nil
		}
		if dst.Selection == pc {
			if initialPC != nil || !ir.IsConst(value, constant.Int) {
				return nil
			}
			initialPC = value
		} else if !ir.IsZero(value) {
			return nil
		}
	}
	if initialPC == nil {
		return nil
	}
	return &zeroSpawnFactory{resume: resume, typ: frame, allocation: allocation, offset: pc.Offset, pc: ir.Int64Val(initialPC)}
}

type spawnBatchLoop struct {
	index, limit *ir.Name
	period       int64
	offset       *ir.Name
}

// planSpawnBatchLoop admits a zero-based loop with a private counter and
// bound. The resolved callee must also have no arguments or captures, so
// finishing several iterations together cannot skip any operand evaluation.
func planSpawnBatchLoop(source ir.Node, loop *ir.ForStmt) (*ir.GoDeferStmt, spawnBatchLoop) {
	if base.Flag.Cfg.Instrumenting || len(loop.Body) != 1 || loop.Cond == nil ||
		len(loop.Cond.Init()) != 0 {
		return nil, spawnBatchLoop{}
	}
	cond, ok := loop.Cond.(*ir.BinaryExpr)
	if !ok || cond.Op() != ir.OLT {
		return nil, spawnBatchLoop{}
	}
	index, ok := cond.X.(*ir.Name)
	if !ok || index.Class != ir.PAUTO || index.Addrtaken() || index.IsClosureVar() ||
		index.Type() != types.Types[types.TINT] {
		return nil, spawnBatchLoop{}
	}
	limit, ok := cond.Y.(*ir.Name)
	if !ok || limit != limit.Canonical() || (limit.Class != ir.PPARAM && limit.Class != ir.PAUTO) ||
		limit.Addrtaken() || limit.IsClosureVar() || limit.Type() != index.Type() {
		return nil, spawnBatchLoop{}
	}
	spawn, period, offset := repeatedSpawn(loop.Body[0], index)
	if spawn == nil {
		return nil, spawnBatchLoop{}
	}
	batch := spawnBatchLoop{index: index, limit: limit, period: period, offset: offset}
	if r, ok := source.(*ir.RangeStmt); ok && r.X.Type() == index.Type() {
		// integerRangeLoop initializes the private counter to zero and
		// saves the range operand once, even across suspension.
		return spawn, batch
	}
	if len(loop.Init()) != 1 {
		return nil, spawnBatchLoop{}
	}
	init, ok := loop.Init()[0].(*ir.AssignStmt)
	if !ok || !init.Def || init.X != index || init.Y == nil ||
		!ir.IsConst(init.Y, constant.Int) || ir.Int64Val(init.Y) != 0 {
		return nil, spawnBatchLoop{}
	}
	post, ok := loop.Post.(*ir.AssignOpStmt)
	if !ok || post.X != index || post.AsOp != ir.OADD || len(post.Init()) != 0 ||
		post.Y == nil || !ir.IsConst(post.Y, constant.Int) || ir.Int64Val(post.Y) != 1 {
		return nil, spawnBatchLoop{}
	}
	return spawn, batch
}

// A modulo partition runs the nonzero branch until the counter next reaches
// a multiple of the period. The two sides keep their distinct go statements.
func repeatedSpawn(stmt ir.Node, index *ir.Name) (*ir.GoDeferStmt, int64, *ir.Name) {
	if spawn := bareSpawn(stmt); spawn != nil {
		return spawn, 0, nil
	}
	branch, ok := stmt.(*ir.IfStmt)
	if !ok || len(branch.Init()) != 0 || len(branch.Body) != 1 || len(branch.Else) != 1 ||
		bareSpawn(branch.Body[0]) == nil || bareSpawn(branch.Else[0]) == nil {
		return nil, 0, nil
	}
	test, ok := branch.Cond.(*ir.BinaryExpr)
	if !ok || len(test.Init()) != 0 || test.Op() != ir.OEQ && test.Op() != ir.ONE ||
		len(test.Y.Init()) != 0 || !ir.IsConst(test.Y, constant.Int) || ir.Int64Val(test.Y) != 0 {
		return nil, 0, nil
	}
	modulo, ok := test.X.(*ir.BinaryExpr)
	if !ok || modulo.Op() != ir.OMOD || len(modulo.Init()) != 0 ||
		len(modulo.Y.Init()) != 0 || !ir.IsConst(modulo.Y, constant.Int) {
		return nil, 0, nil
	}
	period := ir.Int64Val(modulo.Y)
	if period < 3 {
		return nil, 0, nil
	}
	var offset *ir.Name
	if modulo.X != index {
		// Power-of-two periods also work when the sum is negative or
		// overflows: its low bits still locate the next zero remainder.
		addition, ok := modulo.X.(*ir.BinaryExpr)
		if !ok || addition.Op() != ir.OADD || addition.X != index ||
			len(addition.Init()) != 0 || period&(period-1) != 0 {
			return nil, 0, nil
		}
		offset, ok = addition.Y.(*ir.Name)
		if !ok || offset == index || offset != offset.Canonical() || offset.Type() != index.Type() ||
			offset.Addrtaken() || offset.IsClosureVar() || len(offset.Init()) != 0 ||
			(offset.Class != ir.PPARAM && offset.Class != ir.PAUTO) {
			return nil, 0, nil
		}
	}
	if test.Op() == ir.ONE {
		return bareSpawn(branch.Body[0]), period, offset
	}
	return bareSpawn(branch.Else[0]), period, offset
}

func bareSpawn(stmt ir.Node) *ir.GoDeferStmt {
	if spawn, ok := stmt.(*ir.GoDeferStmt); ok && spawn.Op() == ir.OGO && len(spawn.Init()) == 0 {
		return spawn
	}
	return nil
}

func resolveSpawnBatchSites(candidates map[*ir.Func]*lowerCandidate) {
	for _, parent := range candidates {
		for _, site := range parent.spawnBatchSites {
			child := candidates[site.callee]
			if child == nil || child.zeroSpawn == nil {
				continue
			}
			frame := child.zeroSpawn
			pos := site.spawn.Pos()
			ir.WithFunc(site.resume, func() {
				cond, conditional := site.spawn.(*ir.IfStmt)
				integer := types.Types[types.TINT]
				started := typecheck.TempAt(pos, site.resume, integer)
				yield := typecheck.TempAt(pos, site.resume, types.Types[types.TBOOL])
				var remaining ir.Node = ir.NewBinaryExpr(pos, ir.OSUB, site.limit, ir.DeepCopy(pos, site.index))
				if site.period != 0 {
					period := typedInt(pos, integer, site.period)
					counter := ir.DeepCopy(pos, site.index)
					op, divisor := ir.OMOD, period
					if site.offset != nil {
						counter = ir.NewBinaryExpr(pos, ir.OADD, counter, site.offset)
						op, divisor = ir.OAND, typedInt(pos, integer, site.period-1)
					}
					modulo := ir.NewBinaryExpr(pos, op, counter, divisor)
					run := ir.NewBinaryExpr(pos, ir.OSUB, ir.DeepCopy(pos, period), modulo)
					remaining = ir.NewCallExpr(pos, ir.OMIN, nil, ir.Nodes{remaining, run})
				}
				helper := "coroSpawnScalarBatch"
				args := ir.Nodes{site.resume.Dcl[0], frame.resume.Nname, reflectdata.TypePtrAt(pos, frame.typ)}
				if frame.typ.HasPointers() {
					helper = "coroSpawnFrameBatch"
					allocation := typecheck.NodNil()
					if frame.allocation != nil {
						allocation = reflectdata.TypePtrAt(pos, frame.allocation)
					}
					args = append(args, allocation)
				}
				args = append(args,
					typedInt(pos, types.Types[types.TUINTPTR], frame.offset),
					typedInt(pos, types.Types[types.TUINT32], frame.pc), remaining)
				spawn := typecheck.Call(pos, typecheck.LookupRuntime(helper), args, false)
				block := ir.Nodes{
					ir.NewDecl(pos, ir.ODCL, started),
					ir.NewDecl(pos, ir.ODCL, yield),
					ir.NewAssignListStmt(pos, ir.OAS2, ir.Nodes{started, yield}, ir.Nodes{spawn}),
					ir.NewAssignOpStmt(pos, ir.OADD, site.index,
						ir.NewBinaryExpr(pos, ir.OSUB, started, typedInt(pos, integer, 1))),
				}
				if conditional {
					cond.Cond = yield
					block = append(block, cond)
				}
				site.block.List = block
				typecheck.Stmts(site.block.List)
			})
		}
	}
}
