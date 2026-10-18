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
