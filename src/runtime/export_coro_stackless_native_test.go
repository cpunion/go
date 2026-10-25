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

func StacklessCoroNativeYieldUnsupportedForTest() bool {
	return yieldStacklessCoroNativeRoot(nil, nil)
}

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

func StacklessCoroNativePoolSchedulerStacksForTest() (warm, overflow, idle int) {
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
			if ctx.schedulerG != nil {
				warm++
			}
			if !slot.CompareAndSwap(nil, ctx) {
				releaseStacklessCoroNativeContext(ctx)
			}
			break
		}
	}
	lock(&stacklessCoroNativePool.lock)
	for ctx := stacklessCoroNativePool.overflow; ctx != nil; ctx = ctx.poolNext {
		if ctx.schedulerG != nil {
			overflow++
		}
	}
	unlock(&stacklessCoroNativePool.lock)
	for i := range stacklessCoroNativeSchedulerStacks {
		if stacklessCoroNativeSchedulerStacks[i].Load() != nil {
			idle++
		}
	}
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
			releaseStacklessCoroNativeBurst(ctx)
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
func StacklessCoroNativeBurstContextsForTest(oversized bool) (before, first, second, overflow, limit, active uintptr, reserved, afterTrim uint32) {
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
		takeStacklessCoroNativeBurst(ctx)
		for n := uintptr(0); n < stacklessCoroSelfFrameChunkBudget(ctx)/class; n++ {
			p := mallocgc(typ.Size_, typ, true)
			(*stacklessCoroFusedFrameHeader)(p).parent = ctx.selfFrameChunks
			ctx.selfFrameChunks = p
			ctx.selfFrameChunkCount++
		}
	}
	for _, ctx := range contexts {
		active += stacklessCoroNativeSelfFrameCacheBytes(ctx)
	}
	reserved = stacklessCoroNativeBurstContexts.Load()
	for _, ctx := range contexts {
		releaseStacklessCoroNativeContext(ctx)
	}
	contexts = [2 * stacklessCoroWarmExecutorCount]*stacklessCoroNativeContext{}
	before, overflow, _, _ = StacklessCoroNativePoolSelfFrameCacheForTest()
	GC()
	first, _, _, _ = StacklessCoroNativePoolSelfFrameCacheForTest()
	afterTrim = stacklessCoroNativeBurstContexts.Load()
	GC()
	second, _, _, _ = StacklessCoroNativePoolSelfFrameCacheForTest()
	limit = stacklessCoroWarmExecutorCount * stacklessCoroSelfFrameChunkCacheSize
	return
}

func StacklessCoroNativeBurstReservationsForTest() uint32 {
	return stacklessCoroNativeBurstContexts.Load()
}

func StacklessCoroNativeBurstContendForTest() (held, granted, remaining uint32) {
	const n = 64
	results := make(chan bool, n)
	release, done := make(chan struct{}), make(chan struct{}, n)
	for range n {
		go func() {
			ctx := new(stacklessCoroNativeContext)
			takeStacklessCoroNativeBurst(ctx)
			results <- ctx.selfFrameBurst
			<-release
			releaseStacklessCoroNativeBurst(ctx)
			done <- struct{}{}
		}()
	}
	for range n {
		if <-results {
			granted++
		}
	}
	held = stacklessCoroNativeBurstContexts.Load()
	close(release)
	for range n {
		<-done
	}
	remaining = stacklessCoroNativeBurstContexts.Load()
	return
}

// A parked public root keeps its driver. Its completed chunks must nevertheless
// return to the ordinary budget and release the global burst reservation.
func StacklessCoroNativeBurstParkForTest() (before, after uintptr, reserved uint32) {
	StacklessCoroClearNativeTaskCachesForTest()
	ctx := acquireStacklessCoroNativeContext()
	defer releaseStacklessCoroNativeContext(ctx)
	typ := abi.TypeFor[[stacklessCoroLargeFrameChunkSize]StacklessCoroLargeSelfFrameForTest]()
	ctx.selfFrameSize = unsafe.Sizeof(StacklessCoroLargeSelfFrameForTest{})
	ctx.selfFrameChunkType = typ
	ctx.selfFrameChunkSize = roundupsize(typ.Size_, false) + gc.MallocHeaderSize
	takeStacklessCoroNativeBurst(ctx)
	for n := uintptr(0); n < stacklessCoroSelfFrameChunkBudget(ctx)/ctx.selfFrameChunkSize; n++ {
		p := mallocgc(typ.Size_, typ, true)
		(*stacklessCoroFusedFrameHeader)(p).parent = ctx.selfFrameChunks
		ctx.selfFrameChunks = p
		ctx.selfFrameChunkCount++
	}
	before = stacklessCoroNativeSelfFrameCacheBytes(ctx)
	s := &stacklessCoroScheduler{wake: make(chan struct{}, 1)}
	s.wake <- struct{}{}
	driver := stacklessCoroNativeDriver{context: ctx}
	driver.waitForWork(s)
	after = stacklessCoroNativeSelfFrameCacheBytes(ctx)
	reserved = stacklessCoroNativeBurstContexts.Load()
	return
}

// The caller disables automatic GC while taking these test-only snapshots.
func StacklessCoroNativeParkedCacheForTest() (count int, bytes uintptr) {
	for i := range stacklessCoroNativeParkedCaches {
		if ctx := stacklessCoroNativeParkedCaches[i].Load(); ctx != nil {
			count++
			bytes += stacklessCoroNativeSelfFrameCacheBytes(ctx)
		}
	}
	return
}

func StacklessCoroNativeRegisteredCacheForTest(ready chan<- uintptr, release <-chan struct{}, done chan<- bool) {
	ctx := acquireStacklessCoroNativeContext()
	typ := abi.TypeFor[[4]StacklessCoroLargeSelfFrameForTest]()
	ctx.selfFrameSize = unsafe.Sizeof(StacklessCoroLargeSelfFrameForTest{})
	ctx.selfFrameChunkType = typ
	ctx.selfFrameChunkSize = roundupsize(typ.Size_, false) + gc.MallocHeaderSize
	p := mallocgc(typ.Size_, typ, true)
	ctx.selfFrameChunks = p
	ctx.selfFrameChunkCount = 1
	// Cache expiry must leave pending driver state intact.
	sentinel := new(stacklessCoroOperation)
	ctx.deferredSleep = sentinel
	ctx.deferredSleepWhen = 7
	slot := registerStacklessCoroNativeParkedCache(ctx)
	ready <- stacklessCoroNativeSelfFrameCacheBytes(ctx)
	<-release
	if slot != nil {
		slot.Store(nil)
	}
	valid := ctx.selfFrameChunks == nil && ctx.selfFrameChunkCount == 0 && ctx.deferredSleep == sentinel && ctx.deferredSleepWhen == 7 && ctx.gState.native == unsafe.Pointer(ctx)
	ctx.deferredSleep = nil
	ctx.deferredSleepWhen = 0
	releaseStacklessCoroNativeContext(ctx)
	done <- valid
}

type StacklessCoroNativeParkRoundTrip struct {
	driver    stacklessCoroNativeDriver
	scheduler *stacklessCoroScheduler
}

func StacklessCoroNativeParkRoundTripForTest() *StacklessCoroNativeParkRoundTrip {
	ctx := acquireStacklessCoroNativeContext()
	typ := abi.TypeFor[[4]StacklessCoroLargeSelfFrameForTest]()
	ctx.selfFrameSize = unsafe.Sizeof(StacklessCoroLargeSelfFrameForTest{})
	ctx.selfFrameChunkType = typ
	ctx.selfFrameChunkSize = roundupsize(typ.Size_, false) + gc.MallocHeaderSize
	ctx.selfFrameChunks = mallocgc(typ.Size_, typ, true)
	ctx.selfFrameChunkCount = 1
	return &StacklessCoroNativeParkRoundTrip{driver: stacklessCoroNativeDriver{context: ctx}, scheduler: &stacklessCoroScheduler{wake: make(chan struct{}, 1)}}
}
func (r *StacklessCoroNativeParkRoundTrip) Wait() {
	r.scheduler.wake <- struct{}{}
	r.driver.waitForWork(r.scheduler)
}
func (r *StacklessCoroNativeParkRoundTrip) Close() {
	clearStacklessCoroNativePayloadCache(r.driver.context)
	releaseStacklessCoroNativeContext(r.driver.context)
}
