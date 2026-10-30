// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && ((darwin && arm64) || (linux && amd64))

package runtime

import (
	"internal/abi"
	"unsafe"
)

func CheckStacklessCoroMixedScalarSlotsForTest() bool {
	if raceenabled {
		return true
	}
	s := new(stacklessCoroScheduler)
	lockInit(&s.lock, lockRankStacklessCoro)
	s.batchedTaskCounts = stacklessCoroTypedTaskFrameAllocationMode
	lock(&s.lock)
	for _, live := range [2]uint64{0, stacklessCoroTaskCacheSize + stacklessCoroDirectBatchedTaskCount} {
		s.liveSpawns = live
		task := s.newScalarFrameTaskLocked(stacklessCoroScalarTaskFrameTestResume, nil)
		if task.context.frame != nil || task.frameSize != 0 || s.freeTasks != nil {
			unlock(&s.lock)
			return false
		}
	}
	s.liveSpawns++
	s.cachedFrameTasks = stacklessCoroTaskCacheSize
	first := s.newScalarFrameTaskLocked(stacklessCoroScalarTaskFrameTestResume, nil)
	if !isStacklessCoroScalarTaskCacheMarker(first.frameSize) || !s.onlyUnusedScalarFrameTasksLocked() {
		unlock(&s.lock)
		return false
	}
	chunk, index := stacklessCoroScalarTaskChunkFor(first)
	if index != 0 || first.context.frame != unsafe.Pointer(&chunk.frames[0]) {
		unlock(&s.lock)
		return false
	}
	head := s.freeTasks
	// Reject a list that could contain ordinary, overflow, or unpaired slots.
	frame := head.context.frame
	head.context.frame = nil
	valid := !s.onlyUnusedScalarFrameTasksLocked()
	head.context.frame = frame
	s.freePlainTaskCount = 1
	valid = valid && !s.onlyUnusedScalarFrameTasksLocked()
	s.freePlainTaskCount = 0
	counts := s.batchedTaskCounts
	for _, count := range [2]uint8{0, stacklessCoroTaskChunkSize} {
		s.batchedTaskCounts = stacklessCoroTypedTaskFrameAllocationMode |
			count<<stacklessCoroFreeBatchedTaskCountShift
		valid = valid && !s.onlyUnusedScalarFrameTasksLocked()
	}
	s.batchedTaskCounts = counts
	s.root.context.scheduler = s
	s.root.state = stacklessCoroTaskRunning
	s.root.resuming = true
	unlock(&s.lock)
	if !valid {
		return false
	}
	// This is the real factory; without slot isolation it steals a scalar
	// member and returns nil for its separately allocated typed frame.
	typed := coroTakeTypedTaskFrame(unsafe.Pointer(&s.root.context), stacklessCoroSmallTaskFrameTestResume,
		unsafe.Sizeof(stacklessCoroSmallTaskFrameTestFrame{}), abi.TypeFor[stacklessCoroSmallTaskFrameTestAllocation]())
	if typed == nil || s.freeTasks != head || !isStacklessCoroTypedTaskCacheMarker(s.reservedTasks.frameSize) {
		return false
	}
	lock(&s.lock)
	var seen [4]bool
	seen[0] = true
	for range 3 {
		task := s.newScalarFrameTaskLocked(stacklessCoroScalarTaskFrameTestResume, nil)
		if !isStacklessCoroScalarTaskCacheMarker(task.frameSize) {
			unlock(&s.lock)
			return false
		}
		other, i := stacklessCoroScalarTaskChunkFor(task)
		if other != chunk || seen[i] || task.context.frame != unsafe.Pointer(&chunk.frames[i]) {
			unlock(&s.lock)
			return false
		}
		seen[i] = true
	}
	valid = s.freeTasks == nil && s.batchedTaskCounts == stacklessCoroTypedTaskFrameAllocationMode &&
		s.overflowTaskCounts&stacklessCoroFreeBatchedTaskPresent == 0 && !s.onlyUnusedScalarFrameTasksLocked()
	unlock(&s.lock)
	return valid
}
