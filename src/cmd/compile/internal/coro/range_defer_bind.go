// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/ir"
	"cmd/compile/internal/types"
	"fmt"
)

// bindRangeDeferCaptureCells relates a validated native cleanup body to the
// ordinary callback's already-frozen operands. It never reads an operand or
// reconstructs an expression: wrappers preserve normalization's capture order,
// while source literals bind their actual source declaration identities.
func bindRangeDeferCaptureCells(native, ordinary *ir.Func) (map[*ir.Name]*ir.Name, error) {
	if native == nil || ordinary == nil || native.OClosure == nil || ordinary.OClosure == nil ||
		native.Wrapper() != ordinary.Wrapper() || !types.Identical(native.Type(), ordinary.Type()) ||
		len(native.ClosureVars) != len(ordinary.ClosureVars) {
		return nil, fmt.Errorf("range defer native/ordinary body or capture shape differs")
	}
	bindings := make(map[*ir.Name]*ir.Name, len(native.ClosureVars))
	used := make(map[*ir.Name]bool)
	for i, capture := range native.ClosureVars {
		var cell *ir.Name
		if native.Wrapper() {
			// The noder's operand snapshots have independent temporary names
			// in each body read. Their order and complete types are the ABI;
			// specialization cannot add capture parameters to a defer wrapper.
			candidate := ordinary.ClosureVars[i]
			if types.Identical(capture.Type(), candidate.Type()) {
				cell = candidate.Outer
			}
		} else {
			source := capture.Canonical()
			for _, candidate := range ordinary.ClosureVars {
				declaration := candidate.Canonical()
				if source.Sym() == declaration.Sym() && source.Pos() == declaration.Pos() &&
					types.Identical(capture.Type(), candidate.Type()) {
					if cell != nil {
						return nil, fmt.Errorf("range defer capture has ambiguous ordinary declaration")
					}
					cell = candidate.Outer
				}
			}
		}
		if cell == nil || used[cell] {
			return nil, fmt.Errorf("range defer capture has no unique frozen ordinary cell")
		}
		used[cell] = true
		bindings[capture] = cell
	}
	return bindings, nil
}
