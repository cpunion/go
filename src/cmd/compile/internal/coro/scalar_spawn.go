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

type scalarSpawnFactory struct {
	resume *ir.Func
	typ    *types.Type
	offset int64
	pc     int64
}

type scalarSpawnSite struct {
	block        *ir.BlockStmt
	spawn        ir.Node
	resume       *ir.Func
	callee       *ir.Func
	index, limit ir.Node
}

// newScalarSpawnFactory admits local factories whose pointer-free frame
// needs only zero initialization and an initial resume PC.
func newScalarSpawnFactory(candidate *lowerCandidate, resume *ir.Func, factoryFrame *ir.Name, pc *types.Field, prologue, stores ir.Nodes) *scalarSpawnFactory {
	frame := candidate.frameType
	if base.Flag.Cfg.Instrumenting || frame.Size() > abi.CoroSpawnBatchFrameBytes || frame.HasPointers() || pc == nil ||
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
	return &scalarSpawnFactory{resume: resume, typ: frame, offset: pc.Offset, pc: ir.Int64Val(initialPC)}
}

type scalarSpawnLoop struct{ index, limit *ir.Name }

// planScalarSpawnLoop admits a zero-based loop with a private counter and
// bound. The resolved callee must also have no arguments or captures, so
// finishing several iterations together cannot skip any operand evaluation.
func planScalarSpawnLoop(source ir.Node, loop *ir.ForStmt) (*ir.GoDeferStmt, scalarSpawnLoop) {
	if base.Flag.Cfg.Instrumenting || len(loop.Body) != 1 || loop.Cond == nil ||
		len(loop.Cond.Init()) != 0 {
		return nil, scalarSpawnLoop{}
	}
	spawn, ok := loop.Body[0].(*ir.GoDeferStmt)
	if !ok || spawn.Op() != ir.OGO || len(spawn.Init()) != 0 {
		return nil, scalarSpawnLoop{}
	}
	cond, ok := loop.Cond.(*ir.BinaryExpr)
	if !ok || cond.Op() != ir.OLT {
		return nil, scalarSpawnLoop{}
	}
	index, ok := cond.X.(*ir.Name)
	if !ok || index.Class != ir.PAUTO || index.Addrtaken() || index.IsClosureVar() ||
		index.Type() != types.Types[types.TINT] {
		return nil, scalarSpawnLoop{}
	}
	limit, ok := cond.Y.(*ir.Name)
	if !ok || limit != limit.Canonical() || (limit.Class != ir.PPARAM && limit.Class != ir.PAUTO) ||
		limit.Addrtaken() || limit.IsClosureVar() || limit.Type() != index.Type() {
		return nil, scalarSpawnLoop{}
	}
	if r, ok := source.(*ir.RangeStmt); ok && r.X.Type() == index.Type() {
		// integerRangeLoop initializes the private counter to zero and
		// saves the range operand once, even across suspension.
		return spawn, scalarSpawnLoop{index, limit}
	}
	if len(loop.Init()) != 1 {
		return nil, scalarSpawnLoop{}
	}
	init, ok := loop.Init()[0].(*ir.AssignStmt)
	if !ok || !init.Def || init.X != index || init.Y == nil ||
		!ir.IsConst(init.Y, constant.Int) || ir.Int64Val(init.Y) != 0 {
		return nil, scalarSpawnLoop{}
	}
	post, ok := loop.Post.(*ir.AssignOpStmt)
	if !ok || post.X != index || post.AsOp != ir.OADD || len(post.Init()) != 0 ||
		post.Y == nil || !ir.IsConst(post.Y, constant.Int) || ir.Int64Val(post.Y) != 1 {
		return nil, scalarSpawnLoop{}
	}
	return spawn, scalarSpawnLoop{index, limit}
}

func resolveScalarSpawnSites(candidates map[*ir.Func]*lowerCandidate) {
	for _, parent := range candidates {
		for _, site := range parent.scalarSpawnSites {
			child := candidates[site.callee]
			if child == nil || child.scalarSpawn == nil {
				continue
			}
			frame := child.scalarSpawn
			pos := site.spawn.Pos()
			ir.WithFunc(site.resume, func() {
				integer := types.Types[types.TINT]
				started := typecheck.TempAt(pos, site.resume, integer)
				yield := typecheck.TempAt(pos, site.resume, types.Types[types.TBOOL])
				remaining := ir.NewBinaryExpr(pos, ir.OSUB, site.limit, ir.DeepCopy(pos, site.index))
				spawn := typecheck.Call(pos, typecheck.LookupRuntime("coroSpawnScalarBatch"), ir.Nodes{
					site.resume.Dcl[0], frame.resume.Nname, reflectdata.TypePtrAt(pos, frame.typ),
					typedInt(pos, types.Types[types.TUINTPTR], frame.offset),
					typedInt(pos, types.Types[types.TUINT32], frame.pc), remaining,
				}, false)
				block := ir.Nodes{
					ir.NewDecl(pos, ir.ODCL, started),
					ir.NewDecl(pos, ir.ODCL, yield),
					ir.NewAssignListStmt(pos, ir.OAS2, ir.Nodes{started, yield}, ir.Nodes{spawn}),
					ir.NewAssignOpStmt(pos, ir.OADD, site.index,
						ir.NewBinaryExpr(pos, ir.OSUB, started, typedInt(pos, integer, 1))),
				}
				if conditional, ok := site.spawn.(*ir.IfStmt); ok {
					conditional.Cond = yield
					block = append(block, conditional)
				}
				site.block.List = block
				typecheck.Stmts(site.block.List)
			})
		}
	}
}
