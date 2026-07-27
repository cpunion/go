// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/base"
	"cmd/compile/internal/ir"
	"cmd/compile/internal/typecheck"
	"cmd/compile/internal/types"
	"cmd/internal/obj"
	"cmd/internal/obj/x86"
	"cmd/internal/src"
	"go/constant"
	"slices"
	"strings"
	"sync"
	"testing"
)

var initLowerTest sync.Once

func prepareLowerTest(t *testing.T) {
	t.Helper()
	initLowerTest.Do(func() {
		types.PtrSize = 8
		types.RegSize = 8
		types.MaxWidth = 1 << 50
		base.Ctxt = obj.Linknew(&x86.Linkamd64)
		types.LocalPkg = types.NewPkg("example.com/coro/testinit", "testinit")
		typecheck.InitUniverse()
		ir.Pkgs.Runtime = types.NewPkg("runtime", "runtime")
		typecheck.InitRuntime()
	})
}

func newLowerTestFunc(pkg *types.Pkg, name string) *ir.Func {
	fn := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup(name),
		types.NewSignature(nil, nil, nil))
	fn.DeclareParams(true)
	typecheck.Target.Funcs = append(typecheck.Target.Funcs, fn)
	return fn
}

func newLowerTestCall(fn *ir.Func) *ir.CallExpr {
	call := ir.NewCallExpr(src.NoXPos, ir.OCALLFUNC, fn.Nname, nil)
	call.SetTypecheck(1)
	return call
}

func newLowerTestReturn() *ir.ReturnStmt {
	ret := ir.NewReturnStmt(src.NoXPos, nil)
	ret.SetTypecheck(1)
	return ret
}

func TestLowerStateMachines(t *testing.T) {
	prepareLowerTest(t)

	oldTarget := typecheck.Target
	oldLocalPkg := types.LocalPkg
	oldCurFunc := ir.CurFunc
	defer func() {
		typecheck.Target = oldTarget
		types.LocalPkg = oldLocalPkg
		ir.CurFunc = oldCurFunc
	}()

	pkg := types.NewPkg("example.com/coro/lowertest", "lowertest")
	types.LocalPkg = pkg
	typecheck.Target = new(ir.Package)

	yield := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("yield"),
		types.NewSignature(nil, nil, nil))
	yield.DeclareParams(true)

	child := newLowerTestFunc(pkg, "child")
	local := child.NewLocal(src.NoXPos, pkg.Lookup("local"), types.Types[types.TINT])
	decl := ir.NewDecl(src.NoXPos, ir.ODCL, local)
	assign := ir.NewAssignStmt(src.NoXPos, local,
		ir.NewBasicLit(src.NoXPos, types.Types[types.TINT], constant.MakeInt64(1)))
	assign.Def = true
	local.Defn = assign
	childYield := newLowerTestCall(yield)
	child.Body = []ir.Node{decl, assign, childYield, newLowerTestReturn()}

	parent := newLowerTestFunc(pkg, "parent")
	childCall := newLowerTestCall(child)
	parent.Body = []ir.Node{childCall, newLowerTestReturn()}

	spawned := newLowerTestFunc(pkg, "spawned")
	spawnedYield := newLowerTestCall(yield)
	spawned.Body = []ir.Node{spawnedYield, newLowerTestReturn()}

	spawner := newLowerTestFunc(pkg, "spawner")
	spawnCall := newLowerTestCall(spawned)
	goSpawn := ir.NewGoDeferStmt(src.NoXPos, ir.OGO, spawnCall)
	goSpawn.SetTypecheck(1)
	spawnerYield := newLowerTestCall(yield)
	spawner.Body = []ir.Node{goSpawn, spawnerYield, newLowerTestReturn()}

	timerParam := types.NewField(src.NoXPos, pkg.Lookup("ns"), types.Types[types.TINT64])
	timerOp := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("timerOp"),
		types.NewSignature(nil, []*types.Field{timerParam}, nil))
	timerOp.DeclareParams(true)
	timerCall := ir.NewCallExpr(src.NoXPos, ir.OCALLFUNC, timerOp.Nname,
		[]ir.Node{ir.NewBasicLit(src.NoXPos, types.Types[types.TINT64],
			constant.MakeInt64(1))})
	timerCall.SetTypecheck(1)
	sleeper := newLowerTestFunc(pkg, "sleeper")
	sleeper.Body = []ir.Node{timerCall, newLowerTestReturn()}

	newReadCall := func(name string) *ir.CallExpr {
		readParams := []*types.Field{
			types.NewField(src.NoXPos, nil, types.Types[types.TINT]),
			types.NewField(src.NoXPos, nil, types.NewSlice(types.ByteType)),
			types.NewField(src.NoXPos, nil, types.NewPtr(types.Types[types.TINT])),
			types.NewField(src.NoXPos, nil, types.NewPtr(types.Types[types.TUINTPTR])),
		}
		op := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup(name),
			types.NewSignature(nil, readParams, nil))
		op.DeclareParams(true)
		call := ir.NewCallExpr(src.NoXPos, ir.OCALLFUNC, op.Nname, []ir.Node{
			ir.NewBasicLit(src.NoXPos, types.Types[types.TINT], constant.MakeInt64(1)),
			ir.NewNilExpr(src.NoXPos, types.NewSlice(types.ByteType)),
			ir.NewNilExpr(src.NoXPos, types.NewPtr(types.Types[types.TINT])),
			ir.NewNilExpr(src.NoXPos, types.NewPtr(types.Types[types.TUINTPTR])),
		})
		call.SetTypecheck(1)
		return call
	}
	fileCall := newReadCall("fileOp")
	fileReader := newLowerTestFunc(pkg, "fileReader")
	fileReader.Body = []ir.Node{fileCall, newLowerTestReturn()}
	socketCall := newReadCall("socketOp")
	socketReader := newLowerTestFunc(pkg, "socketReader")
	socketReader.Body = []ir.Node{socketCall, newLowerTestReturn()}

	osPkg := types.NewPkg("os", "os")
	ordinaryRead := ir.NewFunc(src.NoXPos, src.NoXPos,
		osPkg.Lookup("(*File).Read"), types.NewSignature(nil, nil, nil))
	ordinaryRead.DeclareParams(true)
	ordinaryReadCall := newLowerTestCall(ordinaryRead)
	ordinaryFileReader := newLowerTestFunc(pkg, "ordinaryFileReader")
	ordinaryFileReader.Body = []ir.Node{ordinaryReadCall, newLowerTestReturn()}

	asyncParams := []*types.Field{
		types.NewField(src.NoXPos, nil, types.Types[types.TINT]),
		types.NewField(src.NoXPos, nil, types.Types[types.TINT]),
		types.NewField(src.NoXPos, nil, types.Types[types.TUINT64]),
		types.NewField(src.NoXPos, nil, types.NewPtr(types.Types[types.TUINT64])),
		types.NewField(src.NoXPos, nil, types.NewPtr(types.Types[types.TUINTPTR])),
	}
	asyncOp := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("asyncOp"),
		types.NewSignature(nil, asyncParams, nil))
	asyncOp.DeclareParams(true)
	asyncCall := ir.NewCallExpr(src.NoXPos, ir.OCALLFUNC, asyncOp.Nname, []ir.Node{
		ir.NewBasicLit(src.NoXPos, types.Types[types.TINT], constant.MakeInt64(1)),
		ir.NewBasicLit(src.NoXPos, types.Types[types.TINT], constant.MakeInt64(2)),
		ir.NewBasicLit(src.NoXPos, types.Types[types.TUINT64], constant.MakeInt64(21)),
		ir.NewNilExpr(src.NoXPos, types.NewPtr(types.Types[types.TUINT64])),
		ir.NewNilExpr(src.NoXPos, types.NewPtr(types.Types[types.TUINTPTR])),
	})
	asyncCall.SetTypecheck(1)
	asyncCaller := newLowerTestFunc(pkg, "asyncCaller")
	asyncCaller.Body = []ir.Node{asyncCall, newLowerTestReturn()}

	directParam := types.NewField(src.NoXPos, nil, types.Types[types.TINT])
	directType := types.NewSignature(nil, []*types.Field{directParam}, nil)
	directOp := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("directOp"),
		directType)
	directOp.DeclareParams(true)
	directEntry := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("directEntry"),
		directType)
	directEntry.DeclareParams(true)
	directValueResult := types.NewField(src.NoXPos, nil, types.Types[types.TINT])
	directValueType := types.NewSignature(nil, []*types.Field{directParam},
		[]*types.Field{directValueResult})
	directValueOp := ir.NewFunc(src.NoXPos, src.NoXPos,
		pkg.Lookup("directValueOp"), directValueType)
	directValueOp.DeclareParams(true)
	directValueEntry := ir.NewFunc(src.NoXPos, src.NoXPos,
		pkg.Lookup("directValueEntry"), directValueType)
	directValueEntry.DeclareParams(true)
	argumentResult := types.NewField(src.NoXPos, nil, types.Types[types.TINT])
	argument := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("argument"),
		types.NewSignature(nil, nil, []*types.Field{argumentResult}))
	argument.DeclareParams(true)
	argumentCall := newLowerTestCall(argument)
	argumentCall.SetType(types.Types[types.TINT])
	directCall := ir.NewCallExpr(src.NoXPos, ir.OCALLFUNC, directOp.Nname,
		ir.Nodes{argumentCall})
	directCall.SetTypecheck(1)
	blockOp := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("blockOp"),
		types.NewSignature(nil, nil, nil))
	blockOp.DeclareParams(true)
	blockCall := newLowerTestCall(blockOp)
	foreignYield := newLowerTestCall(yield)
	foreignCaller := newLowerTestFunc(pkg, "foreignCaller")
	foreignCaller.Body = []ir.Node{
		directCall, blockCall, foreignYield, newLowerTestReturn(),
	}

	runDirectCall := ir.NewCallExpr(src.NoXPos, ir.OCALLFUNC, directOp.Nname,
		ir.Nodes{ir.NewBasicLit(src.NoXPos, types.Types[types.TINT],
			constant.MakeInt64(1))})
	runDirectCall.SetTypecheck(1)
	runLoop := ir.NewForStmt(src.NoXPos, nil,
		ir.NewBasicLit(src.NoXPos, types.Types[types.TBOOL],
			constant.MakeBool(false)),
		nil, ir.Nodes{runDirectCall}, false)
	runLoop.SetTypecheck(1)
	runToCompletion := newLowerTestFunc(pkg, "runToCompletion")
	runToCompletion.Body = ir.Nodes{runLoop, newLowerTestReturn()}

	singleResult := types.NewField(src.NoXPos, pkg.Lookup("singleResult"),
		types.Types[types.TINT])
	runSingle := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("runSingle"),
		types.NewSignature(nil, nil, []*types.Field{singleResult}))
	runSingle.DeclareParams(true)
	typecheck.Target.Funcs = append(typecheck.Target.Funcs, runSingle)
	singleValue := runSingle.NewLocal(src.NoXPos, pkg.Lookup("singleValue"),
		types.Types[types.TINT])
	singleDecl := ir.NewDecl(src.NoXPos, ir.ODCL, singleValue)
	singleCall := ir.NewCallExpr(src.NoXPos, ir.OCALLFUNC,
		directValueOp.Nname, ir.Nodes{
			ir.NewBasicLit(src.NoXPos, types.Types[types.TINT],
				constant.MakeInt64(1)),
		})
	singleCall.SetType(types.Types[types.TINT])
	singleCall.SetTypecheck(1)
	singleAssign := ir.NewAssignStmt(src.NoXPos, singleValue, singleCall)
	singleAssign.Def = true
	singleValue.Defn = singleAssign
	singleReturn := ir.NewReturnStmt(src.NoXPos, ir.Nodes{singleValue})
	singleReturn.SetTypecheck(1)
	runSingle.Body = ir.Nodes{singleDecl, singleAssign, singleReturn}

	structuredResults := []*types.Field{
		types.NewField(src.NoXPos, pkg.Lookup("first"), types.Types[types.TINT]),
		types.NewField(src.NoXPos, pkg.Lookup("second"), types.Types[types.TINT]),
	}
	runStructured := ir.NewFunc(src.NoXPos, src.NoXPos,
		pkg.Lookup("runStructured"),
		types.NewSignature(nil, nil, structuredResults))
	runStructured.DeclareParams(true)
	typecheck.Target.Funcs = append(typecheck.Target.Funcs, runStructured)
	runValue := runStructured.NewLocal(src.NoXPos, pkg.Lookup("runValue"),
		types.Types[types.TINT])
	runValueDecl := ir.NewDecl(src.NoXPos, ir.ODCL, runValue)
	runValueInit := ir.NewAssignStmt(src.NoXPos, runValue,
		ir.NewBasicLit(src.NoXPos, types.Types[types.TINT],
			constant.MakeInt64(0)))
	runValueInit.Def = true
	runValue.Defn = runValueInit
	runValueCall := ir.NewCallExpr(src.NoXPos, ir.OCALLFUNC,
		directValueOp.Nname, ir.Nodes{
			ir.NewBasicLit(src.NoXPos, types.Types[types.TINT],
				constant.MakeInt64(2)),
		})
	runValueCall.SetType(types.Types[types.TINT])
	runValueCall.SetTypecheck(1)
	runValueAssign := ir.NewAssignStmt(src.NoXPos, runValue, runValueCall)
	runElseAssign := ir.NewAssignStmt(src.NoXPos, runValue,
		ir.NewBasicLit(src.NoXPos, types.Types[types.TINT],
			constant.MakeInt64(3)))
	runIf := ir.NewIfStmt(src.NoXPos,
		ir.NewBasicLit(src.NoXPos, types.Types[types.TBOOL],
			constant.MakeBool(true)),
		ir.Nodes{runValueAssign}, ir.Nodes{runElseAssign})
	runIf.SetInit(ir.Nodes{runValueInit})
	runIf.SetTypecheck(1)
	runBlock := ir.NewBlockStmt(src.NoXPos, ir.Nodes{runIf})
	runReturn := ir.NewReturnStmt(src.NoXPos, ir.Nodes{
		runValue,
		ir.NewBasicLit(src.NoXPos, types.Types[types.TINT],
			constant.MakeInt64(4)),
	})
	runReturn.SetTypecheck(1)
	runStructured.Body = ir.Nodes{runValueDecl, runBlock, runReturn}

	structured := newLowerTestFunc(pkg, "structured")
	loopVar := structured.NewLocal(src.NoXPos, pkg.Lookup("loop"), types.Types[types.TINT])
	loopDecl := ir.NewDecl(src.NoXPos, ir.ODCL, loopVar)
	loopInit := ir.NewAssignStmt(src.NoXPos, loopVar,
		ir.NewBasicLit(src.NoXPos, types.Types[types.TINT], constant.MakeInt64(0)))
	loopInit.Def = true
	loopVar.Defn = loopInit
	thenYield := newLowerTestCall(yield)
	elseYield := newLowerTestCall(yield)
	ifStmt := ir.NewIfStmt(src.NoXPos,
		ir.NewBasicLit(src.NoXPos, types.Types[types.TBOOL], constant.MakeBool(true)),
		ir.Nodes{thenYield, newLowerTestReturn()}, ir.Nodes{elseYield})
	ifStmt.SetTypecheck(1)
	structuredTimer := ir.NewCallExpr(src.NoXPos, ir.OCALLFUNC, timerOp.Nname,
		[]ir.Node{ir.NewBasicLit(src.NoXPos, types.Types[types.TINT64],
			constant.MakeInt64(1))})
	structuredTimer.SetTypecheck(1)
	loopPost := ir.NewAssignOpStmt(src.NoXPos, ir.OADD, loopVar,
		ir.NewBasicLit(src.NoXPos, types.Types[types.TINT], constant.MakeInt64(1)))
	loopPost.IncDec = true
	loopPost.SetTypecheck(1)
	loop := ir.NewForStmt(src.NoXPos, loopInit,
		nil,
		loopPost, ir.Nodes{structuredTimer}, false)
	loop.SetTypecheck(1)
	structured.Body = []ir.Node{
		loopDecl,
		ir.NewBlockStmt(src.NoXPos, ir.Nodes{ifStmt}),
		loop,
		newLowerTestReturn(),
	}

	cleanupParam := types.NewField(src.NoXPos, pkg.Lookup("value"),
		types.Types[types.TINT])
	cleanup := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("cleanup"),
		types.NewSignature(nil, []*types.Field{cleanupParam}, nil))
	cleanup.DeclareParams(true)
	deferred := newLowerTestFunc(pkg, "deferred")
	deferValue := deferred.NewLocal(src.NoXPos, pkg.Lookup("deferValue"),
		types.Types[types.TINT])
	deferDecl := ir.NewDecl(src.NoXPos, ir.ODCL, deferValue)
	deferAssign := ir.NewAssignStmt(src.NoXPos, deferValue,
		ir.NewBasicLit(src.NoXPos, types.Types[types.TINT],
			constant.MakeInt64(42)))
	deferAssign.Def = true
	deferValue.Defn = deferAssign
	deferWrapper := ir.NewClosureFunc(src.NoXPos, src.NoXPos, ir.ODEFER,
		types.NewSignature(nil, nil, nil), deferred, typecheck.Target, 0)
	deferWrapper.DeclareParams(true)
	deferWrapper.SetWrapper(true)
	deferWrapper.WrappedFunc = cleanup
	deferCapture := ir.NewClosureVar(src.NoXPos, deferWrapper, deferValue)
	deferCleanupCall := ir.NewCallExpr(src.NoXPos, ir.OCALLFUNC,
		cleanup.Nname, ir.Nodes{deferCapture})
	deferCleanupCall.SetTypecheck(1)
	deferWrapper.Body = ir.Nodes{deferCleanupCall}
	deferCall := ir.NewCallExpr(src.NoXPos, ir.OCALLFUNC,
		deferWrapper.OClosure, nil)
	deferCall.SetTypecheck(1)
	deferStmt := ir.NewGoDeferStmt(src.NoXPos, ir.ODEFER, deferCall)
	deferStmt.SetTypecheck(1)
	deferStmt.SetInit(ir.Nodes{deferAssign})
	deferIf := ir.NewIfStmt(src.NoXPos,
		ir.NewBasicLit(src.NoXPos, types.Types[types.TBOOL],
			constant.MakeBool(true)),
		ir.Nodes{deferStmt}, nil)
	deferIf.SetTypecheck(1)
	cleanupNoArgs := newLowerTestFunc(pkg, "cleanupNoArgs")
	deferNoArgsCall := newLowerTestCall(cleanupNoArgs)
	deferNoArgsStmt := ir.NewGoDeferStmt(src.NoXPos, ir.ODEFER,
		deferNoArgsCall)
	deferNoArgsStmt.SetTypecheck(1)
	deferredYield := newLowerTestCall(yield)
	deferred.Body = ir.Nodes{
		deferDecl, deferIf, deferNoArgsStmt, deferredYield,
		newLowerTestReturn(),
	}

	functions := map[*ir.Func]*Function{
		child: {
			Func:    child,
			Local:   MaySuspend,
			Effect:  MaySuspend,
			Primary: CoroPrimary,
			Sites: []Site{{
				ID: 1, Kind: SiteYield, Node: childYield,
			}},
		},
		parent: {
			Func:    parent,
			Effect:  MaySuspend,
			Primary: CoroPrimary,
			Edges: []Edge{{
				Kind: DirectCall, Callee: child, CalleeName: symbolName(child.Nname),
				Node: childCall,
			}},
			Sites: []Site{{
				ID: 1, Kind: SiteAwait, Node: childCall,
			}},
		},
		spawned: {
			Func:    spawned,
			Local:   MaySuspend,
			Effect:  MaySuspend,
			Primary: CoroPrimary,
			Sites: []Site{{
				ID: 1, Kind: SiteYield, Node: spawnedYield,
			}},
		},
		spawner: {
			Func:    spawner,
			Local:   MaySuspend,
			Effect:  MaySuspend,
			Primary: CoroPrimary,
			Edges: []Edge{
				{
					Kind: GoCall, Callee: spawned,
					CalleeName: symbolName(spawned.Nname), Node: spawnCall,
				},
			},
			Sites: []Site{
				{ID: 1, Kind: SiteSpawn, Node: spawnCall},
				{ID: 2, Kind: SiteYield, Node: spawnerYield},
			},
		},
		sleeper: {
			Func:    sleeper,
			Local:   MaySuspend,
			Effect:  MaySuspend,
			Primary: CoroPrimary,
			Sites: []Site{{
				ID: 1, Kind: SiteTimer, Node: timerCall,
			}},
		},
		fileReader: {
			Func:    fileReader,
			Local:   MaySuspend,
			Effect:  MaySuspend,
			Primary: CoroPrimary,
			Sites: []Site{{
				ID: 1, Kind: SiteFile, Node: fileCall,
			}},
		},
		socketReader: {
			Func:    socketReader,
			Local:   MaySuspend,
			Effect:  MaySuspend,
			Primary: CoroPrimary,
			Sites: []Site{{
				ID: 1, Kind: SitePoll, Node: socketCall,
			}},
		},
		ordinaryFileReader: {
			Func:    ordinaryFileReader,
			Local:   MaySuspend,
			Effect:  MaySuspend,
			Primary: CoroPrimary,
			Sites: []Site{{
				ID: 1, Kind: SiteFile, Node: ordinaryReadCall,
			}},
		},
		asyncCaller: {
			Func:    asyncCaller,
			Local:   MaySuspend,
			Effect:  MaySuspend,
			Exec:    NeedsSystemABI,
			Primary: CoroPrimary,
			Sites: []Site{{
				ID: 1, Kind: SiteForeign, Node: asyncCall,
				Foreign: AsyncOperation,
			}},
		},
		foreignCaller: {
			Func:    foreignCaller,
			Local:   MaySuspend,
			Effect:  MaySuspend,
			Exec:    NeedsSystemABI | MayBlockThread,
			Primary: CoroPrimary,
			Edges: []Edge{{
				Kind: DirectCall, Callee: directOp,
				CalleeName: symbolName(directOp.Nname),
				Recipe: OperationRecipe{
					Kind: SiteForeign, Foreign: DirectNoBlock,
					Direct: symbolName(directEntry.Nname),
				},
				Node: directCall, Direct: directEntry,
			}},
			Sites: []Site{
				{
					ID: 1, Kind: SiteForeign, Node: directCall,
					Foreign: DirectNoBlock,
				},
				{
					ID: 2, Kind: SiteForeign, Node: blockCall,
					Foreign: DirectMayBlock,
				},
				{ID: 3, Kind: SiteYield, Node: foreignYield},
			},
		},
		runToCompletion: {
			Func:    runToCompletion,
			Effect:  NoSuspend,
			Exec:    NeedsSystemABI | MayBlockThread,
			Primary: CoroPrimary,
			Edges: []Edge{{
				Kind: DirectCall, Callee: directOp,
				CalleeName: symbolName(directOp.Nname),
				Recipe: OperationRecipe{
					Kind: SiteForeign, Foreign: DirectMayBlock,
					Direct: symbolName(directEntry.Nname),
				},
				Node: runDirectCall, Direct: directEntry,
			}},
			Sites: []Site{{
				ID: 1, Kind: SiteForeign, Node: runDirectCall,
				Foreign: DirectMayBlock,
			}},
		},
		runSingle: {
			Func:    runSingle,
			Effect:  NoSuspend,
			Exec:    NeedsSystemABI,
			Primary: CoroPrimary,
			Edges: []Edge{{
				Kind: DirectCall, Callee: directValueOp,
				CalleeName: symbolName(directValueOp.Nname),
				Recipe: OperationRecipe{
					Kind: SiteForeign, Foreign: DirectNoBlock,
					Direct: symbolName(directValueEntry.Nname),
				},
				Node: singleCall, Direct: directValueEntry,
			}},
			Sites: []Site{{
				ID: 1, Kind: SiteForeign, Node: singleCall,
				Foreign: DirectNoBlock,
			}},
		},
		runStructured: {
			Func:    runStructured,
			Effect:  NoSuspend,
			Exec:    NeedsSystemABI,
			Primary: CoroPrimary,
			Edges: []Edge{{
				Kind: DirectCall, Callee: directValueOp,
				CalleeName: symbolName(directValueOp.Nname),
				Recipe: OperationRecipe{
					Kind: SiteForeign, Foreign: DirectNoBlock,
					Direct: symbolName(directValueEntry.Nname),
				},
				Node: runValueCall, Direct: directValueEntry,
			}},
			Sites: []Site{{
				ID: 1, Kind: SiteForeign, Node: runValueCall,
				Foreign: DirectNoBlock,
			}},
		},
		structured: {
			Func:    structured,
			Local:   MaySuspend,
			Effect:  MaySuspend,
			Primary: CoroPrimary,
			Sites: []Site{
				{ID: 1, Kind: SiteYield, Node: thenYield},
				{ID: 2, Kind: SiteYield, Node: elseYield},
				{ID: 3, Kind: SiteTimer, Node: structuredTimer},
			},
		},
		deferred: {
			Func:    deferred,
			Local:   MaySuspend,
			Effect:  MaySuspend,
			Primary: CoroPrimary,
			Edges: []Edge{
				{
					Kind: DeferCall, Callee: deferWrapper,
					CalleeName: symbolName(deferWrapper.Nname), Node: deferCall,
				},
				{
					Kind: DeferCall, Callee: cleanupNoArgs,
					CalleeName: symbolName(cleanupNoArgs.Nname),
					Node:       deferNoArgsCall,
				},
			},
			Sites: []Site{{
				ID: 1, Kind: SiteYield, Node: deferredYield,
			}},
		},
		deferWrapper: {
			Func:    deferWrapper,
			Primary: PlainPrimary,
		},
	}
	result, err := Lower(&Plan{Functions: functions})
	if err != nil {
		t.Fatalf("Lower failed: %v", err)
	}
	if result.Lowered != 15 || result.Skipped != 0 {
		t.Fatalf("Lower result = %+v, want 15 lowered and 0 skipped", result)
	}
	var noSplitResumes int
	for _, generated := range typecheck.Target.Funcs {
		if generated.OClosure != nil && generated.Pragma&ir.Nosplit != 0 {
			noSplitResumes++
		}
	}
	if noSplitResumes != result.Lowered {
		t.Fatalf("nosplit resume functions = %d, want %d", noSplitResumes, result.Lowered)
	}

	var foreignResume *ir.Func
	for _, generated := range typecheck.Target.Funcs {
		if generated.OClosure != nil && generated.ClosureParent != nil &&
			generated.ClosureParent.Sym().Name == "foreignCaller.coro" {
			foreignResume = generated
			break
		}
	}
	if foreignResume == nil {
		t.Fatal("foreign caller has no generated resume function")
	}
	var callOrder []string
	ir.Visit(foreignResume, func(node ir.Node) {
		call, ok := node.(*ir.CallExpr)
		if !ok {
			return
		}
		name := ir.StaticCalleeName(call.Fun)
		if name != nil && name.Sym() != nil {
			callOrder = append(callOrder, name.Sym().Name)
		}
	})
	callIndex := func(name string) int {
		return slices.Index(callOrder, name)
	}
	argumentIndex := callIndex("argument")
	enterIndex := callIndex("coroEnterForeign")
	directIndex := callIndex("directEntry")
	exitIndex := callIndex("coroExitForeign")
	if argumentIndex < 0 || enterIndex < 0 || directIndex < 0 || exitIndex < 0 ||
		!(argumentIndex < enterIndex && enterIndex < directIndex &&
			directIndex < exitIndex) {
		t.Errorf("foreign call order = %v, want argument, enter, direct, exit",
			callOrder)
	}
	if callIndex("directOp") >= 0 {
		t.Errorf("foreign call retained wrapper target: %v", callOrder)
	}

	var runResume *ir.Func
	for _, generated := range typecheck.Target.Funcs {
		if generated.OClosure != nil && generated.ClosureParent != nil &&
			generated.ClosureParent.Sym().Name == "runToCompletion.coro" {
			runResume = generated
			break
		}
	}
	if runResume == nil {
		t.Fatal("run-to-completion caller has no generated resume function")
	}
	var runCalls []string
	var runLoops, runSwitches int
	ir.Visit(runResume, func(node ir.Node) {
		switch node := node.(type) {
		case *ir.ForStmt:
			runLoops++
		case *ir.SwitchStmt:
			runSwitches++
		case *ir.CallExpr:
			name := ir.StaticCalleeName(node.Fun)
			if name != nil && name.Sym() != nil {
				runCalls = append(runCalls, name.Sym().Name)
			}
		}
	})
	if runLoops != 1 || runSwitches != 0 {
		t.Errorf("run-to-completion control flow has %d loops and %d switches, want 1 and 0",
			runLoops, runSwitches)
	}
	if want := []string{"coroEnterBlocking", "directEntry", "coroExitBlocking"}; !slices.Equal(runCalls, want) {
		t.Errorf("run-to-completion calls = %v, want %v", runCalls, want)
	}

	for _, test := range []struct {
		name       string
		wantIf     bool
		wantResult int
	}{
		{"runSingle", false, 1},
		{"runStructured", true, 2},
	} {
		var resume *ir.Func
		for _, generated := range typecheck.Target.Funcs {
			if generated.OClosure != nil && generated.ClosureParent != nil &&
				generated.ClosureParent.Sym().Name == test.name+".coro" {
				resume = generated
				break
			}
		}
		if resume == nil {
			t.Errorf("%s has no generated resume function", test.name)
			continue
		}
		var calls []string
		var ifs, blocks, switches, resultStores int
		ir.Visit(resume, func(node ir.Node) {
			switch node := node.(type) {
			case *ir.IfStmt:
				ifs++
			case *ir.BlockStmt:
				blocks++
			case *ir.SwitchStmt:
				switches++
			case *ir.AssignStmt:
				if _, ok := node.X.(*ir.StarExpr); ok {
					resultStores++
				}
			case *ir.CallExpr:
				name := ir.StaticCalleeName(node.Fun)
				if name != nil && name.Sym() != nil {
					calls = append(calls, name.Sym().Name)
				}
			}
		})
		wantCalls := []string{
			"coroEnterForeign", "directValueEntry", "coroExitForeign",
		}
		if !slices.Equal(calls, wantCalls) {
			t.Errorf("%s calls = %v, want %v", test.name, calls, wantCalls)
		}
		if switches != 0 {
			t.Errorf("%s has %d switches, want none", test.name, switches)
		}
		if test.wantIf && (ifs == 0 || blocks == 0) {
			t.Errorf("%s has %d ifs and %d blocks, want structured control",
				test.name, ifs, blocks)
		}
		// Both an explicit return and the implicit fallthrough completion copy
		// every result to the wrapper-owned slots.
		if want := 2 * test.wantResult; resultStores != want {
			t.Errorf("%s result stores = %d, want %d",
				test.name, resultStores, want)
		}
	}

	for _, fn := range []*ir.Func{
		child, parent, spawned, spawner, sleeper, fileReader, socketReader,
		ordinaryFileReader, asyncCaller, foreignCaller, runToCompletion,
		runSingle, runStructured, structured, deferred,
	} {
		if len(fn.Body) != 2 {
			t.Errorf("%s body has %d statements, want 2", fn.Sym().Name, len(fn.Body))
			continue
		}
		call, ok := fn.Body[0].(*ir.CallExpr)
		if !ok {
			t.Errorf("%s first statement is %T, want call", fn.Sym().Name, fn.Body[0])
			continue
		}
		callee := ir.StaticCalleeName(call.Fun)
		if callee == nil || callee.Sym().Pkg != ir.Pkgs.Runtime ||
			callee.Sym().Name != "coroRun" {
			t.Errorf("%s wrapper calls %v, want runtime.coroRun",
				fn.Sym().Name, callee)
		}
		if fn.Inl != nil {
			t.Errorf("%s retained stale inline body", fn.Sym().Name)
		}
		hasFactory := false
		for _, generated := range typecheck.Target.Funcs {
			if generated.Sym().Name == fn.Sym().Name+".coro" {
				hasFactory = true
				break
			}
		}
		if !hasFactory {
			t.Errorf("%s has no resume factory", fn.Sym().Name)
		}
	}
}

func TestCanLowerRunToCompletion(t *testing.T) {
	prepareLowerTest(t)

	oldTarget := typecheck.Target
	oldLocalPkg := types.LocalPkg
	defer func() {
		typecheck.Target = oldTarget
		types.LocalPkg = oldLocalPkg
	}()

	pkg := types.NewPkg("example.com/coro/runpredicate", "runpredicate")
	types.LocalPkg = pkg
	typecheck.Target = new(ir.Package)

	tests := []struct {
		name   string
		change func(*lowerCandidate, *ir.CallExpr)
		want   bool
	}{
		{"direct no block", func(*lowerCandidate, *ir.CallExpr) {}, true},
		{"direct may block", func(candidate *lowerCandidate, call *ir.CallExpr) {
			candidate.function.Sites[0].Foreign = DirectMayBlock
			candidate.foreignCalls[call] = DirectMayBlock
		}, true},
		{"may suspend", func(candidate *lowerCandidate, _ *ir.CallExpr) {
			candidate.function.Effect = MaySuspend
		}, false},
		{"defer", func(candidate *lowerCandidate, _ *ir.CallExpr) {
			candidate.defers = []*lowerDefer{{}}
		}, false},
		{"no foreign call", func(candidate *lowerCandidate, call *ir.CallExpr) {
			delete(candidate.foreignCalls, call)
		}, false},
		{"non-foreign site", func(candidate *lowerCandidate, _ *ir.CallExpr) {
			candidate.function.Sites[0].Kind = SiteYield
		}, false},
		{"async foreign site", func(candidate *lowerCandidate, call *ir.CallExpr) {
			candidate.function.Sites[0].Foreign = AsyncOperation
			candidate.foreignCalls[call] = AsyncOperation
		}, false},
		{"foreign call in for post", func(candidate *lowerCandidate, call *ir.CallExpr) {
			loop := ir.NewForStmt(src.NoXPos, nil, nil, call, nil, false)
			candidate.function.Func.Body = ir.Nodes{loop, newLowerTestReturn()}
		}, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			target := newLowerTestFunc(pkg, "target."+strings.ReplaceAll(test.name, " ", "."))
			call := newLowerTestCall(target)
			fn := newLowerTestFunc(pkg, "caller."+strings.ReplaceAll(test.name, " ", "."))
			fn.Body = ir.Nodes{call, newLowerTestReturn()}
			candidate := &lowerCandidate{
				function: &Function{
					Func:   fn,
					Effect: NoSuspend,
					Sites: []Site{{
						ID: 1, Kind: SiteForeign, Node: call,
						Foreign: DirectNoBlock,
					}},
				},
				foreignCalls: map[*ir.CallExpr]ForeignCallClass{
					call: DirectNoBlock,
				},
			}
			test.change(candidate, call)
			if got := canLowerRunToCompletion(candidate); got != test.want {
				t.Errorf("canLowerRunToCompletion = %t, want %t", got, test.want)
			}
		})
	}
}

func TestLowerRejectsUnsupportedDependency(t *testing.T) {
	prepareLowerTest(t)

	oldTarget := typecheck.Target
	oldLocalPkg := types.LocalPkg
	defer func() {
		typecheck.Target = oldTarget
		types.LocalPkg = oldLocalPkg
	}()

	pkg := types.NewPkg("example.com/coro/lowerreject", "lowerreject")
	types.LocalPkg = pkg
	typecheck.Target = new(ir.Package)

	child := newLowerTestFunc(pkg, "child")
	child.Body = []ir.Node{newLowerTestReturn()}
	parent := newLowerTestFunc(pkg, "parent")
	call := newLowerTestCall(child)
	parent.Body = []ir.Node{call, newLowerTestReturn()}

	plan := &Plan{Functions: map[*ir.Func]*Function{
		child: {
			Func:    child,
			Effect:  MaySuspend,
			Primary: CoroPrimary,
			Sites: []Site{{
				ID: 1, Kind: SiteChannel, Node: child.Body[0],
			}},
		},
		parent: {
			Func:    parent,
			Effect:  MaySuspend,
			Primary: CoroPrimary,
			Edges: []Edge{{
				Kind: DirectCall, Callee: child, CalleeName: symbolName(child.Nname),
				Node: call,
			}},
			Sites: []Site{{
				ID: 1, Kind: SiteAwait, Node: call,
			}},
		},
	}}
	result, err := Lower(plan)
	if err != nil {
		t.Fatalf("Lower failed: %v", err)
	}
	if result.Lowered != 0 || result.Skipped != 2 {
		t.Fatalf("Lower result = %+v, want 0 lowered and 2 skipped", result)
	}
}

func TestLowerRejectsNestedSystemABIRoot(t *testing.T) {
	prepareLowerTest(t)

	oldTarget := typecheck.Target
	oldLocalPkg := types.LocalPkg
	defer func() {
		typecheck.Target = oldTarget
		types.LocalPkg = oldLocalPkg
	}()

	pkg := types.NewPkg("example.com/coro/lowernestedroot", "lowernestedroot")
	types.LocalPkg = pkg
	typecheck.Target = new(ir.Package)

	directOp := newLowerTestFunc(pkg, "directOp")
	directEntry := newLowerTestFunc(pkg, "directEntry")
	newForeignCall := func() *ir.CallExpr {
		return newLowerTestCall(directOp)
	}
	newForeignEdge := func(call *ir.CallExpr) Edge {
		return Edge{
			Kind: DirectCall, Callee: directOp,
			CalleeName: symbolName(directOp.Nname),
			Recipe: OperationRecipe{
				Kind: SiteForeign, Foreign: DirectNoBlock,
				Direct: symbolName(directEntry.Nname),
			},
			Node: call, Direct: directEntry,
		}
	}

	child := newLowerTestFunc(pkg, "child")
	childForeign := newForeignCall()
	child.Body = ir.Nodes{childForeign, newLowerTestReturn()}

	parent := newLowerTestFunc(pkg, "parent")
	parentForeign := newForeignCall()
	childCall := newLowerTestCall(child)
	parent.Body = ir.Nodes{parentForeign, childCall, newLowerTestReturn()}

	plan := &Plan{Functions: map[*ir.Func]*Function{
		child: {
			Func:    child,
			Effect:  NoSuspend,
			Exec:    NeedsSystemABI,
			Primary: CoroPrimary,
			Edges:   []Edge{newForeignEdge(childForeign)},
			Sites: []Site{{
				ID: 1, Kind: SiteForeign, Node: childForeign,
				Foreign: DirectNoBlock,
			}},
		},
		parent: {
			Func:    parent,
			Effect:  NoSuspend,
			Exec:    NeedsSystemABI,
			Primary: CoroPrimary,
			Edges: []Edge{
				newForeignEdge(parentForeign),
				{
					Kind: DirectCall, Callee: child,
					CalleeName: symbolName(child.Nname), Node: childCall,
				},
			},
			Sites: []Site{{
				ID: 1, Kind: SiteForeign, Node: parentForeign,
				Foreign: DirectNoBlock,
			}},
		},
	}}
	result, err := Lower(plan)
	if err != nil {
		t.Fatalf("Lower failed: %v", err)
	}
	if result.Lowered != 1 || result.Skipped != 1 {
		t.Fatalf("Lower result = %+v, want 1 lowered and 1 skipped", result)
	}
	if len(result.Diagnostics) != 1 ||
		!strings.Contains(result.Diagnostics[0], "requires coroutine factory entry") {
		t.Fatalf("Lower diagnostics = %q, want factory-entry rejection",
			result.Diagnostics)
	}
	if len(parent.Body) != 3 {
		t.Fatalf("rejected parent body has %d statements, want 3", len(parent.Body))
	}
}

func TestLowerRejectsUnsupportedControl(t *testing.T) {
	prepareLowerTest(t)

	oldTarget := typecheck.Target
	oldLocalPkg := types.LocalPkg
	defer func() {
		typecheck.Target = oldTarget
		types.LocalPkg = oldLocalPkg
	}()

	pkg := types.NewPkg("example.com/coro/lowercontrol", "lowercontrol")
	types.LocalPkg = pkg
	typecheck.Target = new(ir.Package)

	yield := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("yield"),
		types.NewSignature(nil, nil, nil))
	yield.DeclareParams(true)
	branch := func() ir.Node {
		return ir.NewBranchStmt(src.NoXPos, ir.OBREAK, nil)
	}
	cases := []struct {
		name string
		body func(*ir.CallExpr) ir.Nodes
		want string
	}{
		{
			name: "init",
			body: func(call *ir.CallExpr) ir.Nodes {
				call.SetInit(ir.Nodes{branch()})
				return ir.Nodes{call}
			},
			want: "control flow break",
		},
		{
			name: "defer",
			body: func(call *ir.CallExpr) ir.Nodes {
				return ir.Nodes{ir.NewGoDeferStmt(src.NoXPos, ir.ODEFER, call)}
			},
			want: "suspending defer",
		},
		{
			name: "non-call-defer",
			body: func(call *ir.CallExpr) ir.Nodes {
				return ir.Nodes{ir.NewGoDeferStmt(src.NoXPos,
					ir.ODEFER, call.Fun)}
			},
			want: "non-call go or defer statement",
		},
		{
			name: "block",
			body: func(*ir.CallExpr) ir.Nodes {
				return ir.Nodes{ir.NewBlockStmt(src.NoXPos, ir.Nodes{branch()})}
			},
			want: "control flow break",
		},
		{
			name: "if-body",
			body: func(*ir.CallExpr) ir.Nodes {
				return ir.Nodes{ir.NewIfStmt(src.NoXPos, nil,
					ir.Nodes{branch()}, nil)}
			},
			want: "control flow break",
		},
		{
			name: "if-else",
			body: func(*ir.CallExpr) ir.Nodes {
				return ir.Nodes{ir.NewIfStmt(src.NoXPos, nil,
					nil, ir.Nodes{branch()})}
			},
			want: "control flow break",
		},
		{
			name: "labeled-for",
			body: func(*ir.CallExpr) ir.Nodes {
				loop := ir.NewForStmt(src.NoXPos, nil, nil, nil, nil, false)
				loop.Label = pkg.Lookup("loop")
				return ir.Nodes{loop}
			},
			want: "labeled for loop",
		},
		{
			name: "for-post",
			body: func(*ir.CallExpr) ir.Nodes {
				return ir.Nodes{ir.NewForStmt(src.NoXPos, nil, nil,
					branch(), nil, false)}
			},
			want: "control flow break",
		},
		{
			name: "for-body",
			body: func(*ir.CallExpr) ir.Nodes {
				return ir.Nodes{ir.NewForStmt(src.NoXPos, nil, nil,
					nil, ir.Nodes{branch()}, false)}
			},
			want: "control flow break",
		},
		{
			name: "defer-in-loop",
			body: func(call *ir.CallExpr) ir.Nodes {
				deferStmt := ir.NewGoDeferStmt(src.NoXPos, ir.ODEFER, call)
				return ir.Nodes{ir.NewForStmt(src.NoXPos, nil, nil,
					nil, ir.Nodes{deferStmt}, false)}
			},
			want: "defer in loop",
		},
		{
			name: "non-normalized-defer",
			body: func(call *ir.CallExpr) ir.Nodes {
				call.Args = ir.Nodes{ir.NewBasicLit(src.NoXPos,
					types.Types[types.TINT], constant.MakeInt64(1))}
				return ir.Nodes{ir.NewGoDeferStmt(src.NoXPos, ir.ODEFER, call)}
			},
			want: "non-normalized defer call",
		},
		{
			name: "defer-in-switch",
			body: func(call *ir.CallExpr) ir.Nodes {
				deferStmt := ir.NewGoDeferStmt(src.NoXPos, ir.ODEFER, call)
				clause := ir.NewCaseStmt(src.NoXPos, nil, ir.Nodes{deferStmt})
				return ir.Nodes{ir.NewSwitchStmt(src.NoXPos, nil,
					[]*ir.CaseClause{clause})}
			},
			want: "defer in unsupported control flow switch",
		},
		{
			name: "return",
			body: func(*ir.CallExpr) ir.Nodes {
				return ir.Nodes{ir.NewReturnStmt(src.NoXPos, ir.Nodes{
					ir.NewBasicLit(src.NoXPos, types.Types[types.TINT],
						constant.MakeInt64(1)),
				})}
			},
			want: "return has 1 results, want 0",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fn := newLowerTestFunc(pkg, tc.name)
			call := newLowerTestCall(yield)
			fn.Body = tc.body(call)
			function := &Function{
				Func: fn,
				Sites: []Site{{
					ID: 1, Kind: SiteYield, Node: call,
				}},
			}
			plan := &Plan{Functions: map[*ir.Func]*Function{fn: function}}
			_, err := newLowerCandidate(plan, function)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("newLowerCandidate error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestLowerRejectsUnsupportedDefers(t *testing.T) {
	prepareLowerTest(t)

	oldTarget := typecheck.Target
	oldLocalPkg := types.LocalPkg
	defer func() {
		typecheck.Target = oldTarget
		types.LocalPkg = oldLocalPkg
	}()

	pkg := types.NewPkg("example.com/coro/lowerdefer", "lowerdefer")
	types.LocalPkg = pkg
	typecheck.Target = new(ir.Package)

	tests := []struct {
		name    string
		edge    func(*ir.Func, *ir.CallExpr) []Edge
		closure bool
		want    string
	}{
		{
			name: "missing-plan",
			edge: func(*ir.Func, *ir.CallExpr) []Edge {
				return nil
			},
			want: "defer has no static call plan",
		},
		{
			name: "wrong-edge-kind",
			edge: func(callee *ir.Func, call *ir.CallExpr) []Edge {
				return []Edge{{
					Kind: DirectCall, Callee: callee,
					CalleeName: symbolName(callee.Nname), Node: call,
				}}
			},
			want: "defer has no static call plan",
		},
		{
			name: "unknown-effect",
			edge: func(callee *ir.Func, call *ir.CallExpr) []Edge {
				return []Edge{{
					Kind: DeferCall, Callee: callee,
					CalleeName: symbolName(callee.Nname), Unknown: true,
					Node: call,
				}}
			},
			want: "defer target has unknown effects",
		},
		{
			name: "suspending",
			edge: func(callee *ir.Func, call *ir.CallExpr) []Edge {
				return []Edge{{
					Kind: DeferCall, Callee: callee,
					CalleeName: symbolName(callee.Nname),
					Imported:   FuncSummary{Effect: MaySuspend}, Node: call,
				}}
			},
			want: "suspending defer",
		},
		{
			name: "execution-constraint",
			edge: func(callee *ir.Func, call *ir.CallExpr) []Edge {
				return []Edge{{
					Kind: DeferCall, Callee: callee,
					CalleeName: symbolName(callee.Nname),
					Imported:   FuncSummary{Exec: NeedsPreempt}, Node: call,
				}}
			},
			want: "defer target has execution constraints preempt",
		},
		{
			name:    "closure",
			closure: true,
			edge: func(callee *ir.Func, call *ir.CallExpr) []Edge {
				return []Edge{{
					Kind: DeferCall, Callee: callee,
					CalleeName: symbolName(callee.Nname), Node: call,
				}}
			},
			want: "defer target is not a fixed direct call",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			yield := newLowerTestFunc(pkg, tc.name+"Yield")
			callee := newLowerTestFunc(pkg, tc.name+"Cleanup")
			var fun ir.Node = callee.Nname
			if tc.closure {
				callee = ir.NewClosureFunc(src.NoXPos, src.NoXPos,
					ir.OCLOSURE, types.NewSignature(nil, nil, nil),
					newLowerTestFunc(pkg, tc.name+"Outer"),
					typecheck.Target, 0)
				callee.DeclareParams(true)
				fun = callee.OClosure
			}
			deferCall := ir.NewCallExpr(src.NoXPos, ir.OCALLFUNC, fun, nil)
			deferCall.SetTypecheck(1)
			deferStmt := ir.NewGoDeferStmt(src.NoXPos, ir.ODEFER, deferCall)
			deferStmt.SetTypecheck(1)
			yieldCall := newLowerTestCall(yield)
			fn := newLowerTestFunc(pkg, tc.name)
			fn.Body = ir.Nodes{deferStmt, yieldCall, newLowerTestReturn()}
			function := &Function{
				Func:    fn,
				Local:   MaySuspend,
				Effect:  MaySuspend,
				Primary: CoroPrimary,
				Edges:   tc.edge(callee, deferCall),
				Sites: []Site{{
					ID: 1, Kind: SiteYield, Node: yieldCall,
				}},
			}
			plan := &Plan{Functions: map[*ir.Func]*Function{fn: function}}
			_, err := newLowerCandidate(plan, function)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("newLowerCandidate error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestLowerParametersAndResults(t *testing.T) {
	prepareLowerTest(t)

	oldTarget := typecheck.Target
	oldLocalPkg := types.LocalPkg
	oldCurFunc := ir.CurFunc
	defer func() {
		typecheck.Target = oldTarget
		types.LocalPkg = oldLocalPkg
		ir.CurFunc = oldCurFunc
	}()

	pkg := types.NewPkg("example.com/coro/lowerresult", "lowerresult")
	types.LocalPkg = pkg
	typecheck.Target = new(ir.Package)

	yield := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("yield"),
		types.NewSignature(nil, nil, nil))
	yield.DeclareParams(true)

	param := types.NewField(src.NoXPos, pkg.Lookup("value"), types.Types[types.TINT])
	resultField := types.NewField(src.NoXPos, nil, types.Types[types.TINT])
	leaf := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("leaf"),
		types.NewSignature(nil, []*types.Field{param}, []*types.Field{resultField}))
	leaf.DeclareParams(true)
	yieldCall := newLowerTestCall(yield)
	returnValue, _ := param.Nname.(ir.Node)
	ret := ir.NewReturnStmt(src.NoXPos, []ir.Node{returnValue})
	ret.SetTypecheck(1)
	leaf.Body = []ir.Node{yieldCall, ret}

	caller := newLowerTestFunc(pkg, "caller")
	leafCall := ir.NewCallExpr(src.NoXPos, ir.OCALLFUNC, leaf.Nname,
		[]ir.Node{ir.NewBasicLit(src.NoXPos, types.Types[types.TINT],
			constant.MakeInt64(42))})
	leafCall.SetType(types.Types[types.TINT])
	leafCall.SetTypecheck(1)
	output := ir.NewNameAt(src.NoXPos, pkg.Lookup("output"), types.Types[types.TINT])
	output.Class = ir.PEXTERN
	assign := ir.NewAssignStmt(src.NoXPos, output, leafCall)
	assign.SetTypecheck(1)
	caller.Body = []ir.Node{assign, newLowerTestReturn()}

	plan := &Plan{Functions: map[*ir.Func]*Function{
		leaf: {
			Func:    leaf,
			Local:   MaySuspend,
			Effect:  MaySuspend,
			Primary: CoroPrimary,
			Sites: []Site{{
				ID: 1, Kind: SiteYield, Node: yieldCall,
			}},
		},
		caller: {
			Func:    caller,
			Effect:  MaySuspend,
			Primary: CoroPrimary,
			Edges: []Edge{{
				Kind: DirectCall, Callee: leaf, CalleeName: symbolName(leaf.Nname),
				Node: leafCall,
			}},
			Sites: []Site{{
				ID: 1, Kind: SiteAwait, Node: leafCall,
			}},
		},
	}}
	result, err := Lower(plan)
	if err != nil {
		t.Fatalf("Lower failed: %v", err)
	}
	if result.Lowered != 2 || result.Skipped != 0 {
		t.Fatalf("Lower result = %+v, want 2 lowered and 0 skipped", result)
	}
}
