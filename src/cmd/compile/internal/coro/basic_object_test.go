// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/abi"
	"cmd/compile/internal/ir"
	"cmd/compile/internal/ssa"
	"cmd/compile/internal/ssa/block"
	"cmd/compile/internal/types"
	"cmd/internal/coroobj"
	"cmd/internal/obj"
	"cmd/internal/src"
	"os/exec"
	"strings"
	"testing"
)

type basicObjectFixture struct {
	f      *ssa.Func
	block  *ssa.Block
	call   *ssa.Value
	value  *ssa.Value
	result *ssa.Value
}

func newBasicObjectFixture() *basicObjectFixture {
	memory := &ssa.Value{ID: 1, Op: ssa.OpInitMem, Type: types.TypeMem}
	call := &ssa.Value{
		ID:   2,
		Op:   ssa.OpStaticCall,
		Type: types.TypeMem,
		Aux:  &ssa.AuxCall{Fn: &obj.LSym{Name: "basic.yieldOnce"}},
		Args: []*ssa.Value{memory},
	}
	callMemory := &ssa.Value{ID: 3, Op: ssa.OpSelectN, Type: types.TypeMem, Args: []*ssa.Value{call}}
	value := &ssa.Value{ID: 4, Op: ssa.OpConst64, Type: types.Types[types.TINT64], AuxInt: 42}
	result := &ssa.Value{ID: 5, Op: ssa.OpMakeResult, Type: types.Types[types.TINT64], Args: []*ssa.Value{value, callMemory}}
	block := &ssa.Block{Kind: block.BlockRet, Values: []*ssa.Value{memory, call, callMemory, value, result}}
	block.Controls[0] = result
	f := &ssa.Func{
		Name:    "leaf",
		Blocks:  []*ssa.Block{block},
		Entry:   block,
		OwnAux:  &ssa.AuxCall{Fn: &obj.LSym{Name: "example.com/p.leaf"}},
		ABISelf: abi.NewABIConfig(0, 0, 0, uint8(obj.ABIInternal)),
	}
	block.Func = f
	for _, v := range block.Values {
		v.Block = block
	}
	return &basicObjectFixture{f: f, block: block, call: call, value: value, result: result}
}

func TestBasicObjectLLVMModule(t *testing.T) {
	x := newBasicObjectFixture()
	goName, hostName, module, matched, err := basicObjectLLVMModule(x.f)
	if err != nil {
		t.Fatal(err)
	}
	if !matched {
		t.Fatal("valid object recipe did not match")
	}
	if goName != "example.com/p.leaf" {
		t.Fatalf("Go symbol = %q, want example.com/p.leaf", goName)
	}
	if !strings.HasPrefix(hostName, "go_coro_") {
		t.Fatalf("host symbol = %q, want go_coro_ prefix", hostName)
	}
	text := string(module)
	for _, want := range []string{
		"presplitcoroutine",
		"call i8 @llvm.coro.suspend",
		"store i64 42",
		"define i64 @" + hostName + "()",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("module does not contain %q", want)
		}
	}
}

func TestBasicObjectLLVMModuleRejectsUnsupportedSSA(t *testing.T) {
	tests := []struct {
		name string
		edit func(*basicObjectFixture)
		want string
	}{
		{"no marker", func(x *basicObjectFixture) { x.call.Aux = &ssa.AuxCall{Fn: &obj.LSym{Name: "basic.other"}} }, ""},
		{"two markers", func(x *basicObjectFixture) {
			call := *x.call
			call.ID = 6
			x.block.Values = append(x.block.Values, &call)
		}, "found 2 yieldOnce calls"},
		{"other call", func(x *basicObjectFixture) {
			call := *x.call
			call.ID = 6
			call.Aux = &ssa.AuxCall{Fn: &obj.LSym{Name: "basic.other"}}
			x.block.Values = append(x.block.Values, &call)
		}, "function has 2 calls"},
		{"multiple blocks", func(x *basicObjectFixture) {
			x.f.Blocks = append(x.f.Blocks, &ssa.Block{Kind: block.BlockRet, Func: x.f})
		}, "want one entry block"},
		{"wrong block kind", func(x *basicObjectFixture) { x.block.Kind = block.BlockPlain }, "entry ends in Plain"},
		{"wrong return control", func(x *basicObjectFixture) { x.result.Op = ssa.OpConst64 }, "want MakeResult"},
		{"wrong return", func(x *basicObjectFixture) { x.value.Op = ssa.OpConst32 }, "want Const64"},
		{"wrong call arguments", func(x *basicObjectFixture) { x.call.Args = append(x.call.Args, x.value) }, "want only memory"},
		{"wrong memory", func(x *basicObjectFixture) { x.result.Args[1] = x.call.Args[0] }, "not yieldOnce result memory"},
		{"missing symbol", func(x *basicObjectFixture) { x.f.OwnAux = nil }, "no linker symbol"},
		{"empty symbol", func(x *basicObjectFixture) { x.f.OwnAux.Fn.Name = "" }, "no linker symbol"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			x := newBasicObjectFixture()
			test.edit(x)
			_, _, _, matched, err := basicObjectLLVMModule(x.f)
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

func TestBasicObjectCandidate(t *testing.T) {
	pkg := types.NewPkg("example.com/p", "p")
	name := ir.NewNameAt(src.NoXPos, pkg.Lookup("yieldOnce"), nil)
	name.Class = ir.PFUNC
	fn := &ir.Func{Body: ir.Nodes{ir.NewCallExpr(src.NoXPos, ir.OCALLFUNC, name, nil)}}
	if !BasicObjectCandidate(fn) {
		t.Fatal("yieldOnce call was not recognized")
	}
	name = ir.NewNameAt(src.NoXPos, pkg.Lookup("other"), nil)
	name.Class = ir.PFUNC
	fn.Body = ir.Nodes{ir.NewCallExpr(src.NoXPos, ir.OCALLFUNC, name, nil)}
	if BasicObjectCandidate(fn) {
		t.Fatal("ordinary call was recognized as an object candidate")
	}
}

func TestWriteBasicObject(t *testing.T) {
	clang, err := exec.LookPath("clang")
	if err != nil {
		t.Skip("test requires clang")
	}
	objectArtifacts.Lock()
	saved := objectArtifacts.list
	objectArtifacts.list = nil
	objectArtifacts.Unlock()
	t.Cleanup(func() {
		objectArtifacts.Lock()
		objectArtifacts.list = saved
		objectArtifacts.Unlock()
	})
	if manifest, ok := ObjectManifest(); ok || manifest != nil {
		t.Fatalf("empty ObjectManifest = %#v, %t", manifest, ok)
	}
	if objects := NativeObjects(); len(objects) != 0 {
		t.Fatalf("empty NativeObjects = %#v", objects)
	}

	x := newBasicObjectFixture()
	if handled, err := WriteBasicObject("missing-coro-clang", x.f); !handled || err == nil {
		t.Fatalf("missing-clang WriteBasicObject handled=%t err=%v", handled, err)
	}
	handled, err := WriteBasicObject(clang, x.f)
	if err != nil || !handled {
		t.Fatalf("WriteBasicObject handled=%t err=%v", handled, err)
	}
	manifest, ok := ObjectManifest()
	if !ok || len(manifest.Symbols) != 1 || manifest.Symbols[0].GoName != "example.com/p.leaf" {
		t.Fatalf("ObjectManifest = %#v, %t", manifest, ok)
	}
	objects := NativeObjects()
	if len(objects) != 1 || !strings.HasPrefix(objects[0].Name, "co") || !strings.HasSuffix(objects[0].Name, ".o") || len(objects[0].Name) != 16 || len(objects[0].Data) == 0 {
		t.Fatalf("NativeObjects = %#v", objects)
	}
	objects[0].Data[0] ^= 0xff
	if objects[0].Data[0] == NativeObjects()[0].Data[0] {
		t.Fatal("NativeObjects returned mutable artifact storage")
	}
	if handled, err := WriteBasicObject(clang, x.f); !handled || err == nil || !strings.Contains(err.Error(), "already emitted") {
		t.Fatalf("duplicate WriteBasicObject handled=%t err=%v", handled, err)
	}
	objectArtifacts.Lock()
	objectArtifacts.list = append(objectArtifacts.list, objectArtifact{
		object: NativeObject{Name: "co000000000001.o", Data: []byte{1}},
		symbol: coroobj.Symbol{GoName: "example.com/p.first", GoABI: 1, HostName: "go_coro_first"},
	})
	objectArtifacts.Unlock()
	manifest, ok = ObjectManifest()
	if !ok || len(manifest.Symbols) != 2 || manifest.Symbols[0].GoName != "example.com/p.first" {
		t.Fatalf("sorted ObjectManifest = %#v, %t", manifest, ok)
	}

	x.call.Aux = &ssa.AuxCall{Fn: &obj.LSym{Name: "basic.other"}}
	if handled, err := WriteBasicObject(clang, x.f); handled || err != nil {
		t.Fatalf("non-candidate WriteBasicObject handled=%t err=%v", handled, err)
	}
}

func TestCompileLLVMObjectErrors(t *testing.T) {
	if _, err := compileLLVMObject("", nil); err == nil || !strings.Contains(err.Error(), "empty clang") {
		t.Fatalf("empty clang error = %v", err)
	}
	if _, err := compileLLVMObject("missing-coro-clang", nil); err == nil {
		t.Fatal("missing clang unexpectedly succeeded")
	}
	clang, err := exec.LookPath("clang")
	if err != nil {
		t.Skip("invalid-module test requires clang")
	}
	if _, err := compileLLVMObject(clang, []byte("not LLVM IR")); err == nil || !strings.Contains(err.Error(), clang) {
		t.Fatalf("invalid module error = %v", err)
	}
}
