// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro

package runtime

import (
	"internal/abi"
	"unsafe"
)

// String references are relative to moduledata.rodata. Source entry cells
// use an index into a relocated module table: an external linker may change
// the distance between read-only strings and relro code-pointer cells.
// This metadata does not grant native-call capability.
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

type stacklessCoroP struct {
	sourcePCs stacklessCoroSourcePCCache
}

type stacklessCoroSourcePCCache struct {
	entries [8]stacklessCoroSourcePCCacheEntry
	inUse   bool
}

type stacklessCoroSourcePCCacheKey struct {
	entry uintptr
	file  uintptr
	size  int
	line  int32
}

type stacklessCoroSourcePCCacheEntry struct {
	key stacklessCoroSourcePCCacheKey
	pc  uintptr
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
		if uint64(info.source) >= uint64(len(f.datap.coroSourceEntries)) {
			return funcInfo{}
		}
		entry := *(*uintptr)(unsafe.Pointer(f.datap.coroSourceEntries[info.source]))
		return findfunc(entry)
	}
	return f
}

// Source entries retain their ordinary code and pclntab. The caller's file
// string always points into immutable linker metadata, so its address and
// length identify it for the lifetime of the cache without a GC reference.
func stacklessCoroSourcePC(source funcInfo, file string, line int32) uintptr {
	entry := source.entry()
	if file == "" || line <= 0 || source.pcfile == 0 || source.pcln == 0 {
		return entry
	}
	key := stacklessCoroSourcePCCacheKey{
		entry: entry,
		file:  uintptr(unsafe.Pointer(unsafe.StringData(file))),
		size:  len(file),
		line:  line,
	}
	slot := (entry>>4 ^ uintptr(line)) % uintptr(len(stacklessCoroSourcePCCache{}.entries))

	// Pin the current P during cache access. Its profiling signal can
	// interrupt a lookup or store, so use the same reentrancy rule as
	// pcvalue. Keep the scan preemptible.
	mp := acquirem()
	if pp := mp.p.ptr(); pp != nil {
		cache := &pp.sourcePCs
		if !cache.inUse {
			cache.inUse = true
			saved := cache.entries[slot]
			cache.inUse = false
			if saved.key == key {
				releasem(mp)
				return saved.pc
			}
		}
	}
	releasem(mp)

	pc := findStacklessCoroSourcePC(source, entry, file, line)
	mp = acquirem()
	if pp := mp.p.ptr(); pp != nil {
		cache := &pp.sourcePCs
		if !cache.inUse {
			cache.inUse = true
			cache.entries[slot] = stacklessCoroSourcePCCacheEntry{key: key, pc: pc}
			cache.inUse = false
		}
	}
	releasem(mp)
	return pc
}

// Match the ordinary instruction for a source file and line. This walk may
// run on the system or signal stack and does not allocate.
func findStacklessCoroSourcePC(source funcInfo, entry uintptr, file string, line int32) uintptr {
	// File and line tables encode half-open PC ranges. Advance the two
	// tables together so each candidate starts a new file or line range.
	// Looking up both values separately at each instruction decodes much
	// of the same data again, including during panic, spawn and profiling.
	pctab := source.datap.pctab
	files, lines := pctab[source.pcfile:], pctab[source.pcln:]
	fileEnd, lineEnd := entry, entry
	fileNo, lineNo := int32(-1), int32(-1)
	for pc := entry; ; pc = min(fileEnd, lineEnd) {
		var ok bool
		for fileEnd <= pc {
			files, ok = step(files, &fileEnd, &fileNo, fileEnd == entry)
			if !ok {
				return entry
			}
		}
		for lineEnd <= pc {
			lines, ok = step(lines, &lineEnd, &lineNo, lineEnd == entry)
			if !ok {
				return entry
			}
		}
		if lineNo == line && fileNo >= 0 && int(fileNo) < len(source.datap.filetab) && funcfile(source, fileNo) == file {
			// The PC tables can include bytes beyond the function. Use the
			// same boundary check as a forward stack lookup.
			if findfunc(pc) == source {
				return pc
			}
			return entry
		}
	}
}

func stacklessCoroResumeSourcePC(resume *funcval, frame unsafe.Pointer, leafEntry, leafPC uintptr) (funcInfo, uintptr) {
	f := findfunc(resume.fn)
	info := stacklessCoroSourceFrame(f)
	if info == nil || info.flags&abi.CoroSourceResume == 0 {
		return funcInfo{}, 0
	}
	source := stacklessCoroSourceFunc(f, info)
	if !source.valid() {
		return funcInfo{}, 0
	}
	if leafEntry != 0 && source.entry() == leafEntry {
		return source, leafPC
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
	return walkStacklessCoroSourceTaskFramesAt(task, 0, 0, visit)
}

// A physical panic cursor has the exact position of the first source
// activation. Use that position without decoding its saved resume state.
func walkStacklessCoroSourceTaskFramesAt(task *stacklessCoroTask, leafEntry, leafPC uintptr, visit func(funcInfo, uintptr) bool) (uintptr, bool) {
	var rootPC uintptr
	resume := stacklessCoroResumeIdentity(task.resume)
	frame := task.context.frame
	fused := task.hasFlag(stacklessCoroTaskFusedFrames)
	for resume != nil {
		f, pc := stacklessCoroResumeSourcePC(resume, frame, leafEntry, leafPC)
		if f.valid() {
			leafEntry = 0
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
	if host := stacklessCoroParkedSource(u.g.ptr()); host != nil {
		// The private wake receive is an implementation detail, not a source
		// operation. Locate its driver before consuming skip or buffer space,
		// so even a one-PC profile starts with the suspended source leaf.
		probe := *u
		for probe.valid() && funcname(probe.frame.fn) != "runtime.(*stacklessCoroHost).drive" {
			probe.next()
		}
		if probe.valid() {
			*u = probe
			if trace := host.parkedTrace; trace != nil {
				appendStacklessCoroPanicPCs(u, trace, &skip, buf, &n)
				rootPC = trace.rootPC
			} else {
				rootPC = appendStacklessCoroSourceFrames(u, host.parkedTask, &skip, buf, &n)
			}
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
			if funcname(f) == "runtime.coroRun" {
				// Closure-shaped public entries add this adapter between
				// coroRunFrame and the already-projected source root.
				continue
			}
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
		if info != nil && info.flags&abi.CoroSourceResume != 0 && source.valid() {
			if stacklessCoroActiveSourcePCs(u, source, &skip, buf, &n) {
				return n, true
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
