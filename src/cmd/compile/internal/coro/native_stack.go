// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/base"
	"cmd/compile/internal/ir"
	"cmd/compile/internal/objw"
	"cmd/compile/internal/types"
	"cmd/internal/obj"
	"slices"
	"strings"
)

// Each source entry has an independent linker-written gate. Its second word
// retains the factory: an entry whose native body was otherwise inlined away
// must still have its complete machine proof. Gate contents are never folded
// by the compiler. The code address is not a Go heap pointer.
var nativeSourceGates = make(map[*ir.Func]*ir.Name)
var nativeStackGates = make(map[*ir.Func][]*ir.Name)
var nativeStackCallees = make(map[*ir.Func][]*ir.Func)

// NativeStackEntry and NativeStackGates are read only by parallel backends.
func NativeStackEntry(fn *ir.Func) bool         { return len(nativeStackGates[fn]) != 0 }
func NativeStackGates(fn *ir.Func) []*ir.Name   { return nativeStackGates[fn] }
func NativeStackCallees(fn *ir.Func) []*ir.Func { return nativeStackCallees[fn] }

func newNativeStackGate(source, factory *ir.Func) {
	typ := types.NewArray(types.Types[types.TUINTPTR], 2)
	types.CalcSize(typ)
	sym := source.Sym().Pkg.Lookup(source.Sym().Name + ".coro.stackgate")
	gate := ir.NewNameAt(source.Pos(), sym, typ)
	gate.Class = ir.PEXTERN
	sym.Def = gate
	lsym := gate.Linksym()
	base.Ctxt.LookupInit(lsym.Name, func(lsym *obj.LSym) {
		objw.Uintptr(lsym, 0, 0)
		objw.SymPtr(lsym, types.PtrSize, factory.Linksym(), 0)
		flags := int16(obj.RODATA)
		if source.Dupok() {
			flags |= obj.DUPOK
		}
		objw.Global(lsym, int32(typ.Size()), flags)
	})
	nativeSourceGates[source] = gate
	nativeStackGates[factory] = []*ir.Name{gate}
}

func recordNativeStackEntry(fn *ir.Func, gates []*ir.Name) {
	for _, gate := range gates {
		if !slices.Contains(nativeStackGates[fn], gate) {
			nativeStackGates[fn] = append(nativeStackGates[fn], gate)
		}
	}
}

// Each factory records the exact cleanup code it registers, including imported
// named entries. Import declarations have no local backend invocation, so a
// marker on the callee alone would silently omit their machine proof.
func recordNativeStackMetadata(candidates map[*ir.Func]*lowerCandidate) {
	for _, candidate := range candidates {
		for _, deferred := range candidate.defers {
			var target *ir.Func
			if closure, ok := deferred.call.Fun.(*ir.ClosureExpr); ok {
				target = closure.Func
			} else if name := ir.StaticCalleeName(deferred.call.Fun); name != nil {
				target = name.Func
			}
			if target != nil && !slices.Contains(nativeStackCallees[candidate.factory], target) {
				nativeStackCallees[candidate.factory] = append(nativeStackCallees[candidate.factory], target)
			}
		}
	}
	// Stable ordering also covers cleanup entries shared by source factories.
	for _, gates := range nativeStackGates {
		slices.SortFunc(gates, func(a, b *ir.Name) int { return strings.Compare(a.Linksym().Name, b.Linksym().Name) })
	}
	for _, callees := range nativeStackCallees {
		slices.SortFunc(callees, func(a, b *ir.Func) int { return strings.Compare(ir.PkgFuncName(a), ir.PkgFuncName(b)) })
	}
}
