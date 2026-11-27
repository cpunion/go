// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro

package runtime

import "unsafe"

// coroSemaWake wakes a stackless coroutine parked in semaRoot (called from
// semrelease1 after unlocking root.lock).
func coroSemaWake(s *sudog) {
	task := s.coro
	if task == nil {
		return
	}
	if s.ticket != 0 {
		task.semaTicket.Store(1)
	}
	if raceenabled {
		racereleasemerge(unsafe.Pointer(task))
	}
	s.coro = nil
	s.g = nil
	s.elem.set(nil)
	s.next = nil
	s.prev = nil
	s.parent = nil
	s.waitlink = nil
	s.waittail = nil
	s.ticket = 0
	releaseSudog(s)
	coroReady(task)
}

// coroNotifyWake wakes a stackless coroutine parked in notifyList (called from
// notifyListNotifyOne / notifyListNotifyAll after unlocking l.lock).
func coroNotifyWake(s *sudog) {
	task := s.coro
	if task == nil {
		return
	}
	if raceenabled {
		racereleasemerge(unsafe.Pointer(task))
	}
	s.coro = nil
	s.g = nil
	s.elem.set(nil)
	s.next = nil
	s.ticket = 0
	releaseSudog(s)
	coroReady(task)
}

// coroSemacquire attempts to acquire semaphore *addr without blocking the host
// driver goroutine. Returns false if acquired (either immediately or via a
// handoff ticket); returns true if the coroutine parked on semaRoot and must
// return coroActionWait.
func coroSemacquire(ctx unsafe.Pointer, addr *uint32, lifo bool) bool {
	task := (*coroTask)(ctx)
	if task.semaTicket.Load() != 0 {
		task.semaTicket.Store(0)
		return false
	}
	if cansemacquire(addr) {
		return false
	}
	s := acquireSudog()
	root := semtable.rootFor(addr)
	s.releasetime = 0
	s.acquiretime = 0
	s.ticket = 0
	s.g = nil
	s.coro = task

	lockWithRank(&root.lock, lockRankRoot)
	root.nwait.Add(1)
	if cansemacquire(addr) {
		root.nwait.Add(-1)
		unlock(&root.lock)
		s.coro = nil
		releaseSudog(s)
		return false
	}
	task.state.Store(coroStateWaiting)
	if raceenabled {
		racereleasemerge(unsafe.Pointer(task))
	}
	root.queue(addr, s, lifo)
	unlock(&root.lock)
	return true
}

//go:linkname internal_sync_runtime_coroSemacquire internal/sync.runtime_coroSemacquire
func internal_sync_runtime_coroSemacquire(ctx unsafe.Pointer, addr *uint32, lifo bool) bool {
	return coroSemacquire(ctx, addr, lifo)
}

//go:linkname sync_runtime_coroSemacquire sync.runtime_coroSemacquire
func sync_runtime_coroSemacquire(ctx unsafe.Pointer, addr *uint32, lifo bool) bool {
	return coroSemacquire(ctx, addr, lifo)
}

//go:linkname sync_runtime_coroNotifyListWait sync.runtime_coroNotifyListWait
func sync_runtime_coroNotifyListWait(ctx unsafe.Pointer, l *notifyList, t uint32) bool {
	task := (*coroTask)(ctx)
	s := acquireSudog()
	lockWithRank(&l.lock, lockRankNotifyList)
	if less(t, l.notify) {
		unlock(&l.lock)
		releaseSudog(s)
		return false
	}
	s.g = nil
	s.coro = task
	s.elem.set(unsafe.Pointer(l))
	s.ticket = t
	s.releasetime = 0
	s.next = nil
	task.state.Store(coroStateWaiting)
	if raceenabled {
		racereleasemerge(unsafe.Pointer(task))
	}
	if l.tail == nil {
		l.head = s
	} else {
		l.tail.next = s
	}
	l.tail = s
	unlock(&l.lock)
	return true
}
