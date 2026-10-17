// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/ir"
	"cmd/compile/internal/typecheck"
	"cmd/compile/internal/types"
	"cmd/internal/src"
	"slices"
	"testing"
)

func TestRewriteDeferCapture(t *testing.T) {
	prepareLowerTest(t)
	oldPkg, oldTarget := types.LocalPkg, typecheck.Target
	defer func() { types.LocalPkg, typecheck.Target = oldPkg, oldTarget }()
	pkg := types.NewPkg("example.com/coro/nesteddefercapture", "nesteddefercapture")
	types.LocalPkg, typecheck.Target = pkg, new(ir.Package)
	owner := newLowerTestFunc(pkg, "owner")
	value := owner.NewLocal(src.NoXPos, pkg.Lookup("value"), types.Types[types.TINT])
	snapshot := owner.NewLocal(src.NoXPos, pkg.Lookup("snapshot"), types.NewPtr(value.Type()))
	var functions []*ir.Func
	var captures []*ir.Name
	parent, outer := owner, value
	for range 4 {
		child := ir.NewClosureFunc(src.NoXPos, src.NoXPos, ir.OCLOSURE,
			types.NewSignature(nil, nil, nil), parent, typecheck.Target, 0)
		child.DeclareParams(true)
		capture := ir.NewClosureVar(src.NoXPos, child, outer)
		child.Body = ir.Nodes{ir.NewAssignStmt(src.NoXPos, capture, typedInt(src.NoXPos, value.Type(), 42))}
		if parent != owner {
			parent.Body.Append(typecheck.Call(src.NoXPos, child.OClosure, nil, false))
		}
		functions, captures = append(functions, child), append(captures, capture)
		parent, outer = child, capture
	}
	pointer := ir.NewClosureVar(src.NoXPos, functions[0], snapshot)
	rewriteDeferCapture(functions[0], captures[0], pointer)
	previous := snapshot
	for i, fn := range functions {
		if slices.Contains(fn.ClosureVars, captures[i]) || len(fn.ClosureVars) != 1 {
			t.Fatalf("depth%d captures = %v, want only the snapshot pointer", i, fn.ClosureVars)
		}
		capture := fn.ClosureVars[0]
		if capture.Outer != previous || capture.Canonical() != snapshot || capture.Type() != snapshot.Type() {
			t.Fatalf("depth%d pointer capture has stale ownership", i)
		}
		assign := fn.Body[0].(*ir.AssignStmt)
		star, ok := assign.X.(*ir.StarExpr)
		if !ok || star.X != capture || star.Type() != value.Type() {
			t.Fatalf("depth%d assignment does not dereference its own pointer", i)
		}
		previous = capture
	}
}
