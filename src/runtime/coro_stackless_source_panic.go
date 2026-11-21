// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro

package runtime

import (
	"internal/abi"
	"internal/stringslite"
	"unsafe"
)

// Save the terminal boundary PCs at runtime initialization. Both fatal
// tracebacks and CPU sampling are reachable from the allocator. Referencing
// gopanic or Goexit directly on those paths would introduce a dependency
// cycle in the allocator's statically initialized dispatch tables.
var stacklessCoroPanicPC, stacklessCoroGoexitPC uintptr

func init() {
	stacklessCoroPanicPC = abi.FuncPCABIInternal(gopanic)
	stacklessCoroGoexitPC = abi.FuncPCABIInternal(Goexit)
}

// A terminal task releases its typed frames before its panic reaches an
// ordinary stack. Keep source positions with the same inner/outer text bound,
// and a complete return-PC payload for Callers. Neither payload retains user
// objects, task frames, receivers, or closure environments.
type stacklessCoroPanicTrace struct {
	rootPC       uintptr
	count        int
	frames       []inlineFrame
	goexit       bool
	calleeFuncID abi.FuncID
	// Callers can request positions beyond the bounded text window. Its
	// immutable payload contains return PCs, never user or frame pointers.
	pcs     []uintptr
	pcCount int
}

type stacklessCoroPanicText [tracebackInnerFrames + tracebackOuterFrames]inlineFrame

// Keep both views in a single small object for shallow panics. In particular,
// keep the walk's buffers off the unwinding goroutine's stack.
type stacklessCoroShortPanicTrace struct {
	trace  stacklessCoroPanicTrace
	frames [8]inlineFrame
	pcs    [8]uintptr
}

type stacklessCoroShallowPanicTrace struct {
	trace  stacklessCoroPanicTrace
	frames [2]inlineFrame
	pcs    [2]uintptr
}

type stacklessCoroResumePanicCacheEntry struct {
	leaf, parent stacklessCoroPanicSite
	trace        *stacklessCoroPanicTrace
}

type stacklessCoroPanicSite struct {
	entry uintptr
	info  *stacklessCoroSourceInfo
	state uint32
}

func (site stacklessCoroPanicSite) matches(resume *funcval, frame unsafe.Pointer) bool {
	if resume == nil {
		return site.entry == 0
	}
	return site.entry == resume.fn && stacklessCoroSavedSourceState(resume, frame, site.info) == site.state
}

func stacklessCoroPanicSiteFor(resume *funcval, frame unsafe.Pointer) (stacklessCoroPanicSite, bool) {
	if resume == nil {
		return stacklessCoroPanicSite{}, true
	}
	info := stacklessCoroSourceFrame(findfunc(resume.fn))
	if info == nil || info.flags&abi.CoroSourceResume == 0 {
		return stacklessCoroPanicSite{}, false
	}
	return stacklessCoroPanicSite{resume.fn, info, stacklessCoroSavedSourceState(resume, frame, info)}, true
}

type stacklessCoroPanicState struct {
	coroTrace *stacklessCoroPanicTrace
}

func (trace *stacklessCoroPanicTrace) append(f funcInfo, pc uintptr) {
	for iu, uf := newInlineUnwinder(f, pc); uf.valid(); uf = iu.next(uf) {
		sf := iu.srcFunc(uf)
		callee := trace.calleeFuncID
		trace.calleeFuncID = sf.funcID
		name := sf.name()
		bridge := stringslite.HasPrefix(name, "runtime.stacklessCoro") ||
			stringslite.HasPrefix(name, "runtime.(*stacklessCoro") ||
			stringslite.HasPrefix(name, "runtime.raiseStacklessCoro") ||
			stringslite.HasPrefix(name, "runtime.coro") && len(name) > len("runtime.coro") &&
				name[len("runtime.coro")] >= 'A' && name[len("runtime.coro")] <= 'Z'
		if !bridge && !(sf.funcID == abi.FuncIDWrapper && elideWrapperCalling(callee)) &&
			!(sf.funcID == abi.FuncID_gopanic && trace.pcCount == 0) {
			trace.appendPC(uf.pc + 1)
		}
		if !showfuncinfoUser(sf, trace.count == 0, callee) {
			continue
		}
		trace.appendFrame(uf)
	}
}

func (trace *stacklessCoroPanicTrace) appendPC(pc uintptr) {
	if trace.pcCount < len(trace.pcs) {
		trace.pcs[trace.pcCount] = pc
	}
	trace.pcCount++
}

func (trace *stacklessCoroPanicTrace) appendFrame(uf inlineFrame) {
	i := trace.count
	if i >= tracebackInnerFrames {
		i = tracebackInnerFrames + (i-tracebackInnerFrames)%tracebackOuterFrames
	}
	if i < len(trace.frames) {
		trace.frames[i] = uf
	}
	trace.count++
}

func (trace *stacklessCoroPanicTrace) appendTrace(previous *stacklessCoroPanicTrace) {
	boundary := stacklessCoroPanicPC
	if previous.goexit {
		boundary = stacklessCoroGoexitPC
	}
	if trace.pcCount != 0 {
		trace.appendPC(boundary + 1)
	}
	for _, pc := range previous.pcs {
		trace.appendPC(pc)
	}
	if trace.count != 0 {
		// Ordinary tracebacks expose the panic boundary between deferred
		// code and the original chain. It also counts toward 50/50 elision.
		trace.appendFrame(inlineFrame{pc: boundary, index: -1})
	}
	for i := 0; i < previous.count; i++ {
		// Missing middle entries cannot enter our first inner frames and
		// are overwritten by the retained outer frames before publication.
		uf, _ := previous.at(i)
		trace.appendFrame(uf)
	}
	trace.rootPC = previous.rootPC
	trace.calleeFuncID = previous.calleeFuncID
}

func (trace *stacklessCoroPanicTrace) appendLineage(task *stacklessCoroTask, gp *g, leafEntry, leafPC uintptr) {
	for task != nil {
		s := task.context.scheduler
		if s != nil {
			if previous := s.terminalSourceTrace(task); previous != nil {
				trace.appendTrace(previous)
				return
			}
		}
		if state := gp.stacklessCoro; state != nil && state.deferState != nil && state.deferState.task == task && state.deferState.trace != nil {
			trace.appendTrace(state.deferState.trace)
			return
		}
		pc, _ := walkStacklessCoroSourceTaskFramesAt(task, leafEntry, leafPC, func(f funcInfo, pc uintptr) bool {
			leafEntry = 0
			trace.append(f, pc)
			return true
		})
		if pc != 0 {
			trace.rootPC = pc
		}
		if s != nil {
			if previous := s.deferTrace(task); previous != nil {
				trace.appendTrace(previous)
				return
			}
		}
		task = task.parent()
	}
}

func (trace *stacklessCoroPanicTrace) at(i int) (inlineFrame, bool) {
	if i >= tracebackInnerFrames {
		if i < trace.count-tracebackOuterFrames {
			return inlineFrame{}, false
		}
		i = tracebackInnerFrames + (i-tracebackInnerFrames)%tracebackOuterFrames
	}
	return trace.frames[i], true
}

// The compiler saves a state with the exact panic position before calling
// coroPanic. That state identifies the active source frame directly.
func captureStacklessCoroResumePanicTrace(task *stacklessCoroTask) *stacklessCoroPanicTrace {
	gp := getg()
	parent := task.parent()
	shallow := !task.hasFlag(stacklessCoroTaskFusedFrames) &&
		(parent == nil || parent.parent() == nil && !parent.hasFlag(stacklessCoroTaskFusedFrames))
	if task.hasFlag(stacklessCoroTaskFusedFrames) && parent == nil && task.context.frame != nil {
		header := (*stacklessCoroFusedFrameHeader)(task.context.frame)
		// The active leaf switches resume entries directly above a root.
		shallow = header.marker&(stacklessCoroFusedFrameResume|stacklessCoroFusedFrameParent) ==
			stacklessCoroFusedFrameResume
	}
	if shallow && parent == nil && !task.context.scheduler.hasTerminalRecords() &&
		(gp.stacklessCoro == nil || gp.stacklessCoro.deferState == nil) {
		if trace := cachedStacklessCoroResumePanicTrace(task); trace != nil {
			return trace
		}
	}
	return collectStacklessCoroPanicTrace(shallow, func(trace *stacklessCoroPanicTrace) {
		trace.appendLineage(task, gp, 0, 0)
	})
}

// A shallow chain without a previous terminal trace is determined by its
// resume entries and saved states. Cache snapshots, never activation pointers.
// The scheduler lock pins the P and signal handlers do not fill this cache.
func cachedStacklessCoroResumePanicTrace(task *stacklessCoroTask) *stacklessCoroPanicTrace {
	pp, resume := getg().m.p.ptr(), stacklessCoroResumeIdentity(task.resume)
	if pp == nil || resume == nil {
		return nil
	}
	frame := task.context.frame
	var parent *funcval
	var parentFrame unsafe.Pointer
	if task.hasFlag(stacklessCoroTaskFusedFrames) && frame != nil {
		header := (*stacklessCoroFusedResumeFrameHeader)(frame)
		if header.parent != nil {
			parent, parentFrame = header.resume, header.parent
		}
	}
	slot := &pp.panicTraces[(resume.fn>>4)%uintptr(len(pp.panicTraces))]
	if slot.trace != nil && slot.leaf.matches(resume, frame) && slot.parent.matches(parent, parentFrame) {
		return slot.trace
	}
	leafSite, ok := stacklessCoroPanicSiteFor(resume, frame)
	if !ok {
		return nil
	}
	parentSite, ok := stacklessCoroPanicSiteFor(parent, parentFrame)
	if !ok {
		return nil
	}
	trace := collectStacklessCoroPanicTrace(true, func(trace *stacklessCoroPanicTrace) {
		trace.appendLineage(task, getg(), 0, 0)
	})
	*slot = stacklessCoroResumePanicCacheEntry{leaf: leafSite, parent: parentSite, trace: trace}
	return trace
}

// The resuming task owns its complete structured lineage. An operation panic
// instead holds its scheduler lock and passes no physical cursor. Allocate
// before walking, and walk on g0 so stack copying cannot invalidate u.
func captureStacklessCoroPanicTrace(task *stacklessCoroTask, p *_panic, pc uintptr, sp unsafe.Pointer) *stacklessCoroPanicTrace {
	if p != nil && p.coroTrace != nil {
		return p.coroTrace
	}
	if p != nil {
		pc, sp = p.startPC, p.startSP
	}
	gp := getg()
	return collectStacklessCoroPanicTrace(false, func(trace *stacklessCoroPanicTrace) {
		systemstack(func() {
			var leafEntry, leafPC uintptr
			if pc != 0 {
				var u unwinder
				for u.initAt(pc, uintptr(sp), 0, gp, 0); u.valid(); u.next() {
					f := u.frame.fn
					info := stacklessCoroSourceFrame(f)
					if p != nil && p.goexit && uintptr(p.startSP) == u.frame.sp {
						continue
					}
					if f.funcID == abi.FuncID_gopanic || funcname(f) == "runtime.Goexit" {
						for previous := gp._panic; previous != nil; previous = previous.link {
							if previous.coroTrace != nil && uintptr(previous.startSP) == u.frame.sp {
								trace.appendTrace(previous.coroTrace)
								return
							}
						}
					}
					if info != nil && info.flags&(abi.CoroSourceBridge|abi.CoroSourceResume) != 0 {
						if info.flags&abi.CoroSourceResume != 0 {
							source := stacklessCoroSourceFunc(f, info)
							if source.valid() {
								file, line := funcline1(f, u.symPC(), false)
								leafEntry, leafPC = source.entry(), stacklessCoroSourcePC(source, file, line)
							}
						}
						break
					}
					at := u.symPC()
					if info != nil && info.flags&abi.CoroSourceCopy != 0 {
						source := stacklessCoroSourceFunc(f, info)
						if source.valid() {
							file, line := funcline1(f, at, false)
							f, at = source, stacklessCoroSourcePC(source, file, line)
						}
					}
					trace.append(f, at)
				}
			}
			trace.appendLineage(task, gp, leafEntry, leafPC)
		})
	})
}

func collectStacklessCoroPanicTrace(shallow bool, walk func(*stacklessCoroPanicTrace)) *stacklessCoroPanicTrace {
	var trace *stacklessCoroPanicTrace
	if shallow {
		short := new(stacklessCoroShallowPanicTrace)
		trace = &short.trace
		trace.frames, trace.pcs = short.frames[:], short.pcs[:]
	} else {
		short := new(stacklessCoroShortPanicTrace)
		trace = &short.trace
		trace.frames, trace.pcs = short.frames[:], short.pcs[:]
	}
	for pass := 0; pass < 2; pass++ {
		walk(trace)
		if trace.rootPC == 0 {
			return nil
		}
		if pass == 0 && trace.pcCount <= len(trace.pcs) && trace.count <= len(trace.frames) {
			trace.pcs = trace.pcs[:trace.pcCount]
			trace.frames = trace.frames[:trace.count]
			return trace
		}
		if pass == 0 {
			// The first walk also counted frames and PCs it could not
			// retain. Reserve the full payload before walking again.
			trace = &stacklessCoroPanicTrace{
				pcs:    make([]uintptr, trace.pcCount),
				frames: make([]inlineFrame, min(trace.count, len(stacklessCoroPanicText{}))),
			}
		}
	}
	if trace.pcCount != len(trace.pcs) || min(trace.count, len(stacklessCoroPanicText{})) != len(trace.frames) {
		throw("runtime: incomplete coroutine source PC capture")
	}
	return trace
}

// A synchronous deferred scheduler has no structured parent link. Append its
// caller when transferring the outcome; never mutate a published panic trace.
func extendStacklessCoroPanicTrace(trace *stacklessCoroPanicTrace, task *stacklessCoroTask) *stacklessCoroPanicTrace {
	if trace == nil {
		return captureStacklessCoroPanicTrace(task, nil, 0, nil)
	}
	result := new(stacklessCoroPanicTrace)
	frames := new(stacklessCoroPanicText)
	*result = *trace
	result.frames, result.pcs = frames[:], nil
	copy(result.frames, trace.frames)
	gp := getg()
	result.appendLineage(task, gp, 0, 0)
	pcs := make([]uintptr, result.pcCount)
	copy(pcs, trace.pcs)
	*result = *trace
	result.frames, result.pcs = frames[:], pcs
	copy(result.frames, trace.frames)
	result.appendLineage(task, gp, 0, 0)
	if result.pcCount != len(result.pcs) {
		throw("runtime: incomplete extended coroutine source PC capture")
	}
	return result
}

func appendStacklessCoroPanicPCs(u *unwinder, trace *stacklessCoroPanicTrace, skip *int, buf []uintptr, n *int) {
	for _, pc := range trace.pcs {
		if *n == len(buf) {
			return
		}
		if *skip > 0 {
			*skip--
		} else {
			buf[*n] = pc
			*n++
		}
	}
	u.calleeFuncID = trace.calleeFuncID
}

// The pending record is consumed only by this immediate panic call. The
// ordinary _panic then owns it, so recovery and stack copying use the normal
// panic lifetime without a side table or a retained task/frame pointer.
//
//go:noinline
func raiseStacklessCoroPanic(value any, trace *stacklessCoroPanicTrace) {
	if trace == nil {
		panic(value)
	}
	gp := getg()
	state := gp.stacklessCoro
	temporary := state == nil
	if temporary {
		state = new(stacklessCoroGState)
		gp.stacklessCoro = state
	}
	if state.deferState != nil && state.deferState.pending {
		throw("runtime: duplicate pending coroutine panic trace")
	}
	pending := &stacklessCoroDeferState{trace: trace, previous: state.deferState, pending: true, temporary: temporary}
	state.deferState = pending
	defer func() {
		if state.deferState == pending {
			state.deferState = pending.previous
			pending.previous, pending.trace = nil, nil
			if temporary {
				gp.stacklessCoro = nil
			}
		}
	}()
	panic(value)
}

//go:noinline
func raiseStacklessCoroGoexit(trace *stacklessCoroPanicTrace) {
	if trace == nil {
		Goexit()
	}
	gp := getg()
	state := gp.stacklessCoro
	temporary := state == nil
	if temporary {
		state = new(stacklessCoroGState)
		gp.stacklessCoro = state
	}
	if state.deferState != nil && state.deferState.pending {
		throw("runtime: duplicate pending coroutine Goexit trace")
	}
	pending := &stacklessCoroDeferState{trace: trace, previous: state.deferState, pending: true, temporary: temporary}
	state.deferState = pending
	defer func() {
		if state.deferState == pending {
			state.deferState = pending.previous
			pending.previous, pending.trace = nil, nil
			if temporary {
				gp.stacklessCoro = nil
			}
		}
	}()
	Goexit()
}

func initStacklessCoroPanicTrace(p *_panic, pc uintptr) {
	gp := getg()
	state := gp.stacklessCoro
	if p.deferreturn || state == nil || state.deferState == nil {
		return
	}
	if !state.deferState.pending {
		// An implicit panic can unwind the trace scope before runTasks
		// catches it. Capture while a recovered prior chain is still owned
		// by this deferred activation.
		if task := state.deferState.task; task != nil && state.deferState.trace != nil {
			s := task.context.scheduler
			lock(&s.lock)
			p.coroTrace = captureStacklessCoroPanicTrace(task, p, p.startPC, p.startSP)
			if p.coroTrace != nil {
				p.coroTrace.goexit = p.goexit
			}
			unlock(&s.lock)
		}
		return
	}
	caller := "runtime.raiseStacklessCoroPanic"
	if p.goexit {
		caller = "runtime.raiseStacklessCoroGoexit"
	}
	if funcname(findfunc(pc)) != caller {
		return
	}
	pending := state.deferState
	p.coroTrace = pending.trace
	state.deferState = pending.previous
	pending.trace, pending.previous = nil, nil
	if pending.temporary {
		gp.stacklessCoro = nil
	}
}

func printStacklessCoroPanicTrace(u *unwinder, trace *stacklessCoroPanicTrace, commit func() (bool, bool)) bool {
	for i := 0; i < trace.count; i++ {
		pr, stop := commit()
		if stop {
			return false
		}
		if !pr {
			continue
		}
		uf, ok := trace.at(i)
		if !ok {
			print("... source frame omitted ...\n")
			continue
		}
		f := findfunc(uf.pc)
		iu, _ := newInlineUnwinder(f, uf.pc)
		sf := iu.srcFunc(uf)
		u.calleeFuncID = sf.funcID
		name := sf.name()
		if !iu.isInlined(uf) {
			if source := stacklessCoroSourceName(f); source != "" {
				name = source
			}
		}
		file, line := iu.fileLine(uf)
		printFuncName(name)
		print("(...)\n\t", file, ":", line, "\n")
	}
	return true
}
