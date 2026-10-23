// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import "cmd/compile/internal/ir"

// canPlanParameterDeferTarget admits a private sealed helper to the existing
// run-to-completion defer factory ABI. Its only execution constraint must come
// from dispatching its readonly callback parameter through typed factories.
// This does not relax the public defer contract for arbitrary task creators.
func canPlanParameterDeferTarget(plan *Plan, fn *ir.Func) bool {
	function := plan.Functions[fn]
	if function == nil || function.Effect != NoSuspend ||
		function.Exec != NeedsTaskContext || function.LocalExec != NeedsTaskContext ||
		!resumeFactorySupported(fn) || !readOnlyCallbackParameters(fn) {
		return false
	}
	sealed := false
	for _, edge := range function.Edges {
		// Nested cleanup runs under the same restricted defer ABI. Its
		// concrete targets and factory dependencies are checked by lowering
		// and the defer-factory fixed point. Detached children cannot share
		// this run-to-completion lifetime.
		if edge.Kind != DirectCall && edge.Kind != DeferCall ||
			edge.Recipe.Kind != SiteInvalid && edge.Recipe.Kind != SiteGoexit {
			return false
		}
		summary, known := plan.edgeSummary(edge)
		if !known || summary.Effect != NoSuspend || summary.Exec != 0 ||
			edge.Kind == DirectCall && summary.Terminal&UsesRecover != 0 {
			return false
		}
		sealed = sealed || edge.FactoryRequired
	}
	return sealed
}
