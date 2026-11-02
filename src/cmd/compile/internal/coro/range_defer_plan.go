// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/ir"
	"fmt"
)

// rangeDeferOwnerPlan joins source marker positions to every body that can
// register cleanup there. Bodies need not execute in the owner's task, or even
// on a native stack. Native owner admission must prove all their cleanup targets.
type rangeDeferOwnerPlan struct {
	function *ir.Func
	anchors  map[*ir.Name]*ir.AssignStmt
	bodies   []*rangeDeferBodyPlan
	err      error
}

type rangeDeferBodyPlan struct {
	function *Function
	owner    *rangeDeferOwnerPlan
	cleanup  *lowerCandidate
}

func planRangeDeferOwners(plan *Plan, functions []*Function) map[*ir.Func]*rangeDeferOwnerPlan {
	owners := make(map[*ir.Func]*rangeDeferOwnerPlan)
	for _, function := range functions {
		fn := function.Func
		markers := make(map[*ir.CallExpr]bool)
		ir.Visit(fn, func(node ir.Node) {
			assignment, ok := node.(*ir.AssignStmt)
			if !ok {
				return
			}
			call, ok := assignment.Y.(*ir.CallExpr)
			if !ok || call.Op() != ir.OCALLFUNC {
				return
			}
			callee := ir.StaticCalleeName(call.Fun)
			if callee == nil || !rangeDeferAnchorDeclaration(callee.Func) {
				return
			}
			markers[call] = true
			owner := owners[fn]
			if owner == nil {
				owner = &rangeDeferOwnerPlan{function: fn, anchors: make(map[*ir.Name]*ir.AssignStmt)}
				owners[fn] = owner
			}
			name, ok := assignment.X.(*ir.Name)
			if !ok || name.Canonical().Curfn != fn || name.Type() == nil ||
				!name.Type().IsEmptyInterface() || len(call.Args) != 0 || call.IsDDD || call.GoDefer {
				owner.err = fmt.Errorf("invalid enclosing range marker assignment")
				return
			}
			name = name.Canonical()
			if owner.anchors[name] != nil {
				owner.err = fmt.Errorf("range anchor has multiple defining marker assignments")
				return
			}
			owner.anchors[name] = assignment
		})
		ir.Visit(fn, func(node ir.Node) {
			call, ok := node.(*ir.CallExpr)
			if !ok || call.Op() != ir.OCALLFUNC || markers[call] {
				return
			}
			callee := ir.StaticCalleeName(call.Fun)
			if callee == nil || !rangeDeferAnchorDeclaration(callee.Func) {
				return
			}
			owner := owners[fn]
			if owner == nil {
				owner = &rangeDeferOwnerPlan{function: fn, anchors: make(map[*ir.Name]*ir.AssignStmt)}
				owners[fn] = owner
			}
			owner.err = fmt.Errorf("range marker is not an exact defining assignment")
		})
	}
	for _, function := range functions {
		fn := function.Func
		var defers []*lowerDefer
		var invalid bool
		owner := owners[fn.RangeParent]
		ir.Visit(fn, func(node ir.Node) {
			statement, ok := node.(*ir.GoDeferStmt)
			if !ok || statement.DeferAt == nil {
				return
			}
			call, ok := statement.Call.(*ir.CallExpr)
			anchor, named := statement.DeferAt.(*ir.Name)
			var actual *rangeDeferOwnerPlan
			if named {
				actual = owners[anchor.Canonical().Curfn]
			}
			if owner == nil || !named || anchor.Type() == nil ||
				!anchor.Type().IsEmptyInterface() || anchor.Canonical().Curfn != owner.function ||
				owner.anchors[anchor.Canonical()] == nil || statement.Op() != ir.ODEFER ||
				!ok || call.Op() != ir.OCALLFUNC || call.Fun == nil || call.Fun.Type() == nil ||
				len(call.Args) != 0 || call.Fun.Type().NumParams() != 0 || call.Fun.Type().NumResults() != 0 {
				invalid = true
				if actual != nil {
					actual.err = fmt.Errorf("%s: captured range marker disagrees with frontend enclosing owner", ir.PkgFuncName(fn))
				}
				return
			}
			defers = append(defers, &lowerDefer{statement: statement, call: call})
		})
		if invalid {
			if owner != nil {
				owner.err = fmt.Errorf("%s: range defer has no exact enclosing marker binding", ir.PkgFuncName(fn))
			}
			continue
		}
		if len(defers) == 0 {
			continue
		}
		cleanup := &lowerCandidate{
			function: function, defers: defers, dynamicDefers: true,
			dependencies: make(map[*ir.Func]bool), deferDeps: make(map[*ir.Func]bool),
		}
		edges := make(map[*ir.CallExpr]Edge)
		for _, edge := range function.Edges {
			edges[edge.Node] = edge
		}
		if err := planNativeDefers(plan, cleanup, edges); err != nil {
			owner.err = fmt.Errorf("%s: enclosing range cleanup target: %w", ir.PkgFuncName(fn), err)
			continue
		}
		owner.bodies = append(owner.bodies, &rangeDeferBodyPlan{function: function, owner: owner, cleanup: cleanup})
	}
	return owners
}
