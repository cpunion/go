// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import "cmd/compile/internal/ir"

// invocationLocals finds variables declared and used entirely in a suffix
// after the last coroutine site. These variables need not occupy persistent
// frame storage. Address-taken and captured variables keep their existing
// storage so this does not change allocation points or closure ownership.
//
// This is deliberately not a liveness analysis. Uses outside the suffix,
// captured variables, loop bodies with coroutine sites, and functions with
// unstructured jumps remain unchanged.
func invocationLocals(function *Function) map[*ir.Name]bool {
	sites := make(map[ir.Node]bool, len(function.Sites))
	for _, site := range function.Sites {
		sites[site.Node] = true
	}
	inlineJumps := make(map[ir.Node]bool)
	ir.VisitList(function.Func.Body, func(node ir.Node) {
		if inlined, ok := node.(*ir.InlinedCallExpr); ok &&
			!ir.Any(inlined, func(node ir.Node) bool { return sites[node] }) {
			// Inlining rewrites returns into jumps to its own final label.
			// These stay within one non-suspending expression.
			ir.Visit(inlined, func(node ir.Node) {
				if node.Op() == ir.OGOTO || node.Op() == ir.OLABEL {
					inlineJumps[node] = true
				}
			})
		}
	})
	uses := make(map[*ir.Name]int)
	captured := make(map[*ir.Name]bool)
	jumps := false
	ir.VisitList(function.Func.Body, func(node ir.Node) {
		if (node.Op() == ir.OGOTO || node.Op() == ir.OLABEL) && !inlineJumps[node] {
			jumps = true
		}
		if name, ok := node.(*ir.Name); ok {
			uses[name.Canonical()]++
		}
		if closure, ok := node.(*ir.ClosureExpr); ok {
			for _, name := range closure.Func.ClosureVars {
				captured[name.Canonical()] = true
			}
		}
	})
	locals := make(map[*ir.Name]bool)
	if jumps {
		return locals
	}
	var suffix func(ir.Nodes)
	suffix = func(body ir.Nodes) {
		start := 0
		for i, node := range body {
			if ir.Any(node, func(node ir.Node) bool {
				_, closure := node.(*ir.ClosureExpr)
				return sites[node] || closure
			}) {
				start = i + 1
			}
		}
		// A final conditional can itself contain the final coroutine site.
		// Its branch suffixes are equally safe, but do not descend into loops:
		// an enclosing back edge could suspend before reentering the suffix.
		if start != 0 {
			switch node := body[start-1].(type) {
			case *ir.BlockStmt:
				suffix(node.List)
			case *ir.IfStmt:
				suffix(node.Body)
				suffix(node.Else)
			}
		}
		regionUses := make(map[*ir.Name]int)
		var declarations []*ir.Name
		ir.VisitList(body[start:], func(node ir.Node) {
			if name, ok := node.(*ir.Name); ok {
				regionUses[name.Canonical()]++
			}
			if decl, ok := node.(*ir.Decl); ok {
				declarations = append(declarations, decl.X.Canonical())
			}
		})
		for _, name := range declarations {
			if name.Curfn == function.Func && name.Class == ir.PAUTO && !name.Addrtaken() &&
				!captured[name] && regionUses[name] == uses[name] {
				locals[name] = true
			}
		}
	}
	suffix(function.Func.Body)
	return locals
}
