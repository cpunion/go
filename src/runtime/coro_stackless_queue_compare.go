// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && coropullcompare

package runtime

import "unsafe"

// stacklessCoroPullComparisonMarker is a test-driver sentinel. A real queue
// tail can never refer to it. The comparison reuses head for its current
// structured leaf, which keeps the production scheduler in its original size
// class.
var stacklessCoroPullComparisonMarker stacklessCoroTask

func (s *stacklessCoroScheduler) isPullComparison() bool {
	return s.tail == &stacklessCoroPullComparisonMarker
}

func (s *stacklessCoroScheduler) readyLocked(task *stacklessCoroTask) uint32 {
	if task.resuming {
		if task.state != stacklessCoroTaskWaiting || task.readyPending {
			throw("runtime: invalid stackless coroutine early ready transition")
		}
		// An operation may finish before the active resume call returns Wait.
		// Defer publication until the scheduler consumes that action, so two
		// executors cannot enter the same resume function concurrently.
		task.readyPending = true
		if raceenabled {
			racereleasemerge(unsafe.Pointer(task))
		}
		return 0
	}
	switch task.state {
	case stacklessCoroTaskNew, stacklessCoroTaskRunning, stacklessCoroTaskWaiting:
	default:
		throw("runtime: invalid stackless coroutine ready transition")
	}
	if task.readyPending {
		throw("runtime: stackless coroutine has pending ready transition")
	}
	task.state = stacklessCoroTaskRunnable
	task.next = nil
	var runnable uint32
	if s.isPullComparison() {
		// A pull comparison root has one active structured leaf. An event
		// publishes readiness for that leaf and rings the root doorbell; it
		// does not enqueue the exact continuation. Child completion replaces
		// the leaf with its parent before the next polling episode.
		s.head = task
	} else {
		if s.tail == nil {
			s.head = task
		} else {
			s.tail.next = task
		}
		s.tail = task
		runnable = s.runnableState.Add(1)
	}
	if raceenabled {
		// Runtime mutex operations are invisible to the race detector.
		// Merge both the previous resume episode and the producer that made
		// this task runnable into the next executor's acquire.
		racereleasemerge(unsafe.Pointer(task))
	}
	return runnable
}

func (s *stacklessCoroScheduler) take() *stacklessCoroTask {
	lock(&s.lock)
	pull := s.isPullComparison()
	task := s.head
	if task == nil || pull && task.state != stacklessCoroTaskRunnable {
		unlock(&s.lock)
		return nil
	}
	if !pull {
		s.head = task.next
		if s.head == nil {
			s.tail = nil
		}
		s.runnableState.Add(-1)
	}
	task.next = nil
	if task.state != stacklessCoroTaskRunnable || task.resuming ||
		task.readyPending {
		unlock(&s.lock)
		throw("runtime: dequeued non-runnable stackless coroutine")
	}
	task.state = stacklessCoroTaskRunning
	task.resuming = true
	unlock(&s.lock)
	if raceenabled {
		raceacquire(unsafe.Pointer(task))
	}
	return task
}
