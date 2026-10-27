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

// Channel operations access only this prefix. Detached close completions reuse
// next after dequeue, so waitlink remains in the full-only tail. The semaphore
// tree, goroutine and block-profile fields remain in full sudogs. A compact channel allocation
// puts its operation immediately after the prefix, keeping the complete
// allocation large enough for a sudog view while rooting both parts precisely.
type stacklessCoroChannelWaiter struct {
	coro     stacklessCoroSudog
	next     *sudog
	prev     *sudog
	elem     maybeTraceablePtr
	c        maybeTraceableChan
	ticket   uint32
	isSelect bool
	success  bool
	waiters  uint16
}

// Keep these members flat: linker DWARF substitution looks up sudog.elem.
type sudog struct {
	coro        stacklessCoroSudog
	next        *sudog
	prev        *sudog
	elem        maybeTraceablePtr
	c           maybeTraceableChan
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
}

func isCompactStacklessCoroSudog(sg *sudog, owner unsafe.Pointer) bool {
	// The relation is stable even after coro.owner is cleared. A separate full
	// sudog cannot contain its operation, and race storage puts the operation
	// before its full waiter. No pointer bits or operation IDs encode the kind.
	return owner != nil && owner == add(unsafe.Pointer(sg), unsafe.Sizeof(stacklessCoroChannelWaiter{}))
}

// Reject drift between the two channel prefixes at compile time.
const stacklessCoroChannelWaiterOffsetMismatch = (unsafe.Offsetof(sudog{}.coro) ^ unsafe.Offsetof(stacklessCoroChannelWaiter{}.coro)) |
	(unsafe.Offsetof(sudog{}.next) ^ unsafe.Offsetof(stacklessCoroChannelWaiter{}.next)) |
	(unsafe.Offsetof(sudog{}.prev) ^ unsafe.Offsetof(stacklessCoroChannelWaiter{}.prev)) |
	(unsafe.Offsetof(sudog{}.elem) ^ unsafe.Offsetof(stacklessCoroChannelWaiter{}.elem)) |
	(unsafe.Offsetof(sudog{}.c) ^ unsafe.Offsetof(stacklessCoroChannelWaiter{}.c)) |
	(unsafe.Offsetof(sudog{}.ticket) ^ unsafe.Offsetof(stacklessCoroChannelWaiter{}.ticket)) |
	(unsafe.Offsetof(sudog{}.isSelect) ^ unsafe.Offsetof(stacklessCoroChannelWaiter{}.isSelect)) |
	(unsafe.Offsetof(sudog{}.success) ^ unsafe.Offsetof(stacklessCoroChannelWaiter{}.success)) |
	(unsafe.Offsetof(sudog{}.waiters) ^ unsafe.Offsetof(stacklessCoroChannelWaiter{}.waiters))

var (
	_ [-int(stacklessCoroChannelWaiterOffsetMismatch)]byte
	_ [unsafe.Sizeof(stacklessCoroChannelOperation{}) - unsafe.Sizeof(sudog{})]byte
)
