// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ssacompile

import (
	"testing"

	"cmd/compile/internal/ssa"
	"cmd/compile/internal/ssa/ssahtml"
	"cmd/compile/internal/ssa/ssaop"
	"cmd/compile/internal/types"
)

func TestCompileWithPreLowerObserver(t *testing.T) {
	c := testConfig(t)
	f := makeLoweringObserverTestFunc(c)
	calls := 0

	Compiler{}.CompileWithPreLowerObserver(f, (*ssahtml.HTMLWriter)(nil), func(got *ssa.Func) {
		calls++
		if got != f {
			t.Fatalf("observer received %p, want %p", got, f)
		}
		if got.Scheduled {
			t.Error("function is already scheduled at lowering boundary")
		}
		if len(got.RegAlloc) != 0 {
			t.Error("function already has register allocation at lowering boundary")
		}
		if !hasLoweringObserverTestOp(got, ssaop.OpAdd64) {
			t.Error("generic Add64 was lowered before observer")
		}
	})

	if calls != 1 {
		t.Fatalf("observer called %d times, want 1", calls)
	}
	if !f.Scheduled {
		t.Error("function did not continue into native scheduling")
	}
	if len(f.RegAlloc) == 0 {
		t.Error("function did not continue into native register allocation")
	}
	if hasLoweringObserverTestOp(f, ssaop.OpAdd64) {
		t.Error("generic Add64 remains after native lowering")
	}
}

func hasLoweringObserverTestOp(f *ssa.Func, op ssaop.Op) bool {
	for _, block := range f.Blocks {
		for _, value := range block.Values {
			if value.Op == op {
				return true
			}
		}
	}
	return false
}

func makeLoweringObserverTestFunc(c *Conf) *ssa.Func {
	typ := c.config.Types.UInt64
	ptyp := c.config.Types.BytePtr
	return c.Fun("entry",
		Bloc("entry",
			Valu("mem", ssaop.OpInitMem, types.TypeMem, 0, nil),
			Valu("SP", ssaop.OpSP, c.config.Types.Uintptr, 0, nil),
			Valu("argptr", ssaop.OpOffPtr, ptyp, 8, nil, "SP"),
			Valu("resptr", ssaop.OpOffPtr, ptyp, 16, nil, "SP"),
			Valu("load", ssaop.OpLoad, typ, 0, nil, "argptr", "mem"),
			Valu("one", ssaop.OpConst64, typ, 1, nil),
			Valu("sum", ssaop.OpAdd64, typ, 0, nil, "load", "one"),
			Valu("store", ssaop.OpStore, types.TypeMem, 0, typ, "resptr", "sum", "mem"),
			Exit("store"))).f
}
