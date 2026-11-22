// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro

package runtime

import "internal/runtime/atomic"

const stacklessCoroAssistWidth = 4

type stacklessCoroAssistLane struct {
	lock       mutex
	wake       chan struct{}
	head, tail *stacklessCoroTask
	// The driver owns the retired list. These tasks are already absent
	// from logical goroutine snapshots, though their records may still
	// need to be unlinked. Other drivers can take runnable tasks.
	retired      *stacklessCoroTask
	leased       atomic.Uint32
	enabled      atomic.Bool
	wide         bool
	retiredCount uint8
}

// The manager publishes this set before opening more executors. The
// original assistant keeps its lane through the transition. Each widened
// driver leases one lane per native episode; any driver can steal its work.
type stacklessCoroDispatchLanes struct {
	assistant *stacklessCoroAssistLane
	lanes     []stacklessCoroAssistLane
}

func (r *stacklessCoroReplacementState) assistant() *stacklessCoroAssistLane {
	if dispatch := r.dispatch.Load(); dispatch != nil {
		return dispatch.assistant
	}
	return nil
}

func (r *stacklessCoroReplacementState) enableDispatchBatching() {
	dispatch := &stacklessCoroDispatchLanes{
		assistant: r.assistant(),
		lanes:     make([]stacklessCoroAssistLane, min(gomaxprocs, stacklessCoroMaxWarmExecutors)),
	}
	for i := range dispatch.lanes {
		lane := &dispatch.lanes[i]
		lockInit(&lane.lock, lockRankStacklessCoroCache)
		lane.wide = true
	}
	r.dispatch.Store(dispatch)
}

func (s *stacklessCoroScheduler) acquireDispatchLane() *stacklessCoroAssistLane {
	replacements := s.replacements.Load()
	if replacements == nil {
		return nil
	}
	dispatch := replacements.dispatch.Load()
	if dispatch == nil || len(dispatch.lanes) == 0 || !s.canAssist() {
		return nil
	}
	for i := range dispatch.lanes {
		lane := &dispatch.lanes[i]
		if lane.leased.Load() == 0 && lane.leased.CompareAndSwap(0, 1) {
			return lane
		}
	}
	return nil
}

func (s *stacklessCoroScheduler) releaseDispatchLane(lane *stacklessCoroAssistLane) {
	if !lane.wide {
		s.returnAssistReady(lane)
		return
	}
	// Nobody else adds tasks to this driver's lane. An empty lane stays
	// empty until its next owner acquires the lease.
	lock(&lane.lock)
	remaining := lane.head != nil
	unlock(&lane.lock)
	if remaining {
		s.returnAssistReady(lane)
	}
	lane.leased.Store(0)
}

//go:noinline
func (s *stacklessCoroScheduler) returnDispatchReady(dispatch *stacklessCoroDispatchLanes) {
	if lane := dispatch.assistant; lane != nil {
		s.returnAssistReady(lane)
		lane.signal()
	}
	for i := range dispatch.lanes {
		s.returnAssistReady(&dispatch.lanes[i])
	}
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
	replacements := s.replacements.Load()
	if replacements == nil {
		return nil
	}
	dispatch := replacements.dispatch.Load()
	if dispatch == nil {
		return nil
	}
	if lane := dispatch.assistant; lane != nil {
		if task := s.takeDispatchReady(lane); task != nil {
			return task
		}
	}
	for i := range dispatch.lanes {
		if task := s.takeDispatchReady(&dispatch.lanes[i]); task != nil {
			return task
		}
	}
	return nil
}

func (s *stacklessCoroScheduler) takeDispatchReady(lane *stacklessCoroAssistLane) *stacklessCoroTask {
	lock(&lane.lock)
	task := s.popAssistLocked(lane)
	unlock(&lane.lock)
	return task
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
	if lane.wide && lane.retired != nil {
		stacklessCoroUnlinkRetired(lane.retired)
	}
	lock(&s.lock)
	flushed := lane.wide && s.flushAssistCompletionsLocked(lane)
	first := s.head
	var anchor *stacklessCoroTask
	if first == &s.root {
		anchor, first = first, first.next
	}
	if first == nil || first.parent() != nil || !s.canAssist() {
		unlock(&s.lock)
		if flushed {
			s.logicalTaskComplete(false)
		}
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
	if flushed {
		s.logicalTaskComplete(false)
	}
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
	// Retire the source before running the next task. A widened driver
	// also batches registry unlinking with the next scheduler refill.
	if !lane.wide || !stacklessCoroMarkRetired(task) {
		stacklessCoroCompleteLogicalFrame(task)
	}
	task.next, lane.retired = lane.retired, task
	lane.retiredCount++
	if lane.retiredCount == 2*stacklessCoroAssistWidth ||
		!lane.wide && lane.retiredCount == stacklessCoroAssistWidth {
		s.flushAssistCompletions(lane)
	}
	return true
}

func (s *stacklessCoroScheduler) flushAssistCompletions(lane *stacklessCoroAssistLane) {
	if lane.retired != nil {
		if lane.wide {
			stacklessCoroUnlinkRetired(lane.retired)
		}
		lock(&s.lock)
		s.flushAssistCompletionsLocked(lane)
		unlock(&s.lock)
		s.logicalTaskComplete(false)
	}
}

func (s *stacklessCoroScheduler) flushAssistCompletionsLocked(lane *stacklessCoroAssistLane) bool {
	task := lane.retired
	if task == nil {
		return false
	}
	lane.retired, lane.retiredCount = nil, 0
	for task != nil {
		next := task.next
		task.next = nil
		task.finishNormal()
		s.completeLogicalTaskLocked(task)
		s.recycleTaskLocked(task)
		task = next
	}
	return true
}

func stacklessCoroUnlinkRetired(tasks *stacklessCoroTask) {
	lock(&stacklessCoroGoroutines.lock)
	var removed, unhosted int64
	for task := tasks; task != nil; task = task.next {
		if task.source() != nil {
			r, u := stacklessCoroCompleteLogicalFrameLocked(task)
			removed += r
			unhosted += u
		}
	}
	stacklessCoroFinishLogicalRetirementLocked(removed, unhosted)
	unlock(&stacklessCoroGoroutines.lock)
}
