// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import "cmd/compile/internal/ir"

// entryZeroedFrameLocals finds pointer-free arrays cleared by the straight-line
// prefix of the initial resume state. A reused frame need not clear these same
// bytes in its factory: the source clear still runs before any read, address
// publication, control transfer, or call. Pointerful fields retain factory
// initialization so GC may inspect a queued frame before its first resume.
//
// A declaration marker for the assignment's own variable has no evaluation.
// Other initializers or statements stop the proof. This deliberately does not
// infer dominance across branches or resume states.
func entryZeroedFrameLocals(body ir.Nodes) map[*ir.Name]bool {
	var locals map[*ir.Name]bool
	for _, node := range body {
		assignment, ok := node.(*ir.AssignStmt)
		if !ok {
			break
		}
		name, ok := assignment.X.(*ir.Name)
		if !ok || name.Class != ir.PAUTO {
			break
		}
		declarationsOnly := true
		for _, init := range assignment.Init() {
			decl, ok := init.(*ir.Decl)
			if !ok || decl.X.Canonical() != name.Canonical() || len(decl.Init()) != 0 {
				declarationsOnly = false
				break
			}
		}
		hasInit := func(node ir.Node) bool { return len(node.Init()) != 0 }
		if !declarationsOnly || ir.Any(assignment.X, hasInit) ||
			assignment.Y != nil && ir.Any(assignment.Y, hasInit) {
			break
		}
		// A nil RHS is the frontend's implicit declaration zero.
		zero := assignment.Y == nil || ir.IsZero(assignment.Y)
		if literal, ok := assignment.Y.(*ir.CompLitExpr); ok &&
			literal.Op() == ir.OCOMPLIT && literal.Type().IsArray() && len(literal.List) == 0 {
			zero = true
		}
		if !zero {
			break
		}
		if name.Type().IsArray() && !name.Type().HasPointers() {
			if locals == nil {
				locals = make(map[*ir.Name]bool)
			}
			locals[name.Canonical()] = true
		}
	}
	return locals
}

// zeroInitializedFrameLocals finds factory-local arrays whose only uses before
// frame construction are declarations and zero assignments. Initialize their
// frame fields directly instead of copying a zeroed native-stack temporary.
// Other reads, writes, or address uses conservatively retain the temporary.
func zeroInitializedFrameLocals(declarations ir.Nodes) map[*ir.Name]bool {
	locals := make(map[*ir.Name]bool)
	for _, node := range declarations {
		if decl, ok := node.(*ir.Decl); ok {
			name := decl.X.Canonical()
			if name.Class == ir.PAUTO && name.Type().IsArray() {
				locals[name] = true
			}
		}
	}
	for _, node := range declarations {
		if _, ok := node.(*ir.Decl); ok {
			continue
		}
		if assignment, ok := node.(*ir.AssignStmt); ok && len(node.Init()) == 0 {
			zero := assignment.Y != nil && ir.IsZero(assignment.Y)
			// NewZero's composite literal has not been typechecked yet.
			if literal, ok := assignment.Y.(*ir.CompLitExpr); ok &&
				literal.Op() == ir.OCOMPLIT && literal.Type().IsArray() && len(literal.List) == 0 {
				zero = true
			}
			if _, ok := assignment.X.(*ir.Name); ok && zero &&
				!ir.Any(node, func(node ir.Node) bool { return len(node.Init()) != 0 }) {
				continue
			}
		}
		ir.Visit(node, func(node ir.Node) {
			if name, ok := node.(*ir.Name); ok {
				delete(locals, name.Canonical())
			}
		})
	}
	return locals
}
