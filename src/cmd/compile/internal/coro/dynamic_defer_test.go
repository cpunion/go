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
	for _, mode := range []string{"valid", "repeated", "direct", "direct-repeated", "nil-write", "imported", "unknown", "suspending", "exec", "terminal", "source-closure", "static-wrapper"} {
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
			case "direct", "direct-repeated":
				child := newLowerTestFunc(types.LocalPkg, "zero")
				child.Nname.Defn = child
				child.Body = ir.Nodes{newLowerTestReturn()}
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
			case "imported", "unknown", "exec":
				funcs = funcs[:2]
				if mode != "unknown" {
					summary := FuncSummary{}
					if mode == "exec" {
						summary.Exec = NeedsTaskContext
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
			want := mode == "valid" || mode == "repeated" || mode == "direct" || mode == "direct-repeated" || mode == "nil-write" || mode == "imported" || mode == "static-wrapper"
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
					"exec": "defer target has execution constraints task-context", "terminal": "dynamic defer target has terminal control panic",
					"source-closure": "suspending defer",
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
