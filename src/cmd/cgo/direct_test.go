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
		{"errno result", func(_ *Package, n *Name) {
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

func TestCgoDirectFrame(t *testing.T) {
	call := cgoDirectCall{
		params: []cgoDirectType{
			cgoDirectInt32,
			cgoDirectPointer,
			cgoDirectUint8,
		},
		result: cgoDirectUint32,
	}
	params, result, size := cgoDirectFrame(call, 8)
	if got, want := fmt.Sprint(params), "[0 8 16]"; got != want {
		t.Errorf("parameter offsets = %s, want %s", got, want)
	}
	if result != 24 || size != 28 {
		t.Errorf("result offset and frame size = (%d, %d), want (24, 28)",
			result, size)
	}

	call.result = cgoDirectVoid
	params, result, size = cgoDirectFrame(call, 8)
	if result != 0 || size != 17 {
		t.Errorf("void result offset and frame size = (%d, %d), want (0, 17)",
			result, size)
	}

	call.params = []cgoDirectType{cgoDirectUint8, cgoDirectUint64}
	params, result, size = cgoDirectFrame(call, 4)
	if got, want := fmt.Sprint(params), "[0 4]"; got != want {
		t.Errorf("32-bit parameter offsets = %s, want %s", got, want)
	}
	if result != 0 || size != 12 {
		t.Errorf("32-bit result offset and frame size = (%d, %d), want (0, 12)",
			result, size)
	}

	call.params = []cgoDirectType{
		cgoDirectFloat32,
		cgoDirectUint8,
		cgoDirectFloat64,
	}
	call.result = cgoDirectFloat32
	params, result, size = cgoDirectFrame(call, 8)
	if got, want := fmt.Sprint(params), "[0 4 8]"; got != want {
		t.Errorf("floating parameter offsets = %s, want %s", got, want)
	}
	if result != 16 || size != 20 {
		t.Errorf("floating result offset and frame size = (%d, %d), want (16, 20)",
			result, size)
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
		params: []cgoDirectType{
			cgoDirectUint64,
			cgoDirectUint64,
		},
		result: cgoDirectUint64,
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
	if got := cgoDirectVoid.size(); got != 0 {
		t.Errorf("void size = %d, want 0", got)
	}
}

func TestCgoDirectCBridge(t *testing.T) {
	call := cgoDirectCall{entry: "_cgo_test_Cfunc_add_direct"}
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
		"//go:noescape\nfunc _Cdirect_add(",
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
