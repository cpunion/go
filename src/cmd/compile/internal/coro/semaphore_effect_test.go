// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/ir"
	"cmd/compile/internal/types"
	"cmd/internal/src"
	"testing"
)

func TestSemaphoreNoHandoff(t *testing.T) {
	prepareLowerTest(t)
	if semaphoreNoHandoff(nil) {
		t.Fatal("nil call is a non-handoff release")
	}
	for _, declaration := range semaphoreTestDeclarations {
		t.Run(declaration.path+"."+declaration.name, func(t *testing.T) {
			pkg := types.NewPkg(declaration.path, "sync")
			fn := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup(declaration.name),
				semaphoreTestSignature(pkg, declaration.params))
			fn.DeclareParams(true)
			call := newLowerTestCall(fn)
			call.Args = make(ir.Nodes, declaration.params)
			if semaphoreNoHandoff(call) {
				t.Fatal("missing operands are a non-handoff release")
			}
			if declaration.params > 1 {
				call.Args[1] = ir.NewBool(src.NoXPos, false)
			}
			want := declaration.operation == "coroSemrelease"
			if got := semaphoreNoHandoff(call); got != want {
				t.Fatalf("false handoff = %v, want %v", got, want)
			}
			if declaration.params > 1 {
				call.Args[1] = ir.NewBool(src.NoXPos, true)
				if semaphoreNoHandoff(call) {
					t.Fatal("true handoff is non-handoff")
				}
				call.Args[1] = fn.NewLocal(src.NoXPos, pkg.Lookup("unknown"), types.Types[types.TBOOL])
				if semaphoreNoHandoff(call) {
					t.Fatal("unknown handoff is non-handoff")
				}
				call.Args[1] = ir.NewBool(src.NoXPos, false)
			}
			call.Args = call.Args[:len(call.Args)-1]
			if semaphoreNoHandoff(call) {
				t.Fatal("changed arity is non-handoff")
			}
		})
	}
}

func TestSemaphoreNoHandoffEffects(t *testing.T) {
	prepareLowerTest(t)
	for _, local := range []bool{false, true} {
		for _, mode := range []string{"false", "true", "unknown"} {
			pkg := types.NewPkg("example.com/coro/semaphore-effect/"+mode, "effect")
			primitivePkg := types.NewPkg("sync", "sync")
			primitive := ir.NewFunc(src.NoXPos, src.NoXPos, primitivePkg.Lookup("runtime_Semrelease"),
				semaphoreTestSignature(primitivePkg, 3))
			fn := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("release"), semaphoreTestSignature(pkg, 3))
			fn.DeclareParams(true)
			call := newLowerTestCall(primitive)
			for _, arg := range fn.Dcl {
				call.Args = append(call.Args, arg)
			}
			if mode != "unknown" {
				call.Args[1] = ir.NewBool(src.NoXPos, mode == "true")
			}
			fn.Body = ir.Nodes{call, newLowerTestReturn()}
			funcs := []*ir.Func{fn}
			if local {
				funcs = append(funcs, primitive)
			}
			plan, err := Analyze(funcs, nil)
			if err != nil {
				t.Fatal(err)
			}
			function := plan.Functions[fn]
			wantEffect, wantSites := MaySuspend, 1
			if mode == "false" {
				wantEffect, wantSites = NoSuspend, 0
			}
			if function.Effect != wantEffect || len(function.Sites) != wantSites {
				t.Fatalf("local=%v %s: effect=%s sites=%v, want %s/%d", local, mode,
					function.Effect, function.Sites, wantEffect, wantSites)
			}
			// The no-suspend result must survive the same summary publication
			// and import path used by a named deferred Done caller.
			plan.PublishSummaries()
			caller := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("caller"), fn.Type())
			caller.DeclareParams(true)
			imported := newLowerTestCall(fn)
			for _, arg := range caller.Dcl {
				imported.Args = append(imported.Args, arg)
			}
			caller.Body = ir.Nodes{imported, newLowerTestReturn()}
			importPlan, err := Analyze([]*ir.Func{caller}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := importPlan.Functions[caller].Effect; got != wantEffect {
				t.Fatalf("local=%v imported %s: effect=%s, want %s", local, mode, got, wantEffect)
			}
		}
	}
}

func TestSemaphoreNoHandoffVariable(t *testing.T) {
	prepareLowerTest(t)
	for _, mode := range []string{"immutable", "reassigned", "address", "parameter", "wrong-type"} {
		t.Run(mode, func(t *testing.T) {
			pkg := types.NewPkg("sync", "sync")
			primitive := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("runtime_Semrelease"),
				semaphoreTestSignature(pkg, 3))
			fn := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("release"), primitive.Type())
			fn.DeclareParams(true)
			flag := fn.NewLocal(src.NoXPos, pkg.Lookup("handoff"), types.Types[types.TBOOL])
			assign := ir.NewAssignStmt(src.NoXPos, flag, ir.NewBool(src.NoXPos, false))
			assign.Def, flag.Defn = true, assign
			fn.Body = ir.Nodes{ir.NewDecl(src.NoXPos, ir.ODCL, flag), assign}
			call := newLowerTestCall(primitive)
			call.Args = ir.Nodes{fn.Dcl[0], flag, fn.Dcl[2]}
			switch mode {
			case "reassigned":
				fn.Body.Append(ir.NewAssignStmt(src.NoXPos, flag, ir.NewBool(src.NoXPos, true)))
			case "address":
				flag.SetAddrtaken(true)
			case "parameter":
				call.Args[1] = fn.Dcl[1]
			case "wrong-type":
				params := []*types.Field{
					types.NewField(src.NoXPos, nil, types.NewPtr(types.Types[types.TUINT32])),
					types.NewField(src.NoXPos, nil, types.Types[types.TINT]),
					types.NewField(src.NoXPos, nil, types.Types[types.TINT]),
				}
				bad := ir.NewFunc(src.NoXPos, src.NoXPos, primitive.Sym(), types.NewSignature(nil, params, nil))
				call.Fun = bad.Nname
			}
			if got := semaphoreNoHandoff(call); got != (mode == "immutable") {
				t.Fatalf("non-handoff=%v, want %v", got, mode == "immutable")
			}
		})
	}
}

func TestSemaphoreNoHandoffInit(t *testing.T) {
	prepareLowerTest(t)
	pkg := types.NewPkg("example.com/coro/semaphore-init", "effect")
	primitivePkg := types.NewPkg("sync", "sync")
	primitive := ir.NewFunc(src.NoXPos, src.NoXPos, primitivePkg.Lookup("runtime_Semrelease"),
		semaphoreTestSignature(primitivePkg, 3))
	fn := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("release"), semaphoreTestSignature(pkg, 3))
	fn.DeclareParams(true)
	call := newLowerTestCall(primitive)
	call.Args = ir.Nodes{fn.Dcl[0], ir.NewBool(src.NoXPos, false), fn.Dcl[2]}
	yield := ir.NewFunc(src.NoXPos, src.NoXPos, types.NewPkg("runtime", "runtime").Lookup("Gosched"),
		types.NewSignature(nil, nil, nil))
	init := newLowerTestCall(yield)
	call.SetInit(ir.Nodes{init})
	fn.Body = ir.Nodes{call, newLowerTestReturn()}
	plan, err := Analyze([]*ir.Func{fn}, nil)
	if err != nil {
		t.Fatal(err)
	}
	function := plan.Functions[fn]
	if function.Effect != MaySuspend || len(function.Sites) != 1 || function.Sites[0].Node != init ||
		function.Sites[0].Kind != SiteYield || len(call.Init()) != 1 || call.Init()[0] != init {
		t.Fatal("non-handoff refinement lost or moved a suspending initializer")
	}
}
