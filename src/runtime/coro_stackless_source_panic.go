// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro

package runtime

import (
	"internal/abi"
	"unsafe"
)

// A terminal task releases its typed frames before its panic reaches an
// ordinary stack. Keep only source positions, with the same inner/outer bound
// as text tracebacks. Neither this record nor its entries retain user objects.
type stacklessCoroPanicTrace struct {
	rootPC uintptr
	count  int
	frames [tracebackInnerFrames + tracebackOuterFrames]inlineFrame
}

type stacklessCoroPanicState struct {
	coroTrace *stacklessCoroPanicTrace
}

func (trace *stacklessCoroPanicTrace) append(f funcInfo, pc uintptr, gp *g) {
	for iu, uf := newInlineUnwinder(f, pc); uf.valid(); uf = iu.next(uf) {
		if !showframe(iu.srcFunc(uf), gp, false, abi.FuncIDNormal) {
			continue
		}
		trace.appendFrame(uf)
	}
}

func (trace *stacklessCoroPanicTrace) appendFrame(uf inlineFrame) {
	i := trace.count
	if i >= tracebackInnerFrames {
		i = tracebackInnerFrames + (i-tracebackInnerFrames)%tracebackOuterFrames
	}
	trace.frames[i] = uf
	trace.count++
}

func (trace *stacklessCoroPanicTrace) appendTrace(previous *stacklessCoroPanicTrace) {
	for i := 0; i < previous.count; i++ {
		// Missing middle entries cannot enter our first inner frames and
		// are overwritten by the retained outer frames before publication.
		uf, _ := previous.at(i)
		trace.appendFrame(uf)
	}
	trace.rootPC = previous.rootPC
}

func (trace *stacklessCoroPanicTrace) appendLineage(task *stacklessCoroTask, gp *g, leafEntry, leafPC uintptr) {
	for task != nil {
		s := task.context.scheduler
		if s != nil {
			if previous := s.terminalTrace(task); previous != nil {
				trace.appendTrace(previous)
				return
			}
		}
		if state := gp.stacklessCoro; state != nil && state.deferState != nil && state.deferState.task == task && state.deferState.trace != nil {
			trace.appendTrace(state.deferState.trace)
			return
		}
		pc, _ := walkStacklessCoroSourceTaskFrames(task, func(f funcInfo, pc uintptr) bool {
			if leafEntry != 0 {
				if f.entry() == leafEntry {
					pc = leafPC
				}
				leafEntry = 0
			}
			trace.append(f, pc, gp)
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
		task = task.parent
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
	trace := new(stacklessCoroPanicTrace)
	gp := getg()
	systemstack(func() {
		var leafEntry, leafPC uintptr
		if pc != 0 {
			var u unwinder
			for u.initAt(pc, uintptr(sp), 0, gp, 0); u.valid(); u.next() {
				f := u.frame.fn
				info := stacklessCoroSourceFrame(f)
				if f.funcID == abi.FuncID_gopanic {
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
				trace.append(f, at, gp)
			}
		}
		trace.appendLineage(task, gp, leafEntry, leafPC)
	})
	if trace.rootPC == 0 {
		return nil
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
	*result = *trace
	gp := getg()
	result.appendLineage(task, gp, 0, 0)
	return result
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

func initStacklessCoroPanicTrace(p *_panic, pc uintptr) {
	gp := getg()
	state := gp.stacklessCoro
	if p.goexit || state == nil || state.deferState == nil || !state.deferState.pending ||
		funcname(findfunc(pc)) != "runtime.raiseStacklessCoroPanic" {
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
