// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/ir"
	"cmd/compile/internal/types"
	"fmt"
	"slices"
)

// A resumable cleanup target uses the normal typed factory. Its public entry
// retains ordinary Go behavior, while direct recover observes only the explicit
// structured defer-child identity.
func canPlanSuspendingDefer(plan *Plan, fn *ir.Func) bool {
	function := plan.Functions[fn]
	if function == nil || fn.Wrapper() || !callableFactorySupported(fn) ||
		function.Effect != MaySuspend || function.Exec != 0 ||
		function.Terminal&^(MayPanic|UsesRecover|MayGoexit) != 0 {
		return false
	}
	for _, edge := range function.Edges {
		if edge.Unknown {
			// The target's independently admitted factory handles an open
			// function call through its managed native/ordinary boundary.
			// The caller still depends on that factory surviving admission.
			if edge.Kind == DirectCall && edge.ValueCall && !edge.FactoryRequired &&
				edge.Node.Op() == ir.OCALLFUNC && edge.Node.Fun.Type().Kind() == types.TFUNC {
				continue
			}
			return false
		}
		if _, known := plan.edgeSummary(edge); !known {
			return false
		}
	}
	return true
}

func hasSuspendingDeferSite(plan *Plan, function *Function) bool {
	for _, edge := range function.Edges {
		if edge.Kind == DeferCall && !edge.FactoryRequired &&
			(edge.ValueCall || edge.Unknown || edge.Callee == nil && len(edge.Targets) == 0) {
			call := unwrapGoCall(edge.Node)
			if call.Op() == ir.OCALLFUNC && call.Fun != nil && call.Fun.Type().Kind() == types.TFUNC {
				return true
			}
		}
		if edge.Kind == DeferCall && !edge.Unknown {
			if summary, known := plan.edgeSummary(edge); known && summary.Effect == MaySuspend {
				return true
			}
		}
	}
	return false
}

func planResumableDefer(plan *Plan, candidate *lowerCandidate, deferred *lowerDefer, edge Edge) error {
	targets := edge.Targets
	if len(targets) == 0 {
		target := edge.Callee
		if call := unwrapGoCall(deferred.call); call != deferred.call {
			name := ir.StaticCalleeName(ir.StaticValue(call.Fun))
			if name == nil {
				return fmt.Errorf("suspending defer has no proved frozen target")
			}
			target = name.Func
		}
		targets = []*ir.Func{target}
	}
	for _, target := range targets {
		summary, known := plan.funcSummary(target)
		if !known || summary.Exec != 0 {
			return fmt.Errorf("suspending defer target has unsupported effects")
		}
		entry := lowerResumableDefer{target: target}
		if summary.Effect == MaySuspend {
			if !canPlanSuspendingDefer(plan, target) {
				return fmt.Errorf("suspending defer target has no typed factory")
			}
			entry.async = true
			candidate.dependencies[target] = true
		} else {
			if target == nil || target.Needctxt() || len(target.ClosureVars) != 0 {
				return fmt.Errorf("mixed defer target requires an environment")
			}
			entry.terminal = lowerDeferTerminal{target: target}
			if summary.Terminal != 0 {
				terminal, ok := planDeferTerminal(plan, target, summary)
				if !ok || terminal.target != target || terminal.rewrite != 0 || terminal.directGoexit {
					return fmt.Errorf("mixed defer target has unsupported terminal control")
				}
				entry.terminal = terminal
			}
			deferred.namedRecover = deferred.namedRecover || !entry.terminal.factory && summary.Terminal&UsesRecover != 0
			if entry.terminal.factory {
				candidate.dependencies[target] = true
				candidate.deferDeps[target] = true
			}
		}
		deferred.resumable = append(deferred.resumable, entry)
	}
	deferred.resumableDispatch = len(edge.Targets) != 0
	return nil
}

// planNativeDefers validates the exact cleanup targets and capture lifetimes
// independently of continuation construction. Registration only freezes operands;
// cleanup invokes each target with its separately proved terminal contract.
func planNativeDefers(plan *Plan, candidate *lowerCandidate, edges map[*ir.CallExpr]Edge) error {
	fn := candidate.function.Func
	defers, dynamicDefers := candidate.defers, candidate.dynamicDefers
	for _, deferred := range defers {
		edge, ok := edges[deferred.call]
		if ok && edge.Kind == DeferCall && !edge.FactoryRequired &&
			(edge.ValueCall || edge.Unknown || edge.Callee == nil && len(edge.Targets) == 0) {
			call := unwrapGoCall(deferred.call)
			if call.Op() != ir.OCALLFUNC || call.Fun == nil || call.Fun.Type().Kind() != types.TFUNC {
				return fmt.Errorf("open defer has no frozen function value")
			}
			deferred.open = true
			continue
		}
		if !ok || edge.Kind != DeferCall || edge.Callee == nil && len(edge.Targets) == 0 {
			return fmt.Errorf("defer has no static call plan")
		}
		if edge.Unknown {
			return fmt.Errorf("defer target has unknown effects")
		}
		summary, _ := plan.edgeSummary(edge)
		if summary.Effect != NoSuspend {
			if err := planResumableDefer(plan, candidate, deferred, edge); err != nil {
				return err
			}
			continue
		}
		if target := plainLiteralDeferTarget(plan, deferred.call, edge.Callee, summary); target != nil {
			deferred.terminalTargets = []lowerDeferTerminal{{target: target}}
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
			if !edge.Callee.Wrapper() && dynamicDefers {
				deferred.sourceLiteral = true
				deferred.literalCaptures = slices.Clone(
					edge.Callee.ClosureVars)
				for _, variable := range deferred.literalCaptures {
					source := variable.Canonical()
					if source == nil || source.Type() == nil {
						return fmt.Errorf(
							"repeated source defer capture has no type")
					}
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

// A frontend wrapper freezes the operands of a literal with arguments. Its
// function-value call may acquire a task-context constraint even when the exact
// target needs none. Bind that target directly before checking the wrapper's
// execution flags: cleanup never executes the wrapper's managed dispatch.
func plainLiteralDeferTarget(plan *Plan, call *ir.CallExpr, wrapper *ir.Func, summary FuncSummary) *ir.Func {
	closure, ok := call.Fun.(*ir.ClosureExpr)
	if !ok || wrapper == nil || closure.Func != wrapper || !wrapper.Wrapper() ||
		summary.Effect != NoSuspend || summary.Exec&^NeedsTaskContext != 0 || summary.Terminal != 0 {
		return nil
	}
	target := wrapper.WrappedFunc
	if target == nil || target.OClosure == nil || len(target.ClosureVars) != 0 {
		return nil
	}
	targetSummary, known := plan.funcSummary(target)
	if !known || targetSummary.Effect != NoSuspend || targetSummary.Exec != 0 || targetSummary.Terminal != 0 {
		return nil
	}
	inner := unwrapGoCall(call)
	if inner != call {
		if name := ir.StaticCalleeName(ir.StaticValue(inner.Fun)); name != nil && name.Func == target {
			return target
		}
	}
	return nil
}
