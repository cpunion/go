// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro

package runtime

import "unsafe"

type stacklessCoroGState struct {
	native    unsafe.Pointer
	deferTask *stacklessCoroTask
}

type stacklessCoroG struct {
	stacklessCoro *stacklessCoroGState
}

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
