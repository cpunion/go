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

func TestRewriteDeferBuiltin(t *testing.T) {
	prepareLowerTest(t)
	oldPkg, oldTarget := types.LocalPkg, typecheck.Target
	defer func() { types.LocalPkg, typecheck.Target = oldPkg, oldTarget }()
	for _, op := range []ir.Op{ir.OPRINT, ir.OPRINTLN} {
		t.Run(op.String(), func(t *testing.T) {
			pkg := types.NewPkg("example.com/coro/deferbuiltin/"+t.Name(), "deferbuiltin")
			types.LocalPkg = pkg
			typecheck.Target = new(ir.Package)
			fn := newLowerTestFunc(pkg, "deferred")
			token := fn.NewLocal(src.NoXPos, pkg.Lookup("token"), types.Types[types.TUNSAFEPTR])
			recoverCall := ir.NewCallExpr(src.NoXPos, ir.ORECOVER, nil, nil)
			recoverCall.SetType(types.Types[types.TINTER])
			recoverCall.SetTypecheck(1)
			builtin := ir.NewCallExpr(src.NoXPos, op, nil, ir.Nodes{recoverCall})
			builtin.SetTypecheck(1)
			fn.Body = ir.Nodes{builtin}
			if err := rewriteDeferTerminal(fn, token, UsesRecover); err != nil {
				t.Fatal(err)
			}
			call, ok := builtin.Args[0].(*ir.CallExpr)
			if fn.Body[0] != builtin || builtin.Op() != op || !ok || call.Op() != ir.OCALLFUNC ||
				symbolName(ir.StaticCalleeName(call.Fun)) != "runtime.coroDeferRecover" {
				t.Fatal("builtin changed or its recover argument was not rewritten")
			}
		})
	}
}
