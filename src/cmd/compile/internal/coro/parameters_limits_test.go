// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/ir"
	"cmd/compile/internal/typecheck"
	"cmd/compile/internal/types"
	"cmd/internal/src"
	"fmt"
	"testing"
)

func TestClosedParameterLimits(t *testing.T) {
	prepareLowerTest(t)
	if got := specializeParameters(nil, nil, nil, nil, nil); len(got) != 0 {
		t.Fatal("specialization without an independent source reader")
	}
	for _, mode := range []string{"shared", "helpers", "calls", "targets", "body", "closure-body", "missing", "nil-clone", "invalid-clone"} {
		t.Run(mode, func(t *testing.T) {
			oldPkg, oldTarget := types.LocalPkg, typecheck.Target
			defer func() { types.LocalPkg, typecheck.Target = oldPkg, oldTarget }()
			pkg := types.NewPkg("example.com/coro/parameterlimits/"+mode, "p")
			types.LocalPkg, typecheck.Target = pkg, new(ir.Package)
			callback := types.NewSignature(nil, nil, nil)
			makeSource := func(sym *types.Sym) *ir.Func {
				fn := ir.NewFunc(src.NoXPos, src.NoXPos, sym,
					types.NewSignature(nil, []*types.Field{types.NewField(src.NoXPos, pkg.Lookup("f"), callback)}, nil))
				fn.Nname.Defn = fn
				fn.DeclareParams(true)
				fn.Body = ir.Nodes{ir.NewCallExpr(src.NoXPos, ir.OCALLFUNC, fn.Dcl[0], nil)}
				return fn
			}
			source := makeSource(pkg.Lookup("invoke"))
			ncalls, ntargets, wantCopies, wantRewrites := 70, 1, 1, 70
			switch mode {
			case "helpers":
				ntargets, wantCopies, wantRewrites = 70, 64, 64
			case "calls":
				ncalls, wantRewrites = 260, 256
			case "targets":
				ncalls, ntargets, wantCopies, wantRewrites = 1, 17, 0, 0
			case "body", "closure-body", "missing", "nil-clone", "invalid-clone":
				ncalls, wantCopies, wantRewrites = 1, 0, 0
			}
			if mode == "body" || mode == "closure-body" {
				owner := source
				if mode == "closure-body" {
					owner = ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("largeClosure"), callback)
					closure := &ir.ClosureExpr{Func: owner}
					owner.OClosure = closure
					source.Body = append(source.Body, closure)
				}
				for range 1100 {
					owner.Body = append(owner.Body, ir.NewReturnStmt(src.NoXPos, nil))
				}
			}
			funcs := []*ir.Func{source}
			var targets []*ir.Func
			for i := range ntargets {
				fn := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup(fmt.Sprintf("target%d", i)), callback)
				fn.Body = ir.Nodes{ir.NewReturnStmt(src.NoXPos, nil)}
				targets = append(targets, fn)
				funcs = append(funcs, fn)
			}
			have := map[*ir.Func]bool{source: mode != "missing"}
			var calls []*ir.CallExpr
			for i := range ncalls {
				caller := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup(fmt.Sprintf("caller%d", i)), callback)
				caller.DeclareParams(true)
				var arg ir.Node = targets[i%len(targets)].Nname
				if mode == "targets" {
					arg = caller.NewLocal(src.NoXPos, pkg.Lookup("choice"), callback)
					for _, target := range targets {
						caller.Body = append(caller.Body, ir.NewAssignStmt(src.NoXPos, arg, target.Nname))
					}
				}
				call := ir.NewCallExpr(src.NoXPos, ir.OCALLFUNC, source.Nname, ir.Nodes{arg})
				caller.Body = append(caller.Body, call)
				calls = append(calls, call)
				funcs = append(funcs, caller)
				have[caller] = true
			}
			typecheck.Target.Funcs = funcs
			reader := &BodyReader{Have: func(fn *ir.Func) bool { return have[fn] }, Clone: func(fn *ir.Func, sym *types.Sym) *ir.Func {
				if mode == "nil-clone" {
					return nil
				}
				copy := makeSource(sym)
				if mode == "invalid-clone" {
					copy.Body = append(copy.Body, ir.NewAssignStmt(src.NoXPos, copy.Dcl[0], targets[0].Nname))
				}
				return copy
			}}
			parameters := make(map[*ir.Func]map[*ir.Name][]*ir.Func)
			rewritten := specializeParameters(funcs, reader, parameters, make(map[*ir.Func]func() *ir.Func), make(map[*ir.Func]bool))
			if len(parameters) != wantCopies || len(rewritten) != wantRewrites {
				t.Fatalf("copies=%d rewritten=%d, want %d/%d", len(parameters), len(rewritten), wantCopies, wantRewrites)
			}
			if parameters[source] != nil {
				t.Fatal("original source parameter acquired a sealed proof")
			}
			if mode == "invalid-clone" && len(typecheck.Target.Funcs) != len(funcs)+1 {
				t.Fatal("rejected private copy lost its compiled owner")
			}
			changed := 0
			for _, call := range calls {
				if call.Fun != source.Nname {
					changed++
				}
			}
			if changed != wantRewrites {
				t.Fatalf("changed calls=%d want %d", changed, wantRewrites)
			}
		})
	}
}
