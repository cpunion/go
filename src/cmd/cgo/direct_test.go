// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"os"
	"path/filepath"
	"slices"
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
		{"float32", &Type{Size: 4, Go: ast.NewIdent("float32")}, cgoDirectFloat32, true},
		{"float64", &Type{Size: 8, Go: ast.NewIdent("float64")}, cgoDirectFloat64, true},
		{"pointer", &Type{Size: 8, Go: &ast.StarExpr{X: ast.NewIdent("byte")}}, cgoDirectPointer, true},
		{"void pointer", &Type{Size: 8, Go: ast.NewIdent("unsafe.Pointer")}, cgoDirectPointer, true},
		{"bad pointer", &Type{Size: 8, Go: ast.NewIdent("uintptr"), BadPointer: true}, cgoDirectPointer, true},
		{"wrong fixed size", &Type{Size: 8, Go: ast.NewIdent("uint32")}, cgoDirectUint32, false},
		{"unsupported uint size", &Type{Size: 16, Go: ast.NewIdent("uint")}, "", false},
		{"unsupported int size", &Type{Size: 16, Go: ast.NewIdent("int")}, "", false},
		{"wrong float size", &Type{Size: 4, Go: ast.NewIdent("float64")}, cgoDirectFloat64, false},
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

func TestCgoDirectAggregate(t *testing.T) {
	structType := func(fields ...ast.Expr) *ast.StructType {
		list := make([]*ast.Field, len(fields))
		for i, field := range fields {
			list[i] = &ast.Field{
				Names: []*ast.Ident{ast.NewIdent(fmt.Sprintf("f%d", i))},
				Type:  field,
			}
		}
		return &ast.StructType{Fields: &ast.FieldList{List: list}}
	}

	value, ok := cgoDirectValueOf(&Type{
		Size:  16,
		Align: 8,
		Go: structType(
			ast.NewIdent("uint64"),
			ast.NewIdent("float32"),
			&ast.ArrayType{Len: &ast.BasicLit{}, Elt: ast.NewIdent("uint8")},
		),
	})
	if !ok || value.typ != cgoDirectMemory || value.size() != 16 ||
		value.alignment(8) != 8 || value.String() != "mem16:8" ||
		value.bridgeType() != cgoDirectPointer {
		t.Fatalf("aggregate value = (%+v, %t)", value, ok)
	}

	const (
		structName = "_Ctype_struct_direct_test"
		unionName  = "_Ctype_union_direct_test"
		cycleName  = "_Ctype_direct_test_cycle"
	)
	oldStruct := typedef[structName]
	oldUnion := typedef[unionName]
	oldCycle := typedef[cycleName]
	defer func() {
		for name, old := range map[string]*Type{
			structName: oldStruct,
			unionName:  oldUnion,
			cycleName:  oldCycle,
		} {
			if old == nil {
				delete(typedef, name)
			} else {
				typedef[name] = old
			}
		}
	}()
	typedef[structName] = &Type{
		Size:  8,
		Align: 4,
		Go:    structType(ast.NewIdent("uint32"), ast.NewIdent("int32")),
	}
	value, ok = cgoDirectValueOf(&Type{
		Size: 8, Align: 4, Go: ast.NewIdent(structName),
	})
	if !ok || value.String() != "mem8:4" {
		t.Fatalf("named aggregate value = (%+v, %t)", value, ok)
	}

	typedef[unionName] = &Type{
		Size: 8, Align: 1,
		Go: &ast.ArrayType{Len: &ast.BasicLit{}, Elt: ast.NewIdent("uint8")},
	}
	typedef[cycleName] = &Type{
		Size: 8, Align: 8, Go: ast.NewIdent(cycleName),
	}
	tests := []struct {
		name string
		typ  *Type
	}{
		{"pointer field", &Type{
			Size: 8, Align: 8,
			Go: structType(&ast.StarExpr{X: ast.NewIdent("uint8")}),
		}},
		{"union field", &Type{
			Size: 8, Align: 1,
			Go: structType(ast.NewIdent(unionName)),
		}},
		{"top-level union", &Type{
			Size: 8, Align: 1, Go: ast.NewIdent(unionName),
		}},
		{"recursive typedef", &Type{
			Size: 8, Align: 8, Go: ast.NewIdent(cycleName),
		}},
		{"oversized", &Type{
			Size: cgoDirectMaxAggregateSize + 1, Align: 8,
			Go: structType(ast.NewIdent("uint64")),
		}},
		{"over-aligned", &Type{
			Size: 16, Align: 16,
			Go: structType(ast.NewIdent("uint64")),
		}},
		{"invalid alignment", &Type{
			Size: 8, Align: 3,
			Go: structType(ast.NewIdent("uint64")),
		}},
		{"zero-sized", &Type{
			Size: 0, Align: 1, Go: structType(),
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if value, ok := cgoDirectValueOf(test.typ); ok {
				t.Errorf("cgoDirectValueOf accepted aggregate %+v", value)
			}
		})
	}
}

func TestCgoDirectAggregateSafety(t *testing.T) {
	const (
		safeName       = "_Ctype_direct_test_safe"
		badPointerName = "_Ctype_direct_test_bad_pointer"
		cycleName      = "_Ctype_direct_test_memory_cycle"
		missingName    = "_Ctype_direct_test_missing"
	)
	names := []string{safeName, badPointerName, cycleName, missingName}
	old := make(map[string]*Type)
	for _, name := range names {
		old[name] = typedef[name]
	}
	defer func() {
		for _, name := range names {
			if old[name] == nil {
				delete(typedef, name)
			} else {
				typedef[name] = old[name]
			}
		}
	}()

	typedef[safeName] = &Type{
		Size:  8,
		Align: 4,
		Go: &ast.StructType{Fields: &ast.FieldList{List: []*ast.Field{
			{Type: ast.NewIdent("uint32")},
			{Type: &ast.ArrayType{
				Len: &ast.BasicLit{},
				Elt: ast.NewIdent("uint8"),
			}},
		}}},
	}
	typedef[badPointerName] = &Type{
		Size:       8,
		Align:      8,
		Go:         ast.NewIdent("uintptr"),
		BadPointer: true,
	}
	typedef[cycleName] = &Type{
		Size: 8, Align: 8, Go: ast.NewIdent(cycleName),
	}
	delete(typedef, missingName)

	tests := []struct {
		name string
		expr ast.Expr
		seen map[string]bool
		want bool
	}{
		{"named struct", ast.NewIdent(safeName), nil, true},
		{"missing typedef", ast.NewIdent(missingName), nil, false},
		{"seen typedef", ast.NewIdent(safeName), map[string]bool{safeName: true}, false},
		{"union", ast.NewIdent("_Ctype_union_direct_safety"), nil, false},
		{"class", ast.NewIdent("_Ctype_class_direct_safety"), nil, false},
		{"non aggregate", &ast.ArrayType{
			Len: &ast.BasicLit{},
			Elt: ast.NewIdent("uint8"),
		}, nil, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			seen := test.seen
			if seen == nil {
				seen = make(map[string]bool)
			}
			if got := cgoDirectAggregate(test.expr, seen); got != test.want {
				t.Errorf("cgoDirectAggregate(%T) = %t, want %t",
					test.expr, got, test.want)
			}
		})
	}

	nestedSafe := &ast.StructType{Fields: &ast.FieldList{List: []*ast.Field{
		{Type: ast.NewIdent("complex128")},
		{Type: ast.NewIdent(safeName)},
	}}}
	nestedUnsafe := &ast.StructType{Fields: &ast.FieldList{List: []*ast.Field{
		{Type: ast.NewIdent("uint64")},
		{Type: &ast.StarExpr{X: ast.NewIdent("byte")}},
	}}}
	memoryTests := []struct {
		name string
		expr ast.Expr
		seen map[string]bool
		want bool
	}{
		{"array", &ast.ArrayType{
			Len: &ast.BasicLit{},
			Elt: ast.NewIdent("float64"),
		}, nil, true},
		{"array without length", &ast.ArrayType{
			Elt: ast.NewIdent("uint8"),
		}, nil, false},
		{"nested struct", nestedSafe, nil, true},
		{"unsafe nested struct", nestedUnsafe, nil, false},
		{"bad pointer typedef", ast.NewIdent(badPointerName), nil, false},
		{"missing typedef", ast.NewIdent(missingName), nil, false},
		{"recursive typedef", ast.NewIdent(cycleName), nil, false},
		{"seen typedef", ast.NewIdent(safeName), map[string]bool{safeName: true}, false},
		{"union", ast.NewIdent("_Ctype_union_direct_safety"), nil, false},
		{"class", ast.NewIdent("_Ctype_class_direct_safety"), nil, false},
		{"pointer", &ast.StarExpr{X: ast.NewIdent("byte")}, nil, false},
	}
	for _, test := range memoryTests {
		t.Run("memory/"+test.name, func(t *testing.T) {
			seen := test.seen
			if seen == nil {
				seen = make(map[string]bool)
			}
			if got := cgoDirectMemorySafe(test.expr, seen); got != test.want {
				t.Errorf("cgoDirectMemorySafe(%T) = %t, want %t",
					test.expr, got, test.want)
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
			for len(n.FuncType.Params) < 9 {
				n.FuncType.Params = append(n.FuncType.Params,
					&Type{Size: 8, Go: ast.NewIdent("uint64")})
			}
		}},
		{"void parameter", func(_ *Package, n *Name) {
			n.FuncType.Params[0] = nil
		}},
		{"unsupported parameter", func(_ *Package, n *Name) {
			n.FuncType.Params[0] = &Type{Size: 8, Go: &ast.StructType{}}
		}},
		{"unsupported result", func(_ *Package, n *Name) {
			n.FuncType.Result = &Type{Size: 8, Go: &ast.StructType{}}
		}},
		{"mismatched errno wrapper", func(_ *Package, n *Name) {
			n.AddError = true
		}},
		{"invalid C symbol", func(p *Package, n *Name) {
			n.C = "add.u64"
			p.noCallbacks[n.C] = true
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

	floating := &Name{
		C:      "mix_fp",
		Kind:   "func",
		Mangle: "_Cfunc_mix_fp",
		FuncType: &FuncType{
			Params: []*Type{
				{Size: 8, Go: ast.NewIdent("float64")},
				{Size: 4, Go: ast.NewIdent("int32")},
				{Size: 4, Go: ast.NewIdent("float32")},
				{Size: 8, Go: ast.NewIdent("uint64")},
			},
			Result: &Type{Size: 8, Go: ast.NewIdent("float64")},
		},
	}
	p.noCallbacks["mix_fp"] = true
	call, ok = p.directCall(floating)
	if !ok {
		t.Fatal("directCall rejected floating declaration")
	}
	output.Reset()
	call.writeDirective(&output)
	if got, want := output.String(),
		"//go:cgo_direct v1 _Cfunc_mix_fp _Cdirect_mix_fp mix_fp mayblock f64,i32,f32,u64 f64 -\n"; got != want {
		t.Fatalf("floating directive = %q, want %q", got, want)
	}

	aggregateType := func() *Type {
		return &Type{
			Size:  16,
			Align: 8,
			Go: &ast.StructType{Fields: &ast.FieldList{List: []*ast.Field{
				{Names: []*ast.Ident{ast.NewIdent("x")}, Type: ast.NewIdent("uint64")},
				{Names: []*ast.Ident{ast.NewIdent("y")}, Type: ast.NewIdent("uint64")},
			}}},
		}
	}
	aggregate := &Name{
		C:      "add_pair",
		Kind:   "func",
		Mangle: "_Cfunc_add_pair",
		FuncType: &FuncType{
			Params: []*Type{aggregateType(), aggregateType()},
			Result: aggregateType(),
		},
	}
	p.noCallbacks["add_pair"] = true
	call, ok = p.directCall(aggregate)
	if !ok {
		t.Fatal("directCall rejected aggregate declaration")
	}
	output.Reset()
	call.writeDirective(&output)
	if got, want := output.String(),
		"//go:cgo_direct v1 _Cfunc_add_pair _Cdirect_add_pair add_pair mayblock mem16:8,mem16:8 mem16:8 -\n"; got != want {
		t.Fatalf("aggregate directive = %q, want %q", got, want)
	}
	if got, want := call.bridgeParams(),
		[]cgoDirectType{cgoDirectPointer, cgoDirectPointer, cgoDirectPointer}; !slices.Equal(got, want) {
		t.Fatalf("aggregate bridge parameters = %v, want %v", got, want)
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
	oldExperiment := buildcfg.Experiment
	oldGccgo := *gccgo
	buildcfg.Experiment.Coro = true
	*gccgo = false
	defer func() {
		buildcfg.Experiment = oldExperiment
		*gccgo = oldGccgo
	}()

	n := &Name{
		C:        "read",
		Kind:     "func",
		Mangle:   "_C2func_read",
		AddError: true,
		FuncType: &FuncType{
			Params: []*Type{
				{Size: 4, Go: ast.NewIdent("int32")},
				{Size: 8, Go: &ast.StarExpr{X: ast.NewIdent("byte")}},
				{Size: 8, Go: ast.NewIdent("uint64")},
			},
			Result: &Type{Size: 8, Go: ast.NewIdent("int64")},
		},
	}
	p := &Package{noCallbacks: map[string]bool{"read": true}}
	call, ok := p.directCall(n)
	if !ok {
		t.Fatal("directCall rejected errno declaration")
	}
	if call.direct != "_Cdirect2_read" || !call.errno {
		t.Fatalf("errno direct call = %+v", call)
	}
	var output bytes.Buffer
	call.writeDirective(&output)
	if got := output.String(); !strings.HasSuffix(got, " i32,ptr,u64 i64 errno\n") {
		t.Fatalf("directive = %q, want errno signature", got)
	}

	limit := 8
	if buildcfg.GOARCH == "amd64" {
		limit = 6
	}
	n.FuncType.Params = make([]*Type, limit)
	for i := range n.FuncType.Params {
		n.FuncType.Params[i] = &Type{Size: 8, Go: ast.NewIdent("uint64")}
	}
	if _, ok := p.directCall(n); ok {
		t.Fatal("directCall accepted errno result without a register for its result pointer")
	}
	n.FuncType.Result = nil
	if _, ok := p.directCall(n); !ok {
		t.Fatal("directCall rejected void errno declaration at the register limit")
	}
}

func TestCgoDirectFrame(t *testing.T) {
	call := cgoDirectCall{
		params: []cgoDirectValue{
			{typ: cgoDirectInt32},
			{typ: cgoDirectPointer},
			{typ: cgoDirectUint8},
		},
		result: cgoDirectValue{typ: cgoDirectUint32},
	}
	params, result, errno, size := cgoDirectFrame(call, 8)
	if got, want := fmt.Sprint(params), "[0 8 16]"; got != want {
		t.Errorf("parameter offsets = %s, want %s", got, want)
	}
	if result != 24 || errno != 0 || size != 28 {
		t.Errorf("result offsets and frame size = (%d, %d, %d), want (24, 0, 28)",
			result, errno, size)
	}

	call.errno = true
	params, result, errno, size = cgoDirectFrame(call, 8)
	if result != 24 || errno != 32 || size != 40 {
		t.Errorf("errno result offsets and frame size = (%d, %d, %d), want (24, 32, 40)",
			result, errno, size)
	}
	call.result = cgoDirectValue{typ: cgoDirectVoid}
	params, result, errno, size = cgoDirectFrame(call, 8)
	if result != 0 || errno != 24 || size != 32 {
		t.Errorf("void errno offsets and frame size = (%d, %d, %d), want (0, 24, 32)",
			result, errno, size)
	}

	call.errno = false
	call.params = []cgoDirectValue{
		{typ: cgoDirectUint8},
		{typ: cgoDirectUint64},
	}
	params, result, errno, size = cgoDirectFrame(call, 4)
	if got, want := fmt.Sprint(params), "[0 4]"; got != want {
		t.Errorf("32-bit parameter offsets = %s, want %s", got, want)
	}
	if result != 0 || errno != 0 || size != 12 {
		t.Errorf("32-bit result offsets and frame size = (%d, %d, %d), want (0, 0, 12)",
			result, errno, size)
	}

	call.params = []cgoDirectValue{
		{typ: cgoDirectFloat32},
		{typ: cgoDirectUint8},
		{typ: cgoDirectFloat64},
	}
	call.result = cgoDirectValue{typ: cgoDirectFloat32}
	params, result, errno, size = cgoDirectFrame(call, 8)
	if got, want := fmt.Sprint(params), "[0 4 8]"; got != want {
		t.Errorf("floating parameter offsets = %s, want %s", got, want)
	}
	if result != 16 || errno != 0 || size != 20 {
		t.Errorf("floating result offsets and frame size = (%d, %d, %d), want (16, 0, 20)",
			result, errno, size)
	}

	call.params = []cgoDirectValue{
		{typ: cgoDirectMemory, width: 16, align: 8},
		{typ: cgoDirectUint32},
	}
	call.result = cgoDirectValue{
		typ: cgoDirectMemory, width: 24, align: 8,
	}
	params, result, errno, size = cgoDirectFrame(call, 8)
	if got, want := fmt.Sprint(params), "[0 16]"; got != want {
		t.Errorf("aggregate parameter offsets = %s, want %s", got, want)
	}
	if result != 24 || errno != 0 || size != 48 {
		t.Errorf("aggregate result offsets and frame size = (%d, %d, %d), want (24, 0, 48)",
			result, errno, size)
	}
}

func TestCgoDirectAssemblyCall(t *testing.T) {
	if !cgoDirectSupported() {
		t.Skip("direct C assembly is not supported on this target")
	}
	call := cgoDirectCall{
		direct: "_Cdirect_add",
		entry:  "add",
		symbol: "add",
		params: []cgoDirectValue{
			{typ: cgoDirectUint64},
			{typ: cgoDirectUint64},
		},
		result: cgoDirectValue{typ: cgoDirectUint64},
	}
	var output bytes.Buffer
	(&Package{PtrSize: 8}).writeDirectAssemblyCall(&output, call)
	text := output.String()
	for _, want := range []string{
		"TEXT ·_Cdirect_add(SB),NOSPLIT,$0-24",
		"p0+0(FP)",
		"p1+8(FP)",
		"CALL\tadd(SB)",
		"ret+16(FP)",
		"\tRET\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("assembly does not contain %q:\n%s", want, text)
		}
	}
	if buildcfg.GOARCH == "arm64" {
		if !strings.Contains(text, "MOVD\tp0+0(FP), R0") {
			t.Errorf("arm64 assembly has wrong first argument:\n%s", text)
		}
	} else if !strings.Contains(text, "MOVQ\tp0+0(FP), DI") {
		t.Errorf("amd64 assembly has wrong first argument:\n%s", text)
	}

	oldObjDir := *objDir
	*objDir = t.TempDir()
	defer func() {
		*objDir = oldObjDir
	}()
	p := &Package{PtrSize: 8, directCalls: []cgoDirectCall{call}}
	p.writeDirectAssembly()
	data, err := os.ReadFile(filepath.Join(*objDir, "_cgo_direct.s"))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); !strings.Contains(got, text) {
		t.Errorf("generated assembly does not contain call:\n%s", got)
	}

	errnoCall := cgoDirectCall{
		direct: "_Cdirect2_read",
		entry:  "read_errno",
		params: []cgoDirectValue{{typ: cgoDirectInt32}},
		result: cgoDirectValue{typ: cgoDirectInt64},
		errno:  true,
	}
	output.Reset()
	(&Package{PtrSize: 8}).writeDirectAssemblyCall(&output, errnoCall)
	text = output.String()
	for _, want := range []string{
		"TEXT ·_Cdirect2_read(SB),NOSPLIT,$0-24",
		"CALL\tread_errno(SB)",
		"errno+16(FP)",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("errno assembly does not contain %q:\n%s", want, text)
		}
	}
	if buildcfg.GOARCH == "arm64" {
		for _, want := range []string{"MOVD\t$ret+8(FP), R1", "MOVD\tR0, errno+16(FP)"} {
			if !strings.Contains(text, want) {
				t.Errorf("arm64 errno assembly does not contain %q:\n%s", want, text)
			}
		}
	} else {
		for _, want := range []string{"LEAQ\tret+8(FP), SI", "MOVQ\tAX, errno+16(FP)"} {
			if !strings.Contains(text, want) {
				t.Errorf("amd64 errno assembly does not contain %q:\n%s", want, text)
			}
		}
	}

	aggregateCall := cgoDirectCall{
		direct: "_Cdirect_pair",
		entry:  "pair",
		params: []cgoDirectValue{
			{typ: cgoDirectMemory, width: 16, align: 8},
			{typ: cgoDirectUint32},
		},
		result: cgoDirectValue{
			typ: cgoDirectMemory, width: 16, align: 8,
		},
	}
	output.Reset()
	(&Package{PtrSize: 8}).writeDirectAssemblyCall(&output, aggregateCall)
	text = output.String()
	for _, want := range []string{
		"TEXT ·_Cdirect_pair(SB),NOSPLIT,$0-40",
		"CALL\tpair(SB)",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("aggregate assembly does not contain %q:\n%s", want, text)
		}
	}
	if buildcfg.GOARCH == "arm64" {
		for _, want := range []string{
			"MOVD\t$p0+0(FP), R0",
			"MOVWU\tp1+16(FP), R1",
			"MOVD\t$ret+24(FP), R2",
		} {
			if !strings.Contains(text, want) {
				t.Errorf("arm64 aggregate assembly does not contain %q:\n%s", want, text)
			}
		}
	} else {
		for _, want := range []string{
			"LEAQ\tp0+0(FP), DI",
			"MOVL\tp1+16(FP), SI",
			"LEAQ\tret+24(FP), DX",
		} {
			if !strings.Contains(text, want) {
				t.Errorf("amd64 aggregate assembly does not contain %q:\n%s", want, text)
			}
		}
	}
	if strings.Contains(text, "AX, ret+") || strings.Contains(text, "R0, ret+") {
		t.Errorf("aggregate assembly copied a register result:\n%s", text)
	}
}

func TestCgoDirectAssemblyInstructions(t *testing.T) {
	tests := []struct {
		typ        cgoDirectType
		arm64Load  string
		amd64Load  string
		arm64Store string
		amd64Store string
	}{
		{cgoDirectInt8, "MOVB", "MOVBQSX", "MOVB", "MOVB"},
		{cgoDirectUint8, "MOVBU", "MOVBQZX", "MOVB", "MOVB"},
		{cgoDirectInt16, "MOVH", "MOVWQSX", "MOVH", "MOVW"},
		{cgoDirectUint16, "MOVHU", "MOVWQZX", "MOVH", "MOVW"},
		{cgoDirectInt32, "MOVW", "MOVLQSX", "MOVW", "MOVL"},
		{cgoDirectUint32, "MOVWU", "MOVL", "MOVW", "MOVL"},
		{cgoDirectInt64, "MOVD", "MOVQ", "MOVD", "MOVQ"},
		{cgoDirectUint64, "MOVD", "MOVQ", "MOVD", "MOVQ"},
		{cgoDirectFloat32, "FMOVS", "MOVSS", "FMOVS", "MOVSS"},
		{cgoDirectFloat64, "FMOVD", "MOVSD", "FMOVD", "MOVSD"},
		{cgoDirectPointer, "MOVD", "MOVQ", "MOVD", "MOVQ"},
	}
	for _, test := range tests {
		t.Run(string(test.typ), func(t *testing.T) {
			if got := cgoDirectLoad("arm64", test.typ); got != test.arm64Load {
				t.Errorf("arm64 load = %s, want %s", got, test.arm64Load)
			}
			if got := cgoDirectLoad("amd64", test.typ); got != test.amd64Load {
				t.Errorf("amd64 load = %s, want %s", got, test.amd64Load)
			}
			if got := cgoDirectStore("arm64", test.typ); got != test.arm64Store {
				t.Errorf("arm64 store = %s, want %s", got, test.arm64Store)
			}
			if got := cgoDirectStore("amd64", test.typ); got != test.amd64Store {
				t.Errorf("amd64 store = %s, want %s", got, test.amd64Store)
			}
		})
	}

	for _, test := range []struct {
		goarch  string
		address string
		reg     string
		want    string
	}{
		{"arm64", "p0+0(FP)", "R0", "\tMOVD\t$p0+0(FP), R0\n"},
		{"amd64", "p0+0(FP)", "DI", "\tLEAQ\tp0+0(FP), DI\n"},
	} {
		var output bytes.Buffer
		writeDirectAddress(&output, test.goarch, test.address, test.reg)
		if got := output.String(); got != test.want {
			t.Errorf("%s address = %q, want %q", test.goarch, got, test.want)
		}
	}

	params := []cgoDirectType{
		cgoDirectUint64,
		cgoDirectFloat64,
		cgoDirectInt32,
		cgoDirectFloat32,
	}
	for _, test := range []struct {
		goarch string
		want   []string
	}{
		{"arm64", []string{"R0", "F0", "R1", "F1"}},
		{"amd64", []string{"DI", "X0", "SI", "X1"}},
	} {
		got, ok := cgoDirectArgRegisters(test.goarch, params)
		if !ok || !slices.Equal(got, test.want) {
			t.Errorf("%s argument registers = (%q, %t), want (%q, true)",
				test.goarch, got, ok, test.want)
		}
	}
	for _, test := range []struct {
		goarch string
		count  int
		typ    cgoDirectType
	}{
		{"amd64", 7, cgoDirectUint64},
		{"arm64", 9, cgoDirectUint64},
		{"amd64", 9, cgoDirectFloat64},
		{"arm64", 9, cgoDirectFloat64},
	} {
		params := make([]cgoDirectType, test.count)
		for i := range params {
			params[i] = test.typ
		}
		if got, ok := cgoDirectArgRegisters(test.goarch, params); ok {
			t.Errorf("%s accepted argument registers %q", test.goarch, got)
		}
	}
	for _, test := range []struct {
		goarch   string
		integer  int
		floating int
	}{
		{"amd64", 6, 8},
		{"arm64", 8, 8},
	} {
		params := make([]cgoDirectType, 0, test.integer+test.floating)
		for i := 0; i < test.integer || i < test.floating; i++ {
			if i < test.integer {
				params = append(params, cgoDirectUint64)
			}
			if i < test.floating {
				params = append(params, cgoDirectFloat64)
			}
		}
		if _, ok := cgoDirectArgRegisters(test.goarch, params); !ok {
			t.Errorf("%s rejected %d integer and %d floating arguments",
				test.goarch, test.integer, test.floating)
		}
	}
	if got, ok := cgoDirectArgRegisters("other",
		[]cgoDirectType{cgoDirectFloat64}); ok || got != nil {
		t.Errorf("unsupported architecture registers = (%q, %t), want (nil, false)",
			got, ok)
	}
	if got, ok := cgoDirectArgRegisters("other",
		[]cgoDirectType{cgoDirectUint64}); ok || got != nil {
		t.Errorf("unsupported integer registers = (%q, %t), want (nil, false)",
			got, ok)
	}
	amd64Call := cgoDirectCallInstructions("amd64", "entry")
	wantAMD64 := []string{
		"MOVQ\tSP, R12",
		"ANDQ\t$~15, SP",
		"CALL\tentry(SB)",
		"MOVQ\tR12, SP",
	}
	if len(amd64Call) != len(wantAMD64) {
		t.Fatalf("amd64 call instructions = %q, want %q", amd64Call, wantAMD64)
	}
	for i, want := range wantAMD64 {
		if got := amd64Call[i]; got != want {
			t.Errorf("amd64 call instruction %d = %q, want %q", i, got, want)
		}
	}
	arm64Call := cgoDirectCallInstructions("arm64", "entry")
	wantARM64 := []string{
		"SUB\t$16, RSP",
		"CALL\tentry(SB)",
		"ADD\t$16, RSP",
	}
	if len(arm64Call) != len(wantARM64) {
		t.Fatalf("arm64 call instructions = %q, want %q", arm64Call, wantARM64)
	}
	for i, want := range wantARM64 {
		if got := arm64Call[i]; got != want {
			t.Errorf("arm64 call instruction %d = %q, want %q", i, got, want)
		}
	}
	if got, want := cgoDirectCallInstructions("other", "entry"),
		[]string{"CALL\tentry(SB)"}; !slices.Equal(got, want) {
		t.Errorf("fallback call instructions = %q, want %q", got, want)
	}
	if got := cgoDirectResultRegister("arm64", cgoDirectUint64); got != "R0" {
		t.Errorf("arm64 result register = %s, want R0", got)
	}
	if got := cgoDirectResultRegister("amd64", cgoDirectUint64); got != "AX" {
		t.Errorf("amd64 result register = %s, want AX", got)
	}
	if got := cgoDirectResultRegister("arm64", cgoDirectFloat64); got != "F0" {
		t.Errorf("arm64 floating result register = %s, want F0", got)
	}
	if got := cgoDirectResultRegister("amd64", cgoDirectFloat32); got != "X0" {
		t.Errorf("amd64 floating result register = %s, want X0", got)
	}
	if got := cgoDirectAddress("arm64"); got != "MOVD" {
		t.Errorf("arm64 address instruction = %q, want MOVD", got)
	}
	if got := cgoDirectAddress("amd64"); got != "LEAQ" {
		t.Errorf("amd64 address instruction = %q, want LEAQ", got)
	}
	if got := cgoDirectVoid.size(); got != 0 {
		t.Errorf("void size = %d, want 0", got)
	}
}

func TestCgoDirectCBridge(t *testing.T) {
	call := cgoDirectCall{
		entry: "_cgo_test_Cfunc_add_direct",
		params: []cgoDirectValue{
			{typ: cgoDirectUint64},
			{typ: cgoDirectUint32},
		},
		result: cgoDirectValue{typ: cgoDirectUint64},
	}
	n := &Name{
		C: "add",
		FuncType: &FuncType{
			Params: []*Type{
				{C: &TypeRepr{Repr: "unsigned long long"}},
				{C: &TypeRepr{Repr: "unsigned int"}, Typedef: "uint32_t"},
			},
			Result: &Type{C: &TypeRepr{Repr: "unsigned long long"}},
		},
	}
	var output bytes.Buffer
	new(Package).writeDirectCBridge(&output, n, call)
	for _, want := range []string{
		"CGO_NO_SANITIZE_THREAD\n",
		"unsigned long long\n_cgo_test_Cfunc_add_direct(",
		"unsigned long long p0, uint32_t p1",
		"\treturn add(p0, p1);\n",
	} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("C bridge does not contain %q:\n%s", want, output.String())
		}
	}

	n.FuncType.Params = nil
	n.FuncType.Result = nil
	call.entry = "_cgo_test_Cfunc_tick_direct"
	call.params = nil
	call.result = cgoDirectValue{typ: cgoDirectVoid}
	n.C = "tick"
	output.Reset()
	new(Package).writeDirectCBridge(&output, n, call)
	for _, want := range []string{
		"void\n_cgo_test_Cfunc_tick_direct()",
		"\ttick();\n",
	} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("void C bridge does not contain %q:\n%s", want, output.String())
		}
	}

	n.FuncType.Params = []*Type{
		{C: &TypeRepr{Repr: "unsigned long long"}},
	}
	n.FuncType.Result = &Type{C: &TypeRepr{Repr: "long long"}}
	call.entry = "_cgo_test_C2func_read_direct"
	call.params = []cgoDirectValue{{typ: cgoDirectUint64}}
	call.result = cgoDirectValue{typ: cgoDirectInt64}
	call.errno = true
	n.C = "read_value"
	output.Reset()
	new(Package).writeDirectCBridge(&output, n, call)
	for _, want := range []string{
		"size_t\n_cgo_test_C2func_read_direct(",
		"unsigned long long p0, long long *result",
		"\tlong long value;",
		"\terrno = 0;",
		"\tvalue = read_value(p0);",
		"\tsaved_errno = errno;",
		"\t*result = value;",
		"\treturn (size_t)saved_errno;",
	} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("errno C bridge does not contain %q:\n%s", want, output.String())
		}
	}

	n.FuncType.Params = nil
	n.FuncType.Result = nil
	call.entry = "_cgo_test_C2func_tick_direct"
	call.params = nil
	call.result = cgoDirectValue{typ: cgoDirectVoid}
	n.C = "tick"
	output.Reset()
	new(Package).writeDirectCBridge(&output, n, call)
	if strings.Contains(output.String(), "*result") ||
		!strings.Contains(output.String(), "\ttick();\n\tsaved_errno = errno;") {
		t.Errorf("void errno C bridge is invalid:\n%s", output.String())
	}

	pairType := &Type{
		C:       &TypeRepr{Repr: "struct pair"},
		Typedef: "pair_t",
	}
	n.FuncType.Params = []*Type{pairType, {
		C: &TypeRepr{Repr: "unsigned int"},
	}}
	n.FuncType.Result = pairType
	n.C = "add_pair"
	call = cgoDirectCall{
		entry: "_cgo_test_Cfunc_add_pair_direct",
		params: []cgoDirectValue{
			{typ: cgoDirectMemory, width: 16, align: 8},
			{typ: cgoDirectUint32},
		},
		result: cgoDirectValue{
			typ: cgoDirectMemory, width: 16, align: 8,
		},
	}
	output.Reset()
	new(Package).writeDirectCBridge(&output, n, call)
	for _, want := range []string{
		"void\n_cgo_test_Cfunc_add_pair_direct(",
		"const pair_t *p0, unsigned int p1, pair_t *result",
		"\t*result = add_pair(*p0, p1);",
	} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("aggregate C bridge does not contain %q:\n%s", want, output.String())
		}
	}
}

func TestCgoDirectGoDeclaration(t *testing.T) {
	oldExperiment := buildcfg.Experiment
	oldGccgo := *gccgo
	buildcfg.Experiment.Coro = true
	*gccgo = false
	defer func() {
		buildcfg.Experiment = oldExperiment
		*gccgo = oldGccgo
	}()

	cType := func() *Type {
		return &Type{Size: 8, Align: 8, Go: ast.NewIdent("uint64")}
	}
	goType := ast.NewIdent("_Ctype_uint64_t")
	n := &Name{
		Go:     "add",
		C:      "add",
		Kind:   "func",
		Mangle: "_Cfunc_add",
		FuncType: &FuncType{
			Params: []*Type{cType(), cType()},
			Result: cType(),
			Go: &ast.FuncType{
				Params: &ast.FieldList{List: []*ast.Field{
					{Type: goType},
					{Type: goType},
				}},
				Results: &ast.FieldList{List: []*ast.Field{{Type: goType}}},
			},
		},
	}
	p := &Package{
		PtrSize:     8,
		noCallbacks: map[string]bool{"add": true},
		noEscapes:   map[string]bool{"add": true},
	}

	var output bytes.Buffer
	callsMalloc := false
	p.writeDefsFunc(&output, n, &callsMalloc)
	text := output.String()
	for _, want := range []string{
		"//go:cgo_direct v1 _Cfunc_add _Cdirect_add add mayblock u64,u64 u64 -",
		"//go:noescape\nfunc _Cdirect_add(p0 _Ctype_uint64_t, p1 _Ctype_uint64_t) (ret _Ctype_uint64_t)",
		"func _Cfunc_add(",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("generated Go does not contain %q:\n%s", want, text)
		}
	}
	if callsMalloc {
		t.Error("ordinary declaration unexpectedly called malloc")
	}
	if len(p.directCalls) != 1 || p.directCalls[0].direct != "_Cdirect_add" {
		t.Fatalf("direct calls = %+v, want _Cdirect_add", p.directCalls)
	}

	n.Mangle = "_C2func_add"
	n.AddError = true
	p.directCalls = nil
	output.Reset()
	p.writeDefsFunc(&output, n, &callsMalloc)
	text = output.String()
	for _, want := range []string{
		"//go:cgo_direct v1 _C2func_add _Cdirect2_add add mayblock u64,u64 u64 errno",
		"func _Cdirect2_add(p0 _Ctype_uint64_t, p1 _Ctype_uint64_t) (ret _Ctype_uint64_t, errno syscall.Errno)",
		"func _C2func_add(",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("generated errno Go does not contain %q:\n%s", want, text)
		}
	}
	if len(p.directCalls) != 1 || p.directCalls[0].direct != "_Cdirect2_add" {
		t.Fatalf("errno direct calls = %+v, want _Cdirect2_add", p.directCalls)
	}

	n.FuncType.Result = nil
	n.FuncType.Go = &ast.FuncType{
		Params: &ast.FieldList{List: []*ast.Field{
			{Type: goType},
			{Type: goType},
		}},
		Results: new(ast.FieldList),
	}
	p.directCalls = nil
	output.Reset()
	p.writeDefsFunc(&output, n, &callsMalloc)
	text = output.String()
	want := "func _Cdirect2_add(p0 _Ctype_uint64_t, p1 _Ctype_uint64_t) (errno syscall.Errno)"
	if !strings.Contains(text, want) {
		t.Errorf("generated void errno Go does not contain %q:\n%s", want, text)
	}
}

func TestValidCgoDirectSymbol(t *testing.T) {
	for _, name := range []string{"f", "_f", "f0", "C_function"} {
		if !validCgoDirectSymbol(name) {
			t.Errorf("validCgoDirectSymbol(%q) = false", name)
		}
	}
	for _, name := range []string{"", "0f", "f.name", "f-name"} {
		if validCgoDirectSymbol(name) {
			t.Errorf("validCgoDirectSymbol(%q) = true", name)
		}
	}
}
