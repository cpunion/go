// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro

package runtime

import (
	"internal/runtime/atomic"
	"unsafe"
)

// stacklessCoroNotifyWaiter belongs to one ticket in a notifyList. The frame
// retains it until the notified task resumes; the list retains it while queued.
// It uses the same sudog list as ordinary waiters without parking an executor G.
type stacklessCoroNotifyWaiter struct {
	scheduler *stacklessCoroScheduler
	task      *stacklessCoroTask
	list      *notifyList
	startTime int64
	waiter    sudog
}

// coroNotifyListWait waits for ticket t. As with semaphore acquisition, the
// compiler evaluates the arguments once and retries with frame-owned state.
// Unlike semaphore acquisition, a notification completes the wait: the resumed
// task must not compete for another ticket or enqueue itself again.
func coroNotifyListWait(ctx, list unsafe.Pointer, t uint32, state *unsafe.Pointer) bool {
	context := (*stacklessCoroContext)(ctx)
	task := context.task()
	l := (*notifyList)(list)
	if context == nil || context.scheduler == nil || task == nil || l == nil || state == nil {
		throw("runtime: invalid stackless coroutine notification wait")
	}
	s := context.scheduler
	w := (*stacklessCoroNotifyWaiter)(*state)
	if w != nil {
		if w.scheduler != s || w.task != task || w.list != l || w.waiter.ticket != t ||
			w.waiter.g != nil || w.waiter.next != nil || w.waiter.getElem() != nil ||
			w.waiter.coro.get() != unsafe.Pointer(w) || !less(t, atomic.Load(&l.notify)) {
			throw("runtime: invalid resumed stackless coroutine notification wait")
		}
		w.finish(state)
		return false
	}
	if less(t, atomic.Load(&l.notify)) {
		return false
	}
	w = &stacklessCoroNotifyWaiter{scheduler: s, task: task, list: l}
	w.waiter.ticket = t
	w.waiter.coro.set(unsafe.Pointer(w))
	if blockprofilerate > 0 {
		w.startTime = cputicks()
		w.waiter.releasetime = -1
	}
	*state = unsafe.Pointer(w)
	return w.park(state)
}

// park publishes the logical wait before checking the notify list again. The
// list lock closes the race with notification between allocation and insertion.
func (w *stacklessCoroNotifyWaiter) park(state *unsafe.Pointer) bool {
	s, task, l := w.scheduler, w.task, w.list
	t := w.waiter.ticket
	lock(&s.lock)
	if task.state != stacklessCoroTaskRunning || !task.resuming ||
		task.readyPending || task.terminal != stacklessCoroTerminalNone || task.goexit {
		unlock(&s.lock)
		throw("runtime: stackless coroutine notification wait outside resume")
	}
	s.initialRootPrivate = false
	s.lifetime.Or(stacklessCoroLifetimeOperationStarted)
	task.state = stacklessCoroTaskWaiting
	unlock(&s.lock)

	lockWithRank(&l.lock, lockRankNotifyList)
	// Signal or Broadcast may have consumed this ticket before insertion.
	if less(t, l.notify) {
		unlock(&l.lock)
		lock(&s.lock)
		task.state = stacklessCoroTaskRunning
		unlock(&s.lock)
		w.finish(state)
		return false
	}
	w.waiter.setElem(unsafe.Pointer(l))
	if l.tail == nil {
		l.head = &w.waiter
	} else {
		l.tail.next = &w.waiter
	}
	l.tail = &w.waiter
	unlock(&l.lock)
	return true
}

func (w *stacklessCoroNotifyWaiter) finish(state *unsafe.Pointer) {
	if w.waiter.releasetime > 0 {
		// finish adds a frame below the notification declaration entry.
		blockevent(w.waiter.releasetime-w.startTime, 3)
	}
	w.waiter.coro.clear()
	w.scheduler = nil
	w.task = nil
	w.list = nil
	*state = nil
}

// readyStacklessCoroNotify is called after removing a logical waiter from the
// notifyList. Do not access it after publishing readiness: another executor may
// immediately resume the task and release its waiter.
func readyStacklessCoroNotify(sg *sudog) {
	w := (*stacklessCoroNotifyWaiter)(sg.coro.get())
	if w == nil || &w.waiter != sg || w.scheduler == nil || w.task == nil {
		throw("runtime: invalid stackless coroutine notification wakeup")
	}
	if sg.releasetime != 0 {
		sg.releasetime = cputicks()
	}
	sg.setElem(nil)
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
