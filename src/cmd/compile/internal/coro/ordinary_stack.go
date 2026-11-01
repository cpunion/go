// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/ir"
	"cmd/compile/internal/types"
	"internal/abi"
)

// ordinaryHelperNeedsGoStack identifies ordinary helpers whose aggregate
// storage cannot be confined to the small fixed-stack budgets used by native
// resumes. These helpers are not rewritten into coroutine frames. Escape
// analysis and walk run later, so do not assume their aggregates will escape
// or that a type assertion's temporary will disappear during optimization.
// This is a conservative source bound, not a general SSA stack-size proof.
func ordinaryHelperNeedsGoStack(fn *ir.Func) bool {
	const localBudget = abi.StackNosplitBase / 4
	var locals int64
	for _, name := range fn.Dcl {
		if name.Class != ir.PAUTO && name.Class != ir.PAUTOHEAP {
			continue
		}
		typ := name.Type()
		if typ == nil || !(typ.IsArray() || typ.IsStruct()) {
			continue
		}
		types.CalcSize(typ)
		if typ.Size() > localBudget-locals {
			return true
		}
		locals = types.RoundUp(locals+typ.Size(), int64(types.PtrSize))
	}

	const assertionBudget = abi.StackNosplitBase / 8
	var assertions int64
	return ir.Any(fn, func(node ir.Node) bool {
		switch node.Op() {
		case ir.ODOTTYPE, ir.ODOTTYPE2, ir.ODYNAMICDOTTYPE, ir.ODYNAMICDOTTYPE2:
			if typ := node.Type(); typ != nil && (typ.IsArray() || typ.IsStruct()) {
				types.CalcSize(typ)
				if typ.Size() > assertionBudget-assertions {
					return true
				}
				assertions += typ.Size()
			}
		}
		return false
	})
}
