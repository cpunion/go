// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro

package runtime

import "unsafe"

type stacklessCoroGState struct {
	native            unsafe.Pointer
	deferTask         *stacklessCoroTask
	blockingScheduler *stacklessCoroScheduler
}

type stacklessCoroG struct {
	stacklessCoro *stacklessCoroGState
}

const stacklessCoroChannelWaiterScanning = true

type stacklessCoroSudog struct {
	owner unsafe.Pointer
}

func (s *stacklessCoroSudog) get() unsafe.Pointer {
	return s.owner
}

func (s *stacklessCoroSudog) set(owner unsafe.Pointer) {
	if owner == nil || s.owner != nil {
		throw("runtime: invalid stackless coroutine channel waiter owner")
	}
	s.owner = owner
}

func (s *stacklessCoroSudog) clear() {
	s.owner = nil
}

func (gp *g) stackIsFixed() bool {
	return gp.stacklessCoro != nil && gp.stacklessCoro.native != nil
}

func stacklessCoroRecover(gp *g) any {
	state := gp.stacklessCoro
	if state == nil || state.deferTask == nil {
		return nil
	}
	return coroDeferRecover(unsafe.Pointer(state.deferTask))
}

// stacklessCoroExitsyscallNoP publishes a native executor that could not
// acquire a P on its return from a foreign call.
//
//go:nosplit
func stacklessCoroExitsyscallNoP(gp *g) bool {
	scheduler := stacklessCoroNativeSchedulerFor(gp)
	if scheduler == nil {
		return false
	}
	if scheduler.foreignReturners.Add(1) == 1 {
		scheduler.runnableState.Or(stacklessCoroForeignReturnerBit)
	}
	return true
}

//go:nosplit
func stacklessCoroExitsyscallDone(gp *g) {
	scheduler := stacklessCoroNativeSchedulerFor(gp)
	if scheduler.foreignReturners.Add(-1) != 0 {
		return
	}
	scheduler.runnableState.And(^stacklessCoroForeignReturnerBit)
	if scheduler.foreignReturners.Load() != 0 {
		// A returner may have arrived between the decrement and the clear.
		scheduler.runnableState.Or(stacklessCoroForeignReturnerBit)
	}
}

// Channels use only this view. Full-only fields are never accessed through a
// compact allocation. Full waiters keep the same flat prefix for semaphore
// trees, goroutine stack adjustment, and DWARF substitution.
type channelWaiter = stacklessCoroChannelWaiter

type stacklessCoroChannelWaiter struct {
	coro stacklessCoroSudog
	next *channelWaiter
	prev *channelWaiter
	elem unsafe.Pointer
	c    *hchan
}

//go:nosplit
func (sg *sudog) channelWaiter() *channelWaiter {
	return (*channelWaiter)(unsafe.Pointer(sg))
}

// fullSudog is used only for an ordinary queued G or a separately allocated
// full native waiter. Queued compact owners are rejected. A cleared compact
// waiter is inspected only with its explicit operation in validReleased.
//
//go:nosplit
func (sg *channelWaiter) fullSudog() *sudog {
	if sg.compactOwner() != nil {
		throw("runtime: full view of compact channel waiter")
	}
	return (*sudog)(unsafe.Pointer(sg))
}

//go:nosplit
func (sg *channelWaiter) compactOwner() *stacklessCoroOperation {
	owner := sg.coro.get()
	if isCompactStacklessCoroSudog(sg, owner) {
		return (*stacklessCoroOperation)(owner)
	}
	return nil
}

//go:nosplit
func (sg *channelWaiter) channelSelect() bool {
	if op := sg.compactOwner(); op != nil {
		return op.waiterState&stacklessCoroWaiterSelect != 0
	}
	return sg.fullSudog().isSelect
}

//go:nosplit
func (sg *channelWaiter) setChannelSelect(v bool) {
	if op := sg.compactOwner(); op != nil {
		if v {
			op.waiterState |= stacklessCoroWaiterSelect
		} else {
			op.waiterState &^= stacklessCoroWaiterSelect
		}
		return
	}
	sg.fullSudog().isSelect = v
}

//go:nosplit
func (sg *channelWaiter) getElem() unsafe.Pointer {
	if sg.coro.get() != nil {
		return sg.elem
	}
	return sg.fullSudog().getFullElem()
}

//go:nosplit
func (sg *channelWaiter) getChannel() *hchan {
	if sg.coro.get() != nil {
		return sg.c
	}
	return sg.fullSudog().getChannel()
}

//go:nosplit
func (sg *channelWaiter) getFullElem() unsafe.Pointer { return sg.fullSudog().getFullElem() }

//go:nosplit
func (sg *channelWaiter) setFullElem(v unsafe.Pointer) { sg.fullSudog().setFullElem(v) }

//go:nosplit
func (sg *channelWaiter) getNativeElem() unsafe.Pointer { return sg.elem }

//go:nosplit
func (sg *channelWaiter) setNativeElem(v unsafe.Pointer) { sg.elem = v }

//go:nosplit
func (sg *channelWaiter) setNativeChannel(c *hchan) { sg.c = c }

// Keep these members flat: linker DWARF substitution looks up sudog.elem.
type sudog struct {
	coro        stacklessCoroSudog
	next        *sudog
	prev        *sudog
	elem        unsafe.Pointer
	c           *hchan
	ticket      uint32
	isSelect    bool
	success     bool
	waiters     uint16
	g           *g
	acquiretime int64
	releasetime int64
	parent      *sudog
	waittail    *sudog
	waitlink    *sudog
	elemAddr    uintptr
	chanAddr    uintptr
}

func isCompactStacklessCoroSudog(sg *channelWaiter, owner unsafe.Pointer) bool {
	// The relation is stable even after coro.owner is cleared. A separate full
	// sudog cannot contain its operation, and race storage puts the operation
	// before its full waiter. No pointer bits or operation IDs encode the kind.
	return owner != nil && owner == add(unsafe.Pointer(sg), unsafe.Offsetof(stacklessCoroChannelOperation{}.operation))
}

// Reject drift between the two channel prefixes at compile time.
const stacklessCoroChannelWaiterOffsetMismatch = (unsafe.Offsetof(sudog{}.coro) ^ unsafe.Offsetof(channelWaiter{}.coro)) |
	(unsafe.Offsetof(sudog{}.next) ^ unsafe.Offsetof(channelWaiter{}.next)) |
	(unsafe.Offsetof(sudog{}.prev) ^ unsafe.Offsetof(channelWaiter{}.prev)) |
	(unsafe.Offsetof(sudog{}.elem) ^ unsafe.Offsetof(channelWaiter{}.elem)) |
	(unsafe.Offsetof(sudog{}.c) ^ unsafe.Offsetof(channelWaiter{}.c))

var _ [-int(stacklessCoroChannelWaiterOffsetMismatch)]byte

// Native owners use traced pointers, including full semaphore and notification
// waiters; they never enter g.waiting or hide these pointers from the collector.
// A released compact waiter must be inspected with its explicit operation,
// because its full-only address mirrors overlap that operation's storage.
//
//go:nosplit
func (sg *sudog) getElem() unsafe.Pointer {
	if sg.coro.get() != nil {
		return sg.elem
	}
	return unsafe.Pointer(sg.elemAddr)
}

//go:nosplit
func (sg *sudog) setElem(v unsafe.Pointer) {
	sg.elem = v
	sg.elemAddr = uintptr(v)
}

//go:nosplit
func (sg *sudog) getChannel() *hchan {
	if sg.coro.get() != nil {
		return sg.c
	}
	return (*hchan)(unsafe.Pointer(sg.chanAddr))
}

//go:nosplit
func (sg *sudog) setChannel(c *hchan) {
	sg.c = c
	sg.chanAddr = uintptr(unsafe.Pointer(c))
}

// Only full waiters are linked from g.waiting. Preserve the raw address while
// temporarily hiding its traced pointer during goroutine leak detection.
//
//go:nosplit
func (sg *sudog) elemUintptr() uintptr { return sg.elemAddr }

//go:nosplit
func (sg *sudog) channelUintptr() uintptr { return sg.chanAddr }

//go:nosplit
func (sg *sudog) setElemUntraceable() { sg.elem = nil }

//go:nosplit
func (sg *sudog) setElemTraceable() { sg.elem = unsafe.Pointer(sg.elemAddr) }

//go:nosplit
func (sg *sudog) setChannelUntraceable() { sg.c = nil }

//go:nosplit
func (sg *sudog) setChannelTraceable() { sg.c = (*hchan)(unsafe.Pointer(sg.chanAddr)) }

func adjustsudogs(gp *g, adjinfo *adjustinfo) {
	for s := gp.waiting; s != nil; s = s.waitlink {
		adjustpointer(adjinfo, unsafe.Pointer(&s.elemAddr))
		adjustpointer(adjinfo, unsafe.Pointer(&s.elem))
	}
}

// Full-only accesses are used where the caller already owns a complete sudog.
// Native channel accesses are used only with a live coroutine owner; channel
// waiters never enter g.waiting and never need untraceable address mirrors.
//
//go:nosplit
func (sg *sudog) getFullElem() unsafe.Pointer { return unsafe.Pointer(sg.elemAddr) }

//go:nosplit
func (sg *sudog) setFullElem(v unsafe.Pointer) {
	sg.elem = v
	sg.elemAddr = uintptr(v)
}

//go:nosplit
func (sg *sudog) getNativeElem() unsafe.Pointer { return sg.elem }

//go:nosplit
func (sg *sudog) setNativeElem(v unsafe.Pointer) { sg.elem = v }

//go:nosplit
func (sg *sudog) setNativeChannel(c *hchan) { sg.c = c }
