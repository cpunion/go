// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ssagen

import (
	"cmd/compile/internal/base"
	"cmd/compile/internal/coro"
	"cmd/compile/internal/ir"
	"cmd/compile/internal/objw"
	"cmd/internal/obj"
	"internal/abi"
	"internal/buildcfg"
)

func emitCoroStackInfo(fn *ir.Func, pp *objw.Progs) {
	if !buildcfg.Experiment.Coro || !coro.NativeStackEntry(fn) {
		return
	}
	fn.LSym.Func().FuncFlag |= abi.FuncFlagCoroStack
	// Funcdata permits section-relative R_ADDROFF references, not pointers.
	// The linker consumes the exact gate identities before laying out funcdata.
	info := base.Ctxt.Lookup(fn.LSym.Name + ".coro.stackinfo")
	off := 0
	for _, gate := range coro.NativeStackGates(fn) {
		// Each pair binds one native activation to its owning source gate.
		off = objw.SymPtrOff(info, off, gate.Linksym())
		off = objw.SymPtrOff(info, off, fn.LSym)
		for _, callee := range coro.NativeStackCallees(fn) {
			off = objw.SymPtrOff(info, off, gate.Linksym())
			off = objw.SymPtrOff(info, off, callee.Linksym())
		}
	}
	fn.LSym.Func().CoroStackInfo = info
	info.Align = 4
	info.Set(obj.AttrContentAddressable, true)
	p := pp.Prog(obj.AFUNCDATA)
	p.From.SetConst(abi.FUNCDATA_CoroStackGates)
	p.To.Type = obj.TYPE_MEM
	p.To.Name = obj.NAME_EXTERN
	p.To.Sym = info
}
