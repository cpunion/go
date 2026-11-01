// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/ir"
	"cmd/compile/internal/typecheck"
	"cmd/compile/internal/types"
	"cmd/internal/src"
	"testing"
)

func TestOrdinaryHelperStackBudget(t *testing.T) {
	prepareLowerTest(t)
	oldPkg, oldTarget := types.LocalPkg, typecheck.Target
	defer func() { types.LocalPkg, typecheck.Target = oldPkg, oldTarget }()
	for _, test := range []struct {
		name    string
		locals  []int64
		asserts []int64
		want    bool
	}{
		{name: "empty"},
		{name: "local-boundary", locals: []int64{200}},
		{name: "local-alignment", locals: []int64{199, 1}, want: true},
		{name: "large-local", locals: []int64{32768}, want: true},
		{name: "assert-boundary", asserts: []int64{100}},
		{name: "assert-beyond", asserts: []int64{101}, want: true},
		{name: "combined-asserts", asserts: []int64{64, 64}, want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			pkg := types.NewPkg("example.com/coro/ordinarystack/"+test.name, "p")
			types.LocalPkg, typecheck.Target = pkg, new(ir.Package)
			fn := newLowerTestFunc(pkg, "helper")
			for _, size := range test.locals {
				local := typecheck.TempAt(src.NoXPos, fn, types.NewArray(types.Types[types.TUINT8], size))
				fn.Body = append(fn.Body, ir.NewDecl(src.NoXPos, ir.ODCL, local))
			}
			for _, size := range test.asserts {
				value := fn.NewLocal(src.NoXPos, pkg.Lookup("value"), types.Types[types.TINTER])
				assert := ir.NewTypeAssertExpr(src.NoXPos, value, types.NewArray(types.Types[types.TUINT8], size))
				assert.SetTypecheck(1)
				fn.Body = append(fn.Body, ir.NewAssignStmt(src.NoXPos, ir.BlankNode, assert))
			}
			if got := ordinaryHelperNeedsGoStack(fn); got != test.want {
				t.Fatalf("needs Go stack=%t want=%t", got, test.want)
			}
		})
	}
}

func TestOrdinaryHelperStackPropagation(t *testing.T) {
	prepareLowerTest(t)
	oldPkg, oldTarget := types.LocalPkg, typecheck.Target
	defer func() { types.LocalPkg, typecheck.Target = oldPkg, oldTarget }()
	for _, mode := range []string{"direct", "imported", "defer", "go"} {
		t.Run(mode, func(t *testing.T) {
			pkg := types.NewPkg("example.com/coro/ordinarystack/"+mode, "p")
			types.LocalPkg, typecheck.Target = pkg, new(ir.Package)
			helper, caller := newLowerTestFunc(pkg, "helper"), newLowerTestFunc(pkg, "caller")
			local := typecheck.TempAt(src.NoXPos, helper, types.NewArray(types.Types[types.TUINT8], 32768))
			helper.Body = ir.Nodes{ir.NewDecl(src.NoXPos, ir.ODCL, local), newLowerTestReturn()}
			var call ir.Node = newLowerTestCall(helper)
			if mode == "defer" || mode == "go" {
				op := ir.ODEFER
				if mode == "go" {
					op = ir.OGO
				}
				call = ir.NewGoDeferStmt(src.NoXPos, op, call)
				call.SetTypecheck(1)
			}
			caller.Body = ir.Nodes{call, newLowerTestReturn()}
			funcs := []*ir.Func{helper, caller}
			if mode == "imported" {
				SetSummary(helper, FuncSummary{Exec: NeedsGoStack})
				defer delete(summaries, helper)
				funcs = funcs[1:]
			}
			plan, err := Analyze(funcs, nil)
			if err != nil {
				t.Fatal(err)
			}
			if mode != "imported" && (plan.Functions[helper].Effect != NoSuspend || plan.Functions[helper].Exec != NeedsGoStack) {
				t.Fatal("large ordinary helper lost its independent stack constraint")
			}
			want := mode != "go"
			if got := plan.Functions[caller].Exec&NeedsGoStack != 0; got != want {
				t.Fatalf("caller Go-stack constraint=%t want=%t", got, want)
			}
			if plan.Functions[caller].Effect != NoSuspend {
				t.Fatal("stack constraint changed suspension effects")
			}
		})
	}
}
