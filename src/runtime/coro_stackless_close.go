// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro

package runtime

import "unsafe"

const stacklessCoroCloseBatchSize = 64

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
	var lastWoken *stacklessCoroScheduler
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
		isFirst := lastWoken != s
		isLast := head == nil
		if !isLast {
			nextOp := (*stacklessCoroOperation)(head.coro.get())
			isLast = !stacklessCoroCloseBatchable(nextOp) || nextOp.scheduler() != s
		}
		publishStacklessCoroClosedOperations(s, completed[:count], isFirst || isLast)
		lastWoken = s
	}
}

func stacklessCoroCloseBatchable(op *stacklessCoroOperation) bool {
	return op != nil && op.id == stacklessCoroChannelOperationID &&
		!op.send && !op.timerWait &&
		!(op.hasResources() && op.resourceState().selection.active()) &&
		op.scheduler() != nil && !stacklessCoroIsPullComparison(op.scheduler())
}

func publishStacklessCoroClosedOperations(s *stacklessCoroScheduler,
	completed []stacklessCoroClosedOperation, wake bool) {
	if s.multiPFastPath() {
		publishStacklessCoroClosedOperationsMultiP(s, completed, wake)
		return
	}
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
	// Wake at most the currently parked executors (capped by signals).
	if signals != 0 {
		if waiting := int(s.waitingExecutors()); waiting != 0 {
			if signals > waiting {
				signals = waiting
			}
			for range signals {
				s.signalParallelCap(0)
			}
		} else if stacklessCoroIsPullComparison(s) {
			s.signal()
		}
	}
}

func publishStacklessCoroClosedOperationsMultiP(s *stacklessCoroScheduler,
	completed []stacklessCoroClosedOperation, wake bool) {
	var opOverflow, opTail *stacklessCoroOperation
	var spillHead, spillTail *stacklessCoroTask
	var spillCount uint32
	var slowHead, slowTail *stacklessCoroTask
	signals := 0
	for i := range completed {
		entry := completed[i]
		completed[i] = stacklessCoroClosedOperation{}
		early := entry.task.completeWaitAtomic(0)
		if !coroPOpCachePut(entry.operation) {
			if opTail == nil {
				opOverflow = entry.operation
			} else {
				opTail.next = entry.operation
			}
			opTail = entry.operation
		}
		if early {
			continue
		}
		signals++
		entry.task.next = nil
		stacklessCoroSetLogicalLeaf(entry.task, nil)
		if coroRunqEligible(s, entry.task) {
			if _, ok := coroRunqPut(s, entry.task); ok {
				continue
			}
			if spillTail == nil {
				spillHead = entry.task
			} else {
				spillTail.next = entry.task
			}
			spillTail = entry.task
			spillCount++
		} else {
			if slowTail == nil {
				slowHead = entry.task
			} else {
				slowTail.next = entry.task
			}
			slowTail = entry.task
		}
	}
	if opOverflow != nil {
		lock(&stacklessCoroSharedOperations.lock)
		for opOverflow != nil {
			op := opOverflow
			opOverflow = op.next
			op.next = nil
			cacheStacklessCoroSharedOperationLocked(op)
		}
		unlock(&stacklessCoroSharedOperations.lock)
	}
	if spillHead != nil || slowHead != nil {
		lock(&s.lock)
		if spillHead != nil {
			if s.tail == nil {
				s.head = spillHead
			} else {
				s.tail.next = spillHead
			}
			s.tail = spillTail
			s.runnableState.Add(int32(spillCount))
		}
		for slowHead != nil {
			task := slowHead
			slowHead = task.next
			task.next = nil
			s.enqueueRunnableLocked(task)
		}
		unlock(&s.lock)
	}
	if wake && signals != 0 {
		if waiting := int(s.waitingExecutors()); waiting != 0 {
			if signals > waiting {
				signals = waiting
			}
			limit := int(gomaxprocs) - 1
			if limit > 0 && signals > limit {
				signals = limit
			}
			for range signals {
				s.signalParallelCap(uint(signals))
			}
		}
	}
}
