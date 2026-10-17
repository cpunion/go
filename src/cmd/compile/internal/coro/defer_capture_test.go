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

func TestRefreshNestedClosureCaptures(t *testing.T) {
	prepareLowerTest(t)
	oldPkg, oldTarget := types.LocalPkg, typecheck.Target
	defer func() { types.LocalPkg, typecheck.Target = oldPkg, oldTarget }()
	pkg := types.NewPkg("example.com/coro/nesteddeferowner", "nesteddeferowner")
	types.LocalPkg, typecheck.Target = pkg, new(ir.Package)
	owner := newLowerTestFunc(pkg, "owner")
	original := owner.NewLocal(src.NoXPos, pkg.Lookup("original"), types.Types[types.TINT])
	replacement := owner.NewLocal(src.NoXPos, pkg.Lookup("replacement"), original.Type())
	var functions []*ir.Func
	var captures []*ir.Name
	parent, outer := owner, original
	for range 4 {
		child := ir.NewClosureFunc(src.NoXPos, src.NoXPos, ir.OCLOSURE,
			types.NewSignature(nil, nil, nil), parent, typecheck.Target, 0)
		child.DeclareParams(true)
		capture := ir.NewClosureVar(src.NoXPos, child, outer)
		if parent != owner {
			parent.Body.Append(typecheck.Call(src.NoXPos, child.OClosure, nil, false))
		}
		functions, captures = append(functions, child), append(captures, capture)
		parent, outer = child, capture
	}
	captures[0].Outer, captures[0].Defn = replacement, replacement
	refreshNestedClosureCaptures(functions[0])
	for i, capture := range captures {
		if capture.Canonical() != replacement {
			t.Fatalf("depth%d retains the original canonical owner", i)
		}
	}
}
