// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && !race && ((darwin && arm64) || (linux && amd64))

package runtime

import "unsafe"

type StacklessCoroParkedFrameSnapshot struct {
	FreeFrames, FreePlain, Reserved, CachedFrames, FreeBytes, CachedBytes int
}

// The caller holds the root at a channel wait and disables automatic GC.
func StacklessCoroParkedFramesForTest(pointer unsafe.Pointer) StacklessCoroParkedFrameSnapshot {
	s := (*stacklessCoroScheduler)(pointer)
	lock(&s.lock)
	defer unlock(&s.lock)
	x := StacklessCoroParkedFrameSnapshot{
		FreePlain: int(s.freePlainTaskCount), CachedFrames: int(s.cachedFrameTasks),
		FreeBytes: int(s.freeFrameBytes), CachedBytes: int(s.cachedFrameBytes),
	}
	for task := s.freeTasks; task != nil; task = task.next {
		if task.hasFlag(stacklessCoroTaskCacheFrame) {
			x.FreeFrames++
		}
	}
	for task := s.reservedTasks; task != nil; task = task.next {
		x.Reserved++
	}
	return x
}

// The caller quiesces native episodes and uses one P for this snapshot.
func StacklessCoroNativeIdleStackBytesForTest() (warm, shared uintptr) {
	for i := range stacklessCoroNativePool.slots {
		if ctx := stacklessCoroNativePool.slots[i].Load(); ctx != nil && ctx.schedulerG != nil {
			warm += ctx.schedulerG.stack.hi - ctx.schedulerG.stack.lo
		}
	}
	for i := range stacklessCoroNativeSchedulerStacks {
		if gp := stacklessCoroNativeSchedulerStacks[i].Load(); gp != nil {
			shared += gp.stack.hi - gp.stack.lo
		}
	}
	return
}
