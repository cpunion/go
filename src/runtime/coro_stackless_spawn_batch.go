// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro

package runtime

import (
	"internal/abi"
	"internal/runtime/sys"
	"unsafe"
)

const stacklessCoroSpawnBatchSize = 16

// coroSpawnScalarBatch runs a bounded portion of a compiler-proven counting
// loop whose only effect is starting identical pointer-free goroutine frames.
func coroSpawnScalarBatch(ctx unsafe.Pointer, child stacklessCoroResume, typ *_type, pcOffset uintptr, pc uint32, remaining int) (int, bool) {
	context := (*stacklessCoroContext)(ctx)
	parent := context.task()
	if parent == nil || context.scheduler == nil || child == nil || remaining <= 0 ||
		typ == nil || typ.Pointers() || typ.Size_ < unsafe.Sizeof(pc) ||
		typ.Size_ > abi.CoroSpawnBatchFrameBytes ||
		pcOffset > typ.Size_-unsafe.Sizeof(pc) || pcOffset%unsafe.Alignof(pc) != 0 {
		throw("runtime: invalid stackless coroutine spawn batch")
	}
	callerpc, callersp := sys.GetCallerPC(), unsafe.Pointer(sys.GetCallerSP())
	s := context.scheduler
	lock(&s.lock)
	if parent.state != stacklessCoroTaskRunning || !parent.resuming ||
		parent.readyPending || parent.terminal != stacklessCoroTerminalNone || parent.goexit {
		unlock(&s.lock)
		throw("runtime: stackless coroutine spawn batch outside resume")
	}
	var tasks [stacklessCoroSpawnBatchSize]*stacklessCoroTask
	count := 0
	yield := false
	size := typ.Size_
	limit := min(remaining, len(tasks), abi.CoroSpawnBatchFrameBytes/int(size))
	// Yield at the end of this prefix so the owner can consume it
	// before the producer resumes the rest of this small counting loop.
	handoff := gomaxprocs > 1 && !raceenabled && remaining > limit && remaining >= 8 && remaining <= 64 &&
		parent == &s.root && !s.spawned() && !stacklessCoroIsPullComparison(s)
	ready := s.runnableState.Load() &^ stacklessCoroForeignReturnerBit
	reserve := !raceenabled && !isStacklessCoroUncachedFrameLineage(parent.frameSize)
	for count < limit {
		var task *stacklessCoroTask
		var frame unsafe.Pointer
		if reserve {
			task = s.reserveFrameTaskLocked(parent, child, size)
			frame = task.context.frame
		}
		if frame == nil {
			frame = newobject(typ)
		} else if size != unsafe.Sizeof(pc) {
			memclrNoHeapPointers(frame, size)
		}
		*(*uint32)(add(frame, pcOffset)) = pc
		if task == nil {
			task = s.newTaskLocked(frame, child, nil)
		} else {
			task.setParent(nil)
			task.context.frame = frame
		}
		s.markUncachedFrameLineageLocked(task, parent, frame)
		if isStacklessCoroCompletedTaskCacheMarker(task.frameSize) {
			task.setFlag(stacklessCoroTaskTypedSpawn, true)
		}
		if s.liveSpawns == ^uint64(0) {
			throw("runtime: stackless coroutine spawn count overflow")
		}
		s.liveSpawns++
		tasks[count] = task
		count++
		yield = ready+uint32(count) >= stacklessCoroTaskCacheSize/2 && s.liveSpawns <= stacklessCoroTaskCacheSize
		if yield {
			break
		}
	}
	live := s.liveSpawns
	s.registerGoroutinesLocked(tasks[:count], parent, callerpc, callersp)
	s.lifetime.Or(stacklessCoroLifetimeSpawned)
	var queued int32
	if !raceenabled && !stacklessCoroIsPullComparison(s) && s.terminalValues == nil {
		queued = s.publishSpawnBatchLocked(tasks[:count])
	} else {
		for _, task := range tasks[:count] {
			s.publishReadyLocked(task, &queued)
		}
	}
	var runnable uint32
	if queued != 0 {
		runnable = s.runnableState.Add(queued)
	}
	unlock(&s.lock)
	s.prepareReplacementExecutorsForInitialBatch(handoff && remaining <= stacklessCoroSpawnBatchSize)
	if host := currentStacklessCoroHost(); host != nil && host.scheduler == s {
		s.signal()
	}
	s.wakeReplacementExecutor(live, runnable)
	return count, yield || handoff
}

// coroSpawnFrameBatch publishes consecutive spawns with the same pointer-
// bearing frame type and source go statement.
func coroSpawnFrameBatch(ctx unsafe.Pointer, child stacklessCoroResume, typ, allocationType *_type,
	pcOffset uintptr, pc uint32, remaining int) (int, bool) {
	context := (*stacklessCoroContext)(ctx)
	parent := context.task()
	if parent == nil || context.scheduler == nil || child == nil || remaining <= 0 ||
		typ == nil || !typ.Pointers() || typ.Size_ < unsafe.Sizeof(pc) ||
		typ.Size_ > abi.CoroSpawnBatchFrameBytes ||
		pcOffset > typ.Size_-unsafe.Sizeof(pc) || pcOffset%unsafe.Alignof(pc) != 0 {
		throw("runtime: invalid stackless coroutine spawn batch")
	}
	callerpc, callersp := sys.GetCallerPC(), unsafe.Pointer(sys.GetCallerSP())
	s := context.scheduler
	lock(&s.lock)
	if parent.state != stacklessCoroTaskRunning || !parent.resuming ||
		parent.readyPending || parent.terminal != stacklessCoroTerminalNone || parent.goexit {
		unlock(&s.lock)
		throw("runtime: stackless coroutine spawn batch outside resume")
	}
	var tasks [stacklessCoroSpawnBatchSize]*stacklessCoroTask
	count := 0
	yield := false
	size := typ.Size_
	limit := min(remaining, len(tasks), abi.CoroSpawnBatchFrameBytes/int(size))
	// Yield at the end of this prefix so the owner can consume it
	// before the producer resumes the rest of this small counting loop.
	handoff := gomaxprocs > 1 && !raceenabled && remaining > limit && remaining >= 8 && remaining <= 64 &&
		parent == &s.root && !s.spawned() && !stacklessCoroIsPullComparison(s)
	ready := s.runnableState.Load() &^ stacklessCoroForeignReturnerBit
	reserve := !raceenabled && !isStacklessCoroUncachedFrameLineage(parent.frameSize)
	for count < limit {
		var task *stacklessCoroTask
		var frame unsafe.Pointer
		if reserve {
			if allocationType != nil && unsafe.Sizeof(uintptr(0)) == 8 {
				task = s.reserveTypedFrameTaskLocked(parent, child, size, allocationType)
			} else {
				task = s.reserveFrameTaskLocked(parent, child, size)
			}
			frame = task.context.frame
		}
		if frame == nil {
			frame = newobject(typ)
		} else {
			typedmemclr(typ, frame)
		}
		*(*uint32)(add(frame, pcOffset)) = pc
		if task == nil {
			task = s.newTaskLocked(frame, child, nil)
		} else {
			task.setParent(nil)
			task.context.frame = frame
		}
		s.markUncachedFrameLineageLocked(task, parent, frame)
		if isStacklessCoroCompletedTaskCacheMarker(task.frameSize) {
			task.setFlag(stacklessCoroTaskTypedSpawn, true)
		}
		if s.liveSpawns == ^uint64(0) {
			throw("runtime: stackless coroutine spawn count overflow")
		}
		s.liveSpawns++
		tasks[count] = task
		count++
		yield = ready+uint32(count) >= stacklessCoroTaskCacheSize/2 && s.liveSpawns <= stacklessCoroTaskCacheSize
		if yield {
			break
		}
	}
	live := s.liveSpawns
	s.registerGoroutinesLocked(tasks[:count], parent, callerpc, callersp)
	s.lifetime.Or(stacklessCoroLifetimeSpawned)
	var queued int32
	if !raceenabled && !stacklessCoroIsPullComparison(s) && s.terminalValues == nil {
		queued = s.publishSpawnBatchLocked(tasks[:count])
	} else {
		for _, task := range tasks[:count] {
			s.publishReadyLocked(task, &queued)
		}
	}
	var runnable uint32
	if queued != 0 {
		runnable = s.runnableState.Add(queued)
	}
	unlock(&s.lock)
	s.prepareReplacementExecutorsForInitialBatch(handoff && remaining <= stacklessCoroSpawnBatchSize)
	if host := currentStacklessCoroHost(); host != nil && host.scheduler == s {
		s.signal()
	}
	s.wakeReplacementExecutor(live, runnable)
	return count, yield || handoff
}

// publishSpawnBatchLocked appends new independent tasks to the shared FIFO
// in spawn order. The caller holds s.lock, has ruled out host routing, and
// publishes the ready count before releasing the lock.
func (s *stacklessCoroScheduler) publishSpawnBatchLocked(tasks []*stacklessCoroTask) int32 {
	var first *stacklessCoroTask
	for i := len(tasks) - 1; i >= 0; i-- {
		task := tasks[i]
		if task.state != stacklessCoroTaskNew || task.resuming || task.readyPending || task.next != nil {
			throw("runtime: invalid stackless coroutine spawn batch ready transition")
		}
		task.state = stacklessCoroTaskRunnable
		task.next = first
		first = task
	}
	if s.tail == nil {
		s.head = first
	} else {
		s.tail.next = first
	}
	s.tail = tasks[len(tasks)-1]
	if s.initialRootPrivate {
		s.initialRootPrivate = false
	}
	s.queueProgress += uint16(len(tasks))
	if s.executorMonitorIdle {
		s.executorMonitorIdle = false
		select {
		case s.executorGrow <- struct{}{}:
		default:
		}
	}
	return int32(len(tasks))
}

func coroUseOrdinarySpawnBatch(count int) bool {
	return stacklessCoroLinuxARM64Managed && gomaxprocs >= 2 && !raceenabled && count >= 8 && count <= 64 &&
		getg().stacklessCoro == nil && !getg().stackIsFixed() && !coroUseOrdinary()
}

// coroOrdinarySpawnBatch distributes a prefix to idle processors and then
// publishes the remainder on this processor. The compiler supplies a static
// zero-argument entry and a private loop bound.
func coroOrdinarySpawnBatch(fn func(), count int) {
	caller := getg()
	pc := sys.GetCallerPC()
	entry := *(**funcval)(unsafe.Pointer(&fn))
	systemstack(func() {
		pp := getg().m.p.ptr()
		if mainStarted && gomaxprocs > 1 {
			var prefix gQueue
			n := min(count, 3, int(gomaxprocs)-1)
			for range n {
				prefix.pushBack(newproc1(entry, caller, pc, false, waitReasonZero))
			}
			injectRunnableGQueue(&prefix)
			count -= n
		}
		for range count {
			runqput(pp, newproc1(entry, caller, pc, false, waitReasonZero), false)
		}
		if mainStarted {
			wakep()
		}
	})
}

func coroOrdinaryBatchYield() {
	checkTimeouts()
	mcall(coroOrdinaryBatchYield_m)
}

func coroOrdinaryBatchYield_m(gp *g) {
	goschedImpl(gp, false, !gp.preempt)
}
