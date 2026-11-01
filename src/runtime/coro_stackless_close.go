// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro

package runtime

import "unsafe"

const stacklessCoroCloseBatchSize = 32

type stacklessCoroClosedOperation struct {
	operation *stacklessCoroOperation
	task      *stacklessCoroTask
}

// finishStacklessCoroClose owns the winners detached by closechan. Ordinary
// goroutines have already been readied and the channel lock is not held.
// Only direct, non-select receives may share a bounded publication step.
// Other sources keep their individual cleanup and completion ordering.
func finishStacklessCoroClose(head *channelWaiter) {
	var completed [stacklessCoroCloseBatchSize]stacklessCoroClosedOperation
	for head != nil {
		op := (*stacklessCoroOperation)(head.coro.get())
		if raceenabled || !stacklessCoroCloseBatchable(op) {
			waiter := head
			head = waiter.next
			waiter.next = nil
			finishStacklessCoroChannel(unsafe.Pointer(op), waiter, false)
			continue
		}
		s := op.scheduler()
		count := 0
		for head != nil && count < len(completed) {
			op = (*stacklessCoroOperation)(head.coro.get())
			if !stacklessCoroCloseBatchable(op) || op.scheduler() != s {
				break
			}
			waiter := head
			head = waiter.next
			waiter.next = nil
			waiter.setNativeChannel(nil)
			waiter.coro.clear()
			releaseStacklessCoroSudog(unsafe.Pointer(op), waiter)
			if received := op.receivedResult(); received != nil {
				*received = false
			}
			// The detached source still owns task until publication, so its
			// scheduler cannot change between the grouping check and clear.
			_, task := clearStacklessCoroOperation(op)
			completed[count] = stacklessCoroClosedOperation{op, task}
			count++
		}
		publishStacklessCoroClosedOperations(s, completed[:count])
	}
}

func stacklessCoroCloseBatchable(op *stacklessCoroOperation) bool {
	return op != nil && op.id == stacklessCoroChannelOperationID &&
		!op.send && !op.timerWait &&
		!(op.hasResources() && op.resourceState().selection.active()) &&
		op.scheduler() != nil && !stacklessCoroIsPullComparison(op.scheduler())
}

func publishStacklessCoroClosedOperations(s *stacklessCoroScheduler,
	completed []stacklessCoroClosedOperation) {
	var overflow, tail *stacklessCoroOperation
	signals := 0
	lock(&s.lock)
	for i := range completed {
		entry := completed[i]
		if !entry.task.resuming {
			signals++
		}
		s.readyLocked(entry.task)
		if !s.recycleOperationLocked(entry.operation) {
			if tail == nil {
				overflow = entry.operation
			} else {
				tail.next = entry.operation
			}
			tail = entry.operation
		}
		completed[i] = stacklessCoroClosedOperation{}
	}
	if overflow != nil {
		// This lock order is also used when a scheduler drains its local
		// cache. Publish every cleared operation before an executor can
		// finish the corresponding task and retire the scheduler's GC age.
		lock(&stacklessCoroSharedOperations.lock)
		for overflow != nil {
			op := overflow
			overflow = op.next
			op.next = nil
			cacheStacklessCoroSharedOperationLocked(op)
		}
		unlock(&stacklessCoroSharedOperations.lock)
	}
	unlock(&s.lock)
	// Keep one wake attempt per completed non-resuming task. A scheduler
	// may have several admitted executors waiting for work on other Ps.
	for range signals {
		s.signal()
	}
}
