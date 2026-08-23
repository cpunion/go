// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ssa

import (
	"cmd/compile/internal/types"
	"testing"
)

func TestCompileWithLoweringHook(t *testing.T) {
	c := testConfig(t)

	t.Run("handled", func(t *testing.T) {
		f := makeLoweringHookTestFunc(c)
		calls := 0
		handled := CompileWithLoweringHook(f, nil, func(got *Func) bool {
			calls++
			if got != f {
				t.Fatalf("hook received %p, want %p", got, f)
			}
			if got.scheduled {
				t.Error("function is already scheduled at lowering boundary")
			}
			if len(got.RegAlloc) != 0 {
				t.Error("function already has register allocation at lowering boundary")
			}
			if !hasLoweringHookTestOp(got, OpAdd64) {
				t.Error("generic Add64 was lowered before hook")
			}
			return true
		})
		if !handled {
			t.Fatal("CompileWithLoweringHook returned handled=false")
		}
		if calls != 1 {
			t.Fatalf("hook called %d times, want 1", calls)
		}
		if f.scheduled {
			t.Error("handled function continued into native scheduling")
		}
		if len(f.RegAlloc) != 0 {
			t.Error("handled function continued into native register allocation")
		}
	})

	t.Run("continue native", func(t *testing.T) {
		f := makeLoweringHookTestFunc(c)
		calls := 0
		handled := CompileWithLoweringHook(f, nil, func(got *Func) bool {
			calls++
			if got.scheduled {
				t.Error("function is already scheduled at lowering boundary")
			}
			return false
		})
		if handled {
			t.Fatal("CompileWithLoweringHook returned handled=true")
		}
		if calls != 1 {
			t.Fatalf("hook called %d times, want 1", calls)
		}
		if !f.scheduled {
			t.Error("function did not continue into native scheduling")
		}
		if len(f.RegAlloc) == 0 {
			t.Error("function did not continue into native register allocation")
		}
		if hasLoweringHookTestOp(f, OpAdd64) {
			t.Error("generic Add64 remains after native lowering")
		}
	})
}

func hasLoweringHookTestOp(f *Func, op Op) bool {
	for _, block := range f.Blocks {
		for _, value := range block.Values {
			if value.Op == op {
				return true
			}
		}
	}
	return false
}

func makeLoweringHookTestFunc(c *Conf) *Func {
	typ := c.config.Types.UInt64
	ptyp := c.config.Types.BytePtr
	return c.Fun("entry",
		Bloc("entry",
			Valu("mem", OpInitMem, types.TypeMem, 0, nil),
			Valu("SP", OpSP, c.config.Types.Uintptr, 0, nil),
			Valu("argptr", OpOffPtr, ptyp, 8, nil, "SP"),
			Valu("resptr", OpOffPtr, ptyp, 16, nil, "SP"),
			Valu("load", OpLoad, typ, 0, nil, "argptr", "mem"),
			Valu("one", OpConst64, typ, 1, nil),
			Valu("sum", OpAdd64, typ, 0, nil, "load", "one"),
			Valu("store", OpStore, types.TypeMem, 0, typ, "resptr", "sum", "mem"),
			Exit("store"))).f
}
