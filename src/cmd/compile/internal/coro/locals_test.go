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
	"strings"
	"testing"
)

func TestInvocationLocals(t *testing.T) {
	prepareLowerTest(t)
	for _, mode := range []string{
		"suffix", "prefix", "outside-use", "inline", "inline-jump", "argument-site",
		"inline-prefix", "inline-outside-use", "inline-captured", "inline-address",
		"block", "branch", "loop", "jump", "captured", "address", "other-function", "parameter",
	} {
		t.Run(mode, func(t *testing.T) {
			fn := testFunc("invocationLocals")
			fn.DeclareParams(true)
			local := typecheck.TempAt(src.NoXPos, fn, types.Types[types.TINT])
			decl := ir.NewDecl(src.NoXPos, ir.ODCL, local)
			call := ir.NewCallExpr(src.NoXPos, ir.OCALLFUNC, testFunc("awaited").Nname, nil)
			function := &Function{Func: fn, Sites: []Site{{Node: call, Kind: SiteAwait}}}
			body := ir.Nodes{decl, local}
			fn.Body = append(ir.Nodes{call}, body...)
			want := true
			switch mode {
			case "prefix":
				fn.Body = append(body, call)
				want = false
			case "outside-use":
				fn.Body = append(ir.Nodes{local}, fn.Body...)
				want = false
			case "inline", "inline-jump", "argument-site", "inline-prefix", "inline-outside-use", "inline-captured", "inline-address":
				local.SetInlLocal(true)
				inlined := ir.NewInlinedCallExpr(src.NoXPos, body, ir.Nodes{local})
				fn.Body = ir.Nodes{call, inlined}
				if strings.HasPrefix(mode, "inline-") && mode != "inline-jump" {
					fn.Body = ir.Nodes{inlined, call}
				}
				if mode == "inline-outside-use" {
					fn.Body.Append(local)
					want = false
				}
				if mode == "inline-address" {
					local.SetAddrtaken(true)
					want = false
				}
				if mode == "inline-captured" {
					closure := ir.NewClosureFunc(src.NoXPos, src.NoXPos, ir.OCLOSURE,
						types.NewSignature(nil, nil, nil), fn, new(ir.Package), 0)
					ir.NewClosureVar(src.NoXPos, closure, local)
					fn.Body.Prepend(closure.OClosure)
					want = false
				}
				if mode == "inline-jump" {
					inlined.Body.Append(ir.NewBranchStmt(src.NoXPos, ir.OGOTO, nil))
					inlined.Body.Append(ir.NewLabelStmt(src.NoXPos, nil))
				}
				if mode == "argument-site" {
					inlined.PtrInit().Append(call)
					fn.Body = ir.Nodes{inlined}
					want = false
				}
			case "branch":
				fn.Body = ir.Nodes{ir.NewIfStmt(src.NoXPos, ir.NewBool(src.NoXPos, true), fn.Body, nil)}
			case "block":
				fn.Body = ir.Nodes{ir.NewBlockStmt(src.NoXPos, fn.Body)}
			case "loop":
				fn.Body = ir.Nodes{ir.NewForStmt(src.NoXPos, nil, nil, nil, fn.Body, false)}
				want = false
			case "jump":
				fn.Body.Append(ir.NewBranchStmt(src.NoXPos, ir.OGOTO, nil))
				want = false
			case "captured":
				closure := ir.NewClosureFunc(src.NoXPos, src.NoXPos, ir.OCLOSURE,
					types.NewSignature(nil, nil, nil), fn, new(ir.Package), 0)
				ir.NewClosureVar(src.NoXPos, closure, local)
				fn.Body = append(ir.Nodes{closure.OClosure}, fn.Body...)
				want = false
			case "other-function":
				local.Curfn = testFunc("other")
				want = false
			case "address":
				local.SetAddrtaken(true)
				want = false
			case "parameter":
				local.Class = ir.PPARAM
				want = false
			}
			got := invocationLocals(function)
			if got[local] != want {
				t.Fatalf("local selected = %v, want %v", got[local], want)
			}
		})
	}
}

func TestLowerInvocationLocals(t *testing.T) {
	prepareLowerTest(t)
	oldTarget, oldLocalPkg, oldCurFunc := typecheck.Target, types.LocalPkg, ir.CurFunc
	defer func() {
		typecheck.Target, types.LocalPkg, ir.CurFunc = oldTarget, oldLocalPkg, oldCurFunc
	}()
	for _, mode := range []string{"statement", "initializer", "inline"} {
		t.Run(mode, func(t *testing.T) {
			pkg := types.NewPkg("example.com/coro/invocation/"+mode, "invocation")
			types.LocalPkg = pkg
			typecheck.Target = new(ir.Package)
			fn := newLowerTestFunc(pkg, "suffix")
			local := fn.NewLocal(src.NoXPos, pkg.Lookup("local"), types.Types[types.TINT])
			decl := ir.NewDecl(src.NoXPos, ir.ODCL, local)
			assign := ir.NewAssignStmt(src.NoXPos, local, typedInt(src.NoXPos, local.Type(), 7))
			assign.SetTypecheck(1)
			assign.Def, local.Defn = true, assign
			body := ir.Nodes{decl, assign}
			if mode == "initializer" {
				assign.PtrInit().Append(decl)
				body = ir.Nodes{assign}
			} else if mode == "inline" {
				local.SetInlLocal(true)
				inlined := ir.NewInlinedCallExpr(src.NoXPos, body, nil)
				inlined.SetTypecheck(1)
				body = ir.Nodes{inlined}
			}
			yield := newLowerTestCall(testFunc("yield"))
			fn.Body = append(ir.Nodes{yield}, body...)
			fn.Body.Append(newLowerTestReturn())
			function := &Function{
				Func: fn, Local: MaySuspend, Effect: MaySuspend, Primary: CoroPrimary,
				Sites: []Site{{ID: 1, Kind: SiteYield, Node: yield}},
			}
			result, err := Lower(&Plan{Functions: map[*ir.Func]*Function{fn: function}}, nil)
			if err != nil || result.Lowered != 1 {
				t.Fatalf("Lower = %+v, %v", result, err)
			}
			resume := local.Curfn
			if resume == fn || !strings.Contains(ir.PkgFuncName(resume), ".coro") ||
				!slices.Contains(resume.Dcl, local) {
				t.Fatalf("local belongs to %v, want generated resume", resume)
			}
			if len(resume.ClosureVars) != 0 {
				t.Fatal("explicit resume unexpectedly captures variables")
			}
			initialized := false
			ir.VisitList(resume.Body, func(node ir.Node) {
				if assign, ok := node.(*ir.AssignStmt); ok && assign.X == local &&
					ir.IsZero(assign.Y) {
					initialized = true
				}
			})
			if !initialized {
				t.Fatal("resume local has no entry initialization")
			}
		})
	}
}
