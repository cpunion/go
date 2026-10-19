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

func TestExpressionSwitchBoundaries(t *testing.T) {
	prepareLowerTest(t)
	pkg := types.NewPkg("example.com/coro/switchboundary", "switchboundary")
	for _, test := range []struct{ name, want string }{
		{"label", "labeled switch"},
		{"tag", "coroutine site in switch header"},
		{"case", "coroutine site in switch header"},
		{"case-init", "coroutine site in switch header"},
		{"fallthrough", "control flow fallthrough"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fn := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup(test.name), types.NewSignature(nil, nil, nil))
			callee := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("callee"),
				types.NewSignature(nil, nil, []*types.Field{types.NewField(src.NoXPos, nil, types.Types[types.TINT])}))
			call := newLowerTestCall(callee)
			call.SetType(types.Types[types.TINT])
			clause := ir.NewCaseStmt(src.NoXPos, nil, ir.Nodes{newLowerTestReturn()})
			stmt := ir.NewSwitchStmt(src.NoXPos, nil, []*ir.CaseClause{clause})
			switch test.name {
			case "label":
				stmt.Label = pkg.Lookup("label")
			case "tag":
				stmt.Tag = call
			case "case":
				clause.List = ir.Nodes{call}
			case "case-init":
				clause.SetInit(ir.Nodes{call})
			case "fallthrough":
				clause.Body = append(clause.Body, ir.NewBranchStmt(src.NoXPos, ir.OFALL, nil))
			}
			fn.Body = ir.Nodes{stmt}
			_, err := newLowerCandidate(&Plan{}, &Function{Func: fn, Primary: CoroPrimary,
				Sites: []Site{{ID: 1, Kind: SiteAwait, Node: call}}})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestLowerExpressionSwitch(t *testing.T) {
	prepareLowerTest(t)
	oldPkg, oldTarget := types.LocalPkg, typecheck.Target
	defer func() { types.LocalPkg, typecheck.Target = oldPkg, oldTarget }()
	pkg := types.NewPkg("example.com/coro/switchlower", "switchlower")
	types.LocalPkg, typecheck.Target = pkg, new(ir.Package)
	input := types.NewField(src.NoXPos, pkg.Lookup("tag"), types.Types[types.TINT])
	result := types.NewField(src.NoXPos, nil, types.Types[types.TUINT64])
	fn := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("f"), types.NewSignature(nil, []*types.Field{input}, []*types.Field{result}))
	fn.Nname.Defn = fn
	fn.DeclareParams(true)
	yield := ir.NewFunc(src.NoXPos, src.NoXPos, types.NewPkg("runtime", "runtime").Lookup("Gosched"), types.NewSignature(nil, nil, nil))
	ret := func(value int64) *ir.ReturnStmt {
		stmt := ir.NewReturnStmt(src.NoXPos, ir.Nodes{typedInt(src.NoXPos, result.Type, value)})
		stmt.SetTypecheck(1)
		return stmt
	}
	clause := ir.NewCaseStmt(src.NoXPos, ir.Nodes{typedInt(src.NoXPos, input.Type, 0)}, ir.Nodes{newLowerTestCall(yield), ret(1 << 40)})
	stmt := ir.NewSwitchStmt(src.NoXPos, fn.Dcl[0], []*ir.CaseClause{clause, ir.NewCaseStmt(src.NoXPos, nil, ir.Nodes{ret(7)})})
	stmt.SetTypecheck(1)
	fn.Body = ir.Nodes{stmt}
	plan, err := Analyze([]*ir.Func{fn}, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Lower(plan, nil)
	if err != nil || got.Lowered != 1 || got.Skipped != 0 {
		t.Fatalf("Lower = (%+v, %v)", got, err)
	}
	if len(clause.Body) != 2 || clause.Body[0].Op() != ir.OCALLFUNC || clause.Body[1].Op() != ir.ORETURN {
		t.Fatal("dispatch rewrite changed the original case body")
	}
	found := false
	for _, generated := range typecheck.Target.Funcs {
		ir.VisitList(generated.Body, func(node ir.Node) {
			dispatch, ok := node.(*ir.SwitchStmt)
			if !ok || dispatch.Tag == nil || dispatch.Tag.Type() != input.Type {
				return
			}
			found = true
			for _, clause := range dispatch.Cases {
				if len(clause.Body) != 1 || clause.Body[0].Op() != ir.OGOTO {
					t.Fatal("source switch dispatch did not select a continuation")
				}
			}
		})
	}
	if !found {
		t.Fatal("source switch dispatch missing from generated resume")
	}
}
