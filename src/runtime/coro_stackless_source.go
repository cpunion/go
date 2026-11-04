// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro

package runtime

import (
	"internal/abi"
	"internal/goarch"
	"unsafe"
)

// Every reference is relative to moduledata.rodata, including entry cells
// that the linker places in relro. This works in PIE and without DWARF and
// does not grant native-call capability.
type stacklessCoroSourceInfo struct {
	version, flags, source   uint32
	pcKind, pcOffset, pcSize uint32
	positions                uint32
	name, nameSize           uint32
}

func stacklessCoroSourceName(f funcInfo) string {
	info := stacklessCoroSourceFrame(f)
	if info == nil || info.flags&abi.CoroSourceCopy == 0 || info.nameSize == 0 {
		return ""
	}
	header := stringStruct{str: unsafe.Pointer(f.datap.rodata + uintptr(info.name)), len: int(info.nameSize)}
	return *(*string)(unsafe.Pointer(&header))
}

type stacklessCoroSourcePosition struct {
	file, fileSize, line uint32
}

func stacklessCoroSourceFrame(f funcInfo) *stacklessCoroSourceInfo {
	if !f.valid() {
		return nil
	}
	info := (*stacklessCoroSourceInfo)(funcdata(f, abi.FUNCDATA_CoroSource))
	if info == nil || info.version != abi.CoroSourceVersion {
		return nil
	}
	return info
}

func stacklessCoroSourceFunc(f funcInfo, info *stacklessCoroSourceInfo) funcInfo {
	if info != nil && info.flags&(abi.CoroSourceResume|abi.CoroSourceCopy) != 0 {
		entry := *(*uintptr)(unsafe.Pointer(f.datap.rodata + uintptr(info.source)))
		return findfunc(entry)
	}
	return f
}

// Source entries retain their ordinary code and pclntab. Select the original
// instruction for this file/line rather than reporting a private helper PC.
// This cold path neither allocates nor calls user code on the system stack.
func stacklessCoroSourcePC(source funcInfo, file string, line int32) uintptr {
	entry := source.entry()
	if file == "" || line <= 0 {
		return entry
	}
	for pc := entry; ; pc += goarch.PCQuantum {
		f := findfunc(pc)
		if !f.valid() || f.entry() != entry {
			break
		}
		atFile, atLine := funcline1(source, pc, false)
		if atLine == line && atFile == file {
			return pc
		}
	}
	return entry
}

func stacklessCoroResumeSourcePC(resume *funcval, frame unsafe.Pointer) (funcInfo, uintptr) {
	f := findfunc(resume.fn)
	info := stacklessCoroSourceFrame(f)
	if info == nil || info.flags&abi.CoroSourceResume == 0 {
		return funcInfo{}, 0
	}
	source := stacklessCoroSourceFunc(f, info)
	if !source.valid() {
		return funcInfo{}, 0
	}
	var cell unsafe.Pointer
	switch info.pcKind {
	case abi.CoroSourcePCFrame:
		if frame != nil {
			cell = add(frame, uintptr(info.pcOffset))
		}
	case abi.CoroSourcePCClosure, abi.CoroSourcePCClosureIndirect:
		cell = add(unsafe.Pointer(resume), uintptr(info.pcOffset))
		if info.pcKind == abi.CoroSourcePCClosureIndirect {
			cell = *(*unsafe.Pointer)(cell)
		}
	}
	if cell == nil {
		return source, source.entry()
	}
	var state uint32
	switch info.pcSize {
	case 1:
		state = uint32(*(*uint8)(cell))
	case 2:
		state = uint32(*(*uint16)(cell))
	case 4:
		state = readUnaligned32(cell)
	default:
		return source, source.entry()
	}
	if state >= info.positions {
		return source, source.entry()
	}
	at := (*stacklessCoroSourcePosition)(add(unsafe.Pointer(info),
		unsafe.Sizeof(*info)+uintptr(state)*unsafe.Sizeof(stacklessCoroSourcePosition{})))
	file := ""
	if at.fileSize != 0 {
		// The linker owns this immutable range. Avoid unsafe.String's panic
		// path: the profiler also reaches this walk without write barriers.
		header := stringStruct{str: unsafe.Pointer(f.datap.rodata + uintptr(at.file)), len: int(at.fileSize)}
		file = *(*string)(unsafe.Pointer(&header))
	}
	return source, stacklessCoroSourcePC(source, file, int32(at.line))
}

func appendStacklessCoroSourcePC(u *unwinder, f funcInfo, pc uintptr, skip *int, buf []uintptr, n *int) {
	for iu, uf := newInlineUnwinder(f, pc); *n < len(buf) && uf.valid(); uf = iu.next(uf) {
		sf := iu.srcFunc(uf)
		if sf.funcID == abi.FuncIDWrapper && elideWrapperCalling(u.calleeFuncID) {
			// Match the ordinary unwinder's logical wrapper elision.
		} else if *skip > 0 {
			*skip--
		} else {
			buf[*n] = uf.pc + 1
			*n++
		}
		u.calleeFuncID = sf.funcID
	}
}

// The stopped host owns this active leaf. Its structured parents cannot run
// until the opaque call returns, so both ordinary task links and fused frame
// links are stable throughout the unwind. Independent go tasks have no parent.
func appendStacklessCoroSourceFrames(u *unwinder, task *stacklessCoroTask, skip *int, buf []uintptr, n *int) uintptr {
	rootPC, _ := walkStacklessCoroSourceFrames(task, func(f funcInfo, pc uintptr) bool {
		appendStacklessCoroSourcePC(u, f, pc, skip, buf, n)
		return *n < len(buf)
	})
	return rootPC
}

// walkStacklessCoroSourceFrames visits complete source activations until visit
// returns false. Both PC collection and text tracebacks use this same chain.
func walkStacklessCoroSourceFrames(task *stacklessCoroTask, visit func(funcInfo, uintptr) bool) (uintptr, bool) {
	var rootPC uintptr
	for task != nil {
		pc, complete := walkStacklessCoroSourceTaskFrames(task, visit)
		if pc != 0 {
			rootPC = pc
		}
		if !complete {
			return rootPC, false
		}
		task = task.parent
	}
	return rootPC, true
}

func walkStacklessCoroSourceTaskFrames(task *stacklessCoroTask, visit func(funcInfo, uintptr) bool) (uintptr, bool) {
	var rootPC uintptr
	resume := stacklessCoroResumeIdentity(task.resume)
	frame := task.context.frame
	fused := task.hasFlag(stacklessCoroTaskFusedFrames)
	for resume != nil {
		f, pc := stacklessCoroResumeSourcePC(resume, frame)
		if f.valid() {
			rootPC = f.entry()
			if !visit(f, pc) {
				return rootPC, false
			}
		}
		if !fused || frame == nil {
			break
		}
		header := (*stacklessCoroFusedFrameHeader)(frame)
		if header.parent == nil {
			break
		}
		if header.marker&stacklessCoroFusedFrameResume != 0 {
			resume = (*stacklessCoroFusedResumeFrameHeader)(frame).resume
		}
		frame = header.parent
		// Self-await uses the same resume and gives every activation a
		// header, including the root. Only a heterogeneous resume switch
		// can point to a plain root without a header; its Parent bit says
		// whether a further fused edge is present.
		fused = header.marker&stacklessCoroFusedFrameResume == 0 ||
			header.marker&stacklessCoroFusedFrameParent != 0
	}
	return rootPC, true
}

func stacklessCoroTracebackPCs(u *unwinder, skip int, buf []uintptr) (int, bool) {
	if !u.valid() {
		return 0, true
	}
	var cgoBuf [32]uintptr
	n := 0
	bridges, afterDriver, terminal := false, false, false
	var rootPC uintptr
	if task := stacklessCoroParkedSourceTask(u.g.ptr()); task != nil {
		// The private wake receive is an implementation detail, not a source
		// operation. Locate its driver before consuming skip or buffer space,
		// so even a one-PC profile starts with the suspended source leaf.
		probe := *u
		for probe.valid() && funcname(probe.frame.fn) != "runtime.(*stacklessCoroHost).drive" {
			probe.next()
		}
		if probe.valid() {
			*u = probe
			rootPC = appendStacklessCoroSourceFrames(u, task, &skip, buf, &n)
			bridges = true
		}
	}
	for ; n < len(buf) && u.valid(); u.next() {
		f := u.frame.fn
		info := stacklessCoroSourceFrame(f)
		source := stacklessCoroSourceFunc(f, info)
		if bridges {
			if rootPC != 0 && source.valid() && source.entry() == rootPC {
				// The source root is already in the logical chain. Its
				// physical public entry contributes no duplicate frame.
				bridges = false
				terminal = false
				continue
			}
			switch funcname(f) {
			case "runtime.coroRunFrame":
				if terminal {
					continue
				}
				bridges, afterDriver = false, true
				continue
			case "runtime.goexit":
				bridges = false
			default:
				continue
			}
		}
		if afterDriver {
			afterDriver = false
			if source.valid() && source.entry() == rootPC {
				continue
			}
		}
		if f.funcID == abi.FuncID_gopanic || funcname(f) == "runtime.Goexit" {
			var trace *stacklessCoroPanicTrace
			for p := u.g.ptr()._panic; p != nil; p = p.link {
				if p.coroTrace != nil && uintptr(p.startSP) == u.frame.sp {
					trace = p.coroTrace
					break
				}
			}
			if trace != nil {
				appendStacklessCoroSourcePC(u, f, u.symPC(), &skip, buf, &n)
				appendStacklessCoroPanicPCs(u, trace, &skip, buf, &n)
				rootPC, bridges, terminal = trace.rootPC, true, true
				continue
			}
		}
		if info != nil && info.flags&abi.CoroSourceBridge != 0 {
			// Profiling may start on g0 and cross morestack or systemstack
			// before reaching this bridge. Resolve its owner from the
			// current unwind cursor, not the goroutine at the first PC.
			state := stacklessCoroHostState(u.g.ptr())
			if state != nil && state.ordinaryDepth != 0 && state.activeTask != nil {
				rootPC = appendStacklessCoroSourceFrames(u, state.activeTask, &skip, buf, &n)
				bridges = true
				continue
			}
		}
		cgoN := u.cgoCallers(cgoBuf[:])
		pc := u.symPC()
		if info != nil && info.flags&abi.CoroSourceCopy != 0 && source.valid() {
			file, line := funcline1(f, pc, false)
			pc = stacklessCoroSourcePC(source, file, line)
			f = source
		}
		appendStacklessCoroSourcePC(u, f, pc, &skip, buf, &n)
		if skip == 0 {
			n += copy(buf[n:], cgoBuf[:cgoN])
		}
	}
	return n, true
}
