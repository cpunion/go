// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ssagen

import (
	"cmd/compile/internal/base"
	"cmd/compile/internal/coro"
	"cmd/compile/internal/ir"
	"cmd/compile/internal/objw"
	"cmd/compile/internal/typecheck"
	"cmd/internal/obj"
	"internal/abi"
	"internal/buildcfg"
)

func emitCoroStackInfo(fn *ir.Func, pp *objw.Progs) {
	if !buildcfg.Experiment.Coro {
		return
	}
	if frame := coro.SourceFrameInfo(fn); frame != nil {
		info := base.Ctxt.Lookup(fn.LSym.Name + ".coro.sourceinfo")
		objw.Uint32(info, 0, abi.CoroSourceVersion)
		objw.Uint32(info, 4, frame.Flags)
		if frame.Source != nil {
			// A source copy can refer into another Go module. Funcdata only
			// permits section offsets, so reference a local ordinary RODATA
			// cell whose code pointer also supports dynamic-import relocations.
			entry := base.Ctxt.Lookup(fn.LSym.Name + ".coro.sourceentry")
			if !entry.OnList() {
				objw.SymPtr(entry, 0, frame.Source, 0)
				objw.Global(entry, int32(base.Ctxt.Arch.PtrSize), obj.RODATA|obj.DUPOK)
			}
			objw.SymPtrOff(info, 8, entry)
		} else {
			objw.Uint32(info, 8, 0)
		}
		kind, offset, width := uint32(abi.CoroSourcePCNone), uint32(0), uint32(0)
		if frame.Field != nil {
			kind = abi.CoroSourcePCFrame
			offset, width = uint32(frame.Field.Offset), uint32(frame.Field.Type.Size())
		} else if frame.PC != nil {
			it := typecheck.NewClosureStructIter(fn.ClosureVars)
			for {
				name, _, off := it.Next()
				if name == nil {
					break
				}
				if name == frame.PC {
					kind = abi.CoroSourcePCClosureIndirect
					if name.Byval() {
						kind = abi.CoroSourcePCClosure
					}
					offset, width = uint32(off), uint32(name.Type().Size())
					break
				}
			}
		}
		objw.Uint32(info, 12, kind)
		objw.Uint32(info, 16, offset)
		objw.Uint32(info, 20, width)
		objw.Uint32(info, 24, uint32(len(frame.Positions)))
		off := 28
		for _, pos := range frame.Positions {
			if pos.File != nil {
				objw.SymPtrOff(info, off, pos.File)
			} else {
				objw.Uint32(info, off, 0)
			}
			objw.Uint32(info, off+4, pos.FileSize)
			objw.Uint32(info, off+8, pos.Line)
			off += 12
		}
		info.Align = 4
		info.Set(obj.AttrContentAddressable, true)
		fn.LSym.Func().CoroSourceInfo = info
		p := pp.Prog(obj.AFUNCDATA)
		p.From.SetConst(abi.FUNCDATA_CoroSource)
		p.To.Type = obj.TYPE_MEM
		p.To.Name = obj.NAME_EXTERN
		p.To.Sym = info
	}
	if provider := coro.NativeCallableProvider(fn); provider != nil {
		info := base.Ctxt.Lookup(fn.LSym.Name + ".coro.callinfo")
		objw.Uint32(info, 0, abi.CoroCallableVersion)
		objw.SymPtrOff(info, 4, provider.Linksym())
		info.Align = 4
		info.Set(obj.AttrContentAddressable, true)
		fn.LSym.Func().CoroCallableInfo = info
		p := pp.Prog(obj.AFUNCDATA)
		p.From.SetConst(abi.FUNCDATA_CoroCallable)
		p.To.Type = obj.TYPE_MEM
		p.To.Name = obj.NAME_EXTERN
		p.To.Sym = info
	}
	if !coro.NativeStackEntry(fn) {
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
