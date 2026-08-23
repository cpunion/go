// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/base"
	"cmd/compile/internal/ssa"
	"cmd/compile/internal/ssa/block"
	"cmd/compile/internal/typecheck"
	"cmd/compile/internal/types"
	"cmd/internal/obj"
	"cmd/internal/sys"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func init() {
	if types.Types[types.TINT64] != nil {
		return
	}
	types.PtrSize = 8
	types.RegSize = 8
	types.MaxWidth = 1 << 50
	base.Ctxt = &obj.Link{Arch: &obj.LinkArch{Arch: &sys.Arch{Alignment: 1, CanMergeLoads: true}}}
	typecheck.InitUniverse()
}

type basicFixture struct {
	f           *ssa.Func
	block       *ssa.Block
	memory      *ssa.Value
	arg         *ssa.Value
	beforeAddr  *ssa.Value
	afterAddr   *ssa.Value
	beforeLoad  *ssa.Value
	beforeAdd   *ssa.Value
	beforeStore *ssa.Value
	call        *ssa.Value
	callMemory  *ssa.Value
	afterLoad   *ssa.Value
	afterAdd    *ssa.Value
	afterStore  *ssa.Value
	answer      *ssa.Value
	result      *ssa.Value
}

func newBasicFixture() *basicFixture {
	int64Type := types.Types[types.TINT64]
	ptrType := types.NewPtr(int64Type)
	value := func(op ssa.Op, typ *types.Type, args ...*ssa.Value) *ssa.Value {
		return &ssa.Value{Op: op, Type: typ, Args: args}
	}

	x := &basicFixture{}
	x.memory = value(ssa.OpInitMem, types.TypeMem)
	sb := value(ssa.OpSB, types.Types[types.TUINTPTR])
	x.arg = value(ssa.OpArgIntReg, int64Type)
	one := value(ssa.OpConst64, int64Type)
	one.AuxInt = 1
	three := value(ssa.OpConst64, int64Type)
	three.AuxInt = 3
	x.beforeAddr = value(ssa.OpAddr, ptrType, sb)
	x.beforeAddr.Aux = &obj.LSym{Name: "basic.before"}
	x.afterAddr = value(ssa.OpAddr, ptrType, sb)
	x.afterAddr.Aux = &obj.LSym{Name: "basic.after"}
	x.beforeLoad = value(ssa.OpLoad, int64Type, x.beforeAddr, x.memory)
	x.beforeAdd = value(ssa.OpAdd64, int64Type, x.beforeLoad, one)
	x.beforeStore = value(ssa.OpStore, types.TypeMem, x.beforeAddr, x.beforeAdd, x.memory)
	x.call = value(ssa.OpStaticCall, types.TypeMem, x.beforeStore)
	x.call.Aux = &ssa.AuxCall{Fn: &obj.LSym{Name: "basic.yieldOnce"}}
	x.callMemory = value(ssa.OpSelectN, types.TypeMem, x.call)
	x.afterLoad = value(ssa.OpLoad, int64Type, x.afterAddr, x.callMemory)
	x.afterAdd = value(ssa.OpAdd64, int64Type, x.afterLoad, one)
	x.afterStore = value(ssa.OpStore, types.TypeMem, x.afterAddr, x.afterAdd, x.callMemory)
	x.answer = value(ssa.OpAdd64, int64Type, three, x.arg)
	x.result = value(ssa.OpMakeResult, int64Type, x.answer, x.afterStore)

	x.block = &ssa.Block{
		Kind: block.BlockRet,
		Values: []*ssa.Value{
			x.memory, sb, x.arg, one, three,
			x.beforeAddr, x.afterAddr,
			x.beforeLoad, x.beforeAdd, x.beforeStore,
			x.call, x.callMemory,
			x.afterLoad, x.afterAdd, x.afterStore,
			x.answer, x.result,
		},
	}
	x.block.Controls[0] = x.result
	x.f = &ssa.Func{Name: "leaf", Blocks: []*ssa.Block{x.block}, Entry: x.block}
	x.block.Func = x.f
	for i, v := range x.block.Values {
		v.ID = ssa.ID(i + 1)
		v.Block = x.block
	}
	return x
}

func TestBasicLLVMModuleRecipe(t *testing.T) {
	x := newBasicFixture()
	module, matched, err := basicLLVMModule(x.f)
	if err != nil {
		t.Fatal(err)
	}
	if !matched {
		t.Fatal("valid recipe did not match")
	}
	text := string(module)
	for _, want := range []string{
		"%answer = add i64 %x, 3",
		"%answer.ok = icmp eq i64 %result.second, 43",
		"presplitcoroutine",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("module does not contain %q", want)
		}
	}

	// The argument and constant may appear in either Add64 operand order.
	x.answer.Args[0], x.answer.Args[1] = x.answer.Args[1], x.answer.Args[0]
	if _, matched, err := basicLLVMModule(x.f); err != nil || !matched {
		t.Fatalf("reversed Add64 operands: matched=%t err=%v", matched, err)
	}
}

func TestBasicLLVMModuleRejectsUnsupportedSSA(t *testing.T) {
	tests := []struct {
		name string
		edit func(*basicFixture)
		want string
	}{
		{
			name: "no marker",
			edit: func(x *basicFixture) {
				x.call.Aux = &ssa.AuxCall{Fn: &obj.LSym{Name: "basic.other"}}
			},
		},
		{
			name: "two markers",
			edit: func(x *basicFixture) {
				extra := &ssa.Value{Op: ssa.OpStaticCall, Type: types.TypeMem, Aux: x.call.Aux, Block: x.block}
				x.block.Values = append(x.block.Values, extra)
			},
			want: "found 2 yieldOnce calls",
		},
		{
			name: "two blocks",
			edit: func(x *basicFixture) {
				x.f.Blocks = append(x.f.Blocks, &ssa.Block{Kind: block.BlockRet})
			},
			want: "function has 2 blocks",
		},
		{
			name: "extra load",
			edit: func(x *basicFixture) {
				x.block.Values = append(x.block.Values, &ssa.Value{Op: ssa.OpLoad, Type: types.Types[types.TINT64]})
			},
			want: "calls=1 loads=3 stores=2",
		},
		{
			name: "wrong terminator",
			edit: func(x *basicFixture) {
				x.block.Kind = block.BlockExit
			},
			want: "want Ret with one control",
		},
		{
			name: "wrong return",
			edit: func(x *basicFixture) {
				x.result.Op = ssa.OpCopy
			},
			want: "want MakeResult",
		},
		{
			name: "wrong answer",
			edit: func(x *basicFixture) {
				x.answer.Op = ssa.OpSub64
			},
			want: "is not int64 argument plus a constant",
		},
		{
			name: "marker arguments",
			edit: func(x *basicFixture) {
				x.call.Args = append(x.call.Args, x.arg)
			},
			want: "want only memory",
		},
		{
			name: "before shape",
			edit: func(x *basicFixture) {
				x.beforeAddr.Aux = &obj.LSym{Name: "basic.wrong"}
			},
			want: "before suspend: store address",
		},
		{
			name: "before memory",
			edit: func(x *basicFixture) {
				memory := &ssa.Value{Op: ssa.OpCopy, Type: types.TypeMem}
				x.beforeStore.Args[2] = memory
				x.beforeLoad.Args[1] = memory
			},
			want: "does not start at InitMem",
		},
		{
			name: "after is not store",
			edit: func(x *basicFixture) {
				x.result.Args[1] = x.callMemory
			},
			want: "return memory is not a Store",
		},
		{
			name: "after memory",
			edit: func(x *basicFixture) {
				x.afterStore.Args[2] = x.memory
			},
			want: "store memory is not yieldOnce result memory",
		},
		{
			name: "after shape",
			edit: func(x *basicFixture) {
				x.afterAddr.Aux = &obj.LSym{Name: "basic.wrong"}
			},
			want: "after suspend: store address",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			x := newBasicFixture()
			test.edit(x)
			_, matched, err := basicLLVMModule(x.f)
			if test.want == "" {
				if matched || err != nil {
					t.Fatalf("matched=%t err=%v, want no match", matched, err)
				}
				return
			}
			if !matched || err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("matched=%t err=%v, want error containing %q", matched, err, test.want)
			}
		})
	}
}

func TestMatchGlobalIncrementRejectsMalformedValues(t *testing.T) {
	tests := []struct {
		name string
		edit func(*basicFixture) *ssa.Value
		want string
	}{
		{
			name: "not store",
			edit: func(x *basicFixture) *ssa.Value { return x.beforeAdd },
			want: "not Store",
		},
		{
			name: "wrong predecessor",
			edit: func(x *basicFixture) *ssa.Value {
				x.beforeStore.Args[2] = x.callMemory
				return x.beforeStore
			},
			want: "wrong memory predecessor",
		},
		{
			name: "not add",
			edit: func(x *basicFixture) *ssa.Value {
				x.beforeAdd.Op = ssa.OpSub64
				return x.beforeStore
			},
			want: "not Add64",
		},
		{
			name: "not plus one",
			edit: func(x *basicFixture) *ssa.Value {
				x.beforeAdd.Args[1].AuxInt = 2
				return x.beforeStore
			},
			want: "not load plus one",
		},
		{
			name: "not load",
			edit: func(x *basicFixture) *ssa.Value {
				x.beforeLoad.Op = ssa.OpConst64
				return x.beforeStore
			},
			want: "not load plus one",
		},
		{
			name: "different address",
			edit: func(x *basicFixture) *ssa.Value {
				x.beforeLoad.Args[0] = x.afterAddr
				return x.beforeStore
			},
			want: "do not share address and memory",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			x := newBasicFixture()
			store := test.edit(x)
			err := matchGlobalIncrement(store, ".before", x.memory)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestWriteBasicLLVM(t *testing.T) {
	x := newBasicFixture()
	x.call.Aux = nil
	if matched, err := WriteBasicLLVM(filepath.Join(t.TempDir(), "ignored.ll"), x.f); matched || err != nil {
		t.Fatalf("non-marker: matched=%t err=%v", matched, err)
	}

	x = newBasicFixture()
	if matched, err := WriteBasicLLVM("", x.f); !matched || err == nil || err.Error() != "empty output path" {
		t.Fatalf("empty path: matched=%t err=%v", matched, err)
	}

	missing := filepath.Join(t.TempDir(), "missing", "basic.ll")
	if matched, err := WriteBasicLLVM(missing, x.f); !matched || err == nil {
		t.Fatalf("missing directory: matched=%t err=%v", matched, err)
	}

	path := filepath.Join(t.TempDir(), "basic.ll")
	if matched, err := WriteBasicLLVM(path, x.f); !matched || err != nil {
		t.Fatalf("write: matched=%t err=%v", matched, err)
	}
	if data, err := os.ReadFile(path); err != nil {
		t.Fatal(err)
	} else if !strings.Contains(string(data), "coro-basic-ok") {
		t.Fatal("written module does not contain success marker")
	}
	if matched, err := WriteBasicLLVM(path, newBasicFixture().f); !matched || err == nil || !strings.Contains(err.Error(), "already emitted") {
		t.Fatalf("duplicate output: matched=%t err=%v", matched, err)
	}
}

func TestCallNameAndMatchArgAddEdges(t *testing.T) {
	if got := callName(&ssa.Value{}); got != "" {
		t.Fatalf("callName without AuxCall = %q", got)
	}
	if got := callName(&ssa.Value{Aux: &ssa.AuxCall{}}); got != "" {
		t.Fatalf("callName without symbol = %q", got)
	}

	int64Type := types.Types[types.TINT64]
	arg := &ssa.Value{Op: ssa.OpArgIntReg, Type: int64Type}
	constant := &ssa.Value{Op: ssa.OpConst64, Type: int64Type, AuxInt: 7}
	for _, value := range []*ssa.Value{
		{Op: ssa.OpSub64, Type: int64Type, Args: []*ssa.Value{arg, constant}},
		{Op: ssa.OpAdd64, Type: int64Type, Args: []*ssa.Value{arg}},
		{Op: ssa.OpAdd64, Type: int64Type, Args: []*ssa.Value{constant, constant}},
	} {
		if _, ok := matchArgAdd(value); ok {
			t.Fatalf("matchArgAdd unexpectedly accepted %s", value.LongString())
		}
	}
}

func TestRenderBasicLLVMNegativeAddend(t *testing.T) {
	text := string(renderBasicLLVM(basicRecipe{addend: -2}))
	for _, want := range []string{
		"%answer = add i64 %x, -2",
		"%answer.ok = icmp eq i64 %result.second, 38",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("module does not contain %q", want)
		}
	}
}
