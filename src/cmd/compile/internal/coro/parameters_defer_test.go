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

func TestClosedParameterDeferContract(t *testing.T) {
	prepareLowerTest(t)
	for _, mode := range []string{"plain", "panic", "recover", "exit", "missing", "unsealed", "writable", "suspending", "exec", "local-exec", "callback-suspend", "callback-exec", "callback-recover", "unknown", "spawn", "defer", "defer-recover", "defer-suspend", "defer-exec", "defer-unknown", "operation"} {
		t.Run(mode, func(t *testing.T) {
			pkg := types.NewPkg("example.com/coro/parameterdefer/"+mode, "p")
			callback := types.NewSignature(nil, nil, nil)
			fn := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("sealed"),
				types.NewSignature(nil, []*types.Field{types.NewField(src.NoXPos, pkg.Lookup("f"), callback)}, nil))
			fn.DeclareParams(true)
			leaf := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("leaf"), callback)
			call := ir.NewCallExpr(src.NoXPos, ir.OCALLFUNC, fn.Dcl[0], nil)
			fn.Body = ir.Nodes{call, ir.NewReturnStmt(src.NoXPos, nil)}
			leaf.Body = ir.Nodes{ir.NewReturnStmt(src.NoXPos, nil)}
			plan, err := analyze([]*ir.Func{fn, leaf}, nil,
				map[*ir.Func]map[*ir.Name][]*ir.Func{fn: {fn.Dcl[0]: {leaf}}})
			if err != nil {
				t.Fatal(err)
			}
			function := plan.Functions[fn]
			edge := &function.Edges[0]
			want := mode == "plain" || mode == "panic" || mode == "recover" || mode == "exit" || mode == "defer" || mode == "defer-recover"
			switch mode {
			case "panic":
				function.Terminal = MayPanic
			case "recover":
				function.Terminal = UsesRecover
			case "exit":
				function.Terminal = MayGoexit
			case "missing":
				fn = nil
			case "unsealed":
				edge.FactoryRequired = false
			case "writable":
				fn.Body = append(fn.Body, ir.NewAssignStmt(src.NoXPos, fn.Dcl[0], leaf.Nname))
			case "suspending":
				function.Effect = MaySuspend
			case "exec":
				function.Exec |= ThreadAffine
			case "local-exec":
				function.LocalExec |= NeedsPreempt
			case "callback-suspend":
				plan.Functions[leaf].Effect = MaySuspend
			case "callback-exec":
				plan.Functions[leaf].Exec = NeedsTaskContext
			case "callback-recover":
				plan.Functions[leaf].Terminal = UsesRecover
			case "unknown":
				edge.Targets, edge.Unknown = nil, true
			case "spawn":
				edge.Kind = GoCall
			case "defer", "defer-recover", "defer-suspend", "defer-exec", "defer-unknown":
				edge.Kind = DeferCall
				switch mode {
				case "defer-recover":
					plan.Functions[leaf].Terminal = UsesRecover
				case "defer-suspend":
					plan.Functions[leaf].Effect = MaySuspend
				case "defer-exec":
					plan.Functions[leaf].Exec = NeedsTaskContext
				case "defer-unknown":
					edge.Targets, edge.Unknown = nil, true
				}
			case "operation":
				edge.Recipe.Kind = SiteSemaphore
			}
			if got := canPlanParameterDeferTarget(plan, fn); got != want {
				t.Fatalf("run-to-completion defer contract=%t, want %t", got, want)
			}
			if want && !deferFactoryCandidate(plan, &lowerCandidate{function: function}) {
				t.Fatal("approved sealed helper lost its private defer factory contract")
			}
		})
	}
}
