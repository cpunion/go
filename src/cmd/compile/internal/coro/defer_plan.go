// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/ir"
	"fmt"
	"slices"
)

// A fixed source literal has a private, typed environment and no invocation
// operands left to evaluate during cleanup. Its ordinary entry remains intact.
// Direct recover and arbitrary dynamic/range registrations need a separate
// resumable defer contract and retain their existing fallback here.
func canPlanSuspendingDefer(plan *Plan, fn *ir.Func) bool {
	function := plan.Functions[fn]
	if function == nil || fn.OClosure == nil || fn.Wrapper() ||
		fn.Type().NumParams() != 0 || fn.Type().NumResults() != 0 ||
		!callableFactorySupported(fn) || function.Effect != MaySuspend ||
		function.Exec != 0 || function.Terminal&^MayPanic != 0 {
		return false
	}
	for _, edge := range function.Edges {
		// MaySuspend can also come from an unknown call. It is not proof
		// that a typed child factory exists for that call or its captures.
		if edge.Unknown {
			return false
		}
		if _, known := plan.edgeSummary(edge); !known {
			return false
		}
	}
	return true
}

// planNativeDefers validates the exact cleanup targets and capture lifetimes
// independently of continuation construction. Registration only freezes operands;
// cleanup invokes each target with its separately proved terminal contract.
func planNativeDefers(plan *Plan, candidate *lowerCandidate, edges map[*ir.CallExpr]Edge) error {
	fn := candidate.function.Func
	defers, dynamicDefers := candidate.defers, candidate.dynamicDefers
	for _, deferred := range defers {
		edge, ok := edges[deferred.call]
		if !ok || edge.Kind != DeferCall || edge.Callee == nil && len(edge.Targets) == 0 {
			return fmt.Errorf("defer has no static call plan")
		}
		if edge.Unknown {
			return fmt.Errorf("defer target has unknown effects")
		}
		summary, _ := plan.edgeSummary(edge)
		if summary.Effect != NoSuspend {
			closure, literal := deferred.call.Fun.(*ir.ClosureExpr)
			if dynamicDefers || deferred.rangeAnchor != nil || len(edge.Targets) != 0 ||
				!literal || closure.Func != edge.Callee || !canPlanSuspendingDefer(plan, edge.Callee) {
				return fmt.Errorf("suspending defer requires a fixed source literal")
			}
			deferred.asyncTarget = edge.Callee
			deferred.snapshotCallee = true
			candidate.dependencies[edge.Callee] = true
			continue
		}
		var parameterTarget *ir.Func
		if call := unwrapGoCall(deferred.call); call != deferred.call {
			if name := ir.StaticCalleeName(call.Fun); name != nil &&
				canPlanParameterDeferTarget(plan, name.Func) {
				parameterTarget = name.Func
			}
		}
		if summary.Exec != 0 && (summary.Exec != NeedsTaskContext || parameterTarget == nil) {
			return fmt.Errorf("defer target has execution constraints %s",
				summary.Exec)
		}
		if len(edge.Targets) != 0 {
			if unwrapDynamicDeferCall(deferred.call) == deferred.call {
				name, ok := deferred.call.Fun.(*ir.Name)
				if !ok || name.Curfn != fn {
					return fmt.Errorf("dynamic defer has no frozen callee")
				}
				// A func() call is already normalized without a wrapper.
				// Save the selected value before arming a fixed defer site.
				// Repeated sites save it directly in the common defer stack.
				deferred.snapshotCallee = true
			}
			for _, target := range edge.Targets {
				if target == nil || target.Needctxt() || len(target.ClosureVars) != 0 {
					// Dispatch below calls a static entry without a closure
					// context. A captured value needs its environment explicitly
					// represented before it can use this cleanup contract.
					return fmt.Errorf("captured dynamic defer target requires an environment")
				}
				targetSummary, _ := plan.funcSummary(target)
				terminal := lowerDeferTerminal{target: target}
				if targetSummary.Terminal != 0 {
					var ok bool
					terminal, ok = planDeferTerminal(plan, target, targetSummary)
					if !ok || terminal.target != target || terminal.rewrite != 0 || terminal.directGoexit {
						return fmt.Errorf("dynamic defer target %s has unsupported terminal control %s",
							ir.PkgFuncName(target), targetSummary.Terminal)
					}
				}
				deferred.terminalTargets = append(deferred.terminalTargets, terminal)
				deferred.namedRecover = deferred.namedRecover ||
					!terminal.factory && targetSummary.Terminal&UsesRecover != 0
				if terminal.factory {
					candidate.dependencies[target] = true
					candidate.deferDeps[target] = true
				}
			}
			// Dispatch the frozen closed set directly during cleanup. This
			// also binds plain targets for the machine-stack proof. Nil is
			// still checked at invocation, after registration froze operands.
			continue
		}
		closure, isClosure := deferred.call.Fun.(*ir.ClosureExpr)
		if edge.Callee.OClosure != nil {
			if !isClosure || closure.Func != edge.Callee {
				return fmt.Errorf("defer closure does not match call plan")
			}
			if edge.Callee.Wrapper() && edge.Callee.WrappedFunc == nil {
				return fmt.Errorf("defer wrapper has no target")
			}
			if target := edge.Callee.WrappedFunc; target != nil && target.OClosure != nil &&
				len(target.ClosureVars) == 0 && summary.Terminal == 0 {
				// A literal with arguments is frozen in a frontend wrapper.
				// Even without an environment its ordinary call can remain
				// indirect when inlining is disabled. Bind the exact static
				// target through the existing cleanup dispatch, preserving
				// operand snapshots while giving the linker a real call edge.
				inner := unwrapGoCall(deferred.call)
				if inner != deferred.call {
					name := ir.StaticCalleeName(ir.StaticValue(inner.Fun))
					if name != nil && name.Func == target {
						deferred.terminalTargets = []lowerDeferTerminal{{target: target}}
						continue
					}
				}
			}
			if !edge.Callee.Wrapper() && dynamicDefers {
				deferred.sourceLiteral = true
				deferred.literalCaptures = slices.Clone(
					edge.Callee.ClosureVars)
				captured := make(map[*ir.Name]bool,
					len(deferred.literalCaptures))
				for _, variable := range deferred.literalCaptures {
					source := variable.Canonical()
					if source == nil || source.Type() == nil {
						return fmt.Errorf(
							"repeated source defer capture has no type")
					}
					captured[source] = true
				}
				nested := false
				ir.Visit(edge.Callee, func(node ir.Node) {
					closure, ok := node.(*ir.ClosureExpr)
					if !ok || closure.Func == edge.Callee {
						return
					}
					for _, variable := range closure.Func.ClosureVars {
						if captured[variable.Canonical()] {
							nested = true
						}
					}
				})
				if nested {
					return fmt.Errorf(
						"nested repeated source defer capture")
				}
			}
		}
		if parameterTarget != nil {
			// Even an all-plain sealed callback uses its typed factory. Its
			// task-context flag is internal to dispatch, not permission to
			// suspend, create children, or enter an arbitrary task context.
			deferred.terminalTarget = parameterTarget
			deferred.runTerminal = true
			candidate.dependencies[parameterTarget] = true
			candidate.deferDeps[parameterTarget] = true
			continue
		}
		if summary.Terminal != 0 {
			terminal, ok := planDeferTerminal(plan, edge.Callee, summary)
			if !ok {
				return fmt.Errorf("defer target has terminal control %s",
					summary.Terminal)
			}
			deferred.rewriteTerminal = terminal.rewrite
			deferred.terminalTarget = terminal.target
			deferred.runTerminal = terminal.factory
			deferred.directGoexit = terminal.directGoexit
			deferred.namedRecover = terminal.target != nil &&
				!deferred.runTerminal && !deferred.directGoexit &&
				summary.Terminal&UsesRecover != 0
			if deferred.runTerminal {
				candidate.dependencies[terminal.target] = true
				candidate.deferDeps[terminal.target] = true
			}
		}
	}
	return nil
}
