// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/ir"
	"cmd/compile/internal/typecheck"
	"cmd/compile/internal/types"
	"cmd/internal/src"
	"testing"
)

func TestSpawnExecution(t *testing.T) {
	prepareLowerTest(t)
	pkg := types.NewPkg("example.com/coro/spawn", "spawn")
	newFunc := func(name string) *ir.Func {
		fn := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup(name), types.NewSignature(nil, nil, nil))
		fn.Nname.Defn = fn
		fn.DeclareParams(true)
		return fn
	}
	worker, spawn, caller, leaf := newFunc("worker"), newFunc("spawn"), newFunc("caller"), newFunc("leaf")
	runtimePkg := types.NewPkg("runtime", "runtime")
	yield := ir.NewFunc(src.NoXPos, src.NoXPos, runtimePkg.Lookup("Gosched"), types.NewSignature(nil, nil, nil))
	worker.Body = ir.Nodes{newLowerTestCall(yield), newLowerTestReturn()}
	goStmt := ir.NewGoDeferStmt(src.NoXPos, ir.OGO, newLowerTestCall(worker))
	goStmt.SetTypecheck(1)
	spawn.Body = ir.Nodes{goStmt, newLowerTestReturn()}
	caller.Body = ir.Nodes{newLowerTestCall(spawn), newLowerTestReturn()}
	leaf.Body = ir.Nodes{newLowerTestReturn()}
	plan, err := Analyze([]*ir.Func{worker, spawn, caller, leaf}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.Verify(); err != nil {
		t.Fatal(err)
	}
	for _, fn := range []*ir.Func{spawn, caller} {
		function := plan.Functions[fn]
		if function.Effect != NoSuspend || len(function.Seeds) != 0 ||
			function.Exec != NeedsTaskContext || function.Primary != CoroPrimary {
			t.Fatalf("%s: task context changed suspension effect: %+v", fn.Sym().Name, function)
		}
		if !plan.PreserveCall(fn) {
			t.Fatalf("%s: task-creating call may be inlined away", fn.Sym().Name)
		}
		want := SiteAwait
		if fn == spawn {
			want = SiteSpawn
		}
		if len(function.Sites) != 1 || function.Sites[0].Kind != want {
			t.Fatalf("%s: missing %s site", fn.Sym().Name, want)
		}
	}
	if function := plan.Functions[worker]; function.Exec != 0 || function.Effect != MaySuspend {
		t.Fatal("task context propagated from creator to child")
	}
	if function := plan.Functions[leaf]; function.Exec != 0 || function.Primary != PlainPrimary {
		t.Fatal("unrelated leaf requires a task context")
	}
	deferred := newFunc("deferred")
	deferStmt := ir.NewGoDeferStmt(src.NoXPos, ir.ODEFER, newLowerTestCall(spawn))
	deferStmt.SetTypecheck(1)
	deferred.Body = ir.Nodes{deferStmt, newLowerTestReturn()}
	deferPlan, err := Analyze([]*ir.Func{deferred, spawn, worker}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if function := deferPlan.Functions[deferred]; function.Effect != NoSuspend ||
		function.Exec != NeedsTaskContext || function.Primary != CoroPrimary {
		t.Fatal("deferred task creation lost its execution requirement")
	}
	plan.PublishSummaries()
	imported := newFunc("imported")
	imported.Body = ir.Nodes{newLowerTestCall(caller), newLowerTestReturn()}
	importPlan, err := Analyze([]*ir.Func{imported}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if function := importPlan.Functions[imported]; function.Effect != NoSuspend ||
		function.Exec != NeedsTaskContext || function.Primary != CoroPrimary ||
		len(function.Sites) != 1 || function.Sites[0].Kind != SiteAwait {
		t.Fatal("imported summary lost task-creation requirements")
	}
}

func TestPlainSpawnFactory(t *testing.T) {
	prepareLowerTest(t)
	oldPkg, oldTarget := types.LocalPkg, typecheck.Target
	defer func() { types.LocalPkg, typecheck.Target = oldPkg, oldTarget }()
	pkg := types.NewPkg("example.com/coro/plainspawn", "plainspawn")
	types.LocalPkg, typecheck.Target = pkg, new(ir.Package)
	leaf, unused, caller := newLowerTestFunc(pkg, "leaf"), newLowerTestFunc(pkg, "unused"), newLowerTestFunc(pkg, "caller")
	leafReturn := newLowerTestReturn()
	leaf.Body = ir.Nodes{leafReturn}
	unused.Body = ir.Nodes{newLowerTestReturn()}
	goStmt := ir.NewGoDeferStmt(src.NoXPos, ir.OGO, newLowerTestCall(leaf))
	goStmt.SetTypecheck(1)
	caller.Body = ir.Nodes{goStmt, newLowerTestReturn()}
	plan, err := Analyze([]*ir.Func{caller, leaf, unused}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, fn := range []*ir.Func{leaf, unused} {
		if function := plan.Functions[fn]; !plainSpawnFactorySupported(function) ||
			function.Primary != PlainPrimary || function.Exec != 0 || len(function.Sites) != 0 {
			t.Fatalf("plain leaf requires execution context: %+v", function)
		}
	}
	result, err := Lower(plan, nil)
	if err != nil || result.Lowered != 1 || result.Skipped != 0 {
		t.Fatalf("Lower = (%+v, %v), want only the creator rewritten", result, err)
	}
	if len(leaf.Body) != 1 || leaf.Body[0] != leafReturn ||
		plan.Functions[leaf].Factory != FactoryABI1 || plan.Functions[unused].Factory != NoFactory {
		t.Fatal("factory demand changed an ordinary body or generated an unrelated factory")
	}
	plan.PublishSummaries()
	defer delete(summaries, leaf)
	summary, ok := Summary(leaf)
	if !ok || summary.Factory != FactoryABI1 || summary.Primary() != PlainPrimary ||
		summary.Effect != NoSuspend || summary.Exec != 0 {
		t.Fatalf("plain factory summary = (%+v, %t)", summary, ok)
	}
	if factory, ok := importedResumeFactory(leaf); !ok || factory == nil {
		t.Fatal("plain factory cannot be imported with the existing ABI")
	}
}

func TestPlainSpawnFactoryUnsupported(t *testing.T) {
	prepareLowerTest(t)
	pkg := types.NewPkg("example.com/coro/plainspawnunsupported", "plainspawn")
	fn := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("leaf"), types.NewSignature(nil, nil, nil))
	for _, function := range []*Function{
		nil,
		{},
		{Func: fn, Primary: CoroPrimary},
		{Func: fn, Effect: MaySuspend},
		{Func: fn, Exec: MayBlockThread},
		{Func: fn, Terminal: UsesRecover},
		{Func: fn}, // A declaration without an available Go body.
	} {
		if plainSpawnFactorySupported(function) {
			t.Fatalf("unsupported plain factory = %+v", function)
		}
	}
	result := types.NewField(src.NoXPos, nil, types.Types[types.TINT])
	returning := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("returning"),
		types.NewSignature(nil, nil, []*types.Field{result}))
	if plainSpawnFactorySupported(&Function{Func: returning}) {
		t.Fatal("discarded-result spawn target supports a plain factory")
	}
}
