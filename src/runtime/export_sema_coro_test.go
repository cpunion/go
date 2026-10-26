// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && (darwin || linux)

package runtime

import "unsafe"

var SemacquireMutexOrdinaryForTest = internal_sync_runtime_SemacquireMutex
var SemacquireOrdinaryForTest = sync_runtime_Semacquire
var SemacquireWaitGroupOrdinaryForTest = sync_runtime_SemacquireWaitGroup

var SemacquireStacklessCoroForTest = coroSemacquire
var SemacquireWaitGroupStacklessCoroForTest = coroSemacquireWaitGroup
var SemreleasePollStacklessCoroForTest = coroSemreleasePoll

func SemaphoreProfileStacklessCoroForTest(state unsafe.Pointer) (block, mutex bool) {
	w := (*stacklessCoroSemaWaiter)(state)
	return w.startTime != 0 && w.waiter.releasetime != 0,
		w.waiter.acquiretime != 0
}

const StacklessCoroActionHandoff = stacklessCoroActionHandoff

func SemreleaseStacklessCoroForTest(ctx unsafe.Pointer, addr *uint32, handoff bool) bool {
	return coroSemrelease(ctx, addr, handoff, 0)
}

func SemacquireMutexStacklessCoroForTest(ctx unsafe.Pointer, addr *uint32,
	lifo bool, state *unsafe.Pointer) bool {
	return coroSemacquireMutex(ctx, addr, lifo, 0, state)
}

// NewStacklessCoroSemaphoreContextForTest separates context setup from the
// ready acquisition so allocation measurements do not include root entry.
func NewStacklessCoroSemaphoreContextForTest() unsafe.Pointer {
	task := new(stacklessCoroTask)
	task.context.scheduler = newStacklessCoroTestScheduler()
	return unsafe.Pointer(&task.context)
}

// SemaphoreWaitersStacklessCoroForTest counts only this address, rather than
// every semaphore that hashes to the same semaRoot.
func SemaphoreWaitersStacklessCoroForTest(addr *uint32) (ordinary, logical int) {
	root := semtable.rootFor(addr)
	lockWithRank(&root.lock, lockRankRoot)
	defer unlock(&root.lock)
	for s := root.treap; s != nil; {
		if s.elem.get() == unsafe.Pointer(addr) {
			for ; s != nil; s = s.waitlink {
				if s.coro.get() == nil {
					ordinary++
				} else {
					if s.g != nil {
						throw("runtime: logical semaphore waiter has a parked goroutine")
					}
					logical++
				}
			}
			return
		}
		if uintptr(unsafe.Pointer(addr)) < s.elem.uintptr() {
			s = s.prev
		} else {
			s = s.next
		}
	}
	return
}
