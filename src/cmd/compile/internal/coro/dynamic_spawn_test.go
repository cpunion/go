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

func TestUnwrapDynamicGoCallBoundaries(t *testing.T) {
	prepareLowerTest(t)
	oldPkg, oldTarget := types.LocalPkg, typecheck.Target
	defer func() { types.LocalPkg, typecheck.Target = oldPkg, oldTarget }()
	for _, mode := range []string{"valid", "source-closure", "missing-capture", "foreign-parent", "non-name"} {
		t.Run(mode, func(t *testing.T) {
			f := newGoCallFixture(t)
			value := f.caller.NewLocal(src.NoXPos, types.LocalPkg.Lookup("function"), f.child.Type())
			capture := ir.NewClosureVar(src.NoXPos, f.wrapper, value)
			f.wrapper.WrappedFunc = nil
			f.inner.Fun = capture
			switch mode {
			case "source-closure":
				f.wrapper.SetWrapper(false)
			case "missing-capture":
				f.wrapper.ClosureVars = f.wrapper.ClosureVars[:1]
			case "foreign-parent":
				value.Curfn = f.child
			case "non-name":
				f.inner.Fun = ir.NewNilExpr(src.NoXPos, f.child.Type())
			}
			got := unwrapGoCall(f.call)
			if mode == "valid" {
				if got == f.call || got.Fun != value || got.Args[0] != f.outer || f.inner.Fun != capture {
					t.Fatal("dynamic wrapper lost its snapshot or modified the fallback")
				}
			} else if got != f.call {
				t.Fatal("unrecognized wrapper bypassed the ordinary boundary")
			}
		})
	}
}

func TestLowerDynamicSpawn(t *testing.T) {
	prepareLowerTest(t)
	oldPkg, oldTarget := types.LocalPkg, typecheck.Target
	defer func() { types.LocalPkg, typecheck.Target = oldPkg, oldTarget }()
	for _, mode := range []string{"direct", "wrapped", "missing-factory", "unknown"} {
		t.Run(mode, func(t *testing.T) {
			pkg := types.NewPkg("example.com/coro/dynamicspawn/"+mode, "p")
			types.LocalPkg, typecheck.Target = pkg, new(ir.Package)
			child := func(name string) *ir.Func {
				var params, results []*types.Field
				if mode != "direct" {
					params = []*types.Field{types.NewField(src.NoXPos, pkg.Lookup("argument"), types.Types[types.TINT])}
					results = []*types.Field{types.NewField(src.NoXPos, nil, types.Types[types.TINT])}
				}
				fn := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup(name), types.NewSignature(nil, params, results))
				fn.Nname.Defn = fn
				fn.DeclareParams(true)
				typecheck.Target.Funcs = append(typecheck.Target.Funcs, fn)
				ret := newLowerTestReturn()
				if len(results) != 0 {
					ret.Results = ir.Nodes{fn.Dcl[0]}
				}
				fn.Body = ir.Nodes{ret}
				return fn
			}
			first, second := child("first"), child("second")
			yield := ir.NewFunc(src.NoXPos, src.NoXPos, types.NewPkg("runtime", "runtime").Lookup("Gosched"), types.NewSignature(nil, nil, nil))
			first.Body = append(ir.Nodes{newLowerTestCall(yield)}, first.Body...)
			parent := newLowerTestFunc(pkg, "parent")
			f := parent.NewLocal(src.NoXPos, pkg.Lookup("f"), first.Type())
			parent.Body = ir.Nodes{ir.NewDecl(src.NoXPos, ir.ODCL, f), ir.NewAssignStmt(src.NoXPos, f, first.Nname),
				ir.NewIfStmt(src.NoXPos, ir.NewBool(src.NoXPos, true), ir.Nodes{ir.NewAssignStmt(src.NoXPos, f, second.Nname)}, nil)}
			call := ir.NewCallExpr(src.NoXPos, ir.OCALLFUNC, f, nil)
			call.SetTypecheck(1)
			goStmt := ir.NewGoDeferStmt(src.NoXPos, ir.OGO, call)
			goStmt.SetTypecheck(1)
			funcs := []*ir.Func{first, second, parent}
			if mode != "direct" {
				value := parent.NewLocal(src.NoXPos, pkg.Lookup("snapshot"), f.Type())
				argument := parent.NewLocal(src.NoXPos, pkg.Lookup("argument"), types.Types[types.TINT])
				wrapper := ir.NewClosureFunc(src.NoXPos, src.NoXPos, ir.OGO, types.NewSignature(nil, nil, nil), parent, typecheck.Target, 0)
				wrapper.DeclareParams(true)
				wrapper.SetWrapper(true)
				fun := ir.NewClosureVar(src.NoXPos, wrapper, value)
				arg := ir.NewClosureVar(src.NoXPos, wrapper, argument)
				inner := ir.NewCallExpr(src.NoXPos, ir.OCALLFUNC, fun, ir.Nodes{arg})
				inner.SetTypecheck(1)
				wrapper.Body = ir.Nodes{inner}
				call.Fun = wrapper.OClosure
				goStmt.SetInit(ir.Nodes{ir.NewDecl(src.NoXPos, ir.ODCL, value), ir.NewDecl(src.NoXPos, ir.ODCL, argument),
					ir.NewAssignListStmt(src.NoXPos, ir.OAS2, ir.Nodes{value, argument}, ir.Nodes{f, typedInt(src.NoXPos, types.Types[types.TINT], 42)})})
				funcs = append(funcs, wrapper)
				unwrapped := unwrapGoCall(call)
				if unwrapped == call || unwrapped.Fun != value || unwrapped.Args[0] != argument || inner.Fun != fun || inner.Args[0] != arg {
					t.Fatal("dynamic wrapper did not preserve its frozen caller operands and original body")
				}
			}
			parent.Body = append(parent.Body, goStmt, newLowerTestReturn())
			if mode == "unknown" {
				f.Class = ir.PEXTERN
			}
			if mode == "missing-factory" {
				// A plain target with a helper call has no bounded adapter.
				leaf := newLowerTestFunc(pkg, "leaf")
				leaf.Body = ir.Nodes{newLowerTestReturn()}
				funcs = append(funcs, leaf)
				second.Body = append(ir.Nodes{newLowerTestCall(leaf)}, second.Body...)
			}
			plan, err := Analyze(funcs, nil)
			if err != nil {
				t.Fatal(err)
			}
			var edge Edge
			for _, candidate := range plan.Functions[parent].Edges {
				if candidate.Node == call {
					edge = candidate
				}
			}
			if mode != "unknown" && (edge.Kind != GoCall || edge.Callee != nil || len(edge.Targets) != 2 || edge.Unknown) {
				t.Fatalf("lost closed spawn proof: %+v", edge)
			}
			if function := plan.Functions[parent]; function.Effect != NoSuspend || function.Exec != NeedsTaskContext || function.Terminal != 0 {
				t.Fatalf("child effects leaked into creator: %+v", function)
			}
			result, err := Lower(plan, nil)
			if err != nil {
				t.Fatal(err)
			}
			if result.Lowered != 2 || plan.Functions[parent].Factory == NoFactory ||
				plan.Functions[first].Factory != FactoryABI3 || NativeCallableProvider(first) == nil {
				t.Fatalf("managed spawn lost its creator or native first target: %+v", result)
			}
			plainAvailable := mode == "direct" || mode == "wrapped"
			if plainAvailable {
				if plan.Functions[second].Factory != FactoryABI1 || NativeCallableProvider(second) == nil {
					t.Fatal("bounded plain target lost its typed factory or callable companion")
				}
			} else if plan.Functions[second].Factory != NoFactory || NativeCallableProvider(second) != nil {
				t.Fatal("missing or unproved target acquired native capability")
			}

			calls := map[string]bool{}
			nilGo, hostReturn := false, false
			for _, generated := range typecheck.Target.Funcs {
				ir.VisitList(generated.Body, func(node ir.Node) {
					if node.Op() == ir.OGO {
						nilGo = true
						if NativeStackEntry(generated) {
							t.Error("nil GoStmt remained on a native executor")
						}
					}
					if branch, ok := node.(*ir.IfStmt); ok && NativeStackEntry(generated) {
						if condition, ok := branch.Cond.(*ir.BinaryExpr); ok && condition.Op() == ir.OEQ &&
							condition.Y.Op() == ir.OLITERAL && condition.Y.Type().IsInteger() && ir.Int64Val(condition.Y) == int64(actionHost) {
							for _, statement := range branch.Body {
								if ret, ok := statement.(*ir.ReturnStmt); ok && len(ret.Results) == 1 && ret.Results[0] == condition.X {
									hostReturn = true
								}
							}
						}
					}
					if call, ok := node.(*ir.CallExpr); ok {
						if name := ir.StaticCalleeName(call.Fun); name != nil {
							calls[name.Sym().Name] = true
							if name.Sym().Name == "coroLookupFunction" {
								boxed, ok := call.Args[0].(*ir.ConvExpr)
								if !ok || boxed.Op() != ir.OCONVIFACE ||
									!types.Identical(boxed.X.Type(), callableSignature(first)) {
									t.Error("open spawn lookup lost the frozen source function signature")
								}
							}
						}
					}
				})
			}
			required := []string{"first.coro", "coroLookupFunction", "coroCallableFactory", "coroSpawnFrame", "coroSpawnOpaque", "coroHostCall"}
			if plainAvailable {
				required = append(required, "second.coro")
			}
			for _, name := range required {
				if !calls[name] {
					t.Errorf("missing dynamic spawn entry %s", name)
				}
			}
			if !nilGo || !hostReturn {
				t.Fatalf("nil function startup lost ordinary go or host handoff: go=%t host=%t", nilGo, hostReturn)
			}
		})
	}
}
