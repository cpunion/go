// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro

package runtime

import "unsafe"

// stacklessCoroSemaWaiter belongs to one logical semaphore acquisition. Its
// frame retains it across resumes, and semtable retains it while queued.
// A wakeup does not complete the acquisition: without a handoff ticket, the
// resumed task must compete for the semaphore and may have to queue again.
type stacklessCoroSemaWaiter struct {
	scheduler  *stacklessCoroScheduler
	task       *stacklessCoroTask
	addr       *uint32
	lifo       bool
	skipframes int
	startTime  int64
	waiter     sudog
}

// coroSemacquireMutex attempts one logical semaphore acquisition. State must
// point into the current coroutine frame, initially nil. A true return parks
// the task; after waking, the same frame must retry this call with the same
// arguments. A false return owns the semaphore and clears state.
//
// Like semacquire1, this operation cannot be canceled. A queued task cannot
// return or unwind until it acquires the semaphore and resumes execution.
// The compiler does not yet emit this entry. Acquire and release lowering
// must also preserve logical starvation handoff before public sync calls can
// use it.
func coroSemacquireMutex(ctx unsafe.Pointer, addr *uint32, lifo bool,
	skipframes int, state *unsafe.Pointer) bool {
	context := (*stacklessCoroContext)(ctx)
	task := context.task()
	if context == nil || context.scheduler == nil || task == nil || state == nil {
		throw("runtime: invalid stackless coroutine semaphore")
	}
	s := context.scheduler
	w := (*stacklessCoroSemaWaiter)(*state)
	if w == nil {
		if cansemacquire(addr) {
			return false
		}
		w = &stacklessCoroSemaWaiter{
			scheduler: s, task: task, addr: addr, lifo: lifo, skipframes: skipframes,
		}
		w.waiter.coro.set(unsafe.Pointer(w))
		if blockprofilerate > 0 {
			w.startTime = cputicks()
			w.waiter.releasetime = -1
		}
		if mutexprofilerate > 0 {
			if w.startTime == 0 {
				w.startTime = cputicks()
			}
			w.waiter.acquiretime = w.startTime
		}
		*state = unsafe.Pointer(w)
	} else {
		if w.scheduler != s || w.task != task || w.addr != addr ||
			w.lifo != lifo || w.skipframes != skipframes ||
			w.waiter.elem.get() != nil || w.waiter.g != nil ||
			w.waiter.coro.get() != unsafe.Pointer(w) {
			throw("runtime: invalid resumed stackless coroutine semaphore")
		}
		if w.waiter.ticket != 0 || cansemacquire(addr) {
			w.finish(state)
			return false
		}
	}

	lock(&s.lock)
	if task.state != stacklessCoroTaskRunning || !task.resuming ||
		task.readyPending || task.terminal != stacklessCoroTerminalNone || task.goexit {
		unlock(&s.lock)
		throw("runtime: stackless coroutine semaphore outside resume")
	}
	s.initialRootPrivate = false
	s.lifetime.Or(stacklessCoroLifetimeOperationStarted)
	task.state = stacklessCoroTaskWaiting
	unlock(&s.lock)

	root := semtable.rootFor(addr)
	lockWithRank(&root.lock, lockRankRoot)
	root.nwait.Add(1)
	// As in semacquire1, publish nwait before checking the semaphore again.
	// No release can miss a waiter between this check and queue insertion.
	if cansemacquire(addr) {
		root.nwait.Add(-1)
		unlock(&root.lock)
		lock(&s.lock)
		task.state = stacklessCoroTaskRunning
		unlock(&s.lock)
		w.finish(state)
		return false
	}
	root.queue(addr, &w.waiter, lifo)
	unlock(&root.lock)
	return true
}

func (w *stacklessCoroSemaWaiter) finish(state *unsafe.Pointer) {
	if w.waiter.releasetime > 0 {
		blockevent(w.waiter.releasetime-w.startTime, 3+w.skipframes)
	}
	w.waiter.coro.clear()
	w.waiter.ticket = 0
	w.scheduler = nil
	w.task = nil
	w.addr = nil
	*state = nil
}

// readyStacklessCoroSemaphore is called after semrelease1 has removed the
// waiter from the common semaphore queue and optionally reserved a ticket.
// It must not access w after publishing readiness: the task can immediately
// retry the acquisition and release the waiter on another executor.
func readyStacklessCoroSemaphore(sg *sudog) {
	w := (*stacklessCoroSemaWaiter)(sg.coro.get())
	if w == nil || &w.waiter != sg || w.scheduler == nil || w.task == nil {
		throw("runtime: invalid stackless coroutine semaphore wakeup")
	}
	if sg.releasetime != 0 {
		sg.releasetime = cputicks()
	}
	s, task := w.scheduler, w.task
	wake := s.wake
	lock(&s.lock)
	completedDuringResume := task.resuming
	s.readyLocked(task)
	unlock(&s.lock)
	if !completedDuringResume {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
}
