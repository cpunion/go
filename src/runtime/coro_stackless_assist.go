// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro

package runtime

import "internal/runtime/atomic"

const stacklessCoroAssistWidth = 4

type stacklessCoroAssistLane struct {
	lock       mutex
	enabled    atomic.Bool
	wake       chan struct{}
	head, tail *stacklessCoroTask
	// The assisting driver owns the retired list. Its source records have
	// already been removed. Other drivers may still take runnable tasks.
	retired      *stacklessCoroTask
	retiredCount uint8
}

func (s *stacklessCoroScheduler) canParkAssistant() bool {
	return s.lifetime.Load()&stacklessCoroLifetimeDeferredExecutors != 0 && s.blockingExecutors.Load() == 0
}

func (lane *stacklessCoroAssistLane) signal() {
	select {
	case lane.wake <- struct{}{}:
	default:
	}
}

func (s *stacklessCoroScheduler) canAssist() bool {
	const incompatible = stacklessCoroLifetimeHostStarted | stacklessCoroLifetimeTerminalRecords |
		stacklessCoroLifetimeOperationStarted | stacklessCoroLifetimeRootComplete
	return s.lifetime.Load()&incompatible == 0 && s.blockingExecutors.Load() == 0
}

// An unstarted or yielded task is reachable from a scheduler queue until
// a driver claims it. Drivers can drain this lane when the main FIFO empties.
func (s *stacklessCoroScheduler) takeAssistReady() *stacklessCoroTask {
	if replacements := s.replacements.Load(); replacements != nil {
		if lane := replacements.lane.Load(); lane != nil {
			lock(&lane.lock)
			task := s.popAssistLocked(lane)
			unlock(&lane.lock)
			return task
		}
	}
	return nil
}

func (s *stacklessCoroScheduler) popAssistLocked(lane *stacklessCoroAssistLane) *stacklessCoroTask {
	task := lane.head
	if task != nil {
		lane.head = task.next
		if lane.head == nil {
			lane.tail = nil
		}
		task.next = nil
		stacklessCoroStartReady(task)
		s.runnableState.Add(-1)
	}
	return task
}

// Claim a bounded prefix only while the helper is executing. The runnable
// count stays unchanged until a task is taken from either ready queue.
func (s *stacklessCoroScheduler) takeAssistBatch(lane *stacklessCoroAssistLane) *stacklessCoroTask {
	if !s.canAssist() || getg().preempt || s.runnableState.Load()&stacklessCoroForeignReturnerBit != 0 {
		return nil
	}
	lock(&lane.lock)
	task := s.popAssistLocked(lane)
	unlock(&lane.lock)
	if task != nil {
		return task
	}
	lock(&s.lock)
	first := s.head
	var anchor *stacklessCoroTask
	if first == &s.root {
		anchor, first = first, first.next
	}
	if first == nil || first.parent() != nil || !s.canAssist() {
		unlock(&s.lock)
		return nil
	}
	last := first
	for n := 1; n < stacklessCoroAssistWidth; n++ {
		next := last.next
		if next == nil || next == &s.root || next.parent() != nil {
			break
		}
		last = next
	}
	lock(&lane.lock)
	if lane.head != nil || lane.tail != nil {
		throw("runtime: invalid coroutine assist lane refill")
	}
	if anchor == nil {
		s.head = last.next
	} else {
		anchor.next = last.next
	}
	if last.next == nil {
		s.tail = anchor
	}
	last.next = nil
	lane.head, lane.tail = first, last
	task = s.popAssistLocked(lane)
	unlock(&lane.lock)
	unlock(&s.lock)
	return task
}

func (s *stacklessCoroScheduler) requeueAssist(lane *stacklessCoroAssistLane, task *stacklessCoroTask) bool {
	if task.parent() != nil || !s.canAssist() || getg().preempt ||
		s.runnableState.Load()&stacklessCoroForeignReturnerBit != 0 {
		return false
	}
	if task.state != stacklessCoroTaskRunning || !task.resuming || task.readyPending ||
		task.terminal != stacklessCoroTerminalNone || task.goexit || task.next != nil {
		throw("runtime: invalid coroutine assist yield")
	}
	lock(&lane.lock)
	task.state, task.resuming = stacklessCoroTaskRunnable, false
	if lane.tail == nil {
		lane.head = task
	} else {
		lane.tail.next = task
	}
	lane.tail = task
	runnable := s.runnableState.Add(1)
	unlock(&lane.lock)
	if runnable == 1 {
		s.signal()
	}
	return true
}

// Merge ready work when the helper leaves or a foreign call may block.
// The runnable count already includes these tasks.
//
//go:noinline
func (s *stacklessCoroScheduler) returnAssistReady(lane *stacklessCoroAssistLane) {
	lock(&s.lock)
	lock(&lane.lock)
	head := lane.head
	if head != nil {
		lane.tail.next = s.head
		if s.tail == nil {
			s.tail = lane.tail
		}
		s.head = head
		lane.head, lane.tail = nil, nil
	}
	unlock(&lane.lock)
	unlock(&s.lock)
	if head != nil {
		s.signal()
	}
}

func (s *stacklessCoroScheduler) retireAssistTask(lane *stacklessCoroAssistLane, task *stacklessCoroTask) bool {
	if task == &s.root || task.parent() != nil || !s.canAssist() {
		return false
	}
	if !task.canComplete() || task.next != nil || task.context.scheduler != nil {
		throw("runtime: invalid coroutine assist completion")
	}
	// Retire the source before running the next task. The batch delays
	// only scheduler-local accounting and frame reuse.
	stacklessCoroCompleteLogicalFrame(task)
	task.next, lane.retired = lane.retired, task
	lane.retiredCount++
	if lane.retiredCount == stacklessCoroAssistWidth {
		s.flushAssistCompletions(lane)
	}
	return true
}

func (s *stacklessCoroScheduler) flushAssistCompletions(lane *stacklessCoroAssistLane) {
	task := lane.retired
	if task == nil {
		return
	}
	lane.retired, lane.retiredCount = nil, 0
	lock(&s.lock)
	for task != nil {
		next := task.next
		task.next = nil
		task.finishNormal()
		s.completeLogicalTaskLocked(task)
		s.recycleTaskLocked(task)
		task = next
	}
	unlock(&s.lock)
	s.logicalTaskComplete(false)
}
