// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/ir"
	"cmd/compile/internal/types"
	"cmd/internal/src"
	"testing"
)

func TestClosedParameterProof(t *testing.T) {
	prepareLowerTest(t)
	for _, mode := range []string{"read", "write", "tuple-write", "range-write", "address", "capture", "unknown"} {
		t.Run(mode, func(t *testing.T) {
			pkg := types.NewPkg("example.com/coro/parameterproof/"+mode, "p")
			callback := types.NewSignature(nil, nil, nil)
			fn := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("invoke"),
				types.NewSignature(nil, []*types.Field{types.NewField(src.NoXPos, pkg.Lookup("f"), callback)}, nil))
			fn.DeclareParams(true)
			f := fn.Dcl[0]
			a := fn.NewLocal(src.NoXPos, pkg.Lookup("alias"), callback)
			first := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("first"), callback)
			second := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("second"), callback)
			fn.Body = ir.Nodes{ir.NewAssignStmt(src.NoXPos, a, f)}
			switch mode {
			case "write":
				fn.Body = append(fn.Body, ir.NewAssignStmt(src.NoXPos, f, second.Nname))
			case "tuple-write":
				fn.Body = append(fn.Body, ir.NewAssignListStmt(src.NoXPos, ir.OAS2, ir.Nodes{f}, ir.Nodes{second.Nname}))
			case "range-write":
				loop := ir.NewRangeStmt(src.NoXPos, f, nil, nil, nil, false)
				fn.Body = append(fn.Body, loop)
			case "address":
				fn.Body = append(fn.Body, ir.NewAddrExpr(src.NoXPos, f))
			case "capture":
				second.ClosureVars = []*ir.Name{ir.NewClosureVar(src.NoXPos, second, f)}
				fn.Body = append(fn.Body, &ir.ClosureExpr{Func: second})
			case "unknown":
				unknown := fn.NewLocal(src.NoXPos, pkg.Lookup("unknown"), callback)
				unknown.Class = ir.PPARAM
				fn.Body = append(fn.Body, ir.NewAssignStmt(src.NoXPos, a, unknown))
			}
			funcs := []*ir.Func{fn, first, second}
			if got := packageFunctionTargets(funcs)[fn]; len(got[f]) != 0 || len(got[a]) != 0 {
				t.Fatalf("open source parameter acquired proof: %v", got)
			}
			seeds := map[*ir.Func]map[*ir.Name][]*ir.Func{fn: {f: {first}}}
			got := packageFunctionTargetsWithParameters(funcs, seeds)[fn][a]
			want := 1
			if mode == "write" || mode == "tuple-write" {
				want = 2
			}
			if mode == "address" || mode == "capture" || mode == "unknown" || mode == "range-write" {
				want = 0
			}
			if len(got) != want {
				t.Fatalf("sealed alias targets=%v, want %d", got, want)
			}
			if readonly := readOnlyCallbackParameters(fn); readonly != (mode == "read" || mode == "unknown") {
				t.Fatalf("read-only callback parameter=%t", readonly)
			}
		})
	}
}

func TestClosedParameterTaskContext(t *testing.T) {
	prepareLowerTest(t)
	pkg := types.NewPkg("example.com/coro/plainparameter", "p")
	callback := types.NewSignature(nil, nil, nil)
	fn := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("sealed"),
		types.NewSignature(nil, []*types.Field{types.NewField(src.NoXPos, pkg.Lookup("f"), callback)}, nil))
	fn.DeclareParams(true)
	plain := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("plain"), callback)
	call := ir.NewCallExpr(src.NoXPos, ir.OCALLFUNC, fn.Dcl[0], nil)
	fn.Body = ir.Nodes{call, ir.NewReturnStmt(src.NoXPos, nil)}
	plain.Body = ir.Nodes{ir.NewReturnStmt(src.NoXPos, nil)}
	plan, err := analyze([]*ir.Func{fn, plain}, nil,
		map[*ir.Func]map[*ir.Name][]*ir.Func{fn: {fn.Dcl[0]: {plain}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.Verify(); err != nil {
		t.Fatal(err)
	}
	got := plan.Functions[fn]
	if got.Effect != NoSuspend || got.Exec&NeedsTaskContext == 0 || got.Primary != CoroPrimary ||
		len(got.Sites) != 1 || got.Sites[0].Kind != SiteAwait {
		t.Fatalf("plain sealed callback changed effects or lost context: %+v", got)
	}
}
