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

func TestResumeFactorySupported(t *testing.T) {
	prepareLowerTest(t)

	if resumeFactorySupported(nil) {
		t.Fatal("nil function supports a resume factory")
	}
	pkg := types.NewPkg("example.com/coro/factorysupport", "factorysupport")
	fn := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("F"),
		types.NewSignature(nil, nil, nil))
	if !resumeFactorySupported(fn) {
		t.Fatal("plain function does not support a resume factory")
	}
	recv := types.NewField(src.NoXPos, pkg.Lookup("recv"),
		types.NewStruct(nil))
	method := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("M"),
		types.NewSignature(recv, nil, nil))
	if resumeFactorySupported(method) {
		t.Fatal("method supports a resume factory")
	}
	results := []*types.Field{
		types.NewField(src.NoXPos, nil, types.Types[types.TINT]),
		types.NewField(src.NoXPos, nil, types.Types[types.TINT]),
	}
	pair := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("Pair"),
		types.NewSignature(nil, nil, results))
	if !resumeFactorySupported(pair) {
		t.Fatal("multi-result function does not support a resume factory")
	}
	shapeType := types.NewSignature(nil, nil, nil)
	shapeType.SetHasShape(true)
	shape := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("Shape"), shapeType)
	if resumeFactorySupported(shape) {
		t.Fatal("generic shape supports a resume factory")
	}
	if _, err := newLowerCandidate(&Plan{}, &Function{Func: shape}); err == nil ||
		!strings.Contains(err.Error(), "generic shape") {
		t.Fatalf("newLowerCandidate error = %v, want generic-shape rejection",
			err)
	}
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

func TestLowerNestedSystemABIUsesFactory(t *testing.T) {
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
			}, {
				ID: 2, Kind: SiteAwait, Node: childCall,
			}},
		},
	}}
	result, err := Lower(plan)
	if err != nil {
		t.Fatalf("Lower failed: %v", err)
	}
	if result.Lowered != 2 || result.Skipped != 0 {
		t.Fatalf("Lower result = %+v, want 2 lowered and none skipped", result)
	}
	if len(result.Diagnostics) != 0 {
		t.Fatalf("Lower diagnostics = %q, want none", result.Diagnostics)
	}

	wantFactory := symbolName(child.Nname) + ".coro"
	var factoryCalls, wrapperCalls int
	for _, fn := range typecheck.Target.Funcs {
		if fn.ClosureParent == nil || fn.ClosureParent.Sym() == nil ||
			fn.ClosureParent.Sym().Name != parent.Sym().Name+".coro" {
			continue
		}
		ir.Visit(fn, func(node ir.Node) {
			call, ok := node.(*ir.CallExpr)
			if !ok {
				return
			}
			switch symbolName(ir.StaticCalleeName(ir.StaticValue(call.Fun))) {
			case wantFactory:
				factoryCalls++
			case symbolName(child.Nname):
				wrapperCalls++
			}
		})
	}
	if factoryCalls != 1 {
		t.Fatalf("generated IR has %d calls to %s, want 1",
			factoryCalls, wantFactory)
	}
	if wrapperCalls != 0 {
		t.Fatalf("generated IR has %d calls to ordinary child wrapper, want 0",
			wrapperCalls)
	}
}

func TestLowerImportedFactory(t *testing.T) {
	prepareLowerTest(t)

	oldTarget := typecheck.Target
	oldLocalPkg := types.LocalPkg
	oldCurFunc := ir.CurFunc
	defer func() {
		typecheck.Target = oldTarget
		types.LocalPkg = oldLocalPkg
		ir.CurFunc = oldCurFunc
	}()

	for _, test := range []struct {
		name       string
		factory    FactoryABI
		lowered    int
		skipped    int
		callers    int
		diagnostic string
	}{
		{
			name: "available", factory: FactoryABI1,
			lowered: 2, callers: 2,
		},
		{
			name: "missing", skipped: 1, callers: 1,
			diagnostic: "unsupported coroutine dependency",
		},
		{
			name: "unknown", factory: FactoryABI(2),
			skipped: 1, callers: 1,
			diagnostic: "unsupported coroutine dependency",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			pkg := types.NewPkg("example.com/coro/factorycaller/"+test.name,
				"factorycaller")
			leafPkg := types.NewPkg("example.com/coro/factoryleaf/"+test.name,
				"factoryleaf")
			types.LocalPkg = pkg
			typecheck.Target = new(ir.Package)
			ir.CurFunc = nil

			param := types.NewField(src.NoXPos, leafPkg.Lookup("value"),
				types.Types[types.TINT])
			resultField := types.NewField(src.NoXPos, nil,
				types.Types[types.TINT])
			leaf := ir.NewFunc(src.NoXPos, src.NoXPos,
				leafPkg.Lookup("Suspend"),
				types.NewSignature(nil, []*types.Field{param},
					[]*types.Field{resultField}))
			leaf.DeclareParams(true)
			summary := FuncSummary{
				Effect: MaySuspend, Factory: test.factory,
			}
			SetSummary(leaf, summary)

			plan := &Plan{
				Functions: make(map[*ir.Func]*Function),
			}
			var functions []*Function
			for i := 0; i < test.callers; i++ {
				call := ir.NewCallExpr(src.NoXPos, ir.OCALLFUNC, leaf.Nname,
					ir.Nodes{ir.NewBasicLit(src.NoXPos,
						types.Types[types.TINT], constant.MakeInt64(41))})
				call.SetType(types.Types[types.TINT])
				call.SetTypecheck(1)
				output := ir.NewNameAt(src.NoXPos,
					pkg.LookupNum("output", i), types.Types[types.TINT])
				output.Class = ir.PEXTERN
				assign := ir.NewAssignStmt(src.NoXPos, output, call)
				assign.SetTypecheck(1)

				caller := newLowerTestFunc(pkg,
					pkg.LookupNum("caller", i).Name)
				statement := ir.Node(assign)
				if test.factory == FactoryABI1 && i == 1 {
					statement = call
				}
				caller.Body = ir.Nodes{statement, newLowerTestReturn()}
				function := &Function{
					Func:    caller,
					Effect:  MaySuspend,
					Primary: CoroPrimary,
					Edges: []Edge{{
						Kind: DirectCall, Callee: leaf,
						CalleeName: symbolName(leaf.Nname),
						Imported:   summary, Node: call,
					}},
					Sites: []Site{{
						ID: 1, Kind: SiteAwait, Node: call,
					}},
				}
				plan.Functions[caller] = function
				functions = append(functions, function)
			}

			result, err := Lower(plan)
			if err != nil {
				t.Fatalf("Lower failed: %v", err)
			}
			if result.Lowered != test.lowered ||
				result.Skipped != test.skipped {
				t.Fatalf("Lower result = %+v, want %d lowered and %d skipped",
					result, test.lowered, test.skipped)
			}
			if test.diagnostic != "" {
				if len(result.Diagnostics) != 1 ||
					!strings.Contains(result.Diagnostics[0], test.diagnostic) {
					t.Fatalf("Lower diagnostics = %q, want %q",
						result.Diagnostics, test.diagnostic)
				}
				for _, function := range functions {
					if function.Factory != NoFactory {
						t.Fatalf("skipped function factory = %v, want none",
							function.Factory)
					}
				}
				return
			}
			if len(result.Diagnostics) != 0 {
				t.Fatalf("Lower diagnostics = %q, want none",
					result.Diagnostics)
			}
			for _, function := range functions {
				if function.Factory != FactoryABI1 {
					t.Fatalf("lowered function factory = %v, want v1",
						function.Factory)
				}
			}

			wantFactory := symbolName(leaf.Nname) + ".coro"
			var factoryCalls, wrapperCalls int
			for _, fn := range typecheck.Target.Funcs {
				ir.Visit(fn, func(node ir.Node) {
					call, ok := node.(*ir.CallExpr)
					if !ok {
						return
					}
					name := ir.StaticCalleeName(ir.StaticValue(call.Fun))
					switch symbolName(name) {
					case wantFactory:
						factoryCalls++
					case symbolName(leaf.Nname):
						wrapperCalls++
					}
				})
			}
			if factoryCalls != test.callers {
				t.Fatalf("generated IR has %d calls to %s, want %d",
					factoryCalls, wantFactory, test.callers)
			}
			if wrapperCalls != 0 {
				t.Fatalf("generated IR has %d calls to ordinary wrapper, want 0",
					wrapperCalls)
			}
		})
	}
}

func TestLowerRejectsSpawnResults(t *testing.T) {
	prepareLowerTest(t)

	oldTarget := typecheck.Target
	oldLocalPkg := types.LocalPkg
	defer func() {
		typecheck.Target = oldTarget
		types.LocalPkg = oldLocalPkg
	}()

	pkg := types.NewPkg("example.com/coro/spawnresults", "spawnresults")
	types.LocalPkg = pkg
	typecheck.Target = new(ir.Package)

	result := types.NewField(src.NoXPos, nil, types.Types[types.TINT])
	child := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("child"),
		types.NewSignature(nil, nil, []*types.Field{result}))
	child.DeclareParams(true)
	call := newLowerTestCall(child)
	spawn := ir.NewGoDeferStmt(src.NoXPos, ir.OGO, call)
	spawn.SetTypecheck(1)
	parent := newLowerTestFunc(pkg, "parent")
	parent.Body = ir.Nodes{spawn, newLowerTestReturn()}
	function := &Function{
		Func: parent,
		Edges: []Edge{{
			Kind: GoCall, Callee: child,
			CalleeName: symbolName(child.Nname), Node: call,
		}},
		Sites: []Site{{
			ID: 1, Kind: SiteSpawn, Node: call,
		}},
	}
	plan := &Plan{Functions: map[*ir.Func]*Function{parent: function}}
	if _, err := newLowerCandidate(plan, function); err == nil ||
		!strings.Contains(err.Error(), "spawn target returns values") {
		t.Fatalf("newLowerCandidate error = %v, want result rejection", err)
	}
}

func TestLowerRejectsAwaitResults(t *testing.T) {
	prepareLowerTest(t)

	oldTarget := typecheck.Target
	oldLocalPkg := types.LocalPkg
	defer func() {
		typecheck.Target = oldTarget
		types.LocalPkg = oldLocalPkg
	}()

	pkg := types.NewPkg("example.com/coro/awaitresults", "awaitresults")
	types.LocalPkg = pkg
	typecheck.Target = new(ir.Package)

	results := []*types.Field{
		types.NewField(src.NoXPos, nil, types.Types[types.TINT]),
		types.NewField(src.NoXPos, nil, types.Types[types.TINT]),
	}
	pair := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("pair"),
		types.NewSignature(nil, nil, results))
	pair.DeclareParams(true)
	single := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("single"),
		types.NewSignature(nil, nil, results[:1]))
	single.DeclareParams(true)

	for _, test := range []struct {
		name string
		body func(*ir.Func) (*ir.CallExpr, ir.Nodes)
		want string
	}{
		{
			name: "complex-normalized",
			body: func(caller *ir.Func) (*ir.CallExpr, ir.Nodes) {
				call := newLowerTestCall(pair)
				call.SetType(pair.Type().ResultsTuple())
				first := caller.NewLocal(src.NoXPos, pkg.Lookup("first"),
					types.Types[types.TINT])
				second := caller.NewLocal(src.NoXPos, pkg.Lookup("second"),
					types.Types[types.TINT])
				inner := ir.NewAssignListStmt(src.NoXPos, ir.OAS2FUNC,
					ir.Nodes{first, second}, ir.Nodes{call})
				projection := ir.NewConvExpr(src.NoXPos, ir.OCONVNOP,
					types.Types[types.TINT], first)
				projection.SetInit(ir.Nodes{inner})
				target := caller.NewLocal(src.NoXPos, pkg.Lookup("target"),
					types.Types[types.TINT])
				outer := ir.NewAssignListStmt(src.NoXPos, ir.OAS2,
					ir.Nodes{ir.NewStarExpr(src.NoXPos, target), second},
					ir.Nodes{projection, second})
				return call, ir.Nodes{outer, newLowerTestReturn()}
			},
			want: "without matching assignment",
		},
		{
			name: "direct-complex",
			body: func(caller *ir.Func) (*ir.CallExpr, ir.Nodes) {
				call := newLowerTestCall(single)
				call.SetType(types.Types[types.TINT])
				target := caller.NewLocal(src.NoXPos, pkg.Lookup("pointer"),
					types.NewPtr(types.Types[types.TINT]))
				assign := ir.NewAssignStmt(src.NoXPos,
					ir.NewStarExpr(src.NoXPos, target), call)
				return call, ir.Nodes{assign, newLowerTestReturn()}
			},
			want: "result 0 is not a variable",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			caller := newLowerTestFunc(pkg, test.name)
			call, body := test.body(caller)
			caller.Body = body
			callee := pair
			if test.name == "direct-complex" {
				callee = single
			}
			function := &Function{
				Func:    caller,
				Effect:  MaySuspend,
				Primary: CoroPrimary,
				Edges: []Edge{{
					Kind: DirectCall, Callee: callee,
					CalleeName: symbolName(callee.Nname), Node: call,
				}},
				Sites: []Site{{
					ID: 1, Kind: SiteAwait, Node: call,
				}},
			}
			plan := &Plan{Functions: map[*ir.Func]*Function{
				caller: function,
			}}
			if _, err := newLowerCandidate(plan, function); err == nil ||
				!strings.Contains(err.Error(), test.want) {
				t.Fatalf("newLowerCandidate error = %v, want %q",
					err, test.want)
			}
		})
	}
}

func TestCallResultTargets(t *testing.T) {
	prepareLowerTest(t)

	pkg := types.NewPkg("example.com/coro/resulttargets", "resulttargets")
	fn := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("call"),
		types.NewSignature(nil, nil, nil))
	call := newLowerTestCall(fn)
	if got, err := callResultTargets(call, call, 0); err != nil ||
		len(got) != 0 {
		t.Fatalf("no-result call targets = %v, %v, want none", got, err)
	}
	if got, err := callResultTargets(call, nil, 0); err != nil ||
		len(got) != 0 {
		t.Fatalf("spawn targets = %v, %v, want none", got, err)
	}
	nestedNoResult := newLowerTestCall(fn)
	if _, err := callResultTargets(call, nestedNoResult, 0); err == nil ||
		!strings.Contains(err.Error(), "nested in another statement") {
		t.Fatalf("nested no-result error = %v", err)
	}
	if got, err := callResultTargets(call, call, 1); err != nil ||
		len(got) != 1 || got[0] != nil {
		t.Fatalf("discarded call targets = %v, %v, want one empty target",
			got, err)
	}

	target := ir.NewNameAt(src.NoXPos, pkg.Lookup("target"),
		types.Types[types.TINT])
	assign := ir.NewAssignStmt(src.NoXPos, target, call)
	if got, err := callResultTargets(call, assign, 1); err != nil ||
		len(got) != 1 || got[0] != target {
		t.Fatalf("assignment targets = %v, %v, want %v", got, err, target)
	}

	list := ir.NewAssignListStmt(src.NoXPos, ir.OAS2,
		ir.Nodes{target, ir.BlankNode}, ir.Nodes{call})
	if got, err := callResultTargets(call, list, 2); err != nil ||
		len(got) != 2 || got[0] != target || got[1] != nil {
		t.Fatalf("list targets = %v, %v, want [%v <nil>]",
			got, err, target)
	}

	if _, err := callResultTargets(call, assign, 2); err == nil ||
		!strings.Contains(err.Error(), "without matching assignment") {
		t.Fatalf("mismatched target error = %v", err)
	}

	outer := newLowerTestCall(fn)
	outer.SetInit(ir.Nodes{list})
	if _, err := callResultTargets(call, outer, 2); err == nil ||
		!strings.Contains(err.Error(), "without matching assignment") {
		t.Fatalf("nested target error = %v", err)
	}

	multiCall := newLowerTestCall(fn)
	second := ir.NewNameAt(src.NoXPos, pkg.Lookup("second"),
		types.Types[types.TINT])
	inner := ir.NewAssignListStmt(src.NoXPos, ir.OAS2FUNC,
		ir.Nodes{target, second}, ir.Nodes{multiCall})
	projection := ir.NewConvExpr(src.NoXPos, ir.OCONVNOP,
		types.Types[types.TINT], target)
	projection.SetInit(ir.Nodes{inner})
	outerTarget := ir.NewNameAt(src.NoXPos, pkg.Lookup("outer"),
		types.Types[types.TINT])
	normalized := ir.NewAssignListStmt(src.NoXPos, ir.OAS2,
		ir.Nodes{outerTarget, ir.BlankNode}, ir.Nodes{projection, second})
	if got, err := callResultTargets(multiCall, normalized, 2); err != nil ||
		len(got) != 2 || got[0] != target || got[1] != second {
		t.Fatalf("normalized targets = %v, %v, want [%v %v]",
			got, err, target, second)
	}

	inner.Lhs[1] = ir.BlankNode
	normalized.Rhs[1] = ir.BlankNode
	if got, err := callResultTargets(multiCall, normalized, 2); err != nil ||
		len(got) != 2 || got[0] != target || got[1] != nil {
		t.Fatalf("normalized blank targets = %v, %v, want [%v <nil>]",
			got, err, target)
	}
	inner.Lhs[1] = second
	normalized.Rhs[1] = second

	missing := ir.NewAssignListStmt(src.NoXPos, ir.OAS2,
		ir.Nodes{outerTarget, ir.BlankNode}, ir.Nodes{target, second})
	if _, err := callResultTargets(multiCall, missing, 2); err == nil ||
		!strings.Contains(err.Error(), "without matching assignment") {
		t.Fatalf("missing normalized assignment error = %v", err)
	}
	notInitialized := ir.NewAssignListStmt(src.NoXPos, ir.OAS2FUNC,
		ir.Nodes{outerTarget}, ir.Nodes{multiCall})
	if _, ok := normalizedResultAssignment(multiCall, notInitialized, 1); ok {
		t.Fatal("direct assignment is a normalized result assignment")
	}

	normalized.Rhs[1] = target
	if _, err := callResultTargets(multiCall, normalized, 2); err == nil ||
		!strings.Contains(err.Error(), "without matching assignment") {
		t.Fatalf("mismatched normalized projection error = %v", err)
	}
	normalized.Rhs[1] = second

	normalized.Lhs[0] = ir.NewStarExpr(src.NoXPos, outerTarget)
	if _, err := callResultTargets(multiCall, normalized, 2); err == nil ||
		!strings.Contains(err.Error(), "without matching assignment") {
		t.Fatalf("complex normalized target error = %v", err)
	}
	normalized.Lhs[0] = outerTarget

	unsupported := ir.NewConvExpr(src.NoXPos, ir.OCONVIFACE,
		types.Types[types.TINT], target)
	if isResultProjection(unsupported, target) {
		t.Fatal("interface conversion is a result projection")
	}

	if got := takeCallInit(normalized, multiCall); len(got) != 1 ||
		got[0] != inner {
		t.Fatalf("normalized call init = %v, want [%v]", got, inner)
	}
	if got := takeCallInit(normalized, multiCall); len(got) != 0 {
		t.Fatalf("normalized call init after take = %v, want none", got)
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

func TestLowerNormalizedResults(t *testing.T) {
	prepareLowerTest(t)

	oldTarget := typecheck.Target
	oldLocalPkg := types.LocalPkg
	oldCurFunc := ir.CurFunc
	defer func() {
		typecheck.Target = oldTarget
		types.LocalPkg = oldLocalPkg
		ir.CurFunc = oldCurFunc
	}()

	pkg := types.NewPkg("example.com/coro/normalizedresult",
		"normalizedresult")
	types.LocalPkg = pkg
	typecheck.Target = new(ir.Package)

	yield := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("yield"),
		types.NewSignature(nil, nil, nil))
	yield.DeclareParams(true)

	resultFields := []*types.Field{
		types.NewField(src.NoXPos, nil, types.Types[types.TINT]),
		types.NewField(src.NoXPos, nil, types.Types[types.TINT]),
	}
	leaf := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("leaf"),
		types.NewSignature(nil, nil, resultFields))
	leaf.DeclareParams(true)
	typecheck.Target.Funcs = append(typecheck.Target.Funcs, leaf)
	yieldCall := newLowerTestCall(yield)
	ret := ir.NewReturnStmt(src.NoXPos, ir.Nodes{
		ir.NewBasicLit(src.NoXPos, types.Types[types.TINT],
			constant.MakeInt64(1)),
		ir.NewBasicLit(src.NoXPos, types.Types[types.TINT],
			constant.MakeInt64(2)),
	})
	ret.SetTypecheck(1)
	leaf.Body = ir.Nodes{yieldCall, ret}

	caller := newLowerTestFunc(pkg, "caller")
	first := caller.NewLocal(src.NoXPos, pkg.Lookup("first"),
		types.Types[types.TINT])
	second := caller.NewLocal(src.NoXPos, pkg.Lookup("second"),
		types.Types[types.TINT])
	resultFirst := caller.NewLocal(src.NoXPos, pkg.Lookup("resultFirst"),
		types.Types[types.TINT])
	resultSecond := caller.NewLocal(src.NoXPos, pkg.Lookup("resultSecond"),
		types.Types[types.TINT])
	directFirst := caller.NewLocal(src.NoXPos, pkg.Lookup("directFirst"),
		types.Types[types.TINT])
	directSecond := caller.NewLocal(src.NoXPos, pkg.Lookup("directSecond"),
		types.Types[types.TINT])
	initTemp := caller.NewLocal(src.NoXPos, pkg.Lookup("initTemp"),
		types.Types[types.TINT])

	leafCall := newLowerTestCall(leaf)
	leafCall.SetType(leaf.Type().ResultsTuple())
	inner := ir.NewAssignListStmt(src.NoXPos, ir.OAS2FUNC,
		ir.Nodes{first, second}, ir.Nodes{leafCall})
	inner.SetTypecheck(1)
	firstProjection := ir.NewConvExpr(src.NoXPos, ir.OCONVNOP,
		types.Types[types.TINT], first)
	firstProjection.SetTypecheck(1)
	firstProjection.SetInit(ir.Nodes{inner})
	outer := ir.NewAssignListStmt(src.NoXPos, ir.OAS2,
		ir.Nodes{resultFirst, resultSecond},
		ir.Nodes{firstProjection, second})
	outer.SetTypecheck(1)
	outer.SetInit(ir.Nodes{ir.NewDecl(src.NoXPos, ir.ODCL, initTemp)})
	discardCall := newLowerTestCall(leaf)
	discardCall.SetType(leaf.Type().ResultsTuple())
	prelude := ir.NewAssignStmt(src.NoXPos, resultFirst,
		ir.NewBasicLit(src.NoXPos, types.Types[types.TINT],
			constant.MakeInt64(0)))
	prelude.SetTypecheck(1)
	prelude.SetInit(ir.Nodes{discardCall})
	directCall := newLowerTestCall(leaf)
	directCall.SetType(leaf.Type().ResultsTuple())
	direct := ir.NewAssignListStmt(src.NoXPos, ir.OAS2FUNC,
		ir.Nodes{directFirst, directSecond}, ir.Nodes{directCall})
	direct.SetTypecheck(1)
	caller.Body = ir.Nodes{prelude, direct, outer, newLowerTestReturn()}

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
				Kind: DirectCall, Callee: leaf,
				CalleeName: symbolName(leaf.Nname), Node: leafCall,
			}, {
				Kind: DirectCall, Callee: leaf,
				CalleeName: symbolName(leaf.Nname), Node: discardCall,
			}, {
				Kind: DirectCall, Callee: leaf,
				CalleeName: symbolName(leaf.Nname), Node: directCall,
			}},
			Sites: []Site{
				{ID: 1, Kind: SiteAwait, Node: discardCall},
				{ID: 2, Kind: SiteAwait, Node: directCall},
				{ID: 3, Kind: SiteAwait, Node: leafCall},
			},
		},
	}}
	result, err := Lower(plan)
	if err != nil {
		t.Fatalf("Lower failed: %v", err)
	}
	if result.Lowered != 2 || result.Skipped != 0 {
		t.Fatalf("Lower result = %+v, want 2 lowered and 0 skipped", result)
	}

	wantFactory := symbolName(leaf.Nname) + ".coro"
	var factoryCalls, wrapperCalls int
	for _, fn := range typecheck.Target.Funcs {
		ir.Visit(fn, func(node ir.Node) {
			call, ok := node.(*ir.CallExpr)
			if !ok {
				return
			}
			switch symbolName(ir.StaticCalleeName(ir.StaticValue(call.Fun))) {
			case wantFactory:
				factoryCalls++
			case symbolName(leaf.Nname):
				wrapperCalls++
			}
		})
	}
	if factoryCalls != 4 {
		t.Fatalf("generated IR has %d calls to %s, want 4",
			factoryCalls, wantFactory)
	}
	if wrapperCalls != 0 {
		t.Fatalf("generated IR has %d calls to ordinary wrapper, want 0",
			wrapperCalls)
	}
}
