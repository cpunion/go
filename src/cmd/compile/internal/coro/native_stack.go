// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
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

// NativeStackEntry and NativeStackGates are read only by parallel backends.
func NativeStackEntry(fn *ir.Func) bool       { return len(nativeStackGates[fn]) != 0 }
func NativeStackGates(fn *ir.Func) []*ir.Name { return nativeStackGates[fn] }

func newNativeStackGate(source, factory *ir.Func) {
	typ := types.NewArray(types.Types[types.TUINTPTR], 2)
	types.CalcSize(typ)
	sym := source.Sym().Pkg.Lookup(source.Sym().Name + ".coro.stackgate")
	gate := ir.NewNameAt(source.Pos(), sym, typ)
	gate.Class = ir.PEXTERN
	sym.Def = gate
	lsym := gate.Linksym()
	objw.Uintptr(lsym, 0, 0)
	objw.SymPtr(lsym, types.PtrSize, factory.Linksym(), 0)
	flags := int16(obj.RODATA)
	if source.Dupok() {
		flags |= obj.DUPOK
	}
	objw.Global(lsym, int32(typ.Size()), flags)
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

// Only the actual resume and registered cleanup entries execute natively.
// Ordinary frontend go wrappers remain ordinary, even if their source parent
// was moved into a factory. Root-array implementations share the public gate.
func recordNativeStackMetadata(candidates map[*ir.Func]*lowerCandidate) {
	for source, candidate := range candidates {
		gates := []*ir.Name{nativeSourceGates[source]}
		for _, deferred := range candidate.defers {
			if closure, ok := deferred.call.Fun.(*ir.ClosureExpr); ok {
				recordNativeStackEntry(closure.Func, gates)
			} else if target := ir.StaticCalleeName(deferred.call.Fun); target != nil && target.Func != nil {
				recordNativeStackEntry(target.Func, gates)
			}
		}
	}
	// Shared cleanup entries may belong to several source factories. Stable
	// metadata order is required for reproducible compiler and package builds.
	for _, gates := range nativeStackGates {
		slices.SortFunc(gates, func(a, b *ir.Name) int { return strings.Compare(a.Linksym().Name, b.Linksym().Name) })
	}
}
