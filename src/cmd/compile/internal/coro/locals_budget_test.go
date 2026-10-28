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

func TestInvocationLocalStorageBudget(t *testing.T) {
	prepareLowerTest(t)
	oldTarget, oldPkg, oldFunc := typecheck.Target, types.LocalPkg, ir.CurFunc
	defer func() { typecheck.Target, types.LocalPkg, ir.CurFunc = oldTarget, oldPkg, oldFunc }()
	for _, test := range []struct {
		name                                        string
		sizes                                       []int64
		prefix, address, inlined, structure, reject bool
	}{
		{name: "empty", sizes: []int64{0}},
		{name: "boundary", sizes: []int64{200}},
		{name: "alignment", sizes: []int64{199, 1}, reject: true},
		{name: "beyond", sizes: []int64{201}, reject: true},
		{name: "combined", sizes: []int64{104, 104}, reject: true},
		{name: "large", sizes: []int64{5120}, reject: true},
		{name: "structure", sizes: []int64{5120}, structure: true, reject: true},
		{name: "inlined", sizes: []int64{5120}, inlined: true, reject: true},
		{name: "persistent-prefix", sizes: []int64{5120}, prefix: true},
		{name: "persistent-address", sizes: []int64{5120}, address: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			pkg := types.NewPkg("example.com/coro/localbudget/"+test.name, "localbudget")
			types.LocalPkg, typecheck.Target = pkg, new(ir.Package)
			fn := newLowerTestFunc(pkg, "bounded")
			yield := newLowerTestCall(testFunc("yield"))
			var body ir.Nodes
			for _, size := range test.sizes {
				typ := types.NewArray(types.Types[types.TUINT8], size)
				if test.structure {
					typ = types.NewStruct([]*types.Field{types.NewField(src.NoXPos, pkg.Lookup("pad"), typ)})
				}
				local := typecheck.TempAt(src.NoXPos, fn, typ)
				local.SetAddrtaken(test.address)
				local.SetInlLocal(test.inlined)
				body.Append(ir.NewDecl(src.NoXPos, ir.ODCL, local))
				assign := ir.NewAssignStmt(src.NoXPos, local, ir.NewZero(src.NoXPos, typ))
				assign.SetTypecheck(1)
				body.Append(assign)
			}
			if test.inlined {
				inline := ir.NewInlinedCallExpr(src.NoXPos, body, nil)
				inline.SetTypecheck(1)
				body = ir.Nodes{inline}
			}
			fn.Body = append(ir.Nodes{yield}, body...)
			if test.prefix {
				fn.Body = append(body, yield)
			}
			fn.Body.Append(newLowerTestReturn())
			function := &Function{Func: fn, Local: MaySuspend, Effect: MaySuspend, Primary: CoroPrimary,
				Sites: []Site{{ID: 1, Kind: SiteYield, Node: yield}}}
			plan := &Plan{Functions: map[*ir.Func]*Function{fn: function}}
			_, err := newLowerCandidate(plan, function)
			if test.reject {
				if err == nil || !strings.Contains(err.Error(), "invocation locals exceed native stack budget") {
					t.Fatalf("newLowerCandidate error = %v, want invocation-local budget rejection", err)
				}
			} else if err != nil {
				t.Fatalf("newLowerCandidate: %v", err)
			}
		})
	}
}
