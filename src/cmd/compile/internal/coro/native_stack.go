// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/ir"
	"cmd/compile/internal/typecheck"
)

// NativeStackEntry identifies compiler-owned functions that can execute on a
// native stack. The backend records this in FuncInfo after final SSA layout;
// the linker then checks actual machine frames and ordinary helper chains.
// An indirect machine call without a complete linked target proof rejects
// native execution. A source-level union of callees is not a proof for an
// arbitrary call introduced later by walk or SSA.
var nativeStackEntries = make(map[*ir.Func]bool)

// NativeStackEntry is read only during parallel backend compilation.
func NativeStackEntry(fn *ir.Func) bool { return nativeStackEntries[fn] }

func recordNativeStackMetadata(functions []*Function, factories map[*ir.Func]*ir.Func) {
	for _, factory := range factories {
		nativeStackEntries[factory] = true
	}
	for _, fn := range typecheck.Target.Funcs {
		for parent := fn.ClosureParent; parent != nil; parent = parent.ClosureParent {
			if nativeStackEntries[parent] {
				nativeStackEntries[fn] = true
				break
			}
		}
	}
	// Source defer literals retain their in-place cleanup entry.
	for _, function := range functions {
		if factories[function.Func] == nil {
			continue
		}
		for _, edge := range function.Edges {
			if edge.Kind != DeferCall {
				continue
			}
			if edge.Callee != nil && edge.Callee.OClosure != nil {
				nativeStackEntries[edge.Callee] = true
			}
			for _, target := range edge.Targets {
				if target.OClosure != nil {
					nativeStackEntries[target] = true
				}
			}
		}
	}
}
