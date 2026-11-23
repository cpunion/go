// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/base"
	"cmd/compile/internal/ir"
	"cmd/compile/internal/typecheck"
	"cmd/compile/internal/types"
	"internal/abi"
	"internal/buildcfg"
	"slices"
)

type ordinarySpawnWorkers struct {
	batch *ir.Func
	plain *ir.Func
}

// The ordinary route uses the source entry for a bounded spawn and join loop.
// The same body handles the standard fallback, including all call operands.
func installOrdinarySpawnBatchEntry(candidate *lowerCandidate, candidates map[*ir.Func]*lowerCandidate,
	bodies *BodyReader, workers map[*ir.Func]ordinarySpawnWorkers) bool {
	fn := candidate.function.Func
	ordinary := candidate.ordinary
	if buildcfg.GOOS != "linux" || (buildcfg.GOARCH != "arm64" && buildcfg.GOARCH != "amd64") || base.Flag.Cfg.Instrumenting ||
		bodies == nil || bodies.Clone == nil || fn.OClosure != nil || fn.Type().HasShape() ||
		candidate.rangeOwner != nil || candidate.rangeBody != nil || candidate.function.Terminal != 0 ||
		candidate.function.Exec != NeedsTaskContext || len(candidate.function.Sites) != 2 {
		return false
	}
	var spawnSite, yieldSite bool
	for _, site := range candidate.function.Sites {
		spawnSite = spawnSite || site.Kind == SiteSpawn
		yieldSite = yieldSite || site.Kind == SiteYield
	}
	if !spawnSite || !yieldSite {
		return false
	}

	var launch *ir.ForStmt
	var wait *ir.CallExpr
	var limit *ir.Name
	var target *ir.Func
	ir.Visit(ordinary, func(node ir.Node) {
		loop, ok := node.(*ir.ForStmt)
		if !ok || len(loop.Body) != 1 {
			return
		}
		if yield := ordinaryBatchGosched(loop.Body[0]); yield != nil &&
			len(loop.Init()) == 0 && loop.Post == nil && loop.Cond != nil {
			wait = yield
			return
		}
		spawn, batch := planSpawnBatchLoop(loop, loop)
		if spawn == nil || batch.period != 0 || batch.limit.Class != ir.PPARAM || ir.Reassigned(batch.limit) {
			return
		}
		call, ok := spawn.Call.(*ir.CallExpr)
		if !ok || len(call.Args) != 0 || len(call.Init()) != 0 || len(call.Fun.Init()) != 0 {
			return
		}
		name, ok := call.Fun.(*ir.Name)
		if !ok || name.Class != ir.PFUNC || name.Func == nil || name.Func.OClosure != nil {
			return
		}
		launch, limit, target = loop, batch.limit, name.Func
	})
	if launch == nil || wait == nil {
		return false
	}
	param := slices.Index(ordinary.Dcl, limit)
	if param < 0 || param >= len(fn.Type().RecvParams()) {
		return false
	}
	argument := fn.Dcl[param]
	child := candidates[target]
	if child == nil || child.function.Exec != 0 || child.function.Terminal != 0 || len(child.function.Sites) != 1 ||
		child.function.Sites[0].Kind != SiteYield || target.Type().NumParams() != 0 || target.Type().NumResults() != 0 {
		return false
	}
	worker, ok := workers[target]
	if !ok {
		worker.batch = bodies.Clone(target, types.LocalPkg.Lookup(target.Sym().Name+".coro.batch"))
		if worker.batch == nil || !ordinarySpawnWorker(worker.batch) {
			return false
		}
		worker.plain = bodies.Clone(target, types.LocalPkg.Lookup(target.Sym().Name+".coro.batchplain"))
		if worker.plain == nil {
			return false
		}
		RecordSourceCopy(worker.batch, target)
		RecordSourceCopy(worker.plain, target)
		workers[target] = worker
		typecheck.Target.Funcs = append(typecheck.Target.Funcs, worker.batch, worker.plain)
	}

	remapOrdinaryBody(fn, ordinary)
	ir.WithFunc(fn, func() {
		pos := fn.Pos()
		ordinaryRoute := typecheck.TempAt(pos, fn, types.Types[types.TBOOL])
		batch := typecheck.TempAt(pos, fn, types.Types[types.TBOOL])
		choose := typecheck.Call(pos, typecheck.LookupRuntime("coroUseOrdinarySpawnBatch"), ir.Nodes{argument}, false)
		assignRoute := ir.NewAssignStmt(pos, ordinaryRoute, choose)
		withinLimit := ir.NewBinaryExpr(pos, ir.OLE, argument, ir.NewInt(pos, abi.CoroOrdinarySpawnBatchLimit))
		assignBatch := ir.NewAssignStmt(pos, batch, ir.NewLogicalExpr(pos, ir.OANDAND, ordinaryRoute, withinLimit))
		var edit func(ir.Node) ir.Node
		edit = func(node ir.Node) ir.Node {
			switch node {
			case launch:
				entry := typecheck.LookupRuntime("coroOrdinarySpawnBatch")
				spawnPos := launch.Body[0].Pos()
				batchCall := typecheck.Call(spawnPos, entry, ir.Nodes{worker.batch.Nname, argument}, false)
				plainCall := typecheck.Call(spawnPos, entry, ir.Nodes{worker.plain.Nname, argument}, false)
				ordinaryLaunch := ir.NewIfStmt(node.Pos(), batch, ir.Nodes{batchCall}, ir.Nodes{plainCall})
				return ir.NewIfStmt(node.Pos(), ordinaryRoute, ir.Nodes{ordinaryLaunch}, ir.Nodes{node})
			case wait:
				call := typecheck.Call(node.Pos(), typecheck.LookupRuntime("coroOrdinaryBatchYield"), nil, false)
				return ir.NewIfStmt(node.Pos(), batch, ir.Nodes{call}, ir.Nodes{node})
			}
			ir.EditChildren(node, edit)
			return node
		}
		for i, stmt := range ordinary.Body {
			ordinary.Body[i] = edit(stmt)
		}
		condition := ir.NewLogicalExpr(pos, ir.OOROR, ordinaryRoute, ordinaryEntryCondition(fn, nil))
		fn.Body = ir.Nodes{assignRoute, assignBatch, ir.NewIfStmt(pos, condition, ordinary.Body, fn.Body)}
		typecheck.Stmts(fn.Body)
	})
	return true
}

func ordinaryBatchGosched(node ir.Node) *ir.CallExpr {
	call, ok := node.(*ir.CallExpr)
	if !ok || call.Op() != ir.OCALLFUNC || len(call.Init()) != 0 || len(call.Args) != 0 {
		return nil
	}
	name, ok := call.Fun.(*ir.Name)
	if !ok || name.Sym() == nil || name.Sym().Pkg == nil ||
		name.Sym().Pkg.Path != "runtime" || name.Sym().Name != "Gosched" {
		return nil
	}
	return call
}

// Only a straight-line worker with one explicit Gosched enters the ordinary
// batch. Keep workers with their own control loops on the logical scheduler.
func ordinarySpawnWorker(fn *ir.Func) bool {
	var yield *ir.CallExpr
	for _, stmt := range fn.Body {
		if call := ordinaryBatchGosched(stmt); call != nil {
			if yield != nil {
				return false
			}
			yield = call
			continue
		}
		switch stmt.Op() {
		case ir.ODCL, ir.OAS, ir.OAS2, ir.OCALLFUNC, ir.ORETURN:
		default:
			return false
		}
	}
	if yield == nil {
		return false
	}
	ir.WithFunc(fn, func() {
		yield.Fun = typecheck.LookupRuntime("coroOrdinaryBatchYield")
		typecheck.Stmts(fn.Body)
	})
	return true
}
