// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import "cmd/compile/internal/ir"

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
