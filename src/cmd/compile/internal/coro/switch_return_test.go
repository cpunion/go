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

func TestSwitchReturnBoundary(t *testing.T) {
	prepareLowerTest(t)
	oldTarget := typecheck.Target
	t.Cleanup(func() { typecheck.Target = oldTarget })
	for _, test := range []struct {
		name                     string
		foreign, yield, escaping bool
		closure, structured      bool
		localBreak               bool
		localGoto                bool
		inlined                  bool
		wantError                bool
	}{
		{name: "yield-return", yield: true, escaping: true, wantError: true},
		{name: "foreign-return", foreign: true, escaping: true, wantError: true},
		{name: "foreign-if-return", foreign: true, escaping: true, structured: true},
		{name: "foreign-yield-return", foreign: true, yield: true, escaping: true, wantError: true},
		{name: "yield-assignment", yield: true},
		{name: "yield-closure", yield: true, closure: true},
		{name: "yield-local-break", yield: true, localBreak: true},
		{name: "foreign-local-break", foreign: true, localBreak: true},
		{name: "yield-local-goto", yield: true, localGoto: true},
		{name: "yield-inlined-goto", yield: true, localGoto: true, inlined: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			typecheck.Target = new(ir.Package)
			pkg := types.NewPkg("example.com/coro/switchreturn/"+test.name, "switchreturn")
			sig := types.NewSignature(nil, nil, nil)
			fn := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("f"), sig)
			function := &Function{Func: fn, Primary: CoroPrimary}
			if test.foreign {
				callee := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("foreign"), sig)
				call := newLowerTestCall(callee)
				fn.Body = append(fn.Body, call)
				function.Sites = append(function.Sites, Site{
					ID: 1, Kind: SiteForeign, Node: call, Foreign: DirectMayBlock,
				})
			}
			if test.yield {
				callee := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("yield"), sig)
				call := newLowerTestCall(callee)
				fn.Body = append(fn.Body, call)
				function.Effect = MaySuspend
				function.Sites = append(function.Sites, Site{ID: 2, Kind: SiteYield, Node: call})
			}
			var body ir.Nodes
			if test.escaping {
				body = append(body, newLowerTestReturn())
			} else {
				value := ir.Node(ir.NewInt(src.NoXPos, 1))
				if test.closure {
					closure := ir.NewClosureFunc(src.NoXPos, src.NoXPos,
						ir.OCLOSURE, sig, fn, typecheck.Target, 0)
					closure.Body = ir.Nodes{newLowerTestReturn()}
					value = closure.OClosure
				}
				body = append(body, ir.NewAssignStmt(src.NoXPos, ir.BlankNode, value))
			}
			if test.localBreak {
				body = append(body, ir.NewBranchStmt(src.NoXPos, ir.OBREAK, nil))
			}
			if test.localGoto {
				label := pkg.Lookup("inlinedReturn")
				body = append(body, ir.NewBranchStmt(src.NoXPos, ir.OGOTO, label),
					ir.NewLabelStmt(src.NoXPos, label))
			}
			clause := ir.NewCaseStmt(src.NoXPos, nil, body)
			var stmt ir.Node = ir.NewSwitchStmt(src.NoXPos, nil, []*ir.CaseClause{clause})
			if test.inlined {
				stmt = ir.NewInlinedCallExpr(src.NoXPos, body, nil)
			}
			if test.structured {
				stmt = ir.NewIfStmt(src.NoXPos, ir.NewBool(src.NoXPos, true), body, nil)
			}
			fn.Body = append(fn.Body, stmt, newLowerTestReturn())
			candidate, err := newLowerCandidate(&Plan{}, function)
			if test.wantError {
				if err == nil || !strings.Contains(err.Error(), "return in unsupported control flow switch") {
					t.Fatalf("candidate error = %v, want switch return boundary", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if test.foreign && !canLowerRunToCompletion(candidate) {
				t.Fatal("foreign structured return lost its run-to-completion entry")
			}
		})
	}
}
