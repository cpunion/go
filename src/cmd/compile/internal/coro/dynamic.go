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

// functionTargetKey denotes either a local cell or an indexed function result.
// Result vertices belong only to functions in the package being analyzed; this
// proof is not an exported callable capability.
type functionTargetKey struct {
	local  *ir.Name
	result *ir.Func
	index  int
}

// localFunctionTargets is the single-function form of packageFunctionTargets.
func localFunctionTargets(fn *ir.Func) map[*ir.Name][]*ir.Func {
	return packageFunctionTargets([]*ir.Func{fn})[fn]
}

// packageFunctionTargets proves closed sets of named functions for local
// function variables and result cells. Copies and static calls form dependencies,
// and every explicit return contributes to the corresponding result vertices. Unknown
// writes and returns poison their dependents throughout the package, including
// recursive return/copy cycles. No effects or factory ABIs are inferred here.
func packageFunctionTargets(funcs []*ir.Func) map[*ir.Func]map[*ir.Name][]*ir.Func {
	return packageFunctionTargetsWithParameters(funcs, nil)
}

// Only compiler-owned specializations may seed parameter cells. Source
// parameters remain unknown regardless of the apparent set of callers.
func packageFunctionTargetsWithParameters(funcs []*ir.Func,
	parameters map[*ir.Func]map[*ir.Name][]*ir.Func) map[*ir.Func]map[*ir.Name][]*ir.Func {
	locals := make(map[*ir.Func]map[*ir.Name][]*ir.Func)
	targets := make(map[functionTargetKey][]*ir.Func)
	invalid := make(map[functionTargetKey]bool)
	dependents := make(map[functionTargetKey][]functionTargetKey)
	results := make(map[*ir.Func]map[int]functionTargetKey)
	outputs := make(map[functionTargetKey]*ir.Name)
	for _, fn := range funcs {
		locals[fn] = make(map[*ir.Name][]*ir.Func)
		for _, name := range fn.Dcl {
			if (name.Class == ir.PAUTO || name.Class == ir.PPARAMOUT ||
				name.Class == ir.PPARAM && len(parameters[fn][name]) != 0) && name.Curfn == fn &&
				name.Type() != nil && name.Type().Kind() == types.TFUNC && !name.Addrtaken() {
				locals[fn][name] = nil
				targets[functionTargetKey{local: name}] = slices.Clone(parameters[fn][name])
			}
		}
		sig := fn.Type()
		if !resumeFactorySupported(fn) || sig.NumRecvs() != 0 {
			continue
		}
		for i, field := range sig.Results() {
			if field.Type.Kind() != types.TFUNC {
				continue
			}
			key := functionTargetKey{result: fn, index: i}
			if field.Sym != nil && !field.Sym.IsBlank() {
				name, ok := field.Nname.(*ir.Name)
				if !ok || name.Class != ir.PPARAMOUT {
					continue
				}
				if _, tracked := locals[fn][name]; !tracked {
					continue
				}
				outputs[key] = name
			}
			if results[fn] == nil {
				results[fn] = make(map[int]functionTargetKey)
			}
			results[fn][i] = key
			targets[key] = nil
		}
	}
	depend := func(source, destination functionTargetKey) {
		if !slices.Contains(dependents[source], destination) {
			dependents[source] = append(dependents[source], destination)
		}
	}
	for result, name := range outputs {
		// Named results have ordinary local storage, initially nil. Every
		// assignment and explicit return writes that cell; a bare return reads
		// it. Addressing or capturing the cell invalidates its returned value,
		// including deferred overwrites after an explicit return expression.
		depend(functionTargetKey{local: name}, result)
	}
	callResult := func(destination functionTargetKey, rhs ir.Node, index, count int) bool {
		if call, ok := rhs.(*ir.CallExpr); ok && call.Op() == ir.OCALLFUNC {
			if name := ir.StaticCalleeName(ir.StaticValue(call.Fun)); name != nil &&
				name.Type().NumResults() == count {
				if result, tracked := results[name.Func][index]; tracked {
					depend(result, destination)
					return true
				}
			}
		}
		return false
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
			if callResult(destination, rhs, 0, 1) {
				return
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
		operandWrappers := make(map[*ir.Func]bool)
		ir.Visit(fn, func(node ir.Node) {
			stmt, ok := node.(*ir.GoDeferStmt)
			if !ok {
				return
			}
			if call, ok := stmt.Call.(*ir.CallExpr); ok {
				if (stmt.Op() == ir.OGO || stmt.Op() == ir.ODEFER) && unwrapGoCall(call) != call {
					operandWrappers[call.Fun.(*ir.ClosureExpr).Func] = true
				}
			}
		})
		localWrite := func(lhs, rhs ir.Node) {
			if name, ok := lhs.(*ir.Name); ok {
				if _, tracked := locals[fn][name]; tracked {
					write(fn, functionTargetKey{local: name}, rhs)
				}
			}
		}
		returned := results[fn]
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
						key := functionTargetKey{local: name}
						if _, tracked := locals[fn][name]; tracked && node.Op() == ir.OAS2FUNC &&
							len(node.Lhs) > 1 && len(node.Rhs) == 1 && callResult(key, node.Rhs[0], i, len(node.Lhs)) {
							continue
						}
						invalid[key] = true
					}
				}
			case *ir.ReturnStmt:
				sawReturn = true
				for i, result := range returned {
					destination := result
					if name := outputs[result]; name != nil {
						destination = functionTargetKey{local: name}
						if len(node.Results) == 0 {
							continue
						}
					}
					if len(node.Results) == fn.Type().NumResults() {
						write(fn, destination, node.Results[i])
					} else if len(node.Results) == 1 && fn.Type().NumResults() > 1 &&
						callResult(destination, node.Results[0], i, fn.Type().NumResults()) {
						continue
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
				// A validated frontend go/defer wrapper only reads the caller's
				// operand snapshots. It cannot mutate a function cell or
				// introduce a new target through its capture.
				if operandWrappers[node.Func] {
					break
				}
				for _, variable := range node.Func.ClosureVars {
					invalid[functionTargetKey{local: variable.Canonical()}] = true
				}
			}
		})
		if !sawReturn {
			// Bodyless declarations and never-returning functions supply no proof
			// of a returned value, even when a call site has other named writes.
			for _, result := range returned {
				invalid[result] = true
			}
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
