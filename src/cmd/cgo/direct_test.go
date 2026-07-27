// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"bytes"
	"go/ast"
	"strings"
	"testing"

	"internal/buildcfg"
)

func TestCgoDirectType(t *testing.T) {
	tests := []struct {
		name string
		typ  *Type
		want cgoDirectType
		ok   bool
	}{
		{"void", nil, cgoDirectVoid, true},
		{"bool", &Type{Size: 1, Go: ast.NewIdent("bool")}, cgoDirectUint8, true},
		{"byte", &Type{Size: 1, Go: ast.NewIdent("byte")}, cgoDirectUint8, true},
		{"uint8", &Type{Size: 1, Go: ast.NewIdent("uint8")}, cgoDirectUint8, true},
		{"int8", &Type{Size: 1, Go: ast.NewIdent("int8")}, cgoDirectInt8, true},
		{"uint16", &Type{Size: 2, Go: ast.NewIdent("uint16")}, cgoDirectUint16, true},
		{"int16", &Type{Size: 2, Go: ast.NewIdent("int16")}, cgoDirectInt16, true},
		{"uint32", &Type{Size: 4, Go: ast.NewIdent("uint32")}, cgoDirectUint32, true},
		{"int32", &Type{Size: 4, Go: ast.NewIdent("int32")}, cgoDirectInt32, true},
		{"uint64", &Type{Size: 8, Go: ast.NewIdent("uint64")}, cgoDirectUint64, true},
		{"int64", &Type{Size: 8, Go: ast.NewIdent("int64")}, cgoDirectInt64, true},
		{"uint32 word", &Type{Size: 4, Go: ast.NewIdent("uint")}, cgoDirectUint32, true},
		{"uint64 word", &Type{Size: 8, Go: ast.NewIdent("uint")}, cgoDirectUint64, true},
		{"uintptr32", &Type{Size: 4, Go: ast.NewIdent("uintptr")}, cgoDirectUint32, true},
		{"uintptr64", &Type{Size: 8, Go: ast.NewIdent("uintptr")}, cgoDirectUint64, true},
		{"int32 word", &Type{Size: 4, Go: ast.NewIdent("int")}, cgoDirectInt32, true},
		{"int64 word", &Type{Size: 8, Go: ast.NewIdent("int")}, cgoDirectInt64, true},
		{"pointer", &Type{Size: 8, Go: &ast.StarExpr{X: ast.NewIdent("byte")}}, cgoDirectPointer, true},
		{"void pointer", &Type{Size: 8, Go: ast.NewIdent("unsafe.Pointer")}, cgoDirectPointer, true},
		{"bad pointer", &Type{Size: 8, Go: ast.NewIdent("uintptr"), BadPointer: true}, cgoDirectPointer, true},
		{"wrong fixed size", &Type{Size: 8, Go: ast.NewIdent("uint32")}, cgoDirectUint32, false},
		{"unsupported uint size", &Type{Size: 16, Go: ast.NewIdent("uint")}, "", false},
		{"unsupported int size", &Type{Size: 16, Go: ast.NewIdent("int")}, "", false},
		{"float", &Type{Size: 8, Go: ast.NewIdent("float64")}, "", false},
		{"struct", &Type{Size: 8, Go: &ast.StructType{}}, "", false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := cgoDirectTypeOf(test.typ)
			if got != test.want || ok != test.ok {
				t.Fatalf("cgoDirectTypeOf(%v) = (%q, %t), want (%q, %t)",
					test.typ.Go, got, ok, test.want, test.ok)
			}
		})
	}
}

func TestCgoDirectCall(t *testing.T) {
	oldExperiment := buildcfg.Experiment
	oldGccgo := *gccgo
	buildcfg.Experiment.Coro = true
	defer func() {
		buildcfg.Experiment = oldExperiment
		*gccgo = oldGccgo
	}()

	p := &Package{
		noCallbacks: map[string]bool{"add_u64": true},
	}
	n := &Name{
		Go:     "add_u64",
		C:      "add_u64",
		Kind:   "func",
		Mangle: "_Cfunc_add_u64",
		FuncType: &FuncType{
			Params: []*Type{
				{Size: 8, Go: ast.NewIdent("uint64")},
				{Size: 8, Go: ast.NewIdent("uint64")},
			},
			Result: &Type{Size: 8, Go: ast.NewIdent("uint64")},
		},
	}

	call, ok := p.directCall(n)
	if !ok {
		t.Fatal("directCall rejected supported declaration")
	}
	var output bytes.Buffer
	call.writeDirective(&output)
	want := "//go:cgo_direct v1 _Cfunc_add_u64 _Cdirect_add_u64 add_u64 mayblock u64,u64 u64 -\n"
	if got := output.String(); got != want {
		t.Fatalf("directive = %q, want %q", got, want)
	}

	tests := []struct {
		name   string
		change func(*Package, *Name)
	}{
		{"experiment disabled", func(_ *Package, _ *Name) {
			buildcfg.Experiment.Coro = false
		}},
		{"gccgo", func(_ *Package, _ *Name) {
			*gccgo = true
		}},
		{"not function", func(_ *Package, n *Name) {
			n.Kind = "var"
		}},
		{"missing function type", func(_ *Package, n *Name) {
			n.FuncType = nil
		}},
		{"may callback", func(p *Package, _ *Name) {
			delete(p.noCallbacks, "add_u64")
		}},
		{"too many parameters", func(_ *Package, n *Name) {
			n.FuncType.Params = append(n.FuncType.Params, make([]*Type, 5)...)
		}},
		{"void parameter", func(_ *Package, n *Name) {
			n.FuncType.Params[0] = nil
		}},
		{"unsupported parameter", func(_ *Package, n *Name) {
			n.FuncType.Params[0] = &Type{Size: 8, Go: ast.NewIdent("float64")}
		}},
		{"unsupported result", func(_ *Package, n *Name) {
			n.FuncType.Result = &Type{Size: 8, Go: ast.NewIdent("float64")}
		}},
		{"bad wrapper name", func(_ *Package, n *Name) {
			n.Mangle = "_Cvar_add_u64"
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			buildcfg.Experiment.Coro = true
			*gccgo = false
			p := &Package{noCallbacks: map[string]bool{"add_u64": true}}
			n := &Name{
				C:      "add_u64",
				Kind:   "func",
				Mangle: "_Cfunc_add_u64",
				FuncType: &FuncType{
					Params: []*Type{
						{Size: 8, Go: ast.NewIdent("uint64")},
						{Size: 8, Go: ast.NewIdent("uint64")},
					},
					Result: &Type{Size: 8, Go: ast.NewIdent("uint64")},
				},
			}
			test.change(p, n)
			if _, ok := p.directCall(n); ok {
				t.Errorf("directCall accepted %s declaration", test.name)
			}
		})
	}

	void := &Name{
		C:        "tick",
		Kind:     "func",
		Mangle:   "_Cfunc_tick",
		FuncType: &FuncType{},
	}
	p.noCallbacks["tick"] = true
	call, ok = p.directCall(void)
	if !ok {
		t.Fatal("directCall rejected void declaration")
	}
	output.Reset()
	call.writeDirective(&output)
	if got, want := output.String(),
		"//go:cgo_direct v1 _Cfunc_tick _Cdirect_tick tick mayblock - void -\n"; got != want {
		t.Fatalf("void directive = %q, want %q", got, want)
	}
}

func TestCgoDirectTypedef(t *testing.T) {
	const name = "_Ctype_direct_test_uint"
	old := typedef[name]
	typedef[name] = &Type{Size: 4, Go: ast.NewIdent("uint32")}
	defer func() {
		if old == nil {
			delete(typedef, name)
		} else {
			typedef[name] = old
		}
	}()

	got, ok := cgoDirectTypeOf(&Type{Size: 4, Go: ast.NewIdent(name)})
	if !ok || got != cgoDirectUint32 {
		t.Fatalf("cgoDirectTypeOf(typedef) = (%q, %t), want (%q, true)",
			got, ok, cgoDirectUint32)
	}

	typedef[name] = &Type{Size: 4, Go: ast.NewIdent(name)}
	if _, ok := cgoDirectTypeOf(&Type{Size: 4, Go: ast.NewIdent(name)}); ok {
		t.Fatal("cgoDirectTypeOf accepted recursive typedef")
	}

	delete(typedef, name)
	if _, ok := cgoDirectTypeOf(&Type{Size: 4, Go: ast.NewIdent(name)}); ok {
		t.Fatal("cgoDirectTypeOf accepted missing typedef")
	}
}

func TestCgoDirectErrno(t *testing.T) {
	call := cgoDirectCall{
		wrapper: "_Cfunc_read",
		direct:  "_Cdirect_read",
		symbol:  "read",
		params:  []cgoDirectType{cgoDirectInt32, cgoDirectPointer, cgoDirectUint64},
		result:  cgoDirectInt64,
		errno:   true,
	}
	var output bytes.Buffer
	call.writeDirective(&output)
	if got := output.String(); !strings.HasSuffix(got, " i32,ptr,u64 i64 errno\n") {
		t.Fatalf("directive = %q, want errno signature", got)
	}
}
