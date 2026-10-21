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

func TestLowerDynamicDefer(t *testing.T) {
	prepareLowerTest(t)
	oldPkg, oldTarget := types.LocalPkg, typecheck.Target
	defer func() { types.LocalPkg, typecheck.Target = oldPkg, oldTarget }()
	for _, mode := range []string{"valid", "repeated", "direct", "direct-repeated", "direct-terminal", "nil-write", "imported", "imported-terminal", "unknown", "suspending", "exec", "terminal", "source-closure", "static-wrapper"} {
		t.Run(mode, func(t *testing.T) {
			f := newGoCallFixture(t)
			f.child.Nname.Defn = f.child
			f.child.Body = ir.Nodes{newLowerTestReturn()}
			value := f.caller.NewLocal(src.NoXPos, types.LocalPkg.Lookup("function"), f.child.Type())
			capture := ir.NewClosureVar(src.NoXPos, f.wrapper, value)
			f.wrapper.WrappedFunc = nil
			f.inner.Fun = capture
			deferred := ir.NewGoDeferStmt(src.NoXPos, ir.ODEFER, f.call)
			deferred.SetTypecheck(1)
			deferred.SetInit(ir.Nodes{
				ir.NewDecl(src.NoXPos, ir.ODCL, f.outer),
				ir.NewAssignStmt(src.NoXPos, f.outer, typedInt(src.NoXPos, types.Types[types.TINT], 42)),
				ir.NewDecl(src.NoXPos, ir.ODCL, value),
				ir.NewAssignStmt(src.NoXPos, value, f.child.Nname),
			})
			yield := ir.NewFunc(src.NoXPos, src.NoXPos, ir.Pkgs.Runtime.Lookup("Gosched"), types.NewSignature(nil, nil, nil))
			yield.DeclareParams(true)
			f.caller.Body = ir.Nodes{deferred, newLowerTestCall(yield), newLowerTestReturn()}
			funcs := []*ir.Func{f.caller, f.wrapper, f.child}
			switch mode {
			case "direct", "direct-repeated", "direct-terminal":
				child := newLowerTestFunc(types.LocalPkg, "zero")
				child.Nname.Defn = child
				child.Body = ir.Nodes{newLowerTestReturn()}
				if mode == "direct-terminal" {
					child.Body = ir.Nodes{ir.NewUnaryExpr(src.NoXPos, ir.OPANIC, ir.NewNilExpr(src.NoXPos, types.Types[types.TINTER]))}
				}
				value.SetType(child.Type())
				deferred.SetInit(ir.Nodes{ir.NewDecl(src.NoXPos, ir.ODCL, value), ir.NewAssignStmt(src.NoXPos, value, child.Nname)})
				f.call.Fun = value
				f.child = child
				funcs = []*ir.Func{f.caller, child}
				if mode == "direct-repeated" {
					f.caller.Body[0] = ir.NewForStmt(src.NoXPos, nil, ir.NewBool(src.NoXPos, false), nil, ir.Nodes{deferred}, false)
				}
			case "repeated":
				f.caller.Body[0] = ir.NewForStmt(src.NoXPos, nil, ir.NewBool(src.NoXPos, false), nil, ir.Nodes{deferred}, false)
			case "nil-write":
				deferred.PtrInit().Append(ir.NewAssignStmt(src.NoXPos, value, ir.NewNilExpr(src.NoXPos, value.Type())))
			case "imported", "imported-terminal", "unknown", "exec":
				funcs = funcs[:2]
				if mode != "unknown" {
					summary := FuncSummary{}
					if mode == "exec" {
						summary.Exec = NeedsTaskContext
					} else if mode == "imported-terminal" {
						summary.Terminal = MayPanic
					}
					SetSummary(f.child, summary)
					defer delete(summaries, f.child)
				}
			case "suspending":
				f.child.Body = append(ir.Nodes{newLowerTestCall(yield)}, f.child.Body...)
			case "terminal":
				panicCall := ir.NewUnaryExpr(src.NoXPos, ir.OPANIC, ir.NewNilExpr(src.NoXPos, types.Types[types.TINTER]))
				f.child.Body = ir.Nodes{panicCall}
			case "source-closure":
				f.wrapper.SetWrapper(false)
			case "static-wrapper":
				f.wrapper.WrappedFunc = f.child
				f.inner.Fun = f.child.Nname
			}
			plan, err := Analyze(funcs, nil)
			if err != nil {
				t.Fatal(err)
			}
			edge := edgeForCall(plan.Functions[f.caller], f.call)
			want := mode == "valid" || mode == "repeated" || mode == "direct" || mode == "direct-repeated" || mode == "direct-terminal" || mode == "nil-write" || mode == "imported" || mode == "static-wrapper" || mode == "terminal"
			if want && mode != "static-wrapper" && (edge.Kind != DeferCall || edge.Callee != nil || edge.Unknown || len(edge.Targets) != 1 || edge.Targets[0] != f.child) {
				t.Fatalf("lost closed defer proof: %+v", edge)
			}
			if err := plan.Verify(); err != nil {
				t.Fatal(err)
			}
			result, err := Lower(plan, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := plan.Functions[f.caller].Factory != NoFactory; got != want {
				t.Fatalf("native defer factory = %t, want %t: %+v", got, want, result)
			}
			if !want {
				wantReason := map[string]string{
					"unknown": "defer target has unknown effects", "suspending": "suspending defer",
					"exec":              "defer target has execution constraints task-context",
					"source-closure":    "suspending defer",
					"imported-terminal": "unsupported terminal control panic",
				}[mode]
				if !strings.Contains(strings.Join(result.Diagnostics, "\n"), wantReason) {
					t.Fatalf("missing fallback %q: %+v", wantReason, result)
				}
			}
		})
	}
}

func TestDynamicDeferWrapperBoundary(t *testing.T) {
	prepareLowerTest(t)
	oldPkg, oldTarget := types.LocalPkg, typecheck.Target
	defer func() { types.LocalPkg, typecheck.Target = oldPkg, oldTarget }()
	f := newGoCallFixture(t)
	if unwrapDynamicDeferCall(f.call) != f.call || unwrapDynamicDeferCall(f.inner) != f.inner {
		t.Fatal("static defer lost its existing call plan")
	}
	// A closed direct function-value edge still needs the frontend's operand
	// wrapper. Lowering must not evaluate an arbitrary callee during cleanup.
	f.child.Body = ir.Nodes{newLowerTestReturn()}
	f.call.Fun = f.caller.Nname
	yield := newLowerTestCall(f.child)
	f.caller.Body = ir.Nodes{ir.NewGoDeferStmt(src.NoXPos, ir.ODEFER, f.call), yield, newLowerTestReturn()}
	function := &Function{Func: f.caller, Edges: []Edge{{Kind: DeferCall, Node: f.call, Targets: []*ir.Func{f.child}}}, Sites: []Site{{ID: 1, Kind: SiteYield, Node: yield}}}
	plan := &Plan{Functions: map[*ir.Func]*Function{f.caller: function, f.child: {Func: f.child}}}
	if _, err := newLowerCandidate(plan, function); err == nil || !strings.Contains(err.Error(), "dynamic defer has no frozen callee") {
		t.Fatalf("unwrapped dynamic defer error = %v", err)
	}
}

func TestRewriteDynamicDeferBoundaries(t *testing.T) {
	prepareLowerTest(t)
	oldPkg, oldTarget := types.LocalPkg, typecheck.Target
	defer func() { types.LocalPkg, typecheck.Target = oldPkg, oldTarget }()
	for _, mode := range []string{"no-wrapper", "empty-body", "non-call", "call-init", "interface", "missing-factory", "valid"} {
		t.Run(mode, func(t *testing.T) {
			f := newGoCallFixture(t)
			deferred := &lowerDefer{call: f.call, statement: ir.NewGoDeferStmt(src.NoXPos, ir.ODEFER, f.call)}
			token := f.caller.NewLocal(src.NoXPos, types.LocalPkg.Lookup("token"), types.Types[types.TUNSAFEPTR])
			factories := make(map[*ir.Func]*ir.Func)
			want := "dynamic terminal defer wrapper has no frozen call"
			switch mode {
			case "no-wrapper":
				f.call.Fun = f.child.Nname
				want = "dynamic terminal defer has no operand wrapper"
			case "empty-body":
				f.wrapper.Body = nil
				want = "dynamic terminal defer has no operand wrapper"
			case "non-call":
				f.wrapper.Body = ir.Nodes{newLowerTestReturn()}
			case "call-init":
				f.inner.SetInit(ir.Nodes{newLowerTestReturn()})
			case "interface":
				f.inner.SetOp(ir.OCALLINTER)
			case "missing-factory":
				deferred.terminalTargets = []lowerDeferTerminal{{target: f.child, factory: true}}
				want = "dynamic terminal defer is missing factory"
			case "valid":
				sig := types.NewSignature(nil, f.child.Type().Params(),
					[]*types.Field{types.NewField(src.NoXPos, nil, types.Types[types.TINT])})
				f.child.Nname.SetType(sig)
				other := ir.NewFunc(src.NoXPos, src.NoXPos, types.LocalPkg.Lookup("other"), sig)
				other.Nname.Defn = other
				other.DeclareParams(true)
				deferred.terminalTargets = []lowerDeferTerminal{{target: f.child, factory: true}, {target: other}}
				factories[f.child] = newResumeFactory(f.child, FactoryABI1)
			}
			err := rewriteDynamicDeferCall(deferred, token, factories)
			if mode != "valid" {
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("rewrite error = %v, want %q", err, want)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			found := false
			ir.VisitList(f.wrapper.Body, func(node ir.Node) {
				if branch, ok := node.(*ir.IfStmt); ok {
					found = true
					if branch.Cond.Type() == nil || branch.Cond.Type().Kind() != types.TBOOL {
						t.Fatal("dispatch reached escape analysis with an untyped condition")
					}
				}
			})
			if !found {
				t.Fatal("missing terminal entry dispatch")
			}
		})
	}
}
