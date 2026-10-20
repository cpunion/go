// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/ir"
	"cmd/compile/internal/types"
	"slices"
	"strings"
)

// functionTargetKey denotes either a local cell or a function's single result.
// Result vertices belong only to functions in the package being analyzed; this
// proof is not an exported callable capability.
type functionTargetKey struct {
	local  *ir.Name
	result *ir.Func
}

// localFunctionTargets is the single-function form of packageFunctionTargets.
func localFunctionTargets(fn *ir.Func) map[*ir.Name][]*ir.Func {
	return packageFunctionTargets([]*ir.Func{fn})[fn]
}

// packageFunctionTargets proves closed sets of named functions for local
// function variables and result cells. Copies and single-result calls form dependencies, and
// every explicit return contributes to the callee's result vertex. Unknown
// writes and returns poison their dependents throughout the package, including
// recursive return/copy cycles. No effects or factory ABIs are inferred here.
func packageFunctionTargets(funcs []*ir.Func) map[*ir.Func]map[*ir.Name][]*ir.Func {
	locals := make(map[*ir.Func]map[*ir.Name][]*ir.Func)
	targets := make(map[functionTargetKey][]*ir.Func)
	invalid := make(map[functionTargetKey]bool)
	dependents := make(map[functionTargetKey][]functionTargetKey)
	results := make(map[*ir.Func]functionTargetKey)
	outputs := make(map[*ir.Func]*ir.Name)
	for _, fn := range funcs {
		locals[fn] = make(map[*ir.Name][]*ir.Func)
		for _, name := range fn.Dcl {
			if (name.Class == ir.PAUTO || name.Class == ir.PPARAMOUT) && name.Curfn == fn &&
				name.Type() != nil && name.Type().Kind() == types.TFUNC && !name.Addrtaken() {
				locals[fn][name] = nil
				targets[functionTargetKey{local: name}] = nil
			}
		}
		sig := fn.Type()
		if resumeFactorySupported(fn) && sig.NumRecvs() == 0 && sig.NumResults() == 1 &&
			sig.Result(0).Type.Kind() == types.TFUNC {
			field := sig.Result(0)
			if field.Sym != nil && !field.Sym.IsBlank() {
				name, ok := field.Nname.(*ir.Name)
				if !ok || name.Class != ir.PPARAMOUT {
					continue
				}
				if _, tracked := locals[fn][name]; !tracked {
					continue
				}
				outputs[fn] = name
			}
			key := functionTargetKey{result: fn}
			results[fn] = key
			targets[key] = nil
		}
	}
	depend := func(source, destination functionTargetKey) {
		if !slices.Contains(dependents[source], destination) {
			dependents[source] = append(dependents[source], destination)
		}
	}
	for fn, name := range outputs {
		// Named results have ordinary local storage, initially nil. Every
		// assignment and explicit return writes that cell; a bare return reads
		// it. Addressing or capturing the cell invalidates its returned value,
		// including deferred overwrites after an explicit return expression.
		depend(functionTargetKey{local: name}, results[fn])
	}
	write := func(fn *ir.Func, destination functionTargetKey, rhs ir.Node) {
		for rhs != nil && (rhs.Op() == ir.OCONV || rhs.Op() == ir.OCONVNOP) {
			rhs = rhs.(*ir.ConvExpr).X
		}
		if rhs == nil || rhs.Op() == ir.ONIL {
			return
		}
		callee := ir.StaticCalleeName(rhs)
		if callee == nil {
			if source, ok := rhs.(*ir.Name); ok && source.Curfn == fn {
				key := functionTargetKey{local: source}
				if _, tracked := targets[key]; tracked {
					depend(key, destination)
					return
				}
			}
			if call, ok := rhs.(*ir.CallExpr); ok && call.Op() == ir.OCALLFUNC {
				if name := ir.StaticCalleeName(ir.StaticValue(call.Fun)); name != nil {
					if result, tracked := results[name.Func]; tracked {
						depend(result, destination)
						return
					}
				}
			}
		}
		if callee == nil || callee.Func == nil ||
			!resumeFactorySupported(callee.Func) || callee.Type().NumRecvs() != 0 {
			invalid[destination] = true
			return
		}
		if !slices.Contains(targets[destination], callee.Func) {
			targets[destination] = append(targets[destination], callee.Func)
		}
	}
	for _, fn := range funcs {
		localWrite := func(lhs, rhs ir.Node) {
			if name, ok := lhs.(*ir.Name); ok {
				if _, tracked := locals[fn][name]; tracked {
					write(fn, functionTargetKey{local: name}, rhs)
				}
			}
		}
		result, returnsFunction := results[fn]
		sawReturn := false
		ir.Visit(fn, func(node ir.Node) {
			switch node := node.(type) {
			case *ir.AssignStmt:
				localWrite(node.X, node.Y)
			case *ir.AssignListStmt:
				for i, lhs := range node.Lhs {
					if len(node.Lhs) == len(node.Rhs) && node.Op() == ir.OAS2 {
						localWrite(lhs, node.Rhs[i])
					} else if name, ok := lhs.(*ir.Name); ok {
						invalid[functionTargetKey{local: name}] = true
					}
				}
			case *ir.ReturnStmt:
				if returnsFunction {
					sawReturn = true
					destination := result
					if name := outputs[fn]; name != nil {
						destination = functionTargetKey{local: name}
						if len(node.Results) == 0 {
							break
						}
					}
					if len(node.Results) == 1 {
						write(fn, destination, node.Results[0])
					} else {
						invalid[destination] = true
					}
				}
			case *ir.RangeStmt:
				for _, lhs := range []ir.Node{node.Key, node.Value} {
					if name, ok := lhs.(*ir.Name); ok {
						invalid[functionTargetKey{local: name}] = true
					}
				}
			case *ir.CaseClause:
				// A type-switch binding receives an unknown guard value implicitly.
				if node.Var != nil {
					invalid[functionTargetKey{local: node.Var.Canonical()}] = true
				}
			case *ir.AddrExpr:
				if name, ok := node.X.(*ir.Name); ok {
					invalid[functionTargetKey{local: name}] = true
				}
			case *ir.ClosureExpr:
				for _, variable := range node.Func.ClosureVars {
					invalid[functionTargetKey{local: variable.Canonical()}] = true
				}
			}
		})
		if returnsFunction && !sawReturn {
			// Bodyless declarations and never-returning functions supply no proof
			// of a returned value, even when a call site has other named writes.
			invalid[result] = true
		}
	}
	// Compute the least fixed point before pruning unknown or empty vertices.
	// A seeded cycle acquires every reachable target; a nil-only cycle stays
	// empty. Invalidity must also cross cycles and later writes.
	var pending []functionTargetKey
	queued := make(map[functionTargetKey]bool)
	enqueue := func(key functionTargetKey) {
		if !queued[key] {
			queued[key] = true
			pending = append(pending, key)
		}
	}
	for key := range targets {
		if invalid[key] || len(targets[key]) != 0 {
			enqueue(key)
		}
	}
	for len(pending) != 0 {
		source := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		queued[source] = false
		for _, destination := range dependents[source] {
			if invalid[destination] {
				continue
			}
			if invalid[source] {
				invalid[destination] = true
				enqueue(destination)
				continue
			}
			for _, target := range targets[source] {
				if !slices.Contains(targets[destination], target) {
					targets[destination] = append(targets[destination], target)
					enqueue(destination)
				}
			}
		}
	}
	for _, names := range locals {
		for name := range names {
			key := functionTargetKey{local: name}
			functions := targets[key]
			if invalid[key] || len(functions) == 0 {
				delete(names, name)
				continue
			}
			slices.SortFunc(functions, func(a, b *ir.Func) int {
				return strings.Compare(ir.PkgFuncName(a), ir.PkgFuncName(b))
			})
			names[name] = functions
		}
	}
	return locals
}
