// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && !race && ((darwin && arm64) || (linux && amd64))

package runtime

import (
	"internal/runtime/sys"
	"unsafe"
)

func StacklessCoroNativeStackForTest() (native bool, sp, lo, hi, g0lo, g0hi uintptr) {
	gp := getg()
	return gp.stackIsFixed(), sys.GetCallerSP(), gp.stack.lo, gp.stack.hi, gp.m.g0.stack.lo, gp.m.g0.stack.hi
}

func StacklessCoroNativeSignalTargetForTest() *M {
	return getg().m
}

func SignalStacklessCoroNativeForTest(target *M) {
	signalM(target, _SIGPROF)
}

func StacklessCoroNativePoolForTest() (count int) {
	lock(&stacklessCoroNativePool.lock)
	count = stacklessCoroNativePool.count
	unlock(&stacklessCoroNativePool.lock)
	return
}

func StacklessCoroNativePoolTaskCacheForTest() (warm, overflow int) {
	for i := range stacklessCoroNativePool.slots {
		slot := &stacklessCoroNativePool.slots[i]
		for {
			ctx := slot.Load()
			if ctx == nil {
				break
			}
			if !slot.CompareAndSwap(ctx, nil) {
				continue
			}
			warm += stacklessCoroTaskListLen(ctx.freeTasks)
			if !slot.CompareAndSwap(nil, ctx) {
				releaseStacklessCoroNativeContext(ctx)
			}
			break
		}
	}
	lock(&stacklessCoroNativePool.lock)
	for ctx := stacklessCoroNativePool.overflow; ctx != nil; ctx = ctx.poolNext {
		overflow += stacklessCoroTaskListLen(ctx.freeTasks)
	}
	unlock(&stacklessCoroNativePool.lock)
	return
}

func StacklessCoroClearNativeTaskCachesForTest() int {
	count := 0
	for i := range stacklessCoroNativePool.slots {
		slot := &stacklessCoroNativePool.slots[i]
		for {
			ctx := slot.Load()
			if ctx == nil {
				break
			}
			if !slot.CompareAndSwap(ctx, nil) {
				continue
			}
			ctx.freeTasks = nil
			ctx.freePlainTaskCount = 0
			ctx.freeFrameBytes = 0
			ctx.cachedFrameTasks = 0
			ctx.cachedFrameBytes = 0
			if !slot.CompareAndSwap(nil, ctx) {
				releaseStacklessCoroNativeContext(ctx)
			}
			count++
			break
		}
	}
	return count
}

var stacklessCoroNativeReplacementDrainTest struct {
	runs   int
	native bool
}

func stacklessCoroNativeReplacementDrainResume(unsafe.Pointer) uint8 {
	state := &stacklessCoroNativeReplacementDrainTest
	state.native = state.native && getg().stackIsFixed()
	state.runs++
	return stacklessCoroActionComplete
}

func StacklessCoroNativeReplacementDrainForTest() (runs int, native bool) {
	s := &stacklessCoroScheduler{
		wake: make(chan struct{}, stacklessCoroWarmExecutorCount),
	}
	lockInit(&s.lock, lockRankLeafRank)
	s.root.state = stacklessCoroTaskWaiting
	s.executorCount.Store(2)
	s.executorState.Store(stacklessCoroExecutorStateRunning)

	state := &stacklessCoroNativeReplacementDrainTest
	state.runs = 0
	state.native = true
	for range 2 {
		s.ready(&stacklessCoroTask{
			resume: stacklessCoroNativeReplacementDrainResume,
		}, false)
		if coroRunOnNativeStack(s) == nil {
			state.native = false
			break
		}
	}
	return state.runs, state.native
}

func stacklessCoroTaskListLen(task *stacklessCoroTask) int {
	count := 0
	for ; task != nil; task = task.next {
		count++
	}
	return count
}

func WriteStacklessCoroNativeForTest(fd int, value byte) int {
	return int(write(uintptr(fd), unsafe.Pointer(&value), 1))
}
