// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro

package runtime

import "unsafe"

func CheckStacklessCoroFusedSchedulerTransitionsForTest() string {
	s := &stacklessCoroScheduler{
		wake: make(chan struct{}, stacklessCoroWarmExecutorCount),
	}
	lockInit(&s.lock, lockRankStacklessCoro)

	// 1. Lock-free empty check in take().
	if s.take() != nil {
		return "empty take returned non-nil task"
	}

	// 2. waitingAndTake pops the next runnable task under the same lock.
	waiter := &stacklessCoroTask{state: stacklessCoroTaskWaiting, resuming: true}
	peer := &stacklessCoroTask{state: stacklessCoroTaskNew}
	lock(&s.lock)
	s.readyLocked(peer)
	unlock(&s.lock)
	next, direct := s.waitingAndTake(waiter, true, false)
	if direct || next != peer || waiter.resuming || peer.state != stacklessCoroTaskRunning || !peer.resuming {
		return "waitingAndTake failed to hand off queued peer"
	}
	if s.take() != nil {
		return "queue non-empty after waitingAndTake consumed sole peer"
	}

	// 3. waitingAndTake with readyPending on empty queue retains direct ownership when native & !race & !pull.
	waiter2 := &stacklessCoroTask{state: stacklessCoroTaskWaiting, resuming: true, readyPending: true}
	next, direct = s.waitingAndTake(waiter2, true, false)
	if !raceenabled && !stacklessCoroIsPullComparison(s) {
		if !direct || next != nil || waiter2.state != stacklessCoroTaskRunning || !waiter2.resuming {
			return "waitingAndTake failed direct readyPending resume"
		}
	} else if direct || next != waiter2 {
		return "waitingAndTake failed fallback readyPending dequeue"
	}

	// 4. yieldAndTake pops peer when another task is runnable and respects foreignReturner.
	peer.state, peer.resuming = stacklessCoroTaskRunning, true
	queued := &stacklessCoroTask{state: stacklessCoroTaskNew}
	lock(&s.lock)
	s.readyLocked(queued)
	unlock(&s.lock)
	next, direct, foreign := s.yieldAndTake(peer, true, false)
	if direct || foreign || next != queued || peer.resuming || queued.state != stacklessCoroTaskRunning || !queued.resuming {
		return "yieldAndTake failed to swap with queued peer"
	}
	// Drain re-enqueued peer.
	if got := s.take(); got != peer {
		return "re-enqueued peer missing after yieldAndTake"
	}

	// 5. completeAndTake pops next independent task under the same lock.
	s.liveSpawns = 2
	doneTask := &stacklessCoroTask{
		resume: func(unsafe.Pointer) uint8 { return stacklessCoroActionComplete },
		state:  stacklessCoroTaskRunning,
		resuming: true,
	}
	nextTask := &stacklessCoroTask{state: stacklessCoroTaskNew}
	lock(&s.lock)
	s.readyLocked(nextTask)
	unlock(&s.lock)
	next = s.completeAndTake(doneTask, false)
	if next != nextTask || nextTask.state != stacklessCoroTaskRunning || !nextTask.resuming {
		return "completeAndTake failed to dequeue next runnable task"
	}
	if !raceenabled && s.freeTasks != doneTask {
		return "completeAndTake did not recycle completed task"
	}
	if s.liveSpawns != 1 {
		return "completeAndTake did not decrement liveSpawns"
	}
	return ""
}
