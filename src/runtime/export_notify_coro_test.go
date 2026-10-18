// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && (darwin || linux)

package runtime

import "unsafe"

type NotifyListStacklessCoroForTest struct {
	list notifyList
}

func (l *NotifyListStacklessCoroForTest) Add() uint32   { return notifyListAdd(&l.list) }
func (l *NotifyListStacklessCoroForTest) Wait(t uint32) { notifyListWait(&l.list, t) }
func (l *NotifyListStacklessCoroForTest) Signal()       { notifyListNotifyOne(&l.list) }
func (l *NotifyListStacklessCoroForTest) Broadcast()    { notifyListNotifyAll(&l.list) }

func (l *NotifyListStacklessCoroForTest) WaitCoro(ctx unsafe.Pointer, t uint32, state *unsafe.Pointer) bool {
	return coroNotifyListWait(ctx, unsafe.Pointer(&l.list), t, state)
}

func (l *NotifyListStacklessCoroForTest) Reset(ticket uint32) {
	l.list.wait.Store(ticket)
	l.list.notify = ticket
}

func (l *NotifyListStacklessCoroForTest) Waiters() (ordinary, logical int) {
	lockWithRank(&l.list.lock, lockRankNotifyList)
	defer unlock(&l.list.lock)
	for s := l.list.head; s != nil; s = s.next {
		if s.coro.get() == nil {
			ordinary++
		} else {
			if s.g != nil {
				throw("runtime: logical notification waiter has a parked goroutine")
			}
			logical++
		}
	}
	if (l.list.head == nil) != (l.list.tail == nil) || l.list.tail != nil && l.list.tail.next != nil {
		throw("runtime: invalid notification test queue")
	}
	return
}

func NotifyProfileStacklessCoroForTest(state unsafe.Pointer) bool {
	w := (*stacklessCoroNotifyWaiter)(state)
	return w.startTime != 0 && w.waiter.releasetime != 0
}

// NotifyBeforeQueue exercises the second ticket check without relying on the
// notifier winning a narrow race between the entry's first check and insertion.
func (l *NotifyListStacklessCoroForTest) NotifyBeforeQueue(ctx unsafe.Pointer, ticket uint32, broadcast bool) bool {
	context := (*stacklessCoroContext)(ctx)
	w := &stacklessCoroNotifyWaiter{scheduler: context.scheduler, task: context.task(), list: &l.list}
	w.waiter.ticket = ticket
	w.waiter.coro.set(unsafe.Pointer(w))
	state := unsafe.Pointer(w)
	if broadcast {
		l.Broadcast()
	} else {
		l.Signal()
	}
	if w.park(&state) {
		throw("runtime: pre-notified stackless coroutine queued")
	}
	return state == nil && w.waiter.coro.get() == nil &&
		w.scheduler == nil && w.task == nil && w.list == nil &&
		context.task().state == stacklessCoroTaskRunning
}
