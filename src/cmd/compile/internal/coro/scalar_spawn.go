// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/base"
	"cmd/compile/internal/ir"
	"cmd/compile/internal/typecheck"
	"cmd/compile/internal/types"
	"go/constant"
)

type scalarSpawnFactory struct {
	resume *ir.Func
	pc     int64
}

type scalarSpawnSite struct {
	block        *ir.BlockStmt
	spawn        ir.Node
	resume       *ir.Func
	callee       *ir.Func
	index, limit ir.Node
}

// newScalarSpawnFactory admits local explicit factories with no captures,
// arguments or destinations, whose whole frame is an initialized uint32 PC.
func newScalarSpawnFactory(candidate *lowerCandidate, resume *ir.Func, pc *types.Field, prologue, stores ir.Nodes) *scalarSpawnFactory {
	frame := candidate.frameType
	if base.Flag.Cfg.Instrumenting || frame.NumFields() != 1 || frame.Field(0) != pc || pc.Type != types.Types[types.TUINT32] ||
		candidate.factory.Type().NumParams() != 1 || len(stores) != 1 {
		return nil
	}
	store, ok := stores[0].(*ir.AssignStmt)
	if !ok {
		return nil
	}
	name, ok := store.Y.(*ir.Name)
	if !ok {
		return nil
	}
	value := chunkFactoryLocals(prologue)[name]
	if value == nil || !ir.IsConst(value, constant.Int) {
		return nil
	}
	return &scalarSpawnFactory{resume, ir.Int64Val(value)}
}

type scalarSpawnLoop struct{ index, limit *ir.Name }

// planScalarSpawnLoop admits a zero-based loop with a private counter and
// bound. The resolved callee must also have no arguments or captures, so
// finishing several iterations together cannot skip any operand evaluation.
func planScalarSpawnLoop(loop *ir.ForStmt) (*ir.GoDeferStmt, scalarSpawnLoop) {
	if base.Flag.Cfg.Instrumenting || len(loop.Body) != 1 || loop.Cond == nil ||
		len(loop.Cond.Init()) != 0 || len(loop.Init()) != 1 {
		return nil, scalarSpawnLoop{}
	}
	spawn, ok := loop.Body[0].(*ir.GoDeferStmt)
	if !ok || spawn.Op() != ir.OGO || len(spawn.Init()) != 0 {
		return nil, scalarSpawnLoop{}
	}
	init, ok := loop.Init()[0].(*ir.AssignStmt)
	if !ok || !init.Def || init.Y == nil || !ir.IsConst(init.Y, constant.Int) || ir.Int64Val(init.Y) != 0 {
		return nil, scalarSpawnLoop{}
	}
	index, ok := init.X.(*ir.Name)
	if !ok || index.Class != ir.PAUTO || index.Addrtaken() || index.IsClosureVar() ||
		index.Type() != types.Types[types.TINT] {
		return nil, scalarSpawnLoop{}
	}
	cond, ok := loop.Cond.(*ir.BinaryExpr)
	if !ok || cond.Op() != ir.OLT || cond.X != index {
		return nil, scalarSpawnLoop{}
	}
	limit, ok := cond.Y.(*ir.Name)
	if !ok || limit != limit.Canonical() || (limit.Class != ir.PPARAM && limit.Class != ir.PAUTO) ||
		limit.Addrtaken() || limit.IsClosureVar() || limit.Type() != index.Type() {
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
					site.resume.Dcl[0], frame.resume.Nname, typedInt(pos, types.Types[types.TUINT32], frame.pc), remaining,
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
