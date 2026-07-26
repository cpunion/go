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
	}
	result, err := Lower(&Plan{Functions: functions})
	if err != nil {
		t.Fatalf("Lower failed: %v", err)
	}
	if result.Lowered != 4 || result.Skipped != 0 {
		t.Fatalf("Lower result = %+v, want 4 lowered and 0 skipped", result)
	}

	for _, fn := range []*ir.Func{child, parent, spawned, spawner} {
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
