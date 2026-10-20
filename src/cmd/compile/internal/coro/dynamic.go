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

// localFunctionTargets proves a closed set of named functions for each local
// function variable. Every write must be a named function, nil, or another
// proved local. Addressed and captured variables, parameters, closures, and
// method values retain the ordinary call boundary. The proof covers the whole
// body, including writes after the call and writes in statement init lists.
func localFunctionTargets(fn *ir.Func) map[*ir.Name][]*ir.Func {
	targets := make(map[*ir.Name][]*ir.Func)
	invalid := make(map[*ir.Name]bool)
	// A copy from source to destination contributes every possible source
	// target, including writes later in the body. Unknown sources poison all
	// their dependents, even when a destination also has known writes.
	dependents := make(map[*ir.Name][]*ir.Name)
	for _, name := range fn.Dcl {
		if name.Class == ir.PAUTO && name.Curfn == fn &&
			name.Type() != nil && name.Type().Kind() == types.TFUNC && !name.Addrtaken() {
			targets[name] = nil
		}
	}
	write := func(lhs, rhs ir.Node) {
		name, ok := lhs.(*ir.Name)
		if !ok {
			return
		}
		if _, tracked := targets[name]; !tracked {
			return
		}
		for rhs != nil && (rhs.Op() == ir.OCONV || rhs.Op() == ir.OCONVNOP) {
			rhs = rhs.(*ir.ConvExpr).X
		}
		if rhs == nil || rhs.Op() == ir.ONIL {
			return
		}
		callee := ir.StaticCalleeName(rhs)
		if callee == nil {
			if source, ok := rhs.(*ir.Name); ok {
				if _, tracked := targets[source]; tracked {
					if !slices.Contains(dependents[source], name) {
						dependents[source] = append(dependents[source], name)
					}
					return
				}
			}
		}
		if callee == nil || callee.Func == nil ||
			!resumeFactorySupported(callee.Func) || callee.Type().NumRecvs() != 0 {
			invalid[name] = true
			return
		}
		if !slices.Contains(targets[name], callee.Func) {
			targets[name] = append(targets[name], callee.Func)
		}
	}
	ir.Visit(fn, func(node ir.Node) {
		switch node := node.(type) {
		case *ir.AssignStmt:
			write(node.X, node.Y)
		case *ir.AssignListStmt:
			for i, lhs := range node.Lhs {
				if len(node.Lhs) == len(node.Rhs) && node.Op() == ir.OAS2 {
					write(lhs, node.Rhs[i])
				} else if name, ok := lhs.(*ir.Name); ok {
					invalid[name] = true
				}
			}
		case *ir.RangeStmt:
			for _, lhs := range []ir.Node{node.Key, node.Value} {
				if name, ok := lhs.(*ir.Name); ok {
					invalid[name] = true
				}
			}
		case *ir.CaseClause:
			// A type-switch case variable receives the selected guard value
			// implicitly, even when its body also assigns a named function.
			if node.Var != nil {
				invalid[node.Var.Canonical()] = true
			}
		case *ir.AddrExpr:
			if name, ok := node.X.(*ir.Name); ok {
				invalid[name] = true
			}
		case *ir.ClosureExpr:
			for _, variable := range node.Func.ClosureVars {
				invalid[variable.Canonical()] = true
			}
		}
	})
	// Compute the least fixed point. A seeded cycle acquires all reachable
	// named targets; a nil-only cycle stays empty and is not lowered. Keep
	// invalid entries until propagation finishes so unknown values cannot be
	// hidden by a copy chain or cycle.
	var pending []*ir.Name
	queued := make(map[*ir.Name]bool)
	enqueue := func(name *ir.Name) {
		if !queued[name] {
			queued[name] = true
			pending = append(pending, name)
		}
	}
	for name := range targets {
		if invalid[name] || len(targets[name]) != 0 {
			enqueue(name)
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
	for name, funcs := range targets {
		if invalid[name] || len(funcs) == 0 {
			delete(targets, name)
			continue
		}
		slices.SortFunc(funcs, func(a, b *ir.Func) int {
			return strings.Compare(ir.PkgFuncName(a), ir.PkgFuncName(b))
		})
	}
	return targets
}
