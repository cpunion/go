// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/ir"
	"cmd/compile/internal/typecheck"
	"cmd/compile/internal/types"
	"strings"
	"testing"
)

func TestSynchronizationDependencies(t *testing.T) {
	prepareLowerTest(t)
	oldTarget, oldLocalPkg, oldCurFunc := typecheck.Target, types.LocalPkg, ir.CurFunc
	t.Cleanup(func() {
		typecheck.Target, types.LocalPkg, ir.CurFunc = oldTarget, oldLocalPkg, oldCurFunc
	})
	for _, name := range []string{
		"internal/sync.runtime_SemacquireMutex",
		"internal/sync.runtime_Semrelease",
		"sync.runtime_Semacquire",
		"sync.runtime_SemacquireRWMutexR",
		"sync.runtime_SemacquireRWMutex",
		"sync.runtime_SemacquireWaitGroup",
		"internal/poll.runtime_Semacquire",
		"sync.runtime_notifyListWait",
	} {
		t.Run(name, func(t *testing.T) {
			path, symbol, _ := strings.Cut(name, ".")
			pkg := types.NewPkg(path, "sync")
			types.LocalPkg = pkg
			typecheck.Target = new(ir.Package)
			primitive := newLowerTestFunc(pkg, symbol)
			wrapper := newLowerTestFunc(pkg, symbol+"Wrapper")
			wrapper.Body = ir.Nodes{newLowerTestCall(primitive), newLowerTestReturn()}
			caller := newLowerTestFunc(pkg, symbol+"Caller")
			caller.Body = ir.Nodes{newLowerTestCall(wrapper), newLowerTestReturn()}
			plan, err := Analyze([]*ir.Func{primitive, wrapper, caller}, nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, fn := range []*ir.Func{wrapper, caller} {
				function := plan.Functions[fn]
				if function.Effect != MaySuspend || function.Primary != CoroPrimary ||
					len(function.Sites) != 1 || function.Sites[0].Kind != SiteAwait {
					t.Fatalf("%s plan = %+v, want suspending await", fn.Sym().Name, function)
				}
			}
			result, err := Lower(plan, nil)
			if err != nil {
				t.Fatal(err)
			}
			if result.Lowered != 0 || result.Skipped != 2 {
				t.Fatalf("Lower = %+v, want both dependent callers skipped", result)
			}
			for _, diagnostic := range result.Diagnostics {
				if !strings.Contains(diagnostic, "unsupported coroutine dependency") {
					t.Errorf("unexpected diagnostic: %s", diagnostic)
				}
			}
			plan.PublishSummaries()
			if summary, ok := Summary(wrapper); !ok || summary.Effect != MaySuspend ||
				summary.Factory != NoFactory {
				t.Fatalf("wrapper summary = %+v, %t, want suspension without factory", summary, ok)
			}

			// The unsupported boundary must survive export and import. Starting
			// a nested ordinary entry from a resume could still park its executor.
			importPkg := types.NewPkg("example.com/coro/syncboundary/"+symbol, "caller")
			types.LocalPkg = importPkg
			typecheck.Target = new(ir.Package)
			importCaller := newLowerTestFunc(importPkg, "Caller")
			importCaller.Body = ir.Nodes{newLowerTestCall(wrapper), newLowerTestReturn()}
			importPlan, err := Analyze([]*ir.Func{importCaller}, nil)
			if err != nil {
				t.Fatal(err)
			}
			result, err = Lower(importPlan, nil)
			if err != nil {
				t.Fatal(err)
			}
			if result.Lowered != 0 || result.Skipped != 1 ||
				importPlan.Functions[importCaller].Effect != MaySuspend {
				t.Fatalf("imported dependency Lower = %+v, want caller skipped", result)
			}
		})
	}
}
