// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/ir"
	"cmd/compile/internal/types"
	"cmd/internal/src"
	"strings"
	"testing"
)

func TestParseCgoDirectDirectives(t *testing.T) {
	directives := [][]string{
		{"cgo_direct", "v1", "_Cfunc_add", "_Cdirect_add", "add",
			"mayblock", "u64,u64", "u64", "-"},
		{"cgo_direct", "v1", "_Cfunc_read", "_Cdirect_read", "read",
			"mayblock", "i32,ptr,u64", "i64", "errno"},
		{"cgo_direct", "v1", "_Cfunc_all", "_Cdirect_all", "all",
			"mayblock", "i8,i16,i32,i64,u8,u16,f32,f64", "u32", "-"},
	}
	calls, err := parseCgoDirectives(directives)
	if err != nil {
		t.Fatal(err)
	}
	add := calls["_Cfunc_add"]
	if add.class != DirectMayBlock || len(add.params) != 2 ||
		add.params[0] != cgoABIUint64 || add.result != cgoABIUint64 ||
		add.errno {
		t.Fatalf("add metadata = %+v", add)
	}
	read := calls["_Cfunc_read"]
	if len(read.params) != 3 || read.params[1] != cgoABIPointer ||
		read.result != cgoABIInt64 || !read.errno {
		t.Fatalf("read metadata = %+v", read)
	}
	all := calls["_Cfunc_all"]
	wantParams := []cgoABIType{
		cgoABIInt8, cgoABIInt16, cgoABIInt32, cgoABIInt64,
		cgoABIUint8, cgoABIUint16, cgoABIFloat32, cgoABIFloat64,
	}
	if len(all.params) != len(wantParams) {
		t.Fatalf("all metadata params = %v, want %v", all.params, wantParams)
	}
	for i, want := range wantParams {
		if all.params[i] != want {
			t.Errorf("all metadata param %d = %v, want %v", i, all.params[i], want)
		}
	}
	if all.result != cgoABIUint32 {
		t.Errorf("all metadata result = %v, want %v", all.result, cgoABIUint32)
	}
}

func TestParseCgoDirectDirectiveErrors(t *testing.T) {
	valid := []string{
		"cgo_direct", "v1", "_Cfunc_f", "_Cdirect_f", "f",
		"mayblock", "-", "void", "-",
	}
	tests := []struct {
		name   string
		change func([]string) []string
		want   string
	}{
		{"fields", func(d []string) []string { return d[:8] }, "invalid"},
		{"version", func(d []string) []string { d[1] = "v2"; return d }, "version"},
		{"class", func(d []string) []string { d[5] = "noblock"; return d }, "class"},
		{"parameter", func(d []string) []string { d[6] = "f128"; return d }, "ABI type"},
		{"empty parameter", func(d []string) []string { d[6] = ""; return d }, "ABI type"},
		{"void parameter", func(d []string) []string { d[6] = "void"; return d }, "void"},
		{"too many parameters", func(d []string) []string {
			d[6] = "i8,i8,i8,i8,i8,i8,i8,i8,i8,i8,i8,i8,i8,i8,i8,i8,i8"
			return d
		}, "maximum"},
		{"result", func(d []string) []string { d[7] = "word"; return d }, "ABI type"},
		{"errno", func(d []string) []string { d[8] = "sometimes"; return d }, "errno"},
		{"empty wrapper", func(d []string) []string { d[2] = ""; return d }, "empty"},
		{"empty direct", func(d []string) []string { d[3] = ""; return d }, "empty"},
		{"empty symbol", func(d []string) []string { d[4] = ""; return d }, "empty"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			directive := append([]string(nil), valid...)
			_, err := parseCgoDirectives([][]string{test.change(directive)})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("parseCgoDirectives error = %v, want %q", err, test.want)
			}
		})
	}

	_, err := parseCgoDirectives([][]string{valid, valid})
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate error = %v", err)
	}
}

func TestCgoDirectRecipe(t *testing.T) {
	tests := []struct {
		class  ForeignCallClass
		effect Effect
		exec   ExecFlags
	}{
		{DirectNoBlock, NoSuspend, NeedsSystemABI},
		{DirectMayBlock, NoSuspend, NeedsSystemABI | MayBlockThread},
		{AsyncOperation, MaySuspend, NeedsSystemABI},
		{NotForeign, NoSuspend, 0},
	}
	for _, test := range tests {
		call := cgoDirectCall{class: test.class}
		recipe := call.recipe()
		if recipe.Kind != SiteForeign || recipe.Foreign != test.class ||
			recipe.Effect != test.effect || recipe.Exec != test.exec {
			t.Errorf("%v recipe = %+v", test.class, recipe)
		}
	}
}

func TestCgoDirectRecipeOverridesWrapper(t *testing.T) {
	wrapper := testFunc("_Cfunc_add")
	caller := testFunc("caller")
	recipe := cgoDirectCall{class: DirectMayBlock}.recipe()
	edge := Edge{
		Callee: wrapper,
		Recipe: recipe,
	}
	function := &Function{
		Func:  caller,
		Edges: []Edge{edge},
	}
	plan := &Plan{
		Functions: map[*ir.Func]*Function{
			wrapper: {Func: wrapper, Effect: MaySuspend, Exec: ThreadAffine},
			caller:  function,
		},
		cgoDirect: map[string]cgoDirectCall{
			"_Cfunc_add": {class: DirectMayBlock},
		},
	}

	got, ok := plan.operationRecipe(wrapper)
	if !ok || got != recipe {
		t.Fatalf("operationRecipe = (%+v, %t), want (%+v, true)",
			got, ok, recipe)
	}
	if plan.edgeMaySuspend(edge) {
		t.Fatal("cgo recipe inherited suspension from its fallback wrapper")
	}
	if got, want := plan.calledExec(function),
		NeedsSystemABI|MayBlockThread; got != want {
		t.Fatalf("calledExec = %v, want %v", got, want)
	}
	if got := plan.edgeEffect(edge); got != NoSuspend {
		t.Fatalf("edgeEffect = %v, want %v", got, NoSuspend)
	}
}

func TestAnalyzeCgoDirectives(t *testing.T) {
	if _, err := Analyze(nil, [][]string{{"cgo_direct"}}); err == nil {
		t.Fatal("Analyze accepted invalid cgo metadata")
	}
	plan, err := Analyze(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Functions) != 0 || len(plan.cgoDirect) != 0 {
		t.Fatalf("empty plan = %+v", plan)
	}
}

func TestAnalyzeCgoDirectEntry(t *testing.T) {
	pkg := types.NewPkg("example.com/coro/cgotest", "cgotest")
	newFunc := func(name string) *ir.Func {
		fn := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup(name),
			types.NewSignature(nil, nil, nil))
		fn.DeclareParams(true)
		return fn
	}

	// ABI wrappers can share the direct entry's linker symbol. The local
	// lookup must retain the source body that cmd/cgo generated.
	directABI := newFunc("_Cdirect_add")
	directABI.SetABIWrapper(true)
	direct := newFunc("_Cdirect_add")
	wrapper := newFunc("_Cfunc_add")
	wrapperABI := newFunc("_Cfunc_add")
	wrapperABI.SetABIWrapper(true)
	wrapperABICall := ir.NewCallExpr(src.NoXPos, ir.OCALLFUNC,
		wrapper.Nname, nil)
	wrapperABICall.SetTypecheck(1)
	wrapperABI.Body = ir.Nodes{wrapperABICall}
	caller := newFunc("caller")
	call := ir.NewCallExpr(src.NoXPos, ir.OCALLFUNC, wrapper.Nname, nil)
	call.SetTypecheck(1)
	caller.Body = ir.Nodes{call}

	directive := []string{
		"cgo_direct", "v1", "_Cfunc_add", "_Cdirect_add", "add",
		"mayblock", "-", "void", "-",
	}
	plan, err := Analyze([]*ir.Func{
		directABI, direct, wrapper, wrapperABI, caller,
	},
		[][]string{directive})
	if err != nil {
		t.Fatal(err)
	}
	function := plan.Functions[caller]
	if function == nil || len(function.Edges) != 1 || len(function.Sites) != 1 {
		t.Fatalf("caller analysis = %+v", function)
	}
	edge := function.Edges[0]
	if edge.Direct != direct {
		t.Fatalf("direct entry = %v, want %v", edge.Direct, direct)
	}
	if edge.Recipe.Direct != "_Cdirect_add" ||
		edge.Recipe.Foreign != DirectMayBlock {
		t.Fatalf("direct recipe = %+v", edge.Recipe)
	}
	if got, want := function.Exec,
		NeedsSystemABI|MayBlockThread; got != want {
		t.Fatalf("caller execution requirements = %v, want %v", got, want)
	}
	if function := plan.Functions[wrapperABI]; len(function.Edges) != 0 ||
		len(function.Sites) != 0 {
		t.Fatalf("cgo ABI wrapper analysis = %+v, want no call sites", function)
	}
}
