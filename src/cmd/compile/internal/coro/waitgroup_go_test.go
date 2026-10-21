// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/ir"
	"cmd/compile/internal/typecheck"
	"cmd/compile/internal/types"
	"cmd/internal/src"
	"strings"
	"testing"
)

func waitGroupGoFixture(t *testing.T) (*types.Type, *ir.Func) {
	t.Helper()
	pkg := types.NewPkg("sync", "sync")
	name := ir.NewDeclNameAt(src.NoXPos, ir.OTYPE, pkg.Lookup("WaitGroup"))
	typ := types.NewNamed(name)
	name.SetType(typ)
	name.SetTypecheck(1)
	typ.SetUnderlying(types.NewStruct(nil))
	recv := types.NewPtr(typ)
	var methods []*types.Field
	var goMethod *ir.Func
	for _, method := range []string{"Add", "Done", "Go"} {
		var params []*types.Field
		if method == "Add" {
			params = []*types.Field{types.NewField(src.NoXPos, nil, types.Types[types.TINT])}
		}
		if method == "Go" {
			params = []*types.Field{types.NewField(src.NoXPos, nil, types.NewSignature(nil, nil, nil))}
		}
		sig := types.NewSignature(types.NewField(src.NoXPos, nil, recv), params, nil)
		fn := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("(*WaitGroup)."+method), sig)
		fn.Nname.Defn = fn
		field := types.NewField(src.NoXPos, types.LocalPkg.Lookup(method), sig)
		field.Nname = fn.Nname
		methods = append(methods, field)
		SetSummary(fn, FuncSummary{Terminal: MayPanic})
		if method == "Go" {
			goMethod = fn
		}
	}
	typ.SetMethods(methods)
	return recv, goMethod
}

func TestWaitGroupGoSignature(t *testing.T) {
	prepareLowerTest(t)
	oldPkg := types.LocalPkg
	defer func() { types.LocalPkg = oldPkg }()
	types.LocalPkg = types.NewPkg("example.com/coro/wgsignature", "p")
	for _, mode := range []string{"valid", "wrong-owner", "wrong-name", "no-receiver", "wrong-callback", "missing-done", "bad-add"} {
		t.Run(mode, func(t *testing.T) {
			recv, fn := waitGroupGoFixture(t)
			switch mode {
			case "wrong-owner":
				fn.Nname.SetSym(types.LocalPkg.Lookup("(*WaitGroup).Go"))
			case "wrong-name":
				fn.Nname.SetSym(fn.Sym().Pkg.Lookup("(*WaitGroup).Other"))
			case "no-receiver":
				fn.Nname.SetType(types.NewSignature(nil, fn.Type().Params(), nil))
			case "wrong-callback":
				fn.Type().Param(0).Type = types.Types[types.TINT]
			case "missing-done":
				recv.Elem().SetMethods([]*types.Field{recv.Elem().Methods()[0]})
			case "bad-add":
				recv.Elem().Methods()[0].Type.Param(0).Type = types.Types[types.TBOOL]
			}
			if got := waitGroupGoSignature(fn); got != (mode == "valid") {
				t.Fatalf("signature accepted=%v", got)
			}
		})
	}
	if waitGroupGoSignature(nil) {
		t.Fatal("nil method accepted")
	}
}

func TestWaitGroupGoCallbackFactories(t *testing.T) {
	prepareLowerTest(t)
	oldPkg, oldTarget := types.LocalPkg, typecheck.Target
	defer func() { types.LocalPkg, typecheck.Target = oldPkg, oldTarget }()
	for _, mode := range []string{"bounded-plain", "unbounded-plain", "open", "method-alias", "callee-init"} {
		t.Run(mode, func(t *testing.T) {
			types.LocalPkg = types.NewPkg("example.com/coro/wgcallback/"+mode, "p")
			typecheck.Target = new(ir.Package)
			recv, goMethod := waitGroupGoFixture(t)
			callback := newLowerTestFunc(types.LocalPkg, "callback")
			callback.Nname.Defn = callback
			callback.Body = ir.Nodes{newLowerTestReturn()}
			if mode == "unbounded-plain" {
				callback.Body = ir.Nodes{ir.NewForStmt(src.NoXPos, nil, ir.NewBool(src.NoXPos, false), nil, nil, false), newLowerTestReturn()}
			}
			sig := types.NewSignature(nil, []*types.Field{
				types.NewField(src.NoXPos, types.LocalPkg.Lookup("wg"), recv),
				types.NewField(src.NoXPos, types.LocalPkg.Lookup("unknown"), callback.Type()),
			}, nil)
			parent := ir.NewFunc(src.NoXPos, src.NoXPos, types.LocalPkg.Lookup("parent"), sig)
			parent.Nname.Defn = parent
			parent.DeclareParams(true)
			typecheck.Target.Funcs = append(typecheck.Target.Funcs, parent)
			var call *ir.CallExpr
			ir.WithFunc(parent, func() {
				var value ir.Node = callback.Nname
				if mode == "open" {
					value = parent.Dcl[1]
				}
				method := typecheck.NewMethodExpr(src.NoXPos, recv, types.LocalPkg.Lookup("Go"))
				var callee ir.Node = method
				if mode == "method-alias" {
					alias := parent.NewLocal(src.NoXPos, types.LocalPkg.Lookup("method"), method.Type())
					assignment := ir.NewAssignStmt(src.NoXPos, alias, method)
					alias.Defn = assignment
					parent.Body = ir.Nodes{ir.NewDecl(src.NoXPos, ir.ODCL, alias), assignment}
					callee = alias
				}
				if mode == "callee-init" {
					method.SetInit(ir.Nodes{ir.NewAssignStmt(src.NoXPos, ir.BlankNode, ir.NewInt(src.NoXPos, 1))})
				}
				call = typecheck.Call(src.NoXPos, callee, ir.Nodes{parent.Dcl[0], value}, false).(*ir.CallExpr)
				parent.Body = append(parent.Body, call, newLowerTestReturn())
				typecheck.Stmts(parent.Body)
			})
			plan, err := AnalyzeWithCallbacks(typecheck.Target.Funcs, nil)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "open" || mode == "method-alias" || mode == "callee-init" {
				if len(plan.specializedBodies) != 0 || edgeForCall(plan.Functions[parent], call).Callee != goMethod {
					t.Fatal("open callback or evaluated method callee acquired a specialization")
				}
				return
			}
			if len(plan.specializedBodies) != 3 {
				t.Fatalf("helpers=%d", len(plan.specializedBodies))
			}
			var forced bool
			for _, function := range plan.Functions {
				for _, edge := range function.Edges {
					if edge.FactoryRequired {
						forced = true
						if len(edge.Targets) != 1 || edge.Targets[0] != callback {
							t.Fatal("callback proof escaped its target set")
						}
					}
				}
			}
			if !forced {
				t.Fatal("all-plain callback bypasses factory validation")
			}
			_, err = Lower(plan, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := plan.Functions[parent].Factory != NoFactory; got != (mode == "bounded-plain") {
				t.Fatalf("parent factory=%v", plan.Functions[parent].Factory)
			}
			for fn := range plan.specializedBodies {
				var calls []string
				var recoverFound bool
				ir.Visit(fn, func(n ir.Node) {
					if n.Op() == ir.ORECOVER {
						recoverFound = true
					}
					if call, ok := n.(*ir.CallExpr); ok && call.Fun != nil {
						calls = append(calls, symbolName(ir.StaticCalleeName(call.Fun)))
					}
				})
				for _, name := range calls {
					if strings.HasPrefix(name, "runtime.coro") {
						t.Fatalf("ordinary helper %s starts native context: %v", fn.Sym().Name, calls)
					}
				}
				if strings.HasSuffix(fn.Sym().Name, ".done") && !recoverFound {
					t.Fatal("ordinary cleanup lost direct recover")
				}
				if !strings.Contains(fn.Sym().Name, ".child") && !strings.HasSuffix(fn.Sym().Name, ".done") {
					found := false
					for _, name := range calls {
						found = found || name == "sync.(*WaitGroup).Go"
					}
					if !found {
						t.Fatalf("ordinary creator lost original method: %v", calls)
					}
				}
			}
		})
	}
}
