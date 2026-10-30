// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro

package runtime

import (
	"internal/abi"
	"unsafe"
)

// These four markers identify the complete GC bitmap of an individual
// sixteen-byte frame. They remain uncached-lineage markers while active;
// only a completed independent spawn may retain its cleared storage.
const (
	stacklessCoroTypedTaskCacheFirst = stacklessCoroShortFrameChunkLast - 1
	stacklessCoroTypedTaskCacheLast  = stacklessCoroTypedTaskCacheFirst - 3
)

func isStacklessCoroTypedTaskCacheMarker(marker uint16) bool {
	return marker <= stacklessCoroTypedTaskCacheFirst && marker >= stacklessCoroTypedTaskCacheLast
}

func validStacklessCoroTaskFrameType(typ *_type, size uintptr) bool {
	headerSize := unsafe.Sizeof(stacklessCoroTask{})
	if unsafe.Sizeof(uintptr(0)) != 8 ||
		headerSize != abi.CoroTaskFrameHeaderWords*8 ||
		size == 0 || size%16 != 0 || size > abi.CoroTaskFrameMaxBytes ||
		typ == nil || typ.Kind() != abi.Struct ||
		typ.Size_ != headerSize+size || typ.Align_ != 8 ||
		typ.PtrBytes < headerSize || typ.GCData == nil ||
		typ.TFlag&abi.TFlagGCMaskOnDemand != 0 ||
		*typ.GCData&0x3f != abi.CoroTaskFrameHeaderPointerMask {
		return false
	}
	fields := (*abi.StructType)(unsafe.Pointer(typ)).Fields
	return len(fields) == 2 && fields[0].Offset == 0 &&
		fields[0].Typ.Size_ == headerSize &&
		fields[1].Offset == headerSize && fields[1].Typ.Size_ == size &&
		*abi.TypeFor[stacklessCoroTask]().GCData&0x3f == abi.CoroTaskFrameHeaderPointerMask
}

// coroTakeTypedTaskFrame preserves the normal frame reservation/cache prefix.
// Only a cache-full reservation with no reusable task receives a combined
// allocation. Only eligible two-word frames can reuse cleared allocations
// from completed independent spawns; structured results retain their lifetime.
func coroTakeTypedTaskFrame(ctx unsafe.Pointer, child stacklessCoroResume,
	size uintptr, allocationType *_type) unsafe.Pointer {
	if ctx == nil || raceenabled || unsafe.Sizeof(uintptr(0)) != 8 ||
		size > stacklessCoroFrameCacheSize {
		return coroTakeFrame(ctx, child, size)
	}
	context := (*stacklessCoroContext)(ctx)
	parent := context.task()
	if context.scheduler == nil || parent == nil || child == nil {
		throw("runtime: invalid stackless coroutine frame reservation")
	}
	s := context.scheduler
	if isStacklessCoroUncachedFrameLineage(parent.frameSize) {
		return nil
	}
	lock(&s.lock)
	if parent.state != stacklessCoroTaskRunning || !parent.resuming ||
		parent.readyPending || parent.terminal != stacklessCoroTerminalNone || parent.goexit {
		unlock(&s.lock)
		throw("runtime: stackless coroutine frame reservation outside resume")
	}
	task := s.takeCachedFrameTaskLocked(child, uint16(size))
	if task == nil {
		if s.cachedFrameTasks == stacklessCoroTaskCacheSize &&
			s.freeFrameBytes == 0 && s.freeTasks == nil {
			// The full task cache need not contain one uniform frame size.
			// Each eligible combined allocation has the same byte charge as
			// its task share and separate frame. Record the allocation mode
			// so later scalar reservations cannot leave unused paired slots.
			if !validStacklessCoroTaskFrameType(allocationType, size) {
				unlock(&s.lock)
				throw("runtime: invalid stackless coroutine task-frame allocation type")
			}
			cached, marker := takeStacklessCoroCompletedTask(allocationType, size)
			storage := unsafe.Pointer(cached)
			if storage == nil {
				storage = newobject(allocationType)
			}
			s.batchedTaskCounts = s.batchedTaskCounts&^stacklessCoroDirectBatchedTaskCountMask | stacklessCoroTypedTaskFrameAllocationMode
			task = initializeStacklessCoroTask((*stacklessCoroTask)(storage),
				add(storage, unsafe.Sizeof(stacklessCoroTask{})), child, parent)
			task.frameSize = marker
		} else {
			task = s.newFrameReservationTaskLocked(child, parent, size)
			if s.cachedFrameTasks < stacklessCoroTaskCacheSize &&
				uintptr(s.cachedFrameBytes)+size <= stacklessCoroFrameCacheSize {
				task.setFlag(stacklessCoroTaskCacheFrame, true)
				task.frameSize = uint16(size)
				s.cachedFrameTasks++
				s.cachedFrameBytes += uint16(size)
			} else if s.cachedFrameTasks == stacklessCoroTaskCacheSize && s.freeFrameBytes == 0 {
				task.frameSize = stacklessCoroUncachedFrameLineage
			}
		}
	} else {
		task.parent = parent
	}
	task.next = s.reservedTasks
	s.reservedTasks = task
	frame := task.context.frame
	unlock(&s.lock)
	return frame
}
