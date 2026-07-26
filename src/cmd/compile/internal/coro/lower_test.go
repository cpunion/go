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

	directOp := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("directOp"),
		types.NewSignature(nil, nil, nil))
	directOp.DeclareParams(true)
	directCall := newLowerTestCall(directOp)
	blockOp := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("blockOp"),
		types.NewSignature(nil, nil, nil))
	blockOp.DeclareParams(true)
	blockCall := newLowerTestCall(blockOp)
	foreignYield := newLowerTestCall(yield)
	foreignCaller := newLowerTestFunc(pkg, "foreignCaller")
	foreignCaller.Body = []ir.Node{
		directCall, blockCall, foreignYield, newLowerTestReturn(),
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
	}
	result, err := Lower(&Plan{Functions: functions})
	if err != nil {
		t.Fatalf("Lower failed: %v", err)
	}
	if result.Lowered != 10 || result.Skipped != 0 {
		t.Fatalf("Lower result = %+v, want 10 lowered and 0 skipped", result)
	}

	for _, fn := range []*ir.Func{
		child, parent, spawned, spawner, sleeper, fileReader, socketReader,
		ordinaryFileReader, asyncCaller, foreignCaller,
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
