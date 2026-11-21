// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro

package runtime

import (
	"internal/runtime/sys"
	"unsafe"
)

const stacklessCoroSpawnBatchSize = 16

// coroSpawnScalarBatch runs a bounded portion of a compiler-proven counting
// loop whose only effect is starting identical scalar-frame goroutines.
func coroSpawnScalarBatch(ctx unsafe.Pointer, child stacklessCoroResume, pc uint32, remaining int) (int, bool) {
	context := (*stacklessCoroContext)(ctx)
	parent := context.task()
	if parent == nil || context.scheduler == nil || child == nil || remaining <= 0 {
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
	limit := min(remaining, len(tasks))
	ready := s.runnableState.Load() &^ stacklessCoroForeignReturnerBit
	reserve := !raceenabled && !isStacklessCoroUncachedFrameLineage(parent.frameSize)
	for count < limit {
		var task *stacklessCoroTask
		var frame unsafe.Pointer
		if reserve {
			task = s.reserveFrameTaskLocked(parent, child, unsafe.Sizeof(pc))
			frame = task.context.frame
		}
		if frame == nil {
			frame = unsafe.Pointer(new(uint32))
		}
		*(*uint32)(frame) = pc
		if task == nil {
			task = s.newTaskLocked(frame, child, nil)
		} else {
			task.parent = nil
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
	for _, task := range tasks[:count] {
		s.publishReadyLocked(task, &queued)
	}
	var runnable uint32
	if queued != 0 {
		runnable = s.runnableState.Add(queued)
	}
	unlock(&s.lock)
	s.prepareReplacementExecutors()
	if host := currentStacklessCoroHost(); host != nil && host.scheduler == s {
		s.signal()
	}
	s.wakeReplacementExecutor(live, runnable)
	return count, yield
}
