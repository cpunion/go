// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/ir"
	"cmd/compile/internal/types"
)

// A normal call in a deferred source literal cannot recover its caller's
// panic. Reuse the bounded normal-callback adapter, keeping the public entry
// available for direct defer calls. Limit this rewrite to capture-free static
// leaves whose recover effect is already present locally in the source literal;
// its published effect and terminal summaries therefore remain unchanged.
func prepareLiteralNormalRecoverCalls(plan *Plan, literals map[*ir.Func]bool,
	adapterFor func(*ir.Func) *ir.Func) {
	for fn := range literals {
		function := plan.Functions[fn]
		if function == nil || fn.Wrapper() || function.Effect != NoSuspend ||
			function.LocalTerminal&UsesRecover == 0 || function.LocalTerminal != function.Terminal {
			continue
		}
		for i := range function.Edges {
			edge := &function.Edges[i]
			child := plan.Functions[edge.Callee]
			if edge.Kind != DirectCall || edge.Unknown || len(edge.Targets) != 0 ||
				child == nil || child.Terminal != UsesRecover || !plainCallbackFactorySupported(child) {
				continue
			}
			target, call := child.Func, edge.Node
			if target.Wrapper() || target.ABIWrapper() || target.Needctxt() ||
				len(target.ClosureVars) != 0 || target.Type().Recv() != nil || call == nil || call.Fun == nil || len(call.Fun.Init()) != 0 {
				continue
			}
			switch value := call.Fun.(type) {
			case *ir.ClosureExpr:
				if value.Func != target {
					continue
				}
			case *ir.Name:
				if value != target.Nname {
					continue
				}
			default:
				continue
			}
			adapter := adapterFor(target)
			if adapter == nil || !types.Identical(adapter.Type(), call.Fun.Type()) {
				continue
			}
			call.Fun = adapter.Nname
			edge.Callee, edge.CalleeName = adapter, symbolName(adapter.Nname)
			plan.Functions[adapter] = &Function{Func: adapter, Primary: PlainPrimary}
		}
	}
}
