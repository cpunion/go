// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && !race && !coropullcompare && (darwin || linux)

package runtime

import (
	"internal/abi"
	"unsafe"
)

type localFusedTakeFrameForTest struct {
	stacklessCoroFusedResumeFrameHeader
	value uintptr
}

type localFusedTakeLargeFrameForTest struct {
	stacklessCoroFusedResumeFrameHeader
	value [5 << 10]byte
}

func CheckStacklessCoroLocalFusedTakeForTest(element int, shortChunk,
	heterogeneous, requested, runnable, locked bool, invalid, fallback string) bool {
	parentResume := stacklessCoroResume(func(unsafe.Pointer) uint8 {
		return stacklessCoroActionWait
	})
	childResume := parentResume
	if heterogeneous {
		childResume = func(unsafe.Pointer) uint8 { return stacklessCoroActionComplete }
	}
	if fallback == "last" {
		element = stacklessCoroFrameChunkSize - 1
		if shortChunk {
			element = stacklessCoroLargeFrameChunkSize - 1
		}
	}
	var frame unsafe.Pointer
	var size uintptr
	var chunkType *_type
	if shortChunk {
		chunk := new([stacklessCoroLargeFrameChunkSize]localFusedTakeLargeFrameForTest)
		frame = unsafe.Pointer(&chunk[element])
		size = unsafe.Sizeof(chunk[0])
		chunkType = abi.TypeFor[[stacklessCoroLargeFrameChunkSize]localFusedTakeLargeFrameForTest]()
	} else {
		chunk := new([stacklessCoroFrameChunkSize]localFusedTakeFrameForTest)
		frame = unsafe.Pointer(&chunk[element])
		size = unsafe.Sizeof(chunk[0])
		chunkType = abi.TypeFor[[stacklessCoroFrameChunkSize]localFusedTakeFrameForTest]()
	}
	header := (*stacklessCoroFusedFrameHeader)(frame)
	ancestor := &stacklessCoroFusedFrameHeader{
		parent: unsafe.Pointer(new(stacklessCoroFusedFrameHeader)),
	}
	header.parent = unsafe.Pointer(ancestor)
	previousResume := stacklessCoroResume(func(unsafe.Pointer) uint8 {
		return stacklessCoroActionYield
	})
	(*stacklessCoroFusedResumeFrameHeader)(frame).resume = stacklessCoroResumeIdentity(previousResume)
	header.marker = stacklessCoroFusedFrameChunkFirst + uint8(element) |
		stacklessCoroFusedFrameChunkValidated | stacklessCoroFusedFrameParent |
		stacklessCoroFusedFrameResume
	if shortChunk {
		header.marker |= stacklessCoroFusedFrameShortChunk
	}
	s := new(stacklessCoroScheduler)
	lockInit(&s.lock, lockRankLeafRank)
	task := &s.root
	task.resume = parentResume
	task.state = stacklessCoroTaskRunning
	task.resuming = true
	task.flags = stacklessCoroTaskFusedFrames
	task.context.scheduler, task.context.frame = s, frame
	if requested {
		task.flags |= stacklessCoroTaskFusedRequested | stacklessCoroTaskFusedChunk
	}
	if fallback == "unproved" {
		task.flags &^= stacklessCoroTaskFusedChunk
	}
	// An unrelated shared reservation and a free cached holder must survive:
	// the next slot belongs to this typed chunk rather than either list.
	reserved := new(stacklessCoroTask)
	free := new(stacklessCoroTask)
	s.reservedTasks, s.freeTasks = reserved, free
	if shortChunk {
		free.context.frame = unsafe.Pointer(new(localFusedTakeLargeFrameForTest))
	} else {
		free.context.frame = unsafe.Pointer(new(localFusedTakeFrameForTest))
	}
	free.resume = childResume
	free.flags = stacklessCoroTaskCacheFrame
	free.frameSize = uint16(size)
	s.cachedFrameTasks, s.cachedFrameBytes = 1, uint16(size)
	s.freeFrameBytes = uint16(size)
	if fallback == "owned" {
		header.marker &^= stacklessCoroFusedFrameAllocationMask
		header.owner = &stacklessCoroTask{flags: stacklessCoroTaskCacheFrame}
		header.owner.context.frame = frame
		s.cachedFrameTasks++
		s.cachedFrameBytes += uint16(size)
	}
	var sibling *stacklessCoroTask
	if runnable {
		sibling = &stacklessCoroTask{state: stacklessCoroTaskRunnable}
		s.head, s.tail = sibling, sibling
		s.runnableState.Store(1)
	}
	switch invalid {
	case "waiting":
		task.state = stacklessCoroTaskWaiting
	case "not-resuming":
		task.resuming = false
	case "ready":
		task.readyPending = true
	case "pending":
		task.flags |= stacklessCoroTaskFusedPending
	case "switch":
		task.flags |= stacklessCoroTaskSwitchPending
	case "panic":
		task.terminal = stacklessCoroTerminalPanic
	case "goexit":
		task.goexit = true
	}
	wantMarker, wantParent, wantOwner := header.marker, header.parent, header.owner
	if locked {
		lock(&s.lock)
	}
	child := coroTakeFusedFrame(unsafe.Pointer(&task.context), childResume,
		size, chunkType, !requested)
	if locked {
		unlock(&s.lock)
	}
	if fallback != "" {
		return child == free.context.frame &&
			task.flags == stacklessCoroTaskFusedFrames|stacklessCoroTaskFusedPending &&
			s.freeTasks == nil && s.reservedTasks == free && free.next == reserved &&
			free.parent == task && s.freeFrameBytes == 0 &&
			header.marker == wantMarker && header.parent == wantParent && header.owner == wantOwner &&
			task.context.frame == frame && s.head == sibling && s.tail == sibling
	}
	wantFlags := stacklessCoroTaskFusedFrames | stacklessCoroTaskFusedPending
	if heterogeneous {
		wantFlags |= stacklessCoroTaskFusedChunk
	}
	childHeader := (*stacklessCoroFusedFrameHeader)(child)
	return child == add(frame, size) && task.flags == wantFlags &&
		task.context.frame == frame && task.state == stacklessCoroTaskRunning &&
		task.resuming && !task.readyPending && !task.goexit &&
		task.terminal == stacklessCoroTerminalNone &&
		header.marker == wantMarker && header.parent == wantParent && header.owner == nil &&
		childHeader.parent == nil && childHeader.owner == nil && childHeader.marker == 0 &&
		s.reservedTasks == reserved && s.freeTasks == free &&
		s.cachedFrameTasks == 1 && s.cachedFrameBytes == uint16(size) &&
		s.freeFrameBytes == uint16(size) &&
		s.head == sibling && s.tail == sibling &&
		s.runnableState.Load() == uint32(bool2int(runnable))
}
