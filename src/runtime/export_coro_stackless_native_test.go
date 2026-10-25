// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && !race && ((darwin && arm64) || (linux && amd64))

package runtime

import (
	"internal/abi"
	"internal/runtime/gc"
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

func StacklessCoroDeferSleepOutsideExecutorForTest() bool {
	return stacklessCoroDeferSleep(nil, nil, 0)
}

func StacklessCoroNativeContextAllocationSizeForTest() uintptr {
	return roundupsize(unsafe.Sizeof(stacklessCoroNativeContext{}), false)
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

func stacklessCoroNativeSelfFrameCacheBytes(ctx *stacklessCoroNativeContext) uintptr {
	return uintptr(ctx.selfFrameCount)*ctx.selfFrameSize +
		uintptr(ctx.selfFrameChunkCount)*ctx.selfFrameChunkSize
}

func StacklessCoroNativePoolSelfFrameCacheForTest() (warm, overflow,
	perContextLimit, chunkLimit uintptr) {
	chunkLimit = stacklessCoroSelfFrameChunkCacheSize
	perContextLimit = stacklessCoroSelfFrameBurstCacheSize +
		stacklessCoroFrameChunkDirectCount*
			(stacklessCoroSelfFrameBurstCacheSize/stacklessCoroLargeFrameChunkSize)
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
			warm += stacklessCoroNativeSelfFrameCacheBytes(ctx)
			if !slot.CompareAndSwap(nil, ctx) {
				releaseStacklessCoroNativeContext(ctx)
			}
			break
		}
	}
	lock(&stacklessCoroNativePool.lock)
	for ctx := stacklessCoroNativePool.overflow; ctx != nil; ctx = ctx.poolNext {
		overflow += stacklessCoroNativeSelfFrameCacheBytes(ctx)
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
			ctx.selfFrames = nil
			ctx.selfFrameChunks = nil
			ctx.selfFrameResume = nil
			ctx.selfFrameChunkType = nil
			ctx.selfFrameSize = 0
			ctx.selfFrameChunkSize = 0
			ctx.selfFrameChunkCount = 0
			ctx.selfFrameCount = 0
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
		wake:       make(chan struct{}, stacklessCoroWarmExecutorCount),
		liveSpawns: 2,
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

// Construct typed idle caches on eight independently acquired executors. This
// exercises the real warm/overflow release and GC hooks without scheduler
// timing determining how many roots happen to overlap.
func StacklessCoroNativeBurstContextsForTest(oversized bool) (before, first, second, overflow, limit uintptr) {
	StacklessCoroClearNativeTaskCachesForTest()
	contexts := [2 * stacklessCoroWarmExecutorCount]*stacklessCoroNativeContext{}
	for i := range contexts {
		contexts[i] = acquireStacklessCoroNativeContext()
	}
	for _, ctx := range contexts {
		var typ *_type
		var frameSize uintptr
		if oversized {
			typ = abi.TypeFor[[4]struct {
				Header stacklessCoroFusedFrameHeader
				Data   [70000]byte
			}]()
		} else {
			typ = abi.TypeFor[[4]struct {
				Header stacklessCoroFusedFrameHeader
				Data   [5120]byte
			}]()
		}
		frameSize = typ.Size_ / 4
		class := roundupsize(typ.Size_, false)
		if typ.Size_ > gc.MinSizeForMallocHeader {
			class += gc.MallocHeaderSize
		}
		ctx.selfFrameSize = frameSize
		ctx.selfFrameChunkSize = class
		ctx.selfFrameChunkType = typ
		for n := uintptr(0); n < stacklessCoroSelfFrameChunkBudget(frameSize)/class; n++ {
			p := mallocgc(typ.Size_, typ, true)
			(*stacklessCoroFusedFrameHeader)(p).parent = ctx.selfFrameChunks
			ctx.selfFrameChunks = p
			ctx.selfFrameChunkCount++
		}
		releaseStacklessCoroNativeContext(ctx)
	}
	contexts = [2 * stacklessCoroWarmExecutorCount]*stacklessCoroNativeContext{}
	before, overflow, _, _ = StacklessCoroNativePoolSelfFrameCacheForTest()
	GC()
	first, _, _, _ = StacklessCoroNativePoolSelfFrameCacheForTest()
	GC()
	second, _, _, _ = StacklessCoroNativePoolSelfFrameCacheForTest()
	limit = stacklessCoroWarmExecutorCount * stacklessCoroSelfFrameChunkCacheSize
	return
}
