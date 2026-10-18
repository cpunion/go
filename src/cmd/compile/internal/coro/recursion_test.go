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

func TestPlainRecursiveExecution(t *testing.T) {
	prepareLowerTest(t)
	for _, mutual := range []bool{false, true} {
		pkg := types.NewPkg("example.com/coro/plain-recursion", "recursion")
		newFunc := func(name string) *ir.Func {
			fn := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup(name), types.NewSignature(nil, nil, nil))
			fn.Nname.Defn = fn
			fn.DeclareParams(true)
			return fn
		}
		first, second := newFunc("first"), newFunc("second")
		caller, leaf, launcher := newFunc("caller"), newFunc("leaf"), newFunc("launcher")
		functions := []*ir.Func{caller, first, leaf, launcher}
		first.Body = ir.Nodes{newLowerTestCall(first), newLowerTestReturn()}
		if mutual {
			first.Body = ir.Nodes{newLowerTestCall(second), newLowerTestReturn()}
			second.Body = ir.Nodes{newLowerTestCall(first), newLowerTestReturn()}
			functions = append(functions, second)
		}
		var closurePkg ir.Package
		cleanup := ir.NewClosureFunc(src.NoXPos, src.NoXPos, ir.OCLOSURE,
			types.NewSignature(nil, nil, nil), first, &closurePkg, 1)
		cleanup.Body = ir.Nodes{newLowerTestReturn()}
		cleanupCall := ir.NewCallExpr(src.NoXPos, ir.OCALLFUNC, cleanup.OClosure, nil)
		cleanupCall.SetTypecheck(1)
		deferred := ir.NewGoDeferStmt(src.NoXPos, ir.ODEFER, cleanupCall)
		deferred.SetTypecheck(1)
		first.Body = append(ir.Nodes{deferred}, first.Body...)
		functions = append(functions, cleanup)
		caller.Body = ir.Nodes{newLowerTestCall(first), newLowerTestReturn()}
		leaf.Body = ir.Nodes{newLowerTestReturn()}
		spawn := ir.NewGoDeferStmt(src.NoXPos, ir.OGO, newLowerTestCall(first))
		spawn.SetTypecheck(1)
		launcher.Body = ir.Nodes{spawn, newLowerTestReturn()}
		plan, err := Analyze(functions, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := plan.Verify(); err != nil {
			t.Fatal(err)
		}
		for _, fn := range functions {
			function := plan.Functions[fn]
			if function.Effect != NoSuspend || len(function.Seeds) != 0 {
				t.Fatalf("%s: recursion changed suspension effect", fn.Sym().Name)
			}
			if fn == leaf || fn == launcher || fn == cleanup {
				if function.Primary != PlainPrimary || function.Exec != 0 {
					t.Fatalf("%s: unrelated function needs a coroutine context", fn.Sym().Name)
				}
				continue
			}
			if function.Recursive != (fn != caller) || function.Primary != CoroPrimary ||
				function.Exec != NeedsPreempt || len(function.Sites) != 1 || function.Sites[0].Kind != SiteAwait {
				t.Fatalf("%s: missing recursive execution context: %+v", fn.Sym().Name, function)
			}
			if !plan.PreserveCall(fn) {
				t.Fatalf("%s: recursive entry may be inlined away", fn.Sym().Name)
			}
		}
		plan.PublishSummaries()
		imported := newFunc("imported")
		imported.Body = ir.Nodes{newLowerTestCall(first), newLowerTestReturn()}
		importPlan, err := Analyze([]*ir.Func{imported}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if function := importPlan.Functions[imported]; function.Effect != NoSuspend ||
			function.Exec != NeedsPreempt || function.Primary != CoroPrimary || len(function.Sites) != 1 {
			t.Fatal("imported summary lost recursive execution requirements")
		}
	}
}

func TestColoredRecursiveExecution(t *testing.T) {
	prepareLowerTest(t)
	for _, kind := range []string{"suspend", "panic", "goexit", "system"} {
		pkg := types.NewPkg("example.com/coro/colored-recursion", "recursion")
		fn := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("recursive"), types.NewSignature(nil, nil, nil))
		fn.Nname.Defn = fn
		fn.DeclareParams(true)
		var seed ir.Node
		if kind == "panic" {
			seed = ir.NewUnaryExpr(src.NoXPos, ir.OPANIC, ir.NewNilExpr(src.NoXPos, types.Types[types.TINTER]))
			seed.SetTypecheck(1)
		} else {
			name := "Gosched"
			if kind == "goexit" {
				name = "Goexit"
			}
			callee := ir.NewFunc(src.NoXPos, src.NoXPos, types.NewPkg("runtime", "runtime").Lookup(name), types.NewSignature(nil, nil, nil))
			if kind == "system" {
				callee = ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("foreign"), types.NewSignature(nil, nil, nil))
				SetSummary(callee, FuncSummary{Exec: NeedsSystemABI})
			}
			seed = newLowerTestCall(callee)
		}
		fn.Body = ir.Nodes{seed, newLowerTestCall(fn), newLowerTestReturn()}
		plan, err := Analyze([]*ir.Func{fn}, nil)
		if err != nil {
			t.Fatal(err)
		}
		function := plan.Functions[fn]
		wantExec := ExecFlags(0)
		if kind == "panic" {
			wantExec = NeedsPreempt
		} else if kind == "system" {
			wantExec = NeedsSystemABI
		}
		if !function.Recursive || function.Primary != CoroPrimary || function.Exec != wantExec {
			t.Fatalf("recursive execution flags = %s, want %s", function.Exec, wantExec)
		}
		wantEffect := NoSuspend
		if kind == "suspend" {
			wantEffect = MaySuspend
		}
		if function.Effect != wantEffect {
			t.Fatalf("%s: recursive suspension effect = %s, want %s", kind, function.Effect, wantEffect)
		}
	}
}
