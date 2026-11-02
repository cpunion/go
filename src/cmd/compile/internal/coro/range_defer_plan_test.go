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

func TestRangeDeferOwnerPlans(t *testing.T) {
	prepareLowerTest(t)
	oldTarget := typecheck.Target
	typecheck.Target = new(ir.Package)
	t.Cleanup(func() { typecheck.Target = oldTarget })
	for _, mode := range []string{"valid", "ordinary-body", "separate-markers", "wrong-owner", "missing-range-metadata", "wrong-anchor", "wrong-token-type", "unknown-target", "suspending-target", "malformed-marker", "marker-without-assignment", "duplicate-marker", "non-runtime-lookalike", "non-normalized-defer"} {
		t.Run(mode, func(t *testing.T) {
			pkg := types.NewPkg("example.com/coro/rangeowner/"+mode, "p")
			owner := newLowerTestFunc(pkg, "owner")
			leaf := newLowerTestFunc(pkg, "cleanup")
			leaf.Body = ir.Nodes{newLowerTestReturn()}
			helper := ir.NewFunc(src.NoXPos, src.NoXPos, ir.Pkgs.Runtime.Lookup("deferrangefunc"),
				types.NewSignature(nil, nil, []*types.Field{types.NewField(src.NoXPos, nil, types.Types[types.TINTER])}))
			anchor := typecheck.TempAt(src.NoXPos, owner, types.Types[types.TINTER])
			marker := newLowerTestCall(helper)
			marker.SetType(types.Types[types.TINTER])
			assign := ir.NewAssignStmt(src.NoXPos, anchor, marker)
			assign.Def = true
			anchor.Defn = assign
			owner.Body = ir.Nodes{ir.NewDecl(src.NoXPos, ir.ODCL, anchor), assign}
			body := ir.NewClosureFunc(src.NoXPos, src.NoXPos, ir.ORANGE,
				types.NewSignature(nil, nil, []*types.Field{types.NewField(src.NoXPos, nil, types.Types[types.TBOOL])}),
				owner, typecheck.Target, 0)
			body.DeclareParams(true)
			captured := ir.NewClosureVar(src.NoXPos, body, anchor)
			call := newLowerTestCall(leaf)
			statement := ir.NewGoDeferStmt(src.NoXPos, ir.ODEFER, call)
			statement.DeferAt = captured
			body.Body = ir.Nodes{statement, ir.NewReturnStmt(src.NoXPos, ir.Nodes{ir.NewBool(src.NoXPos, true)})}
			funcs := []*ir.Func{owner, body, leaf}
			want := mode == "valid" || mode == "ordinary-body" || mode == "separate-markers"
			switch mode {
			case "wrong-owner":
				body.RangeParent = newLowerTestFunc(pkg, "unrelated")
			case "missing-range-metadata":
				body.RangeParent = nil
			case "wrong-anchor":
				other := typecheck.TempAt(src.NoXPos, owner, types.Types[types.TINTER])
				statement.DeferAt = ir.NewClosureVar(src.NoXPos, body, other)
			case "wrong-token-type":
				captured.SetType(types.Types[types.TINT])
			case "unknown-target":
				call.Fun = typecheck.TempAt(src.NoXPos, body, leaf.Type())
			case "suspending-target":
				runtimePkg := types.NewPkg("runtime", "runtime")
				yield := ir.NewFunc(src.NoXPos, src.NoXPos, runtimePkg.Lookup("Gosched"), types.NewSignature(nil, nil, nil))
				leaf.Body = ir.Nodes{newLowerTestCall(yield), newLowerTestReturn()}
			case "malformed-marker":
				marker.Args = ir.Nodes{ir.NewInt(src.NoXPos, 1)}
			case "marker-without-assignment":
				owner.Body = ir.Nodes{marker}
			case "duplicate-marker":
				owner.Body = append(owner.Body, ir.NewAssignStmt(src.NoXPos, anchor, marker))
			case "non-runtime-lookalike":
				spoof := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("deferrangefunc"), helper.Type())
				marker.Fun = spoof.Nname
			case "non-normalized-defer":
				call.Args = ir.Nodes{ir.NewInt(src.NoXPos, 1)}
			case "separate-markers":
				other := typecheck.TempAt(src.NoXPos, owner, types.Types[types.TINTER])
				create := newLowerTestCall(helper)
				create.SetType(types.Types[types.TINTER])
				owner.Body = append(owner.Body, ir.NewAssignStmt(src.NoXPos, other, create))
				second := ir.NewClosureFunc(src.NoXPos, src.NoXPos, ir.ORANGE, types.NewSignature(nil, nil, []*types.Field{types.NewField(src.NoXPos, nil, types.Types[types.TBOOL])}), owner, typecheck.Target, 0)
				second.DeclareParams(true)
				deferred := ir.NewGoDeferStmt(src.NoXPos, ir.ODEFER, newLowerTestCall(leaf))
				deferred.DeferAt = ir.NewClosureVar(src.NoXPos, second, other)
				second.Body = ir.Nodes{deferred, ir.NewReturnStmt(src.NoXPos, ir.Nodes{ir.NewBool(src.NoXPos, true)})}
				funcs = append(funcs, second)
			}
			plan, err := Analyze(funcs, nil)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "ordinary-body" {
				// Registration can run on an unrelated ordinary Go stack. Its
				// body need not satisfy native execution constraints when the
				// enclosing owner only needs the separately proved cleanup.
				plan.Functions[body].Effect = MaySuspend
				plan.Functions[body].Exec = NeedsGoStack | ThreadAffine
			}
			functions := make([]*Function, len(funcs))
			for i, fn := range funcs {
				functions[i] = plan.Functions[fn]
			}
			owners := planRangeDeferOwners(plan, functions)
			got := owners[owner]
			if mode == "non-runtime-lookalike" {
				if got != nil {
					t.Fatal("source spelling acquired native owner metadata")
				}
				return
			}
			if got == nil || (got.err == nil) != want {
				t.Fatalf("owner proof = %+v, want valid=%t", got, want)
			}
			if !want {
				return
			}
			count := 1
			if mode == "separate-markers" {
				count = 2
			}
			if len(got.anchors) != count || len(got.bodies) != count || got.anchors[anchor] != assign {
				t.Fatal("separate marker positions or registration bodies were lost")
			}
			for _, registration := range got.bodies {
				if registration.owner != got || len(registration.cleanup.defers) != 1 ||
					!registration.cleanup.dynamicDefers || registration.cleanup.function != registration.function {
					t.Fatal("cleanup targets lost their exact enclosing owner plan")
				}
			}
		})
	}
}
